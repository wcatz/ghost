package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGhostMemorySearch_RefusesRetentionOverAsOf(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "ghost_memory_search",
		Arguments: map[string]any{"project_id": "test-project", "query": "tunnel", "as_of": "2026-01-01T00:00:00Z", "retention": "persistent"},
	})
	var text string
	if err != nil {
		text = err.Error()
	} else {
		if !res.IsError {
			t.Fatalf("answered: %s", toolText(t, res))
		}
		text = toolText(t, res)
	}
	if !strings.Contains(text, "retention") || !strings.Contains(text, "as_of") || !strings.Contains(text, "memory_history") || strings.Contains(text, "[ghost:outcome=") {
		t.Fatalf("unexpected refusal: %q", text)
	}
}

func TestGhostMemoriesList_KeepsItsOwnScopeWhenNothingIsFiltered(t *testing.T) {
	store := testStore(t)
	if err := store.EnsureProject(context.Background(), "_global", "_global", "global"); err != nil {
		t.Fatal(err)
	}
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	ctx := context.Background()
	const shared = "every repository in this workspace uses 2-space YAML indentation"
	if _, _, _, err := store.Upsert(ctx, "_global", "convention", shared, "mcp", 0.7, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Upsert(ctx, "abc123", "fact", "the relay listens on port 2222 in staging", "mcp", 0.7, nil); err != nil {
		t.Fatal(err)
	}
	res, _ := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_memories_list", Arguments: map[string]any{"project_id": "test-project"}})
	text := toolText(t, res)
	if !strings.Contains(text, "relay listens on port 2222") || strings.Contains(text, shared) {
		t.Fatalf("unexpected scope: %q", text)
	}
	if _, err := store.DeleteProject(ctx, "abc123", true); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureProject(ctx, "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatal(err)
	}
	empty, _ := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_memories_list", Arguments: map[string]any{"project_id": "test-project"}})
	if !strings.Contains(toolText(t, empty), "no memories yet") {
		t.Fatalf("expected empty message: %q", toolText(t, empty))
	}
}
