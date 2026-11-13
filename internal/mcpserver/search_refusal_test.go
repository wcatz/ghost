package mcpserver

import (
	"context"
	"strings"
	"testing"
)

func TestGhostMemorySearchRefusals(t *testing.T) {
	_, session := newCapSession(t)
	if r := callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": "database configuration replication quorum", "category": "gotcha"}); r.IsError {
		t.Fatalf("save: %s", resultText(r))
	}
	for _, p := range []map[string]any{nil, {"explain": true}, {"as_of": "2030-01-01T00:00:00Z"}} {
		for _, cat := range []string{"gotchas", "Gotcha"} {
			args := map[string]any{"project_id": "test-project", "query": "database configuration", "category": cat}
			for k, v := range p {
				args[k] = v
			}
			res := callTool(t, session, "ghost_memory_search", args)
			out := resultText(res)
			if !res.IsError || !strings.Contains(out, "must be one of") || strings.Contains(out, "[ghost:outcome=") {
				t.Fatalf("category refusal %q: %q", cat, out)
			}
		}
	}
	ok := callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "database configuration", "category": "gotcha"})
	if ok.IsError || !strings.Contains(resultText(ok), "replication quorum") {
		t.Fatalf("valid category: %q", resultText(ok))
	}

	srv, session := newCapSession(t)
	srv.SetEmbedder(&mockEmbedder{}, make(chan string, 1))
	callTool(t, session, "ghost_memory_save", map[string]any{"project_id": "test-project", "content": "database configuration replication quorum", "category": "gotcha"})
	callTool(t, session, "ghost_save_global", map[string]any{"content": "database configuration always uses connection pooling", "category": "convention"})
	for _, extra := range []map[string]any{nil, {"explain": true}, {"as_of": "2030-01-01T00:00:00Z"}} {
		args := map[string]any{"project_id": "test-projetc", "query": "database configuration"}
		for k, v := range extra {
			args[k] = v
		}
		out := resultText(callTool(t, session, "ghost_memory_search", args))
		if !strings.Contains(out, `project "test-projetc" not found`) || strings.Contains(out, "connection pooling") || strings.Contains(out, "[ghost:outcome=") {
			t.Fatalf("unregistered: %q", out)
		}
	}
	projects, _ := srv.store.ListProjects(context.Background())
	for _, p := range projects {
		if p.ID == "test-projetc" || p.Name == "test-projetc" {
			t.Fatalf("registered misspelt project %q", p.ID)
		}
	}
	ok = callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "database configuration"})
	if ok.IsError || !strings.Contains(resultText(ok), "replication quorum") {
		t.Fatalf("registered project: %q", resultText(ok))
	}
	g := callTool(t, session, "ghost_memory_search", map[string]any{"project_id": "_global", "query": "database configuration"})
	if g.IsError || !strings.Contains(resultText(g), "connection pooling") {
		t.Fatalf("global search: %q", resultText(g))
	}
}
