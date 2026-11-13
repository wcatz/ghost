package memory

import (
	"os"
	"path/filepath"
	"testing"
)

// preMigrateName builds the only shape prune is allowed to consider: the
// database's base name, the exact ".pre-migrate-" infix, and nothing but
// digits after it.
func preMigrateName(dbPath, suffix string) string {
	return filepath.Base(dbPath) + ".pre-migrate-" + suffix
}

// TestPrunePreMigrateBackupsKeepsNewestThree: a data directory that has seen a
// dozen schema upgrades must not keep every copy forever. After the newest
// backup exists, only the newest three (by the numeric suffix, which is a unix
// timestamp — "1003" is newer than "999", and sorting those as strings gets it
// backwards) stay on disk.
func TestPrunePreMigrateBackupsKeepsNewestThree(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")

	// Chosen so lexicographic order disagrees with numeric order: a string
	// sort descending would keep 999, 1003, 1002 and throw away 1001.
	for _, s := range []string{"999", "1000", "1001", "1002", "1003"} {
		if err := os.WriteFile(filepath.Join(dir, preMigrateName(dbPath, s)), []byte("old"), 0o600); err != nil {
			t.Fatalf("seed backup %s: %v", s, err)
		}
	}

	prunePreMigrateBackups(dbPath, "")

	for _, s := range []string{"1001", "1002", "1003"} {
		if _, err := os.Lstat(filepath.Join(dir, preMigrateName(dbPath, s))); err != nil {
			t.Errorf("backup %s must be kept (it is one of the newest %d): %v", s, preMigrateBackupKeep, err)
		}
	}
	for _, s := range []string{"999", "1000"} {
		if _, err := os.Lstat(filepath.Join(dir, preMigrateName(dbPath, s))); !os.IsNotExist(err) {
			t.Errorf("backup %s must be pruned, Lstat err = %v", s, err)
		}
	}
}

// TestPrunePreMigrateBackupsIgnoresEverythingElse: the glob is deliberately
// narrow. Backups taken by hand, files that merely start with the infix, and
// anything that is not a plain file are none of this function's business —
// deleting a symlink would remove the link the user placed, and following one
// would delete its target.
func TestPrunePreMigrateBackupsIgnoresEverythingElse(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")

	untouched := []string{
		"ghost.db",
		"ghost.db-wal",
		"ghost.db.backup-1700000000",
		"ghost.db.pre-0.33.0-1700000000",
		preMigrateName(dbPath, "abc"),      // suffix is not a number
		preMigrateName(dbPath, "1003.bak"), // trailing junk after the number
		preMigrateName(dbPath, ""),         // no suffix at all
	}
	for _, name := range untouched {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("keep"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	// A directory wearing the name: Lstat reports a dir, not a regular file.
	dirName := preMigrateName(dbPath, "8888")
	if err := os.Mkdir(filepath.Join(dir, dirName), 0o700); err != nil {
		t.Fatalf("seed directory %s: %v", dirName, err)
	}
	// A symlink wearing the name, pointing at a real file that must survive.
	// Its stamp is deliberately the OLDEST shape: an implementation that
	// follows the link with Stat instead of reading it with Lstat counts it
	// as the newest-looking candidate pool's tail and deletes the link.
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatalf("seed target: %v", err)
	}
	linkName := preMigrateName(dbPath, "1")
	if err := os.Symlink(target, filepath.Join(dir, linkName)); err != nil {
		t.Fatalf("seed symlink: %v", err)
	}
	// And one whose stamp would make it the NEWEST entry, so the other
	// direction — keeping the link and deleting a real backup it displaced —
	// is covered too.
	newestLink := preMigrateName(dbPath, "9999")
	if err := os.Symlink(target, filepath.Join(dir, newestLink)); err != nil {
		t.Fatalf("seed newest symlink: %v", err)
	}
	// Enough numeric backups that the prune actually reaches for older files.
	for _, s := range []string{"1000", "1001", "1002", "1003"} {
		if err := os.WriteFile(filepath.Join(dir, preMigrateName(dbPath, s)), []byte("old"), 0o600); err != nil {
			t.Fatalf("seed backup %s: %v", s, err)
		}
	}

	prunePreMigrateBackups(dbPath, "")

	for _, name := range append(untouched, dirName, linkName, newestLink) {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s must be left alone: %v", name, err)
		}
	}
	// A link that counted as a candidate would have displaced one of these:
	// the three newest REAL backups are what the cap is protecting.
	for _, s := range []string{"1001", "1002", "1003"} {
		if _, err := os.Lstat(filepath.Join(dir, preMigrateName(dbPath, s))); err != nil {
			t.Errorf("backup %s must be kept: %v", s, err)
		}
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "target" {
		t.Errorf("the symlink's target must survive, read %q, err = %v", b, err)
	}
	if fi, err := os.Lstat(filepath.Join(dir, linkName)); err != nil {
		t.Errorf("symlink must still exist: %v", err)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink was replaced by a regular file (mode %v) — it was followed, not skipped", fi.Mode())
	}
}
