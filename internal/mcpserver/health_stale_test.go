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

// TestHealthSeparatesStaleFromUnembedded: `ghost_health` must not collapse the
// two halves of "awaiting re-embed" into one number. A stale row (a vector
// written under a retired model, width or task prefix) means the re-embed
// worker has work queued and vector search is missing those memories until it
// finishes; an unembedded row means no vector has ever existed, which is the
// state of a worker that never ran. An operator diagnosing "search seems
// broken" needs to tell those apart without opening the database.
func TestHealthSeparatesStaleFromUnembedded(t *testing.T) {
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	// ghost_health only reports embedding coverage when an embedder is wired.
	srv.SetEmbedder(&mockEmbedder{}, make(chan string, 1))

	ctx := context.Background()
	const (
		current = "nomic-embed-text:v1.5:768+prefix"
		staleID = "nomic-embed-text:v1.5:384+prefix"
	)
	store.SetEmbeddingIdentity(current)

	create := func(content string) string {
		t.Helper()
		id, err := store.Create(ctx, "abc123", memory.Memory{
			Category: "fact", Content: content, Importance: 0.5, Source: "mcp",
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", content, err)
		}
		return id
	}
	covered := create("covered by the current vector space")
	retired := create("written by the retired vector space")
	create("never embedded at all")

	if err := store.StoreEmbedding(ctx, covered, []float32{1, 0, 0}, current); err != nil {
		t.Fatalf("StoreEmbedding(covered): %v", err)
	}
	if err := store.StoreEmbedding(ctx, retired, []float32{1, 0, 0}, staleID); err != nil {
		t.Fatalf("StoreEmbedding(retired): %v", err)
	}

	session := connectedClient(t, srv)
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ghost_health"})
	if err != nil {
		t.Fatalf("CallTool ghost_health: %v", err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("ghost_health returned %T, want text", result.Content[0])
	}

	if !strings.Contains(text.Text, "1/3 memories embedded") {
		t.Errorf("ghost_health does not report the current-identity count as covered:\n%s", text.Text)
	}
	if !strings.Contains(text.Text, "1 stale") {
		t.Errorf("ghost_health does not report the stale rows (written under a retired vector space):\n%s", text.Text)
	}
	if !strings.Contains(text.Text, "1 unembedded") {
		t.Errorf("ghost_health does not report the never-embedded rows separately:\n%s", text.Text)
	}
}
