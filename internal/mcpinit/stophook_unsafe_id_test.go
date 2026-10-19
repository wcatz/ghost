package mcpinit

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSpawnLifecycle_UnsafeProjectIDWarningWithholdsATokenShapedID pins the
// rendering of the stop hook's warning about a resolved project id that cannot
// be used in a filename. A stored id is not token-guarded, so a path-shaped id
// can carry a token-shaped segment, and the warning fires on every turn in that
// project: with the raw id in it, each turn would print the value to the hook's
// stderr. The warning must go through memory.RefusalArg, as docs/invariants.md
// requires.
//
// The control is an ordinary unsafe id. A renderer that withheld everything
// would pass the token half and leave the warning unable to say which id it
// refused, so that half asserts the id is still named.
func TestSpawnLifecycle_UnsafeProjectIDWarningWithholdsATokenShapedID(t *testing.T) {
	// Obviously fake, built from parts and shaped by the detector's own GitHub
	// rule. The slashes make the whole id unsafe for a path.
	tokenSegment := "ghp_" + strings.Repeat("a", 36)
	tokenID := "org/" + tokenSegment + "/repo"
	const ordinaryID = "team/../ordinary-escape"

	run := func(t *testing.T, id string) string {
		t.Helper()
		dataHome := isolatedHome(t)
		writeGhostConfigFile(t, "reflection:\n  auto_resolve: true\n")
		dir := filepath.Join(t.TempDir(), "proj")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		canonical, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("EvalSymlinks: %v", err)
		}
		seedProject(t, dataHome, id, canonical, "Unsafe Project")
		if safeProjectIDComponent(id) {
			t.Fatalf("test setup: %q must be unsafe for a path", id)
		}
		logs := captureLogOutput(t)
		spawnLifecycleIfConfigured(canonical, "", nil, io.Discard)
		return logs.String()
	}

	t.Run("token-shaped id is withheld", func(t *testing.T) {
		out := run(t, tokenID)
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "not a safe filename component") {
			t.Fatalf("the unsafe-id warning was not emitted:\n%s", out)
		}
		if strings.Contains(out, tokenSegment) {
			t.Errorf("the warning repeats the token-shaped segment:\n%s", out)
		}
		if strings.Contains(out, tokenID) {
			t.Errorf("the warning repeats the raw project id:\n%s", out)
		}
		if !strings.Contains(out, "project id withheld: it holds a GitHub personal access token") {
			t.Errorf("the warning does not name the withheld argument and its format:\n%s", out)
		}
	})

	t.Run("ordinary unsafe id is still named", func(t *testing.T) {
		out := run(t, ordinaryID)
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "not a safe filename component") {
			t.Fatalf("the unsafe-id warning was not emitted:\n%s", out)
		}
		if !strings.Contains(out, ordinaryID) {
			t.Errorf("the warning no longer names the id it refused:\n%s", out)
		}
		if strings.Contains(out, "withheld") {
			t.Errorf("an ordinary id was withheld:\n%s", out)
		}
	})
}
