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
func TestMemorySaveReportsRefusedNameBinding(t *testing.T) {
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

// TestDecisionRecordReportsRefusedNameBinding is the product half of #613 on the
// decision path, and it is the test #956 turned around.
func TestDecisionRecordReportsRefusedNameBinding(t *testing.T) {
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

	res := callTool(t, session, "ghost_decision_record", map[string]any{
		"project_id": checkout,
		"title":      "Adopt the nested checkout as its own project",
		"decision":   "Bind the vendored clone to a project of its own",
		"rationale":  "Its remote contradicts the enclosing project, so a save must not bind it there",
	})
	if res.IsError {
		t.Fatalf("a decision for an unresolved project was refused: %s", resultText(res))
	}
	out := resultText(res)
	for _, want := range []string{"infra", recorded, checkout, "real-infra"} {
		if !strings.Contains(out, want) {
			t.Errorf("the decision result does not report %q:\n%s", want, out)
		}
	}

	for _, projectID := range []string{recorded, "real-infra"} {
		if count, err := srv.store.CountMemories(ctx, projectID); err != nil {
			t.Fatalf("CountMemories(%q): %v", projectID, err)
		} else if count != 0 {
			t.Errorf("a refused decision wrote %d memories into %q, want 0", count, projectID)
		}
	}
	if count, err := srv.store.CountMemories(ctx, checkout); err != nil {
		t.Fatalf("CountMemories(%q): %v", checkout, err)
	} else if count != 1 {
		t.Errorf("the decision landed in %d memories at %q, want the companion memory alone", count, checkout)
	}
	decisions, err := srv.store.ListDecisions(ctx, checkout, "", 10)
	if err != nil {
		t.Fatalf("ListDecisions: %v", err)
	}
	if len(decisions) != 1 {
		t.Errorf("the routed project holds %d decision(s), want the one just recorded", len(decisions))
	}

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

func TestMemorySaveWithoutRefusalReportsNothing(t *testing.T) {
	memory.SetDetectRemote(repo.DetectRemote)
	t.Cleanup(func() { memory.SetDetectRemote(nil) })

	const origin = "https://github.com/wcatz/infra.git"
	first := repoDir(t, "infra", origin)
	second := repoDir(t, "infra-elsewhere", origin)
	srv, session := newCapSession(t)
	srv.workingDir = ""

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
