//go:build !windows

package audit

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The sweep runs over the OS temp dir, which every local process can write
// to, so an entry wearing a sidecar's name is as untrusted as its bytes. A
// FIFO there used to block the sweep in open until a writer appeared, and a
// symlink was followed to whatever it named, which hung the lifecycle
// coordinator before its first phase. A sidecar is always a regular file
// this package wrote, so anything else is left alone, unopened.

// sweepDeadline bounds every call here that could block on a FIFO, so a
// regression fails the test instead of wedging the package run.
const sweepDeadline = 5 * time.Second

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo %s: %v", path, err)
	}
}

// ageLink moves a symlink's own modification time into the past. Chtimes
// would follow the link and age its target instead, which would leave the
// link looking fresh, and a fresh entry is skipped by age before its type is
// ever looked at.
func ageLink(t *testing.T, path string, age time.Duration) {
	t.Helper()
	when := unix.NsecToTimeval(time.Now().Add(-age).UnixNano())
	if err := unix.Lutimes(path, []unix.Timeval{when, when}); err != nil {
		t.Fatalf("age the link %s: %v", path, err)
	}
}

// withSweepDeadline runs fn and fails the test if it has not returned within
// sweepDeadline. On a timeout each FIFO in unblock is opened for writing and
// closed, which hands a blocked reader EOF. O_NONBLOCK makes that open fail
// rather than wait when nobody is reading, and the loop repeats it, because a
// sweep that blocked on one FIFO can block on the next one after it. The
// goroutine is joined before the failure, so nothing is left running.
func withSweepDeadline(t *testing.T, unblock []string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return
	case <-time.After(sweepDeadline):
	}
	for {
		for _, p := range unblock {
			if w, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = w.Close()
			}
		}
		select {
		case <-done:
			t.Fatalf("the sweep blocked for more than %s on an entry that is not a regular file", sweepDeadline)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// sweepOnce runs SweepSidecars under the deadline with the package's own age.
func sweepOnce(t *testing.T, dir string, unblock []string) int {
	t.Helper()
	var n int
	var err error
	withSweepDeadline(t, unblock, func() { n, err = SweepSidecars(dir, sidecarStaleAfter) })
	if err != nil {
		t.Fatalf("SweepSidecars: %v", err)
	}
	return n
}

func mustExist(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("the sweep removed %s: %v", what, err)
	}
}

func TestSweepSidecarsLeavesAFIFOUnopened(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "ghost-audit-planted.signals")
	mkfifo(t, fifo)
	if err := touchOlderThan(fifo, 2*sidecarStaleAfter); err != nil {
		t.Fatalf("age the FIFO: %v", err)
	}
	if n := sweepOnce(t, dir, []string{fifo}); n != 0 {
		t.Errorf("SweepSidecars removed %d entries, want 0", n)
	}
	mustExist(t, fifo, "a FIFO it never identified as a sidecar")
}

func TestSweepSidecarsLeavesASymlinkToAFIFOUnopened(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "elsewhere.fifo")
	mkfifo(t, fifo)
	link := filepath.Join(dir, "ghost-audit-planted.signals")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	ageLink(t, link, 2*sidecarStaleAfter)
	if n := sweepOnce(t, dir, []string{fifo}); n != 0 {
		t.Errorf("SweepSidecars removed %d entries, want 0", n)
	}
	mustExist(t, link, "a symlink it should not have followed")
}

// A link to a file that really is a sidecar is still not one: the product
// never writes a link, so the sweep neither reads through it nor removes it,
// and the file it names is not touched either.
func TestSweepSidecarsLeavesASymlinkToASidecarInPlace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.sidecar")
	if err := writeFileString(target, SidecarHeader+"\n"); err != nil {
		t.Fatal(err)
	}
	if err := touchOlderThan(target, 2*sidecarStaleAfter); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "ghost-audit-linked.signals")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	ageLink(t, link, 2*sidecarStaleAfter)
	if n := sweepOnce(t, dir, nil); n != 0 {
		t.Errorf("SweepSidecars removed %d entries, want 0", n)
	}
	mustExist(t, link, "a symlink, which the product never writes")
	mustExist(t, target, "the file a symlink named")
}

func TestSweepSidecarsSkipsADirectoryNamedLikeASidecar(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "ghost-audit-dir.signals")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := touchOlderThan(sub, 2*sidecarStaleAfter); err != nil {
		t.Fatal(err)
	}
	if n := sweepOnce(t, dir, nil); n != 0 {
		t.Errorf("SweepSidecars removed %d entries, want 0", n)
	}
	mustExist(t, sub, "a directory")
}

// The control: beside a planted FIFO, a stale regular sidecar is still
// swept and a fresh one is still kept.
func TestSweepSidecarsStillSweepsRegularSidecarsBesideAFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "ghost-audit-planted.signals")
	mkfifo(t, fifo)
	stale := filepath.Join(dir, "ghost-audit-stale.signals")
	fresh := filepath.Join(dir, "ghost-audit-fresh.signals")
	for _, p := range []string{stale, fresh} {
		if err := writeFileString(p, SidecarHeader+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{fifo, stale} {
		if err := touchOlderThan(p, 2*sidecarStaleAfter); err != nil {
			t.Fatal(err)
		}
	}
	if n := sweepOnce(t, dir, []string{fifo}); n != 1 {
		t.Errorf("SweepSidecars removed %d entries, want 1", n)
	}
	if _, err := os.Lstat(stale); err == nil {
		t.Error("the stale sidecar survived the sweep")
	}
	mustExist(t, fresh, "a sidecar a running child is about to read")
	mustExist(t, fifo, "a FIFO it never identified as a sidecar")
}

// The sweep's lstat skips a FIFO or a symlink before the open is reached, so
// the open's own guard is exercised here directly, as it is when an entry is
// swapped between that lstat and the open. Each case must come back refused,
// at once, and without a byte of the header.
func TestReadSidecarHeadRefusesWhatIsNotARegularFile(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "planted.fifo")
	mkfifo(t, fifo)
	sidecar := filepath.Join(dir, "real.sidecar")
	if err := writeFileString(sidecar, SidecarHeader+"\n"); err != nil {
		t.Fatal(err)
	}
	toFIFO := filepath.Join(dir, "ghost-audit-to-fifo.signals")
	toSidecar := filepath.Join(dir, "ghost-audit-to-sidecar.signals")
	for link, target := range map[string]string{toFIFO: fifo, toSidecar: sidecar} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("FIFO", func(t *testing.T) {
		var err error
		withSweepDeadline(t, []string{fifo}, func() { _, err = readSidecarHead(fifo) })
		if !errors.Is(err, errNotRegularSidecar) {
			t.Errorf("readSidecarHead(FIFO) = %v, want errNotRegularSidecar", err)
		}
	})
	for name, link := range map[string]string{"symlink to a FIFO": toFIFO, "symlink to a sidecar": toSidecar} {
		t.Run(name, func(t *testing.T) {
			var head []byte
			var err error
			withSweepDeadline(t, []string{fifo}, func() { head, err = readSidecarHead(link) })
			if err == nil {
				t.Errorf("readSidecarHead followed the link and read %q", head)
			}
		})
	}
	t.Run("regular sidecar", func(t *testing.T) {
		head, err := readSidecarHead(sidecar)
		if err != nil || string(head) != sidecarHeaderPrefix {
			t.Errorf("readSidecarHead(sidecar) = %q, %v; want %q", head, err, sidecarHeaderPrefix)
		}
	})
}

// The audit reads the same untrusted path while the coordinator holds the
// lifecycle lock, so ReadSidecar owes the sweep's guarantee: a FIFO or a
// symlink at the path is refused at once instead of blocking or being followed.
func TestReadSidecarRefusesWhatIsNotARegularFile(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "ghost-audit-planted.signals")
	mkfifo(t, fifo)
	real := filepath.Join(dir, "ghost-audit-real.signals")
	if err := writeFileString(real, SidecarHeader+"\n"); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "ghost-audit-link.signals")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	t.Run("FIFO", func(t *testing.T) {
		var err error
		withSweepDeadline(t, []string{fifo}, func() { _, err = ReadSidecar(fifo, testHasher) })
		if !errors.Is(err, errNotRegularSidecar) {
			t.Errorf("ReadSidecar(FIFO) = %v, want errNotRegularSidecar", err)
		}
	})
	t.Run("symlink to a valid sidecar", func(t *testing.T) {
		var err error
		withSweepDeadline(t, nil, func() { _, err = ReadSidecar(link, testHasher) })
		if err == nil {
			t.Error("ReadSidecar followed a symlink")
		}
	})
	t.Run("regular sidecar", func(t *testing.T) {
		if _, err := ReadSidecar(real, testHasher); err != nil {
			t.Errorf("ReadSidecar(regular) = %v", err)
		}
	})
}
