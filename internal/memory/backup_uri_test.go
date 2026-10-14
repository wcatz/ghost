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
	"syscall"
	"testing"
)

// TestBackupToAFileColonNameWritesThatLiteralFile: the connection the vacuum
// runs on was opened through a file: URI, so SQLite reads the VACUUM INTO
// argument as a URI too. A bare "file:snap.db" handed to it names ./snap.db,
// while the reservation, the chmod, the size check and the count all act on the
// literal ./file:snap.db. The copy must land in the file that was reserved at
// 0600, and nowhere else.
func TestBackupToAFileColonNameWritesThatLiteralFile(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)

	store := backupTestStoreAt(t, filepath.Join(t.TempDir(), "ghost.db"))
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	work := t.TempDir()
	t.Chdir(work)

	res, err := store.Backup(ctx, "file:snap.db")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(work, "snap.db")); err == nil {
		t.Error("the copy was written to ./snap.db, a path nothing reserved or narrowed")
	}
	info, err := os.Stat(filepath.Join(work, "file:snap.db"))
	if err != nil {
		t.Fatalf("stat the reserved file: %v", err)
	}
	if info.Size() == 0 || info.Size() != res.Bytes {
		t.Errorf("reserved file is %d bytes, result says %d — the snapshot did not land in the file that was reserved", info.Size(), res.Bytes)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("copy mode = %#o, want 0600", got)
	}
	if res.Counts.Projects < 1 {
		t.Errorf("counted %d projects in the snapshot, want at least 1", res.Counts.Projects)
	}
}

// TestBackupToAFileColonNameDoesNotWriteThroughASymlink: with the URI reading,
// ./snap.db was the real target, and a dangling symlink there was followed — a
// full copy of the memory database written at the link's target, at the umask's
// width. Nothing may be written through it.
func TestBackupToAFileColonNameDoesNotWriteThroughASymlink(t *testing.T) {
	store := backupTestStoreAt(t, filepath.Join(t.TempDir(), "ghost.db"))
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	work := t.TempDir()
	t.Chdir(work)
	target := filepath.Join(work, "target.db")
	if err := os.Symlink(target, filepath.Join(work, "snap.db")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := store.Backup(ctx, "file:snap.db")
	if _, lerr := os.Lstat(target); lerr == nil {
		t.Error("the backup was written through the dangling symlink at ./snap.db to its target")
	}
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if got := permOf(t, filepath.Join(work, "file:snap.db")); got != 0o600 {
		t.Errorf("copy mode = %#o, want 0600", got)
	}
}

// TestBackupToMemoryNameIsALiteralFile: ":memory:" is SQLite's name for a
// private in-memory database, so a bare one sent the vacuum nowhere, the count
// then failed, and the run's empty reservation was left behind to refuse the
// retry. It is a filename like any other here.
func TestBackupToMemoryNameIsALiteralFile(t *testing.T) {
	store := backupTestStoreAt(t, filepath.Join(t.TempDir(), "ghost.db"))
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	work := t.TempDir()
	t.Chdir(work)

	res, err := store.Backup(ctx, ":memory:")
	if err != nil {
		if _, lerr := os.Lstat(filepath.Join(work, ":memory:")); lerr == nil {
			t.Fatalf("Backup failed (%v) and left its reservation behind", err)
		}
		t.Fatalf("Backup: %v", err)
	}
	info, err := os.Stat(filepath.Join(work, ":memory:"))
	if err != nil {
		t.Fatalf("stat the reserved file: %v", err)
	}
	if info.Size() == 0 || info.Size() != res.Bytes {
		t.Errorf("reserved file is %d bytes, result says %d", info.Size(), res.Bytes)
	}
	if res.Counts.Projects < 1 {
		t.Errorf("counted %d projects in the snapshot, want at least 1", res.Counts.Projects)
	}
}

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

// TestBackupToANameWithURIMetacharactersWritesThatLiteralFile: the vacuum is now
// handed a URI, so a '?', '#', '%' or space in the destination has to be
// percent-encoded on the way in. Unencoded, '?' and '#' would cut the path short
// and '%' would be decoded, and the copy would land somewhere else again.
func TestBackupToANameWithURIMetacharactersWritesThatLiteralFile(t *testing.T) {
	store := backupTestStoreAt(t, filepath.Join(t.TempDir(), "ghost.db"))
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	work := t.TempDir()
	name := "snap #1 ?x=%41.db"
	dest := filepath.Join(work, name)

	res, err := store.Backup(ctx, dest)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != name && e.Name() != ManifestPath(name) {
			t.Errorf("backup wrote %q beside the destination", e.Name())
		}
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat the reserved file: %v", err)
	}
	if info.Size() == 0 || info.Size() != res.Bytes {
		t.Errorf("reserved file is %d bytes, result says %d", info.Size(), res.Bytes)
	}
}
