//go:build !windows

package memory

import (
	"os"
	"path/filepath"
	"testing"
)

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

// TestFchmodNoFollowNeverChmodsThroughASymlink is the #608 race. The caller's
// Lstat and the chmod are two resolutions of the same name, so anything that
// swaps a regular file for a symlink between them had its chmod applied to the
// link's target instead — a file Ghost has no business touching, and one it
// would then have made UNREADABLE to its owner, which is how a pass whose whole
// promise is "subtractive or nothing" becomes destructive.
//
// Staged as a symlink already at the name rather than as a live swap: a test
// cannot interleave with the production sequence, but the two cases have the
// same consequence and the same test, and the one a test CAN stage is the one
// that decides whether the implementation goes through the name or through a
// descriptor.
func TestFchmodNoFollowNeverChmodsThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "not-ghost's.txt")
	if err := os.WriteFile(target, []byte("someone else's file"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	setPerm(t, target, 0o644)
	link := filepath.Join(dir, "ghost.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := fchmodNoFollow(link, 0o600); err == nil {
		t.Error("fchmodNoFollow reported success on a symlink, so the mode landed on the link's target")
	}
	if got := permOf(t, target); got != 0o644 {
		t.Errorf("the link's target is %#o, want it untouched at 0644", got)
	}
	// The link itself is still a link: a fix that unlinked and re-created would
	// have tightened the file it made and left the name pointing elsewhere.
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink was replaced rather than refused: info=%v err=%v", fi, err)
	}
}
