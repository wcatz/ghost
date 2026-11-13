package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGhostMemorySavePinBehavior(t *testing.T) {
	store := testStore(t)
	srv := New(store, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})), "test")
	session := connectedClient(t, srv)
	ctx := context.Background()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_memory_save", Arguments: map[string]any{
		"project_id": "test-project", "content": "the stop hook spawns the lifecycle chain detached", "category": "architecture", "pin": true,
	}})
	if err != nil || result.IsError {
		t.Fatalf("pin save: err=%v res=%v", err, result)
	}
	all, _ := store.GetAll(ctx, "abc123", 10)
	if len(all) != 1 || !all[0].Pinned || !strings.Contains(toolText(t, result), "pinned") {
		t.Fatalf("expected pinned row/result: %+v %q", all, toolText(t, result))
	}

	const first = "the harness owns its own authentication, so Ghost adds no API key"
	existing, _, _, err := store.Upsert(ctx, "abc123", "fact", first, "mcp", 0.6, nil)
	if err != nil {
		t.Fatal(err)
	}
	fold, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_memory_save", Arguments: map[string]any{
		"project_id": "test-project", "content": "the CLI harness owns its own authentication, so Ghost adds no API key", "category": "fact", "pin": true,
	}})
	if err != nil || fold.IsError {
		t.Fatalf("fold: err=%v res=%v", err, fold)
	}
	text := toolText(t, fold)
	if strings.Contains(text, "likely duplicate") && !strings.Contains(text[strings.Index(text, "pinned"):], existing) {
		t.Fatalf("fold pin did not name existing row: %q", text)
	}

	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_memory_save", Arguments: map[string]any{
		"project_id": "test-project", "content": "the reflect prompt is written to stdin, never to argv", "category": "gotcha",
	}})
	if err != nil || result.IsError {
		t.Fatalf("unpin save: err=%v res=%v", err, result)
	}
	all, _ = store.GetAll(ctx, "abc123", 20)
	found := false
	for _, m := range all {
		if strings.Contains(m.Content, "stdin, never to argv") {
			found = true
			if m.Pinned || strings.Contains(toolText(t, result), "pinned") {
				t.Fatal("unpinned save was pinned")
			}
		}
	}
	if !found {
		t.Fatal("unpinned row not found")
	}

	bad, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_memory_save", Arguments: map[string]any{
		"project_id": "test-project", "content": "a memory whose pin argument is a string", "pin": "true",
	}})
	if err != nil || !bad.IsError {
		t.Fatalf("non-boolean pin: err=%v res=%v", err, bad)
	}
}

func toolText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range result.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}
