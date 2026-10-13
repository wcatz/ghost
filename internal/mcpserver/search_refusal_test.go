package mcpserver

import (
	"context"
	"strings"
	"testing"
)

// TestGhostMemorySearch_RefusesAnUnknownCategory: every other tool that takes a
// category refuses one outside the eight with the allowed list (#1041/#1079).
// Search used to hand it to the assembler, whose category predicate dropped
// every candidate and answered "nothing found passed the category filter" with
// advice to raise the limit — a confident false negative for a plural or
// capitalised category, which is a natural slip. The refusal must reach the
// explain and as_of paths too, since they run the same request.
func TestGhostMemorySearch_RefusesAnUnknownCategory(t *testing.T) {
	_, session := newCapSession(t)
	r := callTool(t, session, "ghost_memory_save", map[string]any{
		"project_id": "test-project", "content": "database configuration replication quorum", "category": "gotcha",
	})
	if r.IsError {
		t.Fatalf("save: %s", resultText(r))
	}
	paths := []struct {
		name  string
		extra map[string]any
	}{
		{"plain", nil},
		{"explain", map[string]any{"explain": true}},
		{"as_of", map[string]any{"as_of": "2030-01-01T00:00:00Z"}},
	}
	for _, p := range paths {
		for _, cat := range []string{"gotchas", "Gotcha"} {
			args := map[string]any{"project_id": "test-project", "query": "database configuration", "category": cat}
			for k, v := range p.extra {
				args[k] = v
			}
			res := callTool(t, session, "ghost_memory_search", args)
			out := resultText(res)
			if !res.IsError || !strings.Contains(out, "must be one of") {
				t.Errorf("%s category=%q: an unknown category was answered, not refused (isError=%v):\n%s", p.name, cat, res.IsError, out)
			}
			if strings.Contains(out, "[ghost:outcome=") {
				t.Errorf("%s category=%q: a refused request still rendered a verdict line:\n%s", p.name, cat, out)
			}
		}
	}
	// The valid spelling still answers, so the refusal is about the value.
	ok := callTool(t, session, "ghost_memory_search", map[string]any{
		"project_id": "test-project", "query": "database configuration", "category": "gotcha",
	})
	if ok.IsError || !strings.Contains(resultText(ok), "replication quorum") {
		t.Errorf("a valid category no longer answers (isError=%v):\n%s", ok.IsError, resultText(ok))
	}
}

// TestGhostMemorySearch_RefusesAnUnregisteredProject: a project name Ghost
// cannot resolve used to be searched as `_global` alone, with nothing in the
// answer saying the named project was never consulted — "widen the limit" on a
// miss, or a `_global` row returned as an answerable hit for the misspelt name.
// It is refused as ghost_task_list and ghost_decisions_list refuse it (#1179),
// on the plain, explain and as_of paths alike, with and without a `_global` row
// that would otherwise have matched.
func TestGhostMemorySearch_RefusesAnUnregisteredProject(t *testing.T) {
	for _, withGlobal := range []bool{false, true} {
		srv, session := newCapSession(t)
		srv.SetEmbedder(&mockEmbedder{}, make(chan string, 1))
		r := callTool(t, session, "ghost_memory_save", map[string]any{
			"project_id": "test-project", "content": "database configuration replication quorum", "category": "gotcha",
		})
		if r.IsError {
			t.Fatalf("save: %s", resultText(r))
		}
		if withGlobal {
			g := callTool(t, session, "ghost_save_global", map[string]any{
				"content": "database configuration always uses connection pooling", "category": "convention",
			})
			if g.IsError {
				t.Fatalf("save_global: %s", resultText(g))
			}
		}
		cases := []struct {
			name  string
			extra map[string]any
		}{
			{"plain", nil},
			{"explain", map[string]any{"explain": true}},
			{"as_of", map[string]any{"as_of": "2030-01-01T00:00:00Z"}},
		}
		for _, c := range cases {
			args := map[string]any{"project_id": "test-projetc", "query": "database configuration"}
			for k, v := range c.extra {
				args[k] = v
			}
			res := callTool(t, session, "ghost_memory_search", args)
			out := resultText(res)
			if !res.IsError || !strings.Contains(out, `project "test-projetc" not found`) {
				t.Errorf("global=%v %s: an unregistered project was answered rather than refused (isError=%v):\n%s", withGlobal, c.name, res.IsError, out)
			}
			for _, bad := range []string{"widen the limit", "raise the limit", "connection pooling", "[ghost:outcome="} {
				if strings.Contains(out, bad) {
					t.Errorf("global=%v %s: the answer for an unregistered project carries %q:\n%s", withGlobal, c.name, bad, out)
				}
			}
		}
		// The refusal registers nothing: the misspelt name is still unknown.
		projects, err := srv.store.ListProjects(context.Background())
		if err != nil {
			t.Fatalf("ListProjects: %v", err)
		}
		for _, p := range projects {
			if p.ID == "test-projetc" || p.Name == "test-projetc" {
				t.Errorf("global=%v: a refused search registered the misspelt project %q", withGlobal, p.ID)
			}
		}
		// A registered project still answers.
		ok := callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "database configuration"})
		if ok.IsError || !strings.Contains(resultText(ok), "replication quorum") {
			t.Errorf("global=%v: the registered project no longer answers (isError=%v):\n%s", withGlobal, ok.IsError, resultText(ok))
		}
		// And so does the global project by name, once it exists: the refusal is
		// for a name nothing resolves, not for `_global`.
		if withGlobal {
			g := callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "_global", "query": "database configuration"})
			if g.IsError || !strings.Contains(resultText(g), "connection pooling") {
				t.Errorf("searching _global by name no longer answers (isError=%v):\n%s", g.IsError, resultText(g))
			}
		}
	}
}
