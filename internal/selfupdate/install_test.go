package selfupdate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readIfPresent(t *testing.T, path string) ([]byte, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return data, true
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// assertAsideHoldsTheReplacedBinary checks the one assertion that holds on
// both platforms: after an install the aside is either gone, or holds exactly
// the binary it replaced and nothing else. On Windows it survives because it is
// the image this process is still running from and the OS refuses the delete;
// on Unix it is removed as soon as the install succeeds.
func assertAsideHoldsTheReplacedBinary(t *testing.T, aside, want string) {
	t.Helper()
	got, present := readIfPresent(t, aside)
	if !present {
		return
	}
	if string(got) != want {
		t.Errorf("%s holds %q, want %q", aside, got, want)
	}
}

// TestInstallNewBinaryMovesARunningTargetAside is the Windows half of #558.
func TestInstallNewBinaryMovesARunningTargetAside(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	staged := filepath.Join(dir, ".ghost-update-staged")
	writeFile(t, target, "old-binary")
	writeFile(t, staged, "new-binary")

	if _, err := installNewBinary(staged, target, true); err != nil {
		t.Fatalf("installNewBinary with the aside path: %v", err)
	}

	if got, _ := readIfPresent(t, target); string(got) != "new-binary" {
		t.Errorf("target holds %q, want the new binary", got)
	}
	assertAsideHoldsTheReplacedBinary(t, target+asideSuffix, "old-binary")
	// The staged file is consumed by the move, not copied and left behind.
	if _, present := readIfPresent(t, staged); present {
		t.Errorf("%s still exists after the move; the staged file must be renamed, not copied", staged)
	}
}

func TestInstallNewBinaryReusesTheAsideName(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	staged := filepath.Join(dir, ".ghost-update-staged")
	writeFile(t, target, "old-binary")
	writeFile(t, target+asideSuffix, "leftover-from-a-previous-upgrade")
	writeFile(t, staged, "new-binary")

	if _, err := installNewBinary(staged, target, true); err != nil {
		t.Fatalf("installNewBinary over a stale aside: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Target plus at most the aside the OS would not let us delete. Anything
	// more means the install left debris of its own.
	if len(entries) > 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %d entries, want the target and at most its aside: %v", len(entries), names)
	}
	assertAsideHoldsTheReplacedBinary(t, target+asideSuffix, "old-binary")
}

func TestInstallNewBinaryRestoresTheTargetWhenTheMoveFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	aside := target + asideSuffix
	writeFile(t, target, "old-binary")

	_, err := installNewBinary(filepath.Join(dir, "never-staged"), target, true)
	if err == nil {
		t.Fatal("expected an error when the staged file cannot be moved into place")
	}

	if got, present := readIfPresent(t, target); !present || string(got) != "old-binary" {
		t.Errorf("target holds %q (present=%v), want the original binary restored", got, present)
	}
	if _, present := readIfPresent(t, aside); present {
		t.Errorf("%s survived a failed install; the original binary is still stranded under the aside name", aside)
	}
}

func TestInstallNewBinaryRenamesDirectlyWhereThePlatformAllowsIt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	staged := filepath.Join(dir, ".ghost-update-staged")
	writeFile(t, target, "old-binary")
	writeFile(t, staged, "new-binary")

	if _, err := installNewBinary(staged, target, false); err != nil {
		t.Fatalf("installNewBinary without the aside path: %v", err)
	}

	if got, _ := readIfPresent(t, target); string(got) != "new-binary" {
		t.Errorf("target holds %q, want the new binary", got)
	}
	if _, present := readIfPresent(t, target+asideSuffix); present {
		t.Errorf("%s was created on a platform that renames over the target directly", target+asideSuffix)
	}
}

func TestReplaceLeavesTheOldBinaryWhenAStagedWriteIsInterrupted(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	writeFile(t, target, "old-binary")

	original := writeStaged
	t.Cleanup(func() { writeStaged = original })
	writeStaged = func(f *os.File, newBinary []byte) error {
		if _, err := f.Write(newBinary[:len(newBinary)/2]); err != nil {
			return err
		}
		return errors.New("no space left on device")
	}

	_, err := Replace(target, []byte("new-binary"))
	if err == nil {
		t.Fatal("expected Replace to report an interrupted staged write")
	}

	if got, present := readIfPresent(t, target); !present || string(got) != "old-binary" {
		t.Errorf("target holds %q (present=%v), want the original binary: a write that did not finish must not have touched the install path",
			got, present)
	}
	assertOnlyEntry(t, dir, target)
}

func TestInstallNewBinaryUsesUniqueAsideWhenFixedAsideCannotBeRemoved(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	staged := filepath.Join(dir, ".ghost-update-staged")
	writeFile(t, target, "old-binary")
	writeFile(t, target+asideSuffix, "leftover-from-a-previous-upgrade")
	writeFile(t, staged, "new-binary")

	locked := errors.New("locked")
	removeFile = func(path string) error {
		if path == target+asideSuffix {
			return locked
		}
		return os.Remove(path)
	}
	t.Cleanup(func() { removeFile = os.Remove })

	note, err := installNewBinary(staged, target, true)
	if err != nil {
		t.Fatalf("installNewBinary: %v", err)
	}
	if got, _ := readIfPresent(t, target); string(got) != "new-binary" {
		t.Errorf("target holds %q, want the new binary", got)
	}
	if note == "" {
		t.Error("expected a note reporting the locked aside")
	}
	if !strings.Contains(note, target+asideSuffix) {
		t.Errorf("note should name the fixed aside path: %q", note)
	}
	if !strings.Contains(note, "ghost mcp") {
		t.Errorf("note should mention a running ghost mcp client: %q", note)
	}
	assertAsideHoldsTheReplacedBinary(t, target+asideSuffix, "leftover-from-a-previous-upgrade")
}

func TestSweepOldAsidesKeepsNonMatchingEntries(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	writeFile(t, target, "old-binary")
	writeFile(t, filepath.Join(dir, "ghost.old"), "fixed")
	writeFile(t, filepath.Join(dir, "ghost.old-123"), "unique")
	writeFile(t, filepath.Join(dir, "ghost.old-notes"), "notes")
	if err := os.MkdirAll(filepath.Join(dir, "ghost.old-456"), 0o755); err != nil {
		t.Fatal(err)
	}

	sweepOldAsides(target)

	if _, err := os.Stat(filepath.Join(dir, "ghost.old-123")); !os.IsNotExist(err) {
		t.Error("ghost.old-123 should have been swept")
	}
	if _, err := os.Stat(filepath.Join(dir, "ghost.old-notes")); err != nil {
		t.Errorf("ghost.old-notes must survive the sweep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ghost.old-456")); err != nil {
		t.Errorf("ghost.old-456 directory must survive the sweep: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ghost.old")); err != nil {
		t.Errorf("the fixed ghost.old must survive the sweep: %v", err)
	}
}

func TestInstallNewBinaryRestoresTargetWhenUniqueAsideFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	staged := filepath.Join(dir, "never-staged")
	writeFile(t, target, "old-binary")
	writeFile(t, target+asideSuffix, "leftover-from-a-previous-upgrade")

	locked := errors.New("locked")
	removeFile = func(path string) error {
		if path == target+asideSuffix {
			return locked
		}
		return os.Remove(path)
	}
	t.Cleanup(func() { removeFile = os.Remove })

	_, err := installNewBinary(staged, target, true)
	if err == nil {
		t.Fatal("expected an error when the staged file cannot be moved into place")
	}

	if got, present := readIfPresent(t, target); !present || string(got) != "old-binary" {
		t.Errorf("target holds %q (present=%v), want the original binary restored", got, present)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), filepath.Base(target)+asideSuffix+"-") {
			t.Errorf("unique aside survived a failed install: %s", e.Name())
		}
	}
}

func assertOnlyEntry(t *testing.T, dir, target string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if asideRunningTarget && e.Name() == filepath.Base(target)+asideSuffix {
			continue
		}
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != filepath.Base(target) {
		t.Errorf("%s holds %v, want only %s: a failed upgrade must leave no debris", dir, names, filepath.Base(target))
	}
}

func TestInstallNewBinaryFailedRestoreNamesAsidePath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	staged := filepath.Join(dir, "never-staged")
	writeFile(t, target, "old-binary")

	renameFile = func(src, dst string) error {
		if src == target+asideSuffix && dst == target {
			return errors.New("restore denied")
		}
		return os.Rename(src, dst)
	}
	t.Cleanup(func() { renameFile = os.Rename })

	_, err := installNewBinary(staged, target, true)
	if err == nil {
		t.Fatal("expected an error when restoring the aside fails")
	}
	if !strings.Contains(err.Error(), target+asideSuffix) {
		t.Errorf("error should name the aside path %q, got: %v", target+asideSuffix, err)
	}
	if !strings.Contains(err.Error(), "could not be moved back") {
		t.Errorf("error should say the previous binary could not be moved back, got: %v", err)
	}
}

func TestInstallNewBinaryDoesNotReportRemovingItsOwnAside(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	staged := filepath.Join(dir, ".ghost-update-staged")
	writeFile(t, target, "old-binary")
	writeFile(t, staged, "new-binary")

	// The first call to removeFile (sweep at the start) finds the fixed
	// aside absent; the post-install call points at the aside this run just
	// created, which is the running image on Windows and always fails. Neither
	// failure may become a warning.
	var calls int
	removeFile = func(path string) error {
		if path != target+asideSuffix {
			return os.Remove(path)
		}
		calls++
		if calls == 1 {
			return os.ErrNotExist
		}
		return errors.New("locked by this process")
	}
	t.Cleanup(func() { removeFile = os.Remove })

	note, err := installNewBinary(staged, target, true)
	if err != nil {
		t.Fatalf("installNewBinary: %v", err)
	}
	if note != "" {
		t.Errorf("must not warn about removing this run's own aside, got note %q", note)
	}
	if got, _ := readIfPresent(t, target); string(got) != "new-binary" {
		t.Errorf("target holds %q, want the new binary", got)
	}
	assertAsideHoldsTheReplacedBinary(t, target+asideSuffix, "old-binary")
}
