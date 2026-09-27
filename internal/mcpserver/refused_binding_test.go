package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/repo"
)

// TestMemorySaveReportsRefusedNameBinding is the product half of #613: the save
// still succeeds — refusing to route it into a project the evidence does not
// support is right, and losing the memory is not better — but the result says
// which project kept the name and where the memory went, because "opened a new
// project" and "lost the context of the project you just saved into" are
// otherwise the same sentence to the agent reading it.
func TestMemorySaveReportsRefusedNameBinding(t *testing.T) {
	// main() wires this for the real binary; tests build stores directly.
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })

	const origin = "https://github.com/wcatz/infra.git"
	checkout := repoDir(t, "infra", origin)
	// A project of the same name, at a checkout of its own, claiming no
	// remote: the state every project upgraded from a v9 database is in, and
	// the one an unrelated clone named after it used to take over.
	recorded := filepath.Join(t.TempDir(), "git", "infra")
	if err := os.MkdirAll(recorded, 0o755); err != nil {
		t.Fatalf("create %s: %v", recorded, err)
	}
	srv, session := newCapSession(t)
	ctx := context.Background()
	if err := srv.store.EnsureProject(ctx, "real-infra", recorded, "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	res := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": checkout,
		"content":    "saved from a clone that shares the project's name",
		"category":   "fact",
	})
	if res.IsError {
		t.Fatalf("save failed: %s", resultText(res))
	}
	out := resultText(res)
	for _, want := range []string{"infra", recorded, checkout, "real-infra"} {
		if !strings.Contains(out, want) {
			t.Errorf("save result does not report %q:\n%s", want, out)
		}
	}

	// The save is in the project it was routed to, and not in the one it could
	// not claim: a notice that also moved the memory would trade a visible
	// refusal for a silent misroute.
	for _, projectID := range []string{recorded, "real-infra"} {
		if count, err := srv.store.CountMemories(ctx, projectID); err != nil {
			t.Fatalf("CountMemories(%q): %v", projectID, err)
		} else if count != 0 {
			t.Errorf("refused save wrote %d memories into %q, want 0", count, projectID)
		}
	}
	if count, err := srv.store.CountMemories(ctx, checkout); err != nil {
		t.Fatalf("CountMemories(%q): %v", checkout, err)
	} else if count != 1 {
		t.Errorf("the save landed in %d memories at %q, want 1", count, checkout)
	}
}

// TestDecisionRecordCannotReachARefusedNameBinding pins why the notice is a
// ghost_memory_save result and not a ghost_decision_record one. This tool
// refuses a project_id that does not already resolve, so the id it hands on is
// one ResolveProject answered with — and ensureProjectFor returns that on the
// exact-id lookup, before it can derive a repository and reach the unique-name
// fallback. A test that expected the notice here would be testing a path the
// tool cannot take.
func TestDecisionRecordCannotReachARefusedNameBinding(t *testing.T) {
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })

	const origin = "https://github.com/wcatz/infra.git"
	checkout := repoDir(t, "infra", origin)
	recorded := filepath.Join(t.TempDir(), "git", "infra")
	if err := os.MkdirAll(recorded, 0o755); err != nil {
		t.Fatalf("create %s: %v", recorded, err)
	}
	srv, session := newCapSession(t)
	ctx := context.Background()
	if err := srv.store.EnsureProject(ctx, "real-infra", recorded, "infra"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// The refusal this save would have to report is one the decision path
	// cannot reach: nothing resolves the unrelated clone to the project that
	// kept the name, so the tool stops before any of it.
	res := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": checkout,
		"title":      "Adopt the nested checkout as its own project",
		"decision":   "Bind the vendored clone to a project of its own",
		"rationale":  "Its remote contradicts the enclosing project, so a save must not bind it there",
	})
	if !res.IsError {
		t.Fatalf("a decision for an unresolved project succeeded: %s", resultText(res))
	}

	// A decision under the project that does resolve still works, and says
	// nothing about a refusal that did not happen.
	res = callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": "real-infra",
		"title":      "Keep the vendored clone out",
		"decision":   "Record a decision under the real project",
		"rationale":  "It is the project that holds the name and the checkout",
	})
	if res.IsError {
		t.Fatalf("decision record failed: %s", resultText(res))
	}
	if out := resultText(res); strings.Contains(out, "instead") {
		t.Errorf("decision result reported a refusal that cannot happen here:\n%s", out)
	}
}

// TestMemorySaveWithoutRefusalReportsNothing pins the quiet side of the same
// contract: an ordinary save under a name, and an ordinary second save that
// repository identity joins to the project it already had, must read exactly
// as they did before. A notice on every result is a notice nobody reads.
func TestMemorySaveWithoutRefusalReportsNothing(t *testing.T) {
	// main() wires this for the real binary; tests build stores directly.
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })

	const origin = "https://github.com/wcatz/infra.git"
	first := repoDir(t, "infra", origin)
	second := repoDir(t, "infra-elsewhere", origin)
	_, session := newCapSession(t)

	for i, projectID := range []string{"infra", first, second} {
		res := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": projectID,
			"content":    "saved without a refused binding",
			"category":   "fact",
		})
		if res.IsError {
			t.Fatalf("save %d failed: %s", i, resultText(res))
		}
		if out := resultText(res); strings.Contains(out, "instead") {
			t.Errorf("save %d reported a refusal that did not happen:\n%s", i, out)
		}
	}
}
