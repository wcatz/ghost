package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestTaskAndDecisionListingsRefuseAMisspeltProject: `ghost_task_list`,
// `ghost_decisions_list` and the `ghost://project/{id}/tasks` and `/decisions`
// resources handed `ResolveProject`'s "" for an unknown name straight to the
// store, whose match on an empty project id found nothing, so a misspelt project was
// answered with the sentence a real empty project gets. An agent told that a
// project has no tasks and no decisions may re-create them. The listings now
// refuse an unresolved name the way `ghost_task_create` does.
func TestTaskAndDecisionListingsRefuseAMisspeltProject(t *testing.T) {
	db, store, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	if err := store.EnsureProject(context.Background(), "empty1", t.TempDir(), "empty-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// The registered project HOLDS a task and a decision, so an empty answer for
	// its misspelling is a statement about the wrong project.
	plantTask(t, db, "abc123", "T1", "a real task", "d", "pending")
	plantDecision(t, db, "abc123", "D1", "a real decision", "we chose A", "for a reason", "active")

	const misspelt = "test-projetc"
	const want = `project "` + misspelt + `" not found`

	readResource := func(t *testing.T, project, tail string) (string, error) {
		t.Helper()
		rr, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{
			URI: "ghost://project/" + project + "/" + tail,
		})
		if err != nil {
			return "", err
		}
		var parts []string
		for _, c := range rr.Contents {
			parts = append(parts, c.Text)
		}
		return strings.Join(parts, "\n"), nil
	}

	for _, tc := range []struct {
		tool, empty, present string
	}{
		{"ghost_task_list", "No tasks found", "a real task"},
		{"ghost_decisions_list", "No decisions found", "a real decision"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			res := callTool(t, session, tc.tool, map[string]any{"project_id": misspelt})
			out := resultText(res)
			if !res.IsError {
				t.Fatalf("a misspelt project was answered instead of refused:\n%s", out)
			}
			if strings.Contains(out, tc.empty) {
				t.Errorf("a misspelt project was answered as an empty project:\n%s", out)
			}
			if !strings.Contains(out, want) {
				t.Errorf("the refusal does not name the project (%q):\n%s", want, out)
			}
			// The controls: the right name still lists, and a registered project
			// with nothing in it still gets the empty sentence rather than a refusal.
			if ok := resultText(callTool(t, session, tc.tool, map[string]any{"project_id": "test-project"})); !strings.Contains(ok, tc.present) {
				t.Errorf("the registered project stopped listing:\n%s", ok)
			}
			empty := callTool(t, session, tc.tool, map[string]any{"project_id": "empty-project"})
			if empty.IsError || !strings.Contains(resultText(empty), tc.empty) {
				t.Errorf("a registered empty project is no longer answered as empty:\n%s", resultText(empty))
			}
		})
	}

	for _, tc := range []struct {
		tail, empty, present string
	}{
		{"tasks", "No open tasks", "a real task"},
		{"decisions", "No live decisions", "a real decision"},
	} {
		t.Run("ghost://project/{id}/"+tc.tail, func(t *testing.T) {
			out, err := readResource(t, misspelt, tc.tail)
			if err == nil {
				t.Fatalf("a misspelt project was answered instead of refused:\n%s", out)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name the project (%q): %v", want, err)
			}
			ok, err := readResource(t, "test-project", tc.tail)
			if err != nil || !strings.Contains(ok, tc.present) {
				t.Errorf("the registered project stopped listing (err %v):\n%s", err, ok)
			}
			empty, err := readResource(t, "empty-project", tc.tail)
			if err != nil || !strings.Contains(empty, tc.empty) {
				t.Errorf("a registered empty project is no longer answered as empty (err %v):\n%s", err, empty)
			}
		})
	}
}
