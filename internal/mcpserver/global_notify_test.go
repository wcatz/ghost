package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// subscribeTestClient connects an in-memory MCP client to srv and subscribes it
// to every uri, returning the session and a channel of the notifications that
// arrive. It is the shared setup for the notification tests: the per-URI
// subscription is confirmed live before the caller triggers a write, because a
// write's notification is emitted once and cannot be retried.
func subscribeTestClient(t *testing.T, srv *Server, uris ...string) (*mcp.ClientSession, <-chan string) {
	t.Helper()
	ctx := context.Background()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}

	updated := make(chan string, 8)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updated <- req.Params.URI
		},
	})
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	for _, uri := range uris {
		if err := session.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}); err != nil {
			t.Fatalf("Subscribe %s: %v", uri, err)
		}
	}
	awaitResourceSubscriptions(t, srv, ctx, updated, uris...)
	return session, updated
}

func newNotifyServer(t *testing.T) *Server {
	t.Helper()
	store := testStore(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(store, logger, "test")
}

// seedGlobalRow builds a server whose store already holds one `_global` memory
// and returns it with that row's id. The row is seeded directly rather than
// through a save tool so the caller can subscribe AFTER it exists: the write
// under test is the edit or pin on this row, and a preceding save would put an
// unrelated notification on the channel first.
func seedGlobalRow(t *testing.T) (*Server, string) {
	t.Helper()
	store := testStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	const id = "gseedrow"
	if _, err := store.CreateWithIDFromCorpus(ctx, memory.GlobalProjectID, id, memory.Memory{
		Category: "fact", Content: "a cross-project fact", Source: "mcp", Importance: 0.7,
	}); err != nil {
		t.Fatalf("seed global row: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(store, logger, "test"), id
}

// TestSaveGlobalNotifiesProjectContextSubscriber: ghost_save_global writes its
// row into `_global`, which is a project as well as the bucket behind
// ghost://memories/global, and ghost://project/_global/context serves exactly
// those rows. A subscriber to the context resource must hear about the write,
// not only a subscriber to the memories resource.
func TestSaveGlobalNotifiesProjectContextSubscriber(t *testing.T) {
	srv := newNotifyServer(t)
	const contextURI = "ghost://project/_global/context"
	session, updated := subscribeTestClient(t, srv, contextURI)

	ctx := context.Background()
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_save_global",
		Arguments: map[string]any{"content": "a cross-project convention", "category": "convention"},
	}); err != nil {
		t.Fatalf("CallTool ghost_save_global: %v", err)
	}
	awaitNotification(t, updated, contextURI)
}

// TestMemorySaveToGlobalNotifiesGlobalMemoriesSubscriber: ghost_memory_save with
// a project_id that resolves to `_global` pushes the project-context
// notification but used not to push ghost://memories/global, so a subscriber to
// that resource kept the pre-save list. This is the mirror of the save-global
// case above.
func TestMemorySaveToGlobalNotifiesGlobalMemoriesSubscriber(t *testing.T) {
	srv := newNotifyServer(t)
	const globalURI = "ghost://memories/global"
	session, updated := subscribeTestClient(t, srv, globalURI)

	ctx := context.Background()
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_memory_save",
		Arguments: map[string]any{"project_id": "_global", "content": "a cross-project fact", "category": "fact"},
	}); err != nil {
		t.Fatalf("CallTool ghost_memory_save: %v", err)
	}
	awaitNotification(t, updated, globalURI)
}

// TestMemoryUpdateOnGlobalNotifiesGlobalMemoriesSubscriber: a ghost_memory_update
// on a `_global` row changes the rows ghost://memories/global serves, so a
// subscriber to that resource must hear about the write, not only a subscriber to
// the project-context resource.
func TestMemoryUpdateOnGlobalNotifiesGlobalMemoriesSubscriber(t *testing.T) {
	srv, id := seedGlobalRow(t)
	const globalURI = "ghost://memories/global"
	session, updated := subscribeTestClient(t, srv, globalURI)

	ctx := context.Background()
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_update",
		Arguments: map[string]any{
			"project_id": memory.GlobalProjectID,
			"memory_id":  id,
			"content":    "a cross-project fact, corrected",
		},
	}); err != nil {
		t.Fatalf("CallTool ghost_memory_update: %v", err)
	}
	awaitNotification(t, updated, globalURI)
}

// TestMemoryPinOnGlobalNotifiesGlobalMemoriesSubscriber: pinning a `_global` row
// changes the bucket's pinned ordering, which ghost://memories/global renders, so
// the bucket's subscriber must hear about it too.
func TestMemoryPinOnGlobalNotifiesGlobalMemoriesSubscriber(t *testing.T) {
	srv, id := seedGlobalRow(t)
	const globalURI = "ghost://memories/global"
	session, updated := subscribeTestClient(t, srv, globalURI)

	ctx := context.Background()
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_pin",
		Arguments: map[string]any{
			"project_id": memory.GlobalProjectID,
			"memory_id":  id,
			"pinned":     true,
		},
	}); err != nil {
		t.Fatalf("CallTool ghost_memory_pin: %v", err)
	}
	awaitNotification(t, updated, globalURI)
}
