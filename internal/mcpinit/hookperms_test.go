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
