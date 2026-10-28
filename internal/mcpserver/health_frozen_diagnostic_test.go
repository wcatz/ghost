package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/memory"
)

// TestGhostHealthKeepsFrozenShareWithoutReadHandle seeds frozen rows on a
// *memory.Store with no read handle and asserts ghost_health still prints the
// frozen-share line through the diagnostic fallback.
func TestGhostHealthKeepsFrozenShareWithoutReadHandle(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	if err := store.EnsureProject(context.Background(), "p1", "/tmp/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ctx := context.Background()
	if _, err := store.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "pinned-one", Source: "mcp", Importance: 0.8, Pinned: true}); err != nil {
		t.Fatalf("create pinned: %v", err)
	}
	if _, err := store.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "persistent-one", Source: "mcp", Importance: 0.8, Retention: "persistent"}); err != nil {
		t.Fatalf("create persistent: %v", err)
	}
	if _, err := store.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "normal-one", Source: "mcp", Importance: 0.5}); err != nil {
		t.Fatalf("create normal: %v", err)
	}

	session := connectedClient(t, srv)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ghost_health"})
	if err != nil {
		t.Fatalf("CallTool ghost_health: %v", err)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("ghost_health returned %T", res.Content[0])
	}
	if !strings.Contains(text.Text, "frozen") || !strings.Contains(text.Text, "excluded from consolidation") {
		t.Fatalf("expected frozen-share line without read handle, got:\n%s", text.Text)
	}
}
