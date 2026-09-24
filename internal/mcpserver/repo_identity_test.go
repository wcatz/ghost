package mcpserver

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/repo"
)

// repoDir creates a directory that git reports as an origin remote. The
// repository content is irrelevant — detection reads `git config
// remote.origin.url`, and two directories configured with the same origin are
// two checkouts of one repository as far as identity is concerned.
func repoDir(t *testing.T, name, origin string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	init := exec.Command("git", "init", "-q")
	init.Dir = dir
	if out, err := init.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	add := exec.Command("git", "remote", "add", "origin", origin)
	add.Dir = dir
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}
	return dir
}

// TestSaveAcrossCheckoutsOfOneRepositoryIsOneProject is the end-to-end proof
// that repository identity is reachable from the product's normal write path.
//
// Schema alone would not be: a column nothing populates and a resolution step
// nothing reaches are inert. This drives two real MCP saves from two different
// absolute paths that share one origin remote, and asserts the second save
// lands in the first project rather than opening a second one.
func TestSaveAcrossCheckoutsOfOneRepositoryIsOneProject(t *testing.T) {
	// main() wires this for the real binary; tests build stores directly.
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })
	const origin = "https://github.com/wcatz/ghost.git"
	checkoutA := repoDir(t, "checkout-a", origin)
	checkoutB := repoDir(t, "checkout-b", origin)

	_, session := newCapSession(t)

	for i, args := range []map[string]any{
		{"project_id": checkoutA, "content": "saved from the first checkout", "category": "fact"},
		{"project_id": checkoutB, "content": "saved from the second checkout", "category": "fact"},
	} {
		res := callTool(t, session, "ghost_memory_save", args)
		if res.IsError {
			t.Fatalf("save %d failed: %s", i, resultText(res))
		}
	}

	// Everything must be reachable from the FIRST checkout: if the two paths
	// had become separate projects, the memory saved from the second would be
	// invisible here, and the agent would be working from half the context.
	out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": checkoutA,
		"query":      "saved from checkout",
		"limit":      10,
	}))
	for _, want := range []string{"saved from the first checkout", "saved from the second checkout"} {
		if !strings.Contains(out, want) {
			t.Errorf("search from the first checkout did not return %q — the two checkouts became separate projects:\n%s", want, out)
		}
	}

	// And the reverse direction, so a merge that only works one way is caught.
	back := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": checkoutB,
		"query":      "saved from checkout",
		"limit":      10,
	}))
	for _, want := range []string{"saved from the first checkout", "saved from the second checkout"} {
		if !strings.Contains(back, want) {
			t.Errorf("search from the second checkout did not return %q:\n%s", want, back)
		}
	}
}

// TestSaveWithNamedProjectDoesNotSpawnGit guards the cost of the feature: a
// caller that names a project ("ghost", "platform-ops") gives no path to
// inspect, so detection must not run at all. Without this, every ordinary
// save would pay for a git process.
func TestSaveWithNamedProjectDoesNotSpawnGit(t *testing.T) {
	// main() wires this for the real binary; tests build stores directly.
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })
	_, session := newCapSession(t)

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project",
		"content":    "a memory saved under a plain project name",
		"category":   "fact",
	})
	if res.IsError {
		t.Fatalf("save failed: %s", resultText(res))
	}
	out := resultText(callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project",
		"query":      "plain project name",
		"limit":      5,
	}))
	if !strings.Contains(out, "a memory saved under a plain project name") {
		t.Errorf("named-project save was not retrievable:\n%s", out)
	}
}
