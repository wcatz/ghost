package mcpserver

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestIssue1774TermlessQueries(t *testing.T) {
	var logBuf bytes.Buffer
	logHandler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(logHandler)

	store := testStore(t)
	srv := New(store, logger, "test")
	ch := make(chan string, 1)
	srv.SetEmbedder(&mockEmbedder{}, ch)

	ctx := context.Background()
	// Seed memories for term-less queries
	for i := 0; i < 3; i++ {
		id, _, _, err := store.Upsert(ctx, "abc123", "fact", "memory content "+string(rune('A'+i)), "mcp", 0.8, []string{})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.StoreEmbedding(ctx, id, []float32{0.1, 0.2, 0.3}, "test"); err != nil {
			t.Fatal(err)
		}
	}
	// Seed a non-Latin memory
	cyrillicID, _, _, err := store.Upsert(ctx, "abc123", "fact", "привет мир", "mcp", 0.7, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(ctx, cyrillicID, []float32{0.4, 0.5, 0.6}, "test"); err != nil {
		t.Fatal(err)
	}

	session := connectedClient(t, srv)

	// Term-less queries: should return no rows, outcome=empty, no error
	for _, q := range []string{"   ", "***", "?!", "\t\n"} {
		t.Run("search_"+strings.ReplaceAll(q, "\t", "\\t"), func(t *testing.T) {
			out := callToolText(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": q})
			if strings.Contains(out, "memory content") {
				t.Errorf("ghost_memory_search %q returned rows: %s", q, out)
			}
			if !strings.Contains(out, "outcome=empty") {
				t.Errorf("ghost_memory_search %q not empty: %s", q, out)
			}
			if strings.Contains(out, "vector_backend_unavailable") {
				t.Errorf("ghost_memory_search %q incorrectly reports vector_backend_unavailable: %s", q, out)
			}
			if strings.Contains(out, "vector leg could not run") {
				t.Errorf("ghost_memory_search %q incorrectly says vector leg could not run: %s", q, out)
			}
		})
		t.Run("search_all_"+strings.ReplaceAll(q, "\t", "\\t"), func(t *testing.T) {
			out := callToolText(t, session, "ghost_search_all", map[string]any{"query": q})
			if strings.Contains(out, "memory content") {
				t.Errorf("ghost_search_all %q returned rows: %s", q, out)
			}
			if !strings.Contains(out, "No matching") {
				t.Errorf("ghost_search_all %q unexpected: %s", q, out)
			}
		})
		// Assert no truncation warning was logged for this term-less query
		if strings.Contains(logBuf.String(), "fts query truncated") {
			t.Errorf("truncation warning logged for term-less query %q: %s", q, logBuf.String())
		}
		logBuf.Reset()
	}

	// Non-Latin query: should run both legs and return the seeded memory
	t.Run("non_latin", func(t *testing.T) {
		out := callToolText(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": "привет"})
		if !strings.Contains(out, "legs=fts:ok,vector:ok") {
			t.Errorf("non-Latin query should run both legs: %s", out)
		}
		if !strings.Contains(out, "привет мир") {
			t.Errorf("non-Latin query should return the seeded memory: %s", out)
		}
	})
	// Assert no truncation warning was logged for the non-Latin query
	if strings.Contains(logBuf.String(), "fts query truncated") {
		t.Errorf("truncation warning logged for non-Latin query: %s", logBuf.String())
	}
}

// TestIssue1774TermlessQueryNoEmbedder tests that a term-less query on a store
// with no embedder reports no_candidates (not vector_backend_unavailable).
func TestIssue1774TermlessQueryNoEmbedder(t *testing.T) {
	var logBuf bytes.Buffer
	logHandler := slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(logHandler)

	store := testStore(t)
	srv := New(store, logger, "test")
	// No embedder configured

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		id, _, _, err := store.Upsert(ctx, "abc123", "fact", "memory content "+string(rune('A'+i)), "mcp", 0.8, []string{})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.StoreEmbedding(ctx, id, []float32{0.1, 0.2, 0.3}, "test"); err != nil {
			t.Fatal(err)
		}
	}

	session := connectedClient(t, srv)

	for _, q := range []string{"   ", "***", "?!"} {
		out := callToolText(t, session, "ghost_memory_search", map[string]any{"project_id": "test-project", "query": q})
		if !strings.Contains(out, "reason=no_candidates") {
			t.Errorf("term-less query %q on no-embedder store should report no_candidates: %s", q, out)
		}
		if strings.Contains(out, "vector_backend_unavailable") {
			t.Errorf("term-less query %q on no-embedder store incorrectly reports vector_backend_unavailable: %s", q, out)
		}
		if strings.Contains(out, "vector leg could not run") {
			t.Errorf("term-less query %q on no-embedder store incorrectly says vector leg could not run: %s", q, out)
		}
	}
	// Assert no truncation warning was logged
	if strings.Contains(logBuf.String(), "fts query truncated") {
		t.Errorf("truncation warning logged for term-less query on no-embedder store: %s", logBuf.String())
	}
}

func callToolText(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	r, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", tool, err)
	}
	if len(r.Content) == 0 {
		return ""
	}
	tc, _ := r.Content[0].(*mcp.TextContent)
	return tc.Text
}
