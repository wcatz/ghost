package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestTaskAndDecisionListingsRefuseAMisspeltProject(t *testing.T) {
	db, store, srv := newStoreWithDB(t)
	session := connectedClient(t, srv)
	if err := store.EnsureProject(context.Background(), "empty1", t.TempDir(), "empty-project"); err != nil {
		t.Fatal(err)
	}
	plantTask(t, db, "abc123", "T1", "a real task", "d", "pending")
	plantDecision(t, db, "abc123", "D1", "a real decision", "we chose A", "for a reason", "active")
	const misspelt = "test-projetc"
	want := `project "` + misspelt + `" not found`
	readResource := func(project, tail string) (string, error) {
		rr, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "ghost://project/" + project + "/" + tail})
		if err != nil {
			return "", err
		}
		var parts []string
		for _, c := range rr.Contents {
			parts = append(parts, c.Text)
		}
		return strings.Join(parts, "\n"), nil
	}
	for _, tc := range []struct{ tool, empty, present string }{
		{"ghost_task_list", "No tasks found", "a real task"},
		{"ghost_decisions_list", "No decisions found", "a real decision"},
	} {
		res := callTool(t, session, tc.tool, map[string]any{"project_id": misspelt})
		out := resultText(res)
		if !res.IsError || strings.Contains(out, tc.empty) || !strings.Contains(out, want) {
			t.Fatalf("%s: %q", tc.tool, out)
		}
		if ok := resultText(callTool(t, session, tc.tool, map[string]any{"project_id": "test-project"})); !strings.Contains(ok, tc.present) {
			t.Fatalf("%s registered: %q", tc.tool, ok)
		}
		empty := callTool(t, session, tc.tool, map[string]any{"project_id": "empty-project"})
		if empty.IsError || !strings.Contains(resultText(empty), tc.empty) {
			t.Fatalf("%s empty: %q", tc.tool, resultText(empty))
		}
	}
	for _, tc := range []struct{ tail, empty, present string }{
		{"tasks", "No open tasks", "a real task"},
		{"decisions", "No live decisions", "a real decision"},
	} {
		out, err := readResource(misspelt, tc.tail)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s misspelt: %v %q", tc.tail, err, out)
		}
		ok, err := readResource("test-project", tc.tail)
		if err != nil || !strings.Contains(ok, tc.present) {
			t.Fatalf("%s registered: %v %q", tc.tail, err, ok)
		}
		empty, err := readResource("empty-project", tc.tail)
		if err != nil || !strings.Contains(empty, tc.empty) {
			t.Fatalf("%s empty: %v %q", tc.tail, err, empty)
		}
	}
}
