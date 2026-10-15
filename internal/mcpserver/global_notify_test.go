package mcpserver

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	return subscribeTestClientNamed(t, srv, "test-client", uris...)
}

// subscribeTestClientNamed is subscribeTestClient with a chosen client name. A
// tool that routes its backend from the calling client's identity — the resolve
// pass picks its harness that way — needs the name set, or session routing falls
// back to probing the ambient environment.
func subscribeTestClientNamed(t *testing.T, srv *Server, name string, uris ...string) (*mcp.ClientSession, <-chan string) {
	t.Helper()
	ctx := context.Background()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := srv.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server Connect: %v", err)
	}

	updated := make(chan string, 8)
	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "0"}, &mcp.ClientOptions{
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

// TestMemoryDeleteOnGlobalNotifiesGlobalMemoriesSubscriber: deleting a `_global`
// row removes it from the rows ghost://memories/global serves, so a subscriber to
// that resource must hear about the write, not only a subscriber to the
// project-context resource.
func TestMemoryDeleteOnGlobalNotifiesGlobalMemoriesSubscriber(t *testing.T) {
	srv, id := seedGlobalRow(t)
	const globalURI = "ghost://memories/global"
	session, updated := subscribeTestClient(t, srv, globalURI)

	ctx := context.Background()
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_delete",
		Arguments: map[string]any{
			"project_id": memory.GlobalProjectID,
			"memory_id":  id,
		},
	}); err != nil {
		t.Fatalf("CallTool ghost_memory_delete: %v", err)
	}
	awaitNotification(t, updated, globalURI)
}

// TestMemoryDeletePurgeHistoryOnGlobalNotifiesGlobalMemoriesSubscriber: the
// purge_history branch of ghost_memory_delete changes the same `_global` rows, so
// the bucket's subscriber must hear about that write too.
func TestMemoryDeletePurgeHistoryOnGlobalNotifiesGlobalMemoriesSubscriber(t *testing.T) {
	srv, id := seedGlobalRow(t)
	const globalURI = "ghost://memories/global"
	session, updated := subscribeTestClient(t, srv, globalURI)

	ctx := context.Background()
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_delete",
		Arguments: map[string]any{
			"project_id":    memory.GlobalProjectID,
			"memory_id":     id,
			"purge_history": true,
		},
	}); err != nil {
		t.Fatalf("CallTool ghost_memory_delete (purge_history): %v", err)
	}
	awaitNotification(t, updated, globalURI)
}

// TestMemoryDeleteAlreadyDeletedOnGlobalNotifiesGlobalMemoriesSubscriber:
// purging the history of a `_global` row that is already deleted takes the
// purgeDeletedMemoryHistory branch, which is the same rule one step later: the
// bucket's subscriber subscribed before this call must still hear about it.
func TestMemoryDeleteAlreadyDeletedOnGlobalNotifiesGlobalMemoriesSubscriber(t *testing.T) {
	srv, id := seedGlobalRow(t)
	ctx := context.Background()
	// Delete the row WITHOUT purging, so its history — and therefore the second
	// call's ownership check — survives for the branch under test.
	if err := srv.store.DeleteWithOptions(ctx, id, memory.DeleteOptions{ProjectID: memory.GlobalProjectID}); err != nil {
		t.Fatalf("DeleteWithOptions: %v", err)
	}
	const globalURI = "ghost://memories/global"
	session, updated := subscribeTestClient(t, srv, globalURI)

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_delete",
		Arguments: map[string]any{
			"project_id":    memory.GlobalProjectID,
			"memory_id":     id,
			"purge_history": true,
		},
	}); err != nil {
		t.Fatalf("CallTool ghost_memory_delete (already-deleted purge): %v", err)
	}
	awaitNotification(t, updated, globalURI)
}

// TestMemoryPromoteNotifiesGlobalContextSubscriber: a promotion moves a row OUT
// of its project and INTO `_global`, which ghost://project/_global/context serves
// exactly. The handler pushed the source project's context and the
// ghost://memories/global bucket but not that context resource, so a subscriber
// to it kept a pre-promotion block.
func TestMemoryPromoteNotifiesGlobalContextSubscriber(t *testing.T) {
	srv := newNotifyServer(t)
	ctx := context.Background()
	id, err := srv.store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "a project fact about to go global", Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := srv.store.EnsureProject(ctx, memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	const contextURI = "ghost://project/_global/context"
	session, updated := subscribeTestClient(t, srv, contextURI)

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_memory_promote",
		Arguments: map[string]any{
			"project_id": "test-project",
			"memory_id":  id,
		},
	}); err != nil {
		t.Fatalf("CallTool ghost_memory_promote: %v", err)
	}
	awaitNotification(t, updated, contextURI)
}

// TestLinkWithdrawOnGlobalNotifiesGlobalMemoriesSubscriber: withdrawing a
// 'supersedes' edge between two `_global` rows changes the ranking that
// ghost://memories/global renders, so that bucket's subscriber must hear about
// the write, not only a subscriber to the project-context resource.
func TestLinkWithdrawOnGlobalNotifiesGlobalMemoriesSubscriber(t *testing.T) {
	srv, store := linkWithdrawServer(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	newer, older := seedLinkWithdrawal(t, store, memory.GlobalProjectID)
	const globalURI = "ghost://memories/global"
	session, updated := subscribeTestClient(t, srv, globalURI)

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "ghost_link_withdraw",
		Arguments: map[string]any{
			"project_id": memory.GlobalProjectID,
			"source_id":  newer,
			"target_id":  older,
		},
	}); err != nil {
		t.Fatalf("CallTool ghost_link_withdraw: %v", err)
	}
	awaitNotification(t, updated, globalURI)
}

// TestResolveApplyOnGlobalNotifiesGlobalMemoriesSubscriber: ghost_resolve with
// apply:true stamps resolved_at, and over the `_global` project it stamps rows
// every project injects — exactly the rows ghost://memories/global serves. The
// apply pushed only the project-context resource, so the bucket's subscriber
// kept the buried rows. This is the resolve pass that can reach `_global`: the
// targeted ghost_resolve_mark cannot, because the store refuses to mark in that
// project at all.
func TestResolveApplyOnGlobalNotifiesGlobalMemoriesSubscriber(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	srv := newNotifyServer(t)
	ctx := context.Background()
	if err := srv.store.EnsureProject(ctx, memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	// Content carrying a resolution keyword, so resolve.Prefilter keeps it and
	// the pass has a row to stamp — an unprefiltered pass writes nothing.
	if _, err := srv.store.Create(ctx, memory.GlobalProjectID, memory.Memory{
		Category: "gotcha", Content: "root cause: fixed in v2, no further action needed", Source: "mcp", Importance: 0.5,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A fake `claude` on PATH answers RESOLVED, so the classifier confirms the
	// seeded row and the apply stamps it. The client names itself claude-code so
	// session routing selects the fake rather than probing the ambient harness.
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	writeFakeClaude(t, bin, resolveAnswer)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	const globalURI = "ghost://memories/global"
	session, updated := subscribeTestClientNamed(t, srv, "claude-code", globalURI)

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ghost_resolve",
		Arguments: map[string]any{"project": memory.GlobalProjectID, "apply": true},
	})
	if err != nil {
		t.Fatalf("CallTool ghost_resolve: %v", err)
	}
	if result.IsError {
		t.Fatalf("ghost_resolve returned an error result: %+v", result.Content)
	}
	if !strings.Contains(resultText(result), "resolved 1") {
		t.Fatalf("the pass did not stamp the seeded row, so there is no write to notify about: %s", resultText(result))
	}
	awaitNotification(t, updated, globalURI)
}
