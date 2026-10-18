package mcpserver

import (
	"context"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/provider"
)

// cancelAfterUpsertStore wraps a real store and, once an upsert has
// committed, cancels the client's call and waits until the request context the
// handler holds is done. The write has landed by then, so everything the
// handler does next runs on a cancelled request context, which is the state
// a client disconnecting right after a commit leaves behind.
type cancelAfterUpsertStore struct {
	provider.MemoryStore
	cancelCall context.CancelFunc
}

func (s cancelAfterUpsertStore) UpsertWithOptions(ctx context.Context, projectID, category, content, source string, importance float32, tags []string, opts memory.UpsertOptions) (string, string, float64, error) {
	id, dup, score, err := s.MemoryStore.UpsertWithOptions(ctx, projectID, category, content, source, importance, tags, opts)
	if err != nil || s.cancelCall == nil {
		return id, dup, score, err
	}
	s.cancelCall()
	select {
	case <-ctx.Done():
	case <-time.After(notificationWait):
	}
	return id, dup, score, err
}

// TestNotifyProjectResource_BothFormsAfterCancelledSave saves through the MCP
// tool, cancels the request after the write commits, and expects the id-form
// and the name-form subscribers to both hear about it.
func TestNotifyProjectResource_BothFormsAfterCancelledSave(t *testing.T) {
	const idURI = "ghost://project/6bdc098af7f5/context"
	const nameURI = "ghost://project/myproj/context"

	for _, tc := range []struct {
		name   string
		cancel bool
	}{
		{"request cancelled after commit", true},
		{"control: request live", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			if err := store.EnsureProject(ctx, "6bdc098af7f5", "/srv/git/myproj", "myproj"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			callCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			wrapped := cancelAfterUpsertStore{MemoryStore: store}
			if tc.cancel {
				wrapped.cancelCall = cancel
			}
			srv := New(wrapped, newNotifyServer(t).logger, "test")
			session, updated := subscribeTestClient(t, srv, idURI, nameURI)

			// The result is not the subject: a cancelled call may report its
			// own cancellation to the client while the write stays committed.
			_, _ = session.CallTool(callCtx, &mcp.CallToolParams{
				Name:      "ghost_memory_save",
				Arguments: map[string]any{"project_id": "myproj", "content": "both forms are notified", "category": "fact"},
			})

			got := map[string]bool{}
			for len(got) < 2 {
				select {
				case uri := <-updated:
					got[uri] = true
				case <-time.After(notificationWait):
					t.Fatalf("notified %v, want both %q and %q", got, idURI, nameURI)
				}
			}
			if !got[idURI] || !got[nameURI] {
				t.Fatalf("notified %v, want both %q and %q", got, idURI, nameURI)
			}
		})
	}
}
