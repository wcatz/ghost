//go:build !windows

package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Point config.DataDirPath at a temp tree and return that tree's ghost
// subdirectory, standing in for ~/.local/share/ghost. Without this the tests
// would tighten the developer's real data directory.
func fakeDataDir(t *testing.T) string {
	t.Helper()
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)
	dir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	return dir
}

func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func setPerm(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %s: %v", path, mode)
	}
}

// TestOpenDBTightensDataDirAndDatabase is the #553 report reproduced: the data
// dir asked for 0700 and the database was never given a mode at all, so a real
// install sat at 0750/0640. The chmod only ever clears bits, so every assertion
// here is an exact equality — that also pins the never-widen half of the
// contract, since a mode wider than 0600 or 0700 would fail these.
func TestOpenDBTightensDataDirAndDatabase(t *testing.T) {
	dir := fakeDataDir(t)
	dbPath := filepath.Join(dir, "ghost.db")
	if err := os.WriteFile(dbPath, nil, 0o640); err != nil {
		t.Fatalf("seed database: %v", err)
	}
	// MkdirAll never tightens an existing directory, so a dir created by an
	// earlier ghost (or a umask-widened one) keeps whatever mode it had.
	setPerm(t, dir, 0o750)
	setPerm(t, dbPath, 0o640)

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	if got := permOf(t, dir); got != 0o700 {
		t.Errorf("data dir mode = %#o, want 0700", got)
	}
	if got := permOf(t, dbPath); got != 0o600 {
		t.Errorf("database mode = %#o, want 0600", got)
	}
	// The WAL and shm files are created by SQLite beside the database and were
	// part of the same 0640 finding. They only exist while a connection is
	// open, so this has to read them before Close checkpoints the WAL away.
	for _, suffix := range []string{"-wal", "-shm"} {
		p := dbPath + suffix
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s does not exist while the database is open (err %v); the test cannot assert its mode", suffix, err)
			continue
		}
		if got := permOf(t, p); got != 0o600 {
			t.Errorf("ghost.db%s mode = %#o, want 0600", suffix, got)
		}
	}
}

// TestOpenDBDoesNotChmodThroughASymlinkedDatabase: a symlink planted as
// ghost.db must not become a chmod of whatever it points at, which may be a
// file Ghost has no business writing to. The data dir is still tightened — it
// is Ghost's own, and the symlink does not change that.
func TestOpenDBDoesNotChmodThroughASymlinkedDatabase(t *testing.T) {
	dir := fakeDataDir(t)
	target := filepath.Join(t.TempDir(), "elsewhere.db")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatalf("seed symlink target: %v", err)
	}
	dbPath := filepath.Join(dir, "ghost.db")
	if err := os.Symlink(target, dbPath); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}
	setPerm(t, dir, 0o750)
	setPerm(t, target, 0o644)

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	if got := permOf(t, target); got != 0o644 {
		t.Errorf("symlink target mode = %#o, want 0644 — chmod followed the link", got)
	}
	if got := permOf(t, dir); got != 0o700 {
		t.Errorf("data dir mode = %#o, want 0700", got)
	}
}

// TestOpenDBLeavesAlreadyTightModesAlone: a database already at 0600 is not
// rewritten. The mode it is left with must be exactly what it had, so a
// tightening that widened, or that churned the file on every open, would show
// up here.
func TestOpenDBLeavesAlreadyTightModesAlone(t *testing.T) {
	dir := fakeDataDir(t)
	dbPath := filepath.Join(dir, "ghost.db")
	if err := os.WriteFile(dbPath, nil, 0o600); err != nil {
		t.Fatalf("seed database: %v", err)
	}
	setPerm(t, dir, 0o700)
	setPerm(t, dbPath, 0o600)

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	if got := permOf(t, dir); got != 0o700 {
		t.Errorf("data dir mode = %#o, want 0700", got)
	}
	if got := permOf(t, dbPath); got != 0o600 {
		t.Errorf("database mode = %#o, want 0600", got)
	}
}

// TestOpenDBLeavesFilesItDoesNotOwnAlone: an unrelated file sitting in the data
// directory is not Ghost's to chmod. Only the paths Ghost itself creates — the
// database, its two sidecars, and the pre-migration backup — are in scope, and
// rewriting anything else would change a file a user put there.
func TestOpenDBLeavesFilesItDoesNotOwnAlone(t *testing.T) {
	dir := fakeDataDir(t)
	// A pre-migration backup from an EARLIER ghost, already on disk. The one
	// OpenDB writes during a migration is tightened as it is created (see
	// TestMigrationBackupIsTightened); a backup nobody is rewriting keeps the
	// mode it was given.
	backup := filepath.Join(dir, "ghost.db.pre-migrate-1700000000")
	if err := os.WriteFile(backup, nil, 0o644); err != nil {
		t.Fatalf("seed backup: %v", err)
	}
	setPerm(t, backup, 0o644)
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, nil, 0o644); err != nil {
		t.Fatalf("seed notes: %v", err)
	}
	setPerm(t, notes, 0o644)

	db, err := OpenDB(filepath.Join(dir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	if got := permOf(t, backup); got != 0o644 {
		t.Errorf("pre-existing pre-migration backup mode = %#o, want 0644 — a file this open did not write was chmod'ed", got)
	}
	if got := permOf(t, notes); got != 0o644 {
		t.Errorf("unrelated file mode = %#o, want 0644", got)
	}
}

// TestMigrationBackupIsTightened: the pre-migration backup is a full copy of the
// memory database, written by this same open, and VACUUM INTO names no mode for
// the file it creates — so without an explicit chmod the copy lands at the same
// width this PR exists to remove. The 0700 data directory usually shields it, but
// a database opened outside that directory has no such shield, so the backup
// itself has to be tight.
//
// The database is opened outside the DataDir on purpose, for the same reason: it
// is the case where nothing else would have covered the copy.
func TestMigrationBackupIsTightened(t *testing.T) {
	fakeDataDir(t) // XDG_DATA_HOME names a tree this database is deliberately not in
	dir := filepath.Join(t.TempDir(), "scratch", "data", "ghost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	setPerm(t, dir, 0o755)
	dbPath := filepath.Join(dir, "ghost.db")

	// Build a real store, then stamp it back to a version that needs migrating
	// so OpenDB takes the backup branch. A synthetic user_version is enough:
	// migrate() is what this is exercising the backup for, not its steps.
	rw, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := rw.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion-1)); err != nil {
		t.Fatalf("stamp user_version: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	setPerm(t, dir, 0o755)
	setPerm(t, dbPath, 0o640)

	migrated, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB (migrating): %v", err)
	}
	defer func() { _ = migrated.Close() }()

	backups, err := filepath.Glob(dbPath + ".pre-migrate-*")
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	if len(backups) != 1 {
		t.Fatalf("found %d pre-migration backups, want 1: %v", len(backups), backups)
	}
	if got := permOf(t, backups[0]); got != 0o600 {
		t.Errorf("pre-migration backup mode = %#o, want 0600 — a full copy of the database left at the umask's width", got)
	}
	// And the directory it landed in is deliberately not tightened, which is
	// the reason the backup needs its own mode.
	if got := permOf(t, dir); got != 0o755 {
		t.Errorf("foreign dir mode = %#o, want 0755", got)
	}
}

// TestChmodTightenNeverWidens covers what no end-to-end case can reach: a
// database tighter than 0600 cannot be opened at all, so OpenDB never hands
// chmodTighten an owner bit it is not allowed to touch. Setting the mode
// directly is the only way to see what happens to a database a user
// deliberately locked down — `chmod 0400 ghost.db` is a plausible thing to do,
// and the answer must be that Ghost leaves it exactly as it found it.
func TestChmodTightenNeverWidens(t *testing.T) {
	for _, tc := range []struct {
		name     string
		perm     os.FileMode
		expected os.FileMode
	}{
		{"group readable file", 0o640, 0o600},
		{"world readable file", 0o644, 0o600},
		{"already tight file", 0o600, 0o600},
		{"read-only file is not widened", 0o400, 0o400},
		{"execute-only file keeps its execute bit", 0o100, 0o100},
		{"executable file keeps its execute bit", 0o700, 0o700},
		{"group-shared dir", 0o750, 0o700},
		{"world-shared dir", 0o755, 0o700},
		{"read-only dir is not widened", 0o500, 0o500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "subject")
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			setPerm(t, path, tc.perm)
			info, err := os.Lstat(path)
			if err != nil {
				t.Fatalf("lstat: %v", err)
			}
			chmodTighten(path, info)
			if got := permOf(t, path); got != tc.expected {
				t.Errorf("mode = %#o, want %#o (was %#o)", got, tc.expected, tc.perm)
			}
		})
	}
}

// TestOpenDBDoesNotChmodAForeignDirectory: eval, cycle and bench harnesses open
// databases in scratch trees, and a user can point GHOST_SCRATCH_DIR anywhere.
// Only the configured DataDir is Ghost's own directory to tighten; the parent
// of some other database is not, however Ghost came to open it.
func TestOpenDBDoesNotChmodAForeignDirectory(t *testing.T) {
	fakeDataDir(t) // XDG_DATA_HOME now names a tree unrelated to the one below
	dir := filepath.Join(t.TempDir(), "eval", "data", "ghost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir foreign dir: %v", err)
	}
	// Seeded with chmod rather than left to the create mode: the process umask
	// decides what MkdirAll actually produces, and the assertion is about
	// whether this code changed the mode at all.
	setPerm(t, dir, 0o755)
	dbPath := filepath.Join(dir, "ghost.db")

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	if got := permOf(t, dir); got != 0o755 {
		t.Errorf("foreign dir mode = %#o, want 0755 — a directory that is not the DataDir was chmod'ed", got)
	}
	if got := permOf(t, dbPath); got != 0o600 {
		t.Errorf("database mode = %#o, want 0600", got)
	}
}
