//go:build !windows

// Not built on Windows: a ':' is not a legal filename character there, so the
// literal destinations these tests reserve cannot exist on that platform.

package memory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBackupNamesTheCopyWhenTheCountFails: a failure after the vacuum is still
// a failed backup, and the CLI prints no report for one. A full copy of the
// memory database is on disk at that point, so the error has to say where — it
// is kept rather than deleted, as a failed manifest keeps it.
func TestBackupNamesTheCopyWhenTheCountFails(t *testing.T) {
	store := backupTestStoreAt(t, filepath.Join(t.TempDir(), "ghost.db"))
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP TABLE decisions`); err != nil {
		t.Fatalf("drop a counted table so the count fails: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	_, err := store.Backup(ctx, dest)
	if err == nil {
		t.Fatal("a backup whose count fails must fail")
	}
	if !strings.Contains(err.Error(), dest) {
		t.Errorf("error = %v, want it to name the copy at %s", err, dest)
	}
	if info, serr := os.Stat(dest); serr != nil || info.Size() == 0 {
		t.Errorf("the written copy should be left in place, got stat %v", serr)
	}
}

// TestAfterVacuumFailureRemovesAnEmptyReservation: an empty file at dest is
// this run's own reservation, holding nothing, and must not survive to refuse
// the retry.
func TestAfterVacuumFailureRemovesAnEmptyReservation(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "snapshot.db")
	if err := reserveBackupPath(dest); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	cause := errors.New("count failed")
	if err := afterVacuumFailure(dest, cause); !errors.Is(err, cause) {
		t.Errorf("error = %v, want the cause", err)
	}
	if _, err := os.Lstat(dest); err == nil {
		t.Error("an empty reservation was left behind to block the retry")
	}
}
