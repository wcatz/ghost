package mcpinit

import (
	"encoding/json"
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

// TestSpawnLifecycle_UnsafeProjectIDLeavesDurableFailure pins that the stop
// hook's refusal of an unsafe project id is not stderr-only. The host owns that
// stream, so a project whose id cannot be keyed into a lifecycle filename left
// no record at all: every turn warned and nothing else remembered that the
// project was never maintained. The hook must write the same per-project
// failure marker it writes for every other spawn it refuses.
func TestSpawnLifecycle_UnsafeProjectIDLeavesDurableFailure(t *testing.T) {
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
	const id = "team/api"
	seedProject(t, dataHome, id, canonical, "Team API")
	if safeProjectIDComponent(id) {
		t.Fatalf("test setup: %q must be unsafe for a path", id)
	}

	spawnLifecycleIfConfigured(canonical, "", nil, io.Discard)

	b, err := os.ReadFile(filepath.Join(dataHome, "ghost", projectMarkerFile(id)))
	if err != nil {
		t.Fatalf("the refusal left no durable failure marker: %v", err)
	}
	var m struct {
		Project      string   `json:"project"`
		PhasesFailed []string `json:"phases_failed"`
		Error        string   `json:"error"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal marker %q: %v", b, err)
	}
	if m.Project != id {
		t.Errorf("marker project = %q, want %q", m.Project, id)
	}
	if len(m.PhasesFailed) != 1 || m.PhasesFailed[0] != "resolve" {
		t.Errorf("marker phases_failed = %v, want [resolve]", m.PhasesFailed)
	}
	if !strings.Contains(m.Error, "not a safe filename component") {
		t.Errorf("marker error = %q, want the unsafe-id explanation", m.Error)
	}
	if _, err := os.Stat(filepath.Join(dataHome, "ghost", "lifecycle.log")); !os.IsNotExist(err) {
		t.Errorf("no lifecycle process may spawn from the refused guard, lifecycle.log stat err = %v", err)
	}

	// The marker's own cause must change what the next session start says:
	// the ordinary alert offers a retry and points at the CLI binaries, and
	// neither can help while the id cannot key lifecycle bookkeeping. The
	// alert has to name the rename instead.
	alert := lifecycleFailureAlert(id, "Team API")
	if !strings.Contains(alert, "cannot be maintained unattended") {
		t.Errorf("the session-start alert does not say the project cannot be maintained:\n%s", alert)
	}
	if !strings.Contains(alert, "not a safe filename component") {
		t.Errorf("the alert does not say why:\n%s", alert)
	}
	if !strings.Contains(alert, "Rename the project id") {
		t.Errorf("the alert does not name the repair:\n%s", alert)
	}
	if strings.Contains(alert, "ghost lifecycle --project") {
		t.Errorf("the alert offers a retry that cannot help the unsafe id:\n%s", alert)
	}
	if strings.Contains(alert, "cli.*_binary") {
		t.Errorf("the alert points at CLI binaries for an id no run can use:\n%s", alert)
	}
}
