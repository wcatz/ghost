//go:build !windows

package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// TestReserveBackupPathCreatesTheCopyAt0600BeforeAnythingWritesToIt: the width
// of a backup at rest says nothing about the width it had a moment earlier.
// TightenPermissions ends at 0600 whether the file was created there or created
// at SQLite's default and narrowed, so a test that only looks at the finished
// copy cannot tell the two apart — and the difference is a window in which a
// full copy of the memory database is group- and world-readable.
//
// So the assertion is on the reservation itself: the file exists, it is empty,
// and it is already 0600 before the vacuum has written a byte. The umask is 000
// so the result is about the code and not the environment.
func TestReserveBackupPathCreatesTheCopyAt0600BeforeAnythingWritesToIt(t *testing.T) {
	old := syscall.Umask(0o000)
	defer syscall.Umask(old)

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	dest := filepath.Join(dir, "snapshot.db")
	if err := reserveBackupPath(dest); err != nil {
		t.Fatalf("reserveBackupPath: %v", err)
	}
	info, err := os.Lstat(dest)
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("the reserved file is %#o under umask 000, want 0600 — it must be created at that width, not narrowed afterwards", got)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("the reservation is not a regular file: %v", info.Mode())
	}
	if info.Size() != 0 {
		t.Errorf("the reservation holds %d bytes, want empty — nothing has been vacuumed into it yet", info.Size())
	}
}

// TestBackupIsNeverGroupReadableWhileItIsBeingWritten: the mode of a finished
// backup proves nothing about the mode it had a moment earlier, so this watches
// the file while the vacuum is running and fails if it is ever group- or
// world-readable. TightenPermissions ends at 0600 either way, which is why the
// end-to-end width assertion above cannot catch this and this one can.
//
// The fixture is sized so the vacuum takes long enough to be sampled: a few
// thousand rows, and a watcher that polls continuously rather than sleeping. A
// vacuum that finished before the first sample would make this vacuous, so the
// sample count is asserted too — a test that watched nothing has not tested
// anything, and saying so is better than a green tick.
func TestBackupIsNeverGroupReadableWhileItIsBeingWritten(t *testing.T) {
	ctx := context.Background()
	store := backupTestStore(t)
	if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// A few thousand rows, so the copy is megabytes and the vacuum is long
	// enough to sample. A hundred-row database is written in under a
	// millisecond, which is not a window anybody can lose data through and is
	// also not a window this test can see.
	big := strings.Repeat("x", 2000)
	for i := 0; i < 4000; i++ {
		if _, err := store.Create(ctx, "p1", Memory{
			Category: "fact", Content: fmt.Sprintf("%d %s", i, big), Source: "mcp",
		}); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}

	// A directory with no shield, and a umask that would let a default-width
	// create through, so the only thing preventing a wide copy is the code.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	old := syscall.Umask(0o000)
	defer syscall.Umask(old)

	dest := filepath.Join(dir, "snapshot.db")
	done := make(chan struct{})
	var (
		mu      sync.Mutex
		samples int
		tooWide []os.FileMode
	)
	watching := make(chan struct{})
	go func() {
		defer close(watching)
		for {
			select {
			case <-done:
				return
			default:
			}
			info, err := os.Lstat(dest)
			if err == nil {
				mode := info.Mode().Perm()
				mu.Lock()
				samples++
				if mode&0o077 != 0 {
					tooWide = append(tooWide, mode)
				}
				mu.Unlock()
			}
		}
	}()

	if _, err := store.Backup(ctx, dest); err != nil {
		close(done)
		<-watching
		t.Fatalf("Backup: %v", err)
	}
	close(done)
	<-watching

	mu.Lock()
	defer mu.Unlock()
	if len(tooWide) > 0 {
		t.Errorf("the copy was group- or world-readable while it was being written (%d of %d samples, first %v) — a full copy of the memory database was exposed",
			len(tooWide), samples, tooWide[0])
	}
	if samples == 0 {
		t.Error("the watcher never saw the file, so this test proves nothing; the fixture is too small to make the vacuum observable")
	}
}

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

// TestReserveBackupPathRefusesANonRegularPath: a symlink at the destination is
// refused by name rather than written through, whatever it points at.
func TestReserveBackupPathRefusesANonRegularPath(t *testing.T) {
	target := filepath.Join(t.TempDir(), "elsewhere.db")
	link := filepath.Join(t.TempDir(), "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err := reserveBackupPath(link)
	if err == nil {
		t.Fatal("reserveBackupPath must refuse a symlink")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error = %v, want it to name the path as not a regular file", err)
	}
	if _, statErr := os.Lstat(target); statErr == nil {
		t.Error("the reservation wrote through the symlink to its target")
	}
}

// TestReserveBackupPathNamesAMissingDirectory: a --out path whose parent does
// not exist has to say so, rather than surfacing SQLite's "unable to open
// database file", which reads as a problem with the database.
//
// The assertion is on the word "directory" and not on the path, because the path
// is in the message either way — a create that failed on a missing parent still
// quotes it. What the explicit check adds is the sentence that says which part of
// the path is wrong and what to do about it, and that sentence is the thing.
func TestReserveBackupPathNamesAMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	dest := filepath.Join(missing, "snapshot.db")
	err := reserveBackupPath(dest)
	if err == nil {
		t.Fatal("a reservation in a directory that does not exist must fail")
	}
	// "does not exist", not "directory": the OS's own message for a failed
	// create is "no such file or directory", so a word match on "directory"
	// would pass whether or not the explicit check ran.
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v, want it to say the directory does not exist", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %v, want it to name the missing directory", err)
	}
}

// TestVacuumIntoNeverCreatesTheCopyGroupOrWorldReadable: VACUUM INTO names no
// mode for the file it creates, so a copy created by it lands at SQLite's
// default minus the umask — 0644 with a permissive umask, and readable by
// anyone who can reach the directory. The data directory is 0700 and hides that;
// an --out destination outside it does not, and chmod'ing the finished copy
// leaves a window in which a full copy of the memory database is world-readable.
//
// The umask is set to 000 for the run so the assertion is about the code and not
// about the developer's environment, and the mode is checked as an exact
// equality — which also pins "never wider", since a mode wider than 0600 fails
// here too.
func TestVacuumIntoNeverCreatesTheCopyGroupOrWorldReadable(t *testing.T) {
	// A directory with no shield of its own, standing in for an --out outside the
	// 0700 data directory.
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	old := syscall.Umask(0o000)
	defer syscall.Umask(old)

	store := backupTestStoreAt(t, filepath.Join(t.TempDir(), "ghost.db"))
	if err := store.EnsureProject(context.Background(), "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	dest := filepath.Join(dir, "snapshot.db")
	if _, err := store.Backup(context.Background(), dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if got := permOf(t, dest); got != 0o600 {
		t.Errorf("copy mode = %#o under umask 000, want 0600 — it must be created at that width, not narrowed afterwards", got)
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
