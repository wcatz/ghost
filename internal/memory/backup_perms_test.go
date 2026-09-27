//go:build !windows

package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testBackupTime is a fixed instant so the default backup name is the same on
// every run of this test.
func testBackupTime() time.Time {
	return time.Date(2026, 9, 26, 15, 32, 7, 0, time.UTC)
}

// TestStoreBackupTightensPermissions: the snapshot is a full copy of the memory
// database, so it must not be wider than the database itself. SQLite names no
// mode for a file it creates, which is exactly how the live database ended up
// 0644 (#553); the copy needs the same pass.
//
// The assertion is an exact equality, which also pins the never-widen half of
// the contract: a mode wider than 0600 fails here just as a narrower one would
// not be handed back to 0600.
func TestStoreBackupTightensPermissions(t *testing.T) {
	dir := fakeDataDir(t)
	dbPath := filepath.Join(dir, "ghost.db")
	store := backupTestStoreAt(t, dbPath)
	if err := store.EnsureProject(context.Background(), "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// A dest inside the data dir, which is where `ghost backup` puts it by
	// default and the case TightenPermissions' directory scoping is about.
	dest := BackupFileName(dbPath, testBackupTime())
	if _, err := store.Backup(context.Background(), dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if got := permOf(t, dest); got != 0o600 {
		t.Errorf("backup mode = %#o, want 0600", got)
	}

	// A dest outside it — the --out case — is still a full copy of the
	// database and is still tightened, because the pass chmods the files it is
	// handed whether or not it owns the directory.
	outside := filepath.Join(t.TempDir(), "elsewhere.db")
	if err := os.WriteFile(outside, nil, 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}
	_ = os.Remove(outside)
	if _, err := store.Backup(context.Background(), outside); err != nil {
		t.Fatalf("Backup outside the data dir: %v", err)
	}
	if got := permOf(t, outside); got != 0o600 {
		t.Errorf("backup outside the data dir has mode %#o, want 0600", got)
	}
}
