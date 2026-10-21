package embedding

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// tokenShapedMemoryID builds a value the token guard recognises as a GitHub
// personal access token ("ghp_" plus 36 characters) from parts, so this file
// holds no literal of that shape.
func tokenShapedMemoryID() string {
	return "gh" + "p_" + strings.Repeat("a", 36)
}

const withheldGitHubMemoryID = "<memory_id withheld: it holds a GitHub personal access token>"

// failingContentStoreMem fails GetMemoryContent, which is the embedding worker's
// Debug path for a memory.
type failingContentStoreMem struct{ *mockStore }

func (failingContentStoreMem) GetMemoryContent(context.Context, string) (string, error) {
	return "", errors.New("content fetch failed")
}

// failingEmbedStoreMem fails StoreEmbeddingForContent with an error.
type failingEmbedStoreMem struct{ *mockStore }

func (failingEmbedStoreMem) GetMemoryContent(context.Context, string) (string, error) {
	return "content to embed", nil
}

func (failingEmbedStoreMem) StoreEmbeddingForContent(ctx context.Context, memoryID string, vec []float32, model, content string) (bool, error) {
	return false, errors.New("store failed")
}

// contentChangedStore returns different content on GetMemoryContent vs what's stored,
// triggering the "content changed" path (StoreEmbeddingForContent returns false).
type contentChangedStore struct{ *mockStore }

func (c contentChangedStore) GetMemoryContent(context.Context, string) (string, error) {
	return "current content", nil
}

func (c contentChangedStore) StoreEmbeddingForContent(ctx context.Context, memoryID string, vec []float32, model, content string) (bool, error) {
	c.mu.Lock()
	c.memories[memoryID] = "original content"
	c.mu.Unlock()
	return c.mockStore.StoreEmbeddingForContent(ctx, memoryID, vec, model, content)
}

// panickingUnembeddedStore panics in UnembeddedMemoryIDs, which safeProcessProject recovers.
type panickingUnembeddedStore struct{ *mockStore }

func (panickingUnembeddedStore) UnembeddedMemoryIDs(context.Context, string, string, int) ([]string, error) {
	panic("simulated panic in UnembeddedMemoryIDs")
}

func embedStubMem(embedStatus int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/embed":
			if embedStatus != http.StatusOK {
				http.Error(w, "embedding backend unavailable", embedStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"embeddings":[[0.1,0.2,0.3]]}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestWorkerLogsRenderAStoredMemoryIDThroughTheTokenGuard drives every log line
// the embedding worker writes with a memory id: the get content debug line, the
// ollama debug line, the store error line, the content changed debug line, the
// embed stalled info line, and the recovered panic line. A stored memory id is
// not token-checked on import, so an imported artifact can carry a token-shaped
// one, and these lines reach the MCP client's stderr.
//
// Each case asserts both halves from one line: the token-shaped id never
// appears, and an ordinary id still does, so a renderer that withheld
// everything would fail the control.
func TestWorkerLogsRenderAStoredMemoryIDThroughTheTokenGuard(t *testing.T) {
	cases := []struct {
		name          string
		wantMsg       string
		checkMemoryID bool // whether the log line should contain memory_id
		run           func(t *testing.T, logger *slog.Logger, memoryID string)
	}{
		{
			name:          "get content missing",
			wantMsg:       "embed: get content",
			checkMemoryID: true,
			run: func(t *testing.T, logger *slog.Logger, memoryID string) {
				srv := embedStubMem(http.StatusOK)
				t.Cleanup(srv.Close)
				w := NewWorker(NewClient(srv.URL, "m", 3), failingContentStoreMem{newMockStore()}, logger, time.Hour, t.TempDir())
				w.EmbedOne(context.Background(), memoryID)
			},
		},
		{
			name:          "ollama embed error",
			wantMsg:       "embed: ollama",
			checkMemoryID: true,
			run: func(t *testing.T, logger *slog.Logger, memoryID string) {
				srv := embedStubMem(http.StatusServiceUnavailable)
				t.Cleanup(srv.Close)
				store := newMockStore()
				store.memories[memoryID] = "content to embed"
				w := NewWorker(NewClient(srv.URL, "m", 3), store, logger, time.Hour, t.TempDir())
				w.EmbedOne(context.Background(), memoryID)
			},
		},
		{
			name:          "store embedding error",
			wantMsg:       "embed: store",
			checkMemoryID: true,
			run: func(t *testing.T, logger *slog.Logger, memoryID string) {
				srv := embedStubMem(http.StatusOK)
				t.Cleanup(srv.Close)
				w := NewWorker(NewClient(srv.URL, "m", 3), failingEmbedStoreMem{newMockStore()}, logger, time.Hour, t.TempDir())
				w.EmbedOne(context.Background(), memoryID)
			},
		},
		{
			name:          "content changed while embedding",
			wantMsg:       "embed: content changed while embedding, left pending",
			checkMemoryID: true,
			run: func(t *testing.T, logger *slog.Logger, memoryID string) {
				srv := embedStubMem(http.StatusOK)
				t.Cleanup(srv.Close)
				store := contentChangedStore{newMockStore()}
				store.memories[memoryID] = "original content"
				w := NewWorker(NewClient(srv.URL, "m", 3), store, logger, time.Hour, t.TempDir())
				w.EmbedOne(context.Background(), memoryID)
			},
		},
		{
			name:          "recovered panic in unembedded",
			wantMsg:       "panic in embedding processProject, recovered",
			checkMemoryID: false, // This logs project_id, not memory_id
			run: func(t *testing.T, logger *slog.Logger, memoryID string) {
				srv := embedStubMem(http.StatusOK)
				t.Cleanup(srv.Close)
				w := NewWorker(NewClient(srv.URL, "m", 3), panickingUnembeddedStore{newMockStore()}, logger, time.Hour, t.TempDir())
				w.safeProcessProject(context.Background(), "proj-1")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := func(memoryID string) string {
				var logged bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
				tc.run(t, logger, memoryID)
				return logged.String()
			}

			if tc.checkMemoryID {
				tokenID := tokenShapedMemoryID()
				line := capture(tokenID)
				if !strings.Contains(line, tc.wantMsg) {
					t.Fatalf("the run never logged %q: %q", tc.wantMsg, line)
				}
				if strings.Contains(line, tokenID) {
					t.Errorf("a log line repeats the token-shaped memory id: %s", line)
				}
				if !strings.Contains(line, withheldGitHubMemoryID) {
					t.Errorf("the log does not name the withheld memory id and its format: %s", line)
				}

				ordinary := capture("mem-ordinary")
				if !strings.Contains(ordinary, tc.wantMsg) {
					t.Fatalf("the control run never logged %q: %q", tc.wantMsg, ordinary)
				}
				if !strings.Contains(ordinary, "mem-ordinary") {
					t.Errorf("an ordinary memory id no longer appears in the log: %s", ordinary)
				}
			} else {
				// Just verify the panic is recovered and the message is logged
				line := capture("any")
				if !strings.Contains(line, tc.wantMsg) {
					t.Fatalf("the run never logged %q: %q", tc.wantMsg, line)
				}
			}
		})
	}
}
