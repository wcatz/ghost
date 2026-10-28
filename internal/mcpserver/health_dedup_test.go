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

func TestHealthCountsNearDuplicatePair(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	// Use existing test-project (abc123) and add a second pair there.
	// First ensure a second project for isolation.
	if err := store.EnsureProject(ctx, "proj2", "/tmp/proj2", "proj2"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id1, err := store.Create(ctx, "proj2", memory.Memory{Category: "fact", Content: "the relay listens on port 2222 in production", Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id2, err := store.Create(ctx, "proj2", memory.Memory{Category: "fact", Content: "relay listens on port 2222", Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_ = id1
	_ = id2
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	session := connectedClient(t, srv)
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_health"})
	if err != nil {
		t.Fatalf("ghost_health: %v", err)
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("want TextContent, got %T", res.Content[0])
	}
	if !strings.Contains(text.Text, "Near duplicates") {
		t.Fatalf("health does not report near duplicates:\n%s", text.Text)
	}
	if !strings.Contains(text.Text, "1 pair") {
		t.Fatalf("health should count 1 pair:\n%s", text.Text)
	}
}

func TestHealthStopsCountingAfterResolve(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "proj3", "/tmp/proj3", "proj3"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	_, err := store.Create(ctx, "proj3", memory.Memory{Category: "fact", Content: "the relay listens on port 2222 in production", Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = store.Create(ctx, "proj3", memory.Memory{Category: "fact", Content: "relay listens on port 2222", Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	mems, _ := store.GetAll(ctx, "proj3", -1)
	pairs := memory.NearDuplicatePairs(mems)
	if len(pairs) != 1 {
		t.Fatalf("want 1 pair before resolve, got %v", pairs)
	}
	// Mark one resolved
	resolved := "2026-01-01T00:00:00Z"
	mems[0].ResolvedAt = &resolved
	pairs = memory.NearDuplicatePairs(mems)
	if len(pairs) != 0 {
		t.Fatalf("resolved pair should not be counted, got %v", pairs)
	}
}

// A near-duplicate pair whose id is shaped like a credential must not print the
// id in full: the pair is counted and the ids go through memory.RefusalArg,
// the same guard the pinned-contradiction block in the same answer uses.
func TestHealthWithholdsTokenShapedNearDuplicateID(t *testing.T) {
	f := newHealthFixture(t)
	ctx := context.Background()
	ms := f.srv.store.(*memory.Store)
	id1, err := ms.CreateWithID(ctx, "abc123", tokenShapedMemoryID, memory.Memory{Category: "fact", Content: "the relay listens on port 2222 in production", Importance: 0.5, Source: "mcp"})
	if err != nil {
		t.Fatalf("CreateWithID: %v", err)
	}
	if id1 != tokenShapedMemoryID {
		t.Fatalf("CreateWithID returned %q", id1)
	}
	if _, err := ms.Create(ctx, "abc123", memory.Memory{Category: "fact", Content: "relay listens on port 2222", Importance: 0.5, Source: "mcp"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	report := f.health(t)
	if !strings.Contains(report, "Near duplicates") {
		t.Fatalf("health does not report near duplicates:\n%s", report)
	}
	if strings.Contains(report, tokenShapedMemoryID) {
		t.Errorf("report printed the token-shaped near-duplicate id in full:\n%s", report)
	}
	if !strings.Contains(report, "<memory_id withheld: it holds a GitHub personal access token>") {
		t.Errorf("report did not withhold the token-shaped near-duplicate id:\n%s", report)
	}
}
