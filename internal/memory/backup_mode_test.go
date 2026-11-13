//go:build !windows

package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReserveBackupPathClaimsThePathExclusively: a second reservation of the same
// path must be refused, not handed the file the first created.
//
// What this does NOT cover is stated rather than left implied: O_EXCL is what
// closes the window between the Lstat classification and the create, and that
// window needs a second writer arriving in between, which no single-threaded test
// can produce. The Lstat classification is the guard a test can see; O_EXCL is
// the guard for the case it cannot. Both are kept, and only the second is
// unverifiable here.
func TestReserveBackupPathClaimsThePathExclusively(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if err := reserveBackupPath(dest); err != nil {
		t.Fatalf("first reserveBackupPath: %v", err)
	}
	err := reserveBackupPath(dest)
	if err == nil {
		t.Fatal("a second reservation of the same path must be refused, not handed the file the first created")
	}
	if !strings.Contains(err.Error(), "existing file") {
		t.Errorf("error = %v, want it to say the file is already there", err)
	}
}

// TestVacuumIntoReleasesItsReservationOnFailure: a vacuum that fails must not
// leave its own empty reservation behind, or the next run is refused by a file
// this run created and that contains nothing.
//
// The failure is a closed database rather than an unwritable directory: the
// reservation has to succeed for the case to mean anything, and a read-only
// parent would fail the *create* instead, before the vacuum is ever reached.
func TestVacuumIntoReleasesItsReservationOnFailure(t *testing.T) {
	store := backupTestStore(t)
	if err := store.EnsureProject(context.Background(), "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close the store so the vacuum fails: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if _, err := store.Backup(context.Background(), dest); err == nil {
		t.Fatal("a backup against a closed database must fail")
	}
	if _, err := os.Lstat(dest); err == nil {
		t.Error("a failed backup left its own empty reservation behind, so the next run is refused by a file this run created")
	}
}

// TestVacuumIntoRefusesAnExistingPathAtomically: O_EXCL is what makes the
// refusal a reservation rather than a check, and it is also what refuses a
// dangling symlink. A stat-based check reads a dangling link as an absent path
// and the copy is then written *through* it, wherever it points.
func TestVacuumIntoRefusesAnExistingPathAtomically(t *testing.T) {
	store := backupTestStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// A dangling symlink at the destination: absent to Stat, present to Lstat.
	target := filepath.Join(t.TempDir(), "elsewhere.db")
	link := filepath.Join(t.TempDir(), "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := store.Backup(ctx, link)
	if err == nil {
		t.Fatal("Backup must refuse a dangling symlink at the destination")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error = %v, want it to say the path is not a regular file", err)
	}
	if _, err := os.Lstat(target); err == nil {
		t.Error("Backup wrote through the symlink to its target")
	}
}
