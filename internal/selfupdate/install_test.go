package selfupdate

import (
	"os"
	"path/filepath"
	"runtime"
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
// A running executable's image is open without delete sharing, so renaming a
// new file over it fails with "Access is denied" and `ghost upgrade` — the
// command that repairs a machine — could never repair a Windows one. Windows
// does allow renaming the running image, so the old binary moves to a fixed
// sibling name and the new one takes the path the old one vacated.
func TestInstallNewBinaryMovesARunningTargetAside(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	staged := filepath.Join(dir, ".ghost-update-staged")
	writeFile(t, target, "old-binary")
	writeFile(t, staged, "new-binary")

	if err := installNewBinary(staged, target, true); err != nil {
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

// TestInstallNewBinaryReusesTheAsideName keeps a leftover from blocking or
// multiplying. The old image cannot be deleted while this process is still
// running from it, so a fixed name is reused on the next upgrade rather than a
// unique one that would leave a file behind per attempt.
func TestInstallNewBinaryReusesTheAsideName(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	staged := filepath.Join(dir, ".ghost-update-staged")
	writeFile(t, target, "old-binary")
	writeFile(t, target+asideSuffix, "leftover-from-a-previous-upgrade")
	writeFile(t, staged, "new-binary")

	if err := installNewBinary(staged, target, true); err != nil {
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

// TestInstallNewBinaryRestoresTheTargetWhenTheMoveFails is the safety
// property of the two-step move: the old binary goes back where it was, so a
// failure between the two renames cannot leave an install with no binary at
// all. A staged path that does not exist fails the second rename
// deterministically, with no permission or timing games.
func TestInstallNewBinaryRestoresTheTargetWhenTheMoveFails(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	aside := target + asideSuffix
	writeFile(t, target, "old-binary")

	err := installNewBinary(filepath.Join(dir, "never-staged"), target, true)
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

// TestInstallNewBinaryRenamesDirectlyWhereThePlatformAllowsIt is the Unix
// path: renaming over the target is atomic there, and moving the old image
// aside first would only open a window where the install path does not exist.
func TestInstallNewBinaryRenamesDirectlyWhereThePlatformAllowsIt(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	staged := filepath.Join(dir, ".ghost-update-staged")
	writeFile(t, target, "old-binary")
	writeFile(t, staged, "new-binary")

	if err := installNewBinary(staged, target, false); err != nil {
		t.Fatalf("installNewBinary without the aside path: %v", err)
	}

	if got, _ := readIfPresent(t, target); string(got) != "new-binary" {
		t.Errorf("target holds %q, want the new binary", got)
	}
	if _, present := readIfPresent(t, target+asideSuffix); present {
		t.Errorf("%s was created on a platform that renames over the target directly", target+asideSuffix)
	}
}

// TestInstallNewBinaryFollowsThePlatformPolicy pins the one build-tagged
// decision in the install path. The Windows CI job runs internal/mcpinit and
// internal/ai, so this package's tests never execute on Windows: without this
// assertion nothing anywhere checks that the platform whose rename fails is the
// one that gets the aside move.
func TestInstallNewBinaryFollowsThePlatformPolicy(t *testing.T) {
	if want := runtime.GOOS == "windows"; asideRunningTarget != want {
		t.Errorf("asideRunningTarget = %v on %s, want %v: a running executable cannot be renamed over on Windows, and nowhere else",
			asideRunningTarget, runtime.GOOS, want)
	}
}

// TestReplaceAppliesThePlatformPolicy drives the exported entry point so the
// wiring between Replace and the policy cannot be dropped. A Replace that passed
// the wrong answer to installNewBinary would look perfectly correct on Unix —
// the two policies agree there — and only fail on Windows, which is exactly the
// platform this guards. The leftover is the observable: Unix removes the aside
// as part of a successful install, Windows cannot remove the image it is still
// running from, and the policy says which of those to expect.
func TestReplaceAppliesThePlatformPolicy(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "ghost")
	writeFile(t, target, "old-binary")

	if err := Replace(target, []byte("new-binary")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if got, _ := readIfPresent(t, target); string(got) != "new-binary" {
		t.Fatalf("target holds %q, want the new binary", got)
	}

	_, asidePresent := readIfPresent(t, target+asideSuffix)
	if asidePresent != asideRunningTarget {
		t.Errorf("after Replace the replaced binary is at %s: present=%v, but this platform's policy is asideRunningTarget=%v",
			target+asideSuffix, asidePresent, asideRunningTarget)
	}
}
