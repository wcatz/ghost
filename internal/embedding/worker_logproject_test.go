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

// tokenShapedProjectID builds a value the token guard recognises as a GitHub
// personal access token ("ghp_" plus 36 characters) from parts, so this file
// holds no literal of that shape.
func tokenShapedProjectID() string {
	return "gh" + "p_" + strings.Repeat("a", 36)
}

const withheldGitHubProjectID = "<project_id withheld: it holds a GitHub personal access token>"

// failingListStore fails the unembedded-id query, which is the embedding
// worker's Error path for a project.
type failingListStore struct{ *mockStore }

func (failingListStore) UnembeddedMemoryIDs(context.Context, string, string, int) ([]string, error) {
	return nil, errors.New("list failed")
}

// panickingListStore panics in the same query, which safeProcessProject recovers.
type panickingListStore struct{ *mockStore }

func (panickingListStore) UnembeddedMemoryIDs(context.Context, string, string, int) ([]string, error) {
	panic("simulated panic in UnembeddedMemoryIDs")
}

func embedStub(embedStatus int) *httptest.Server {
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

// TestWorkerLogsRenderAStoredProjectIDThroughTheTokenGuard drives every log line
// the embedding worker writes with a project id: the recovered panic, the failed
// unembedded-id list, the "embedding memories" and "embedding batch complete"
// lines of a good batch, and the per-sweep failure warning. A stored project id
// is not token-checked on import, so an imported artifact can carry a
// token-shaped one, and these lines reach the MCP client's stderr.
//
// Each case asserts both halves from one line: the token-shaped id never
// appears, and an ordinary id still does, so a renderer that withheld
// everything would fail the control.
func TestWorkerLogsRenderAStoredProjectIDThroughTheTokenGuard(t *testing.T) {
	cases := []struct {
		name string
		// wantMsg is a message the run must have logged, so a case cannot pass
		// by never reaching its line.
		wantMsg string
		run     func(t *testing.T, logger *slog.Logger, projectID string)
	}{
		{
			name:    "recovered panic",
			wantMsg: "panic in embedding processProject, recovered",
			run: func(t *testing.T, logger *slog.Logger, projectID string) {
				srv := embedStub(http.StatusOK)
				t.Cleanup(srv.Close)
				w := NewWorker(NewClient(srv.URL, "m", 3), panickingListStore{newMockStore()}, logger, time.Hour, t.TempDir())
				w.safeProcessProject(context.Background(), projectID)
			},
		},
		{
			name:    "list unembedded failed",
			wantMsg: "embed: list unembedded",
			run: func(t *testing.T, logger *slog.Logger, projectID string) {
				srv := embedStub(http.StatusOK)
				t.Cleanup(srv.Close)
				w := NewWorker(NewClient(srv.URL, "m", 3), failingListStore{newMockStore()}, logger, time.Hour, t.TempDir())
				w.EmbedPending(context.Background(), projectID, 10)
			},
		},
		{
			name:    "failed embeds warn",
			wantMsg: "embedding failures this sweep",
			run: func(t *testing.T, logger *slog.Logger, projectID string) {
				srv := embedStub(http.StatusServiceUnavailable)
				t.Cleanup(srv.Close)
				store := newMockStore()
				store.memories["mem-1"] = "a memory the index does not have yet"
				w := NewWorker(NewClient(srv.URL, "m", 3), store, logger, time.Hour, t.TempDir())
				w.EmbedPending(context.Background(), projectID, 10)
			},
		},
		{
			name:    "good batch",
			wantMsg: "embedding batch complete",
			run: func(t *testing.T, logger *slog.Logger, projectID string) {
				srv := embedStub(http.StatusOK)
				t.Cleanup(srv.Close)
				store := newMockStore()
				store.memories["mem-1"] = "a memory the index does not have yet"
				w := NewWorker(NewClient(srv.URL, "m", 3), store, logger, time.Hour, t.TempDir())
				w.EmbedPending(context.Background(), projectID, 10)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := func(projectID string) string {
				var logged bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
				tc.run(t, logger, projectID)
				return logged.String()
			}

			tokenID := tokenShapedProjectID()
			line := capture(tokenID)
			if !strings.Contains(line, tc.wantMsg) {
				t.Fatalf("the run never logged %q: %q", tc.wantMsg, line)
			}
			if strings.Contains(line, tokenID) {
				t.Errorf("a log line repeats the token-shaped project id: %s", line)
			}
			if !strings.Contains(line, withheldGitHubProjectID) {
				t.Errorf("the log does not name the withheld project id and its format: %s", line)
			}

			ordinary := capture("proj-ordinary")
			if !strings.Contains(ordinary, tc.wantMsg) {
				t.Fatalf("the control run never logged %q: %q", tc.wantMsg, ordinary)
			}
			if !strings.Contains(ordinary, "proj-ordinary") {
				t.Errorf("an ordinary project id no longer appears in the log: %s", ordinary)
			}
		})
	}
}
