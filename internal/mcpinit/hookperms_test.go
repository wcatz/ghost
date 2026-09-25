//go:build !windows

package mcpinit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// This file holds the mcpinit half of #553's permission contract, split out of
// hook_test.go and build-tagged because the assertions are about POSIX mode
// bits. Windows carries access in an ACL rather than in the bits os.Chmod maps
// onto read-only, memory.TightenPermissions is a deliberate no-op there, and
// the mode a Windows filesystem reports for a file the user owns is 0666 (or
// 0777 for a directory) no matter what was asked for — so asserting 0600
// would fail on a correct implementation.

// permOf reads path's permission bits with an Lstat, matching the pass's own
// symlink-safe stat.
func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// setPerm sets path's mode explicitly. A create mode is filtered through the
// process umask, and these tests are about whether this code changed the mode —
// not about what the umask happened to be — so the seeds must not depend on it.
func setPerm(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, mode)
	}
}

// looseDataDir returns a data directory holding a closed database, both at the
// widths #553 reported: 0750 for the directory, 0640 for ghost.db. The
// database is closed before returning, so its -wal and -shm are gone and
// whatever connection a test opens next is the one that recreates them.
func looseDataDir(t *testing.T) (dir, dbPath string) {
	t.Helper()
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dir = filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	dbPath = filepath.Join(dir, "ghost.db")

	rw, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := testStore(rw).EnsureProject(t.Context(), "p1", "/tmp/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// memory.OpenDB tightened both of these on its way through, so they have to
	// be loosened afterwards to reproduce the state an older build leaves.
	setPerm(t, dir, 0o750)
	setPerm(t, dbPath, 0o640)
	return dir, dbPath
}

// TestBumpSessionCountTightensPermissions: the session hook is frequently the
// only Ghost process to touch the database between two MCP sessions, so it is a
// read-write open in its own right. If it wrote without running the pass, a
// database an older build left at 0640 would be written while still readable by
// the user's whole group, and repaired only later — on some unrelated command.
// memory.OpenDB's own coverage of this cannot stand in for that path.
func TestBumpSessionCountTightensPermissions(t *testing.T) {
	dir, dbPath := looseDataDir(t)

	if n := bumpSessionCount(dbPath, "p1"); n != 1 {
		t.Fatalf("bumpSessionCount = %d, want 1", n)
	}

	if got := permOf(t, dbPath); got != 0o600 {
		t.Errorf("database mode after the session hook's write = %#o, want 0600", got)
	}
	if got := permOf(t, dir); got != 0o700 {
		t.Errorf("data dir mode after the session hook's write = %#o, want 0700", got)
	}
}

// TestBumpSessionCountTightensWALAndSHM is the same contract for the two files
// SQLite creates rather than opens. A clean close deletes both, so on the hook's
// own connection they do not exist until its INSERT recreates them — at whatever
// mode SQLite gives a new file. A pass that ran only before the open would miss
// them entirely, and the session counter this function just wrote would sit in
// a world-readable WAL until the connection closed.
//
// A live MCP server holding the same database is what keeps the -wal and -shm
// on disk past the hook in production, so the stand-in below is a second
// connection. It is read-only, which is the production case for a *second*
// connection: the stop hook's own reads, the lifecycle marker and the lifecycle
// lock all use OpenDBReadOnly, and none of them runs the pass.
//
// Scope of what this can prove, since it is narrower than it looks. Whoever
// creates the -wal and -shm, the file ends this function at 0600 — but that is
// the PRE-write pass doing it, because the holder creates both files on its way
// in, before the hook runs at all. The POST-write pass exists for the opposite
// case, where nothing else holds the database: the hook's own INSERT is then
// what creates them, and they sit at the width SQLite chose for the lifetime of
// its connection. Nothing observes them at that point — the deferred Close
// deletes them when no other connection is open — so no test outside
// bumpSessionCount can distinguish that state, and a mutation that drops the
// post-write pass survives here. It is kept because the window is real and the
// call is an Lstat each; the cost of being wrong is a world-readable WAL.
func TestBumpSessionCountTightensWALAndSHM(t *testing.T) {
	_, dbPath := looseDataDir(t)

	holder, err := memory.OpenDBReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenDBReadOnly: %v", err)
	}
	defer func() { _ = holder.Close() }()
	// sql.Open is lazy, so force the connection.
	var one int
	if err := holder.QueryRow(`SELECT count(*) FROM projects`).Scan(&one); err != nil {
		t.Fatalf("holder query: %v", err)
	}

	if n := bumpSessionCount(dbPath, "p1"); n != 1 {
		t.Fatalf("bumpSessionCount = %d, want 1", n)
	}

	for _, suffix := range []string{"-wal", "-shm"} {
		p := dbPath + suffix
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("ghost.db%s does not exist after the hook's write with a second connection open (err %v); the test cannot assert its mode", suffix, err)
			continue
		}
		if got := permOf(t, p); got != 0o600 {
			t.Errorf("ghost.db%s left at %#o, want 0600 — the hook's own connection created it", suffix, got)
		}
	}
}

// TestBumpSessionCountNoPhantomDirPerms pins the other half of the "never
// create anything" contract. The existence check comes first for a reason: had
// the permission pass run before it, a session hook on a machine with no Ghost
// database would still have found a way to make something appear in the data
// directory — here, a directory a test asserted was still group-writable, on a
// path where nothing has ever created one.
func TestBumpSessionCountNoPhantomDirPerms(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	setPerm(t, dir, 0o750)

	if n := bumpSessionCount(filepath.Join(dir, "ghost.db"), "p1"); n != 0 {
		t.Fatalf("bump on missing DB returned %d, want 0", n)
	}
	if got := permOf(t, dir); got != 0o750 {
		t.Errorf("data dir mode = %#o, want 0750 — the permission pass ran before the existence check", got)
	}
}
