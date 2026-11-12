package embedding

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"
)

// failingContentStoreMem fails GetMemoryContent.
type failingContentStoreMem struct{ *mockStore }

func (failingContentStoreMem) GetMemoryContent(context.Context, string) (string, error) {
	return "", errors.New("content fetch failed")
}

// failingEmbedStoreMem fails StoreEmbeddingForContent with an error.
type failingEmbedStoreMem struct{ *mockStore }

func (failingEmbedStoreMem) GetMemoryContent(context.Context, string) (string, error) {
	return "content to embed", nil
}

func (failingEmbedStoreMem) StoreEmbeddingForContent(context.Context, string, []float32, string, string) (bool, error) {
	return false, errors.New("store failed")
}

// contentChangedStore reports different content than it stores, so
// StoreEmbeddingForContent returns false (the "content changed" path).
type contentChangedStore struct{ *mockStore }

func (contentChangedStore) GetMemoryContent(context.Context, string) (string, error) {
	return "current content", nil
}

func (c contentChangedStore) StoreEmbeddingForContent(ctx context.Context, memoryID string, vec []float32, model, content string) (bool, error) {
	c.mu.Lock()
	c.memories[memoryID] = "original content"
	c.mu.Unlock()
	return c.mockStore.StoreEmbeddingForContent(ctx, memoryID, vec, model, content)
}

// failingContentStorePending lists one pending id whose content fetch fails.
type failingContentStorePending struct {
	*mockStore
	memoryID string
}

func (f failingContentStorePending) UnembeddedMemoryIDs(context.Context, string, string, int) ([]string, error) {
	return []string{f.memoryID}, nil
}

func (failingContentStorePending) GetMemoryContent(context.Context, string) (string, error) {
	return "", errors.New("content fetch failed")
}

// TestWorkerLogsRenderAStoredMemoryIDThroughTheTokenGuard drives every worker
// log line that carries a memory id. An imported artifact can carry a
// token-shaped id, and these lines reach the MCP client's stderr.
func TestWorkerLogsRenderAStoredMemoryIDThroughTheTokenGuard(t *testing.T) {
	cases := []struct {
		name, wantMsg string
		status        int
		run           func(w *Worker, id string)
	}{
		{"get content missing", "embed: get content", http.StatusOK, func(w *Worker, id string) {
			w.store = failingContentStoreMem{newMockStore()}
			w.EmbedOne(context.Background(), id)
		}},
		{"ollama embed error", "embed: ollama", http.StatusServiceUnavailable, func(w *Worker, id string) {
			w.store.(*mockStore).memories[id] = "content to embed"
			w.EmbedOne(context.Background(), id)
		}},
		{"store embedding error", "embed: store", http.StatusOK, func(w *Worker, id string) {
			w.store = failingEmbedStoreMem{newMockStore()}
			w.EmbedOne(context.Background(), id)
		}},
		{"content changed while embedding", "embed: content changed while embedding, left pending", http.StatusOK, func(w *Worker, id string) {
			s := contentChangedStore{newMockStore()}
			s.memories[id] = "original content"
			w.store = s
			w.EmbedOne(context.Background(), id)
		}},
		{"embed stalled", "embed stalled, pausing batch", http.StatusOK, func(w *Worker, id string) {
			w.embedStalled(1, id)
		}},
		{"embed pending get content error", "embed: get content", http.StatusOK, func(w *Worker, id string) {
			w.store = failingContentStorePending{mockStore: newMockStore(), memoryID: id}
			w.EmbedPending(context.Background(), "proj-1", 10)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := embedStub(t, tc.status)
			assertLogGuard(t, "memory_id", tc.wantMsg, func(logger *slog.Logger, id string) {
				tc.run(NewWorker(NewClient(srv.URL, "m", 3), newMockStore(), logger, time.Hour, t.TempDir()), id)
			})
		})
	}
}
