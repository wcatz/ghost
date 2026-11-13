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

// tokenShapedID builds a value the token guard recognises as a GitHub personal
// access token from parts, so no test file holds a literal of that shape.
func tokenShapedID() string { return "gh" + "p_" + strings.Repeat("a", 36) }

// failingListStore fails the unembedded-id query.
type failingListStore struct{ *mockStore }

func (failingListStore) UnembeddedMemoryIDs(context.Context, string, string, int) ([]string, error) {
	return nil, errors.New("list failed")
}

// panickingListStore panics in the same query, which safeProcessProject recovers.
type panickingListStore struct{ *mockStore }

func (panickingListStore) UnembeddedMemoryIDs(context.Context, string, string, int) ([]string, error) {
	panic("simulated panic in UnembeddedMemoryIDs")
}

// embedStub answers the probe at "/" and /api/embed with a vector, or with
// embedStatus when that is not 200.
func embedStub(t *testing.T, embedStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(srv.Close)
	return srv
}

// assertLogGuard runs one log-emitting case twice: with a token-shaped id, which
// must never appear (the withheld notice does), and with an ordinary id, which
// still must, so a renderer that withheld everything fails the control.
func assertLogGuard(t *testing.T, kind, wantMsg string, run func(logger *slog.Logger, id string)) {
	t.Helper()
	capture := func(id string) string {
		var logged bytes.Buffer
		run(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})), id)
		return logged.String()
	}
	tokenID := tokenShapedID()
	line := capture(tokenID)
	if !strings.Contains(line, wantMsg) {
		t.Fatalf("the run never logged %q: %q", wantMsg, line)
	}
	if strings.Contains(line, tokenID) {
		t.Errorf("a log line repeats the token-shaped %s: %s", kind, line)
	}
	if want := "<" + kind + " withheld: it holds a GitHub personal access token>"; !strings.Contains(line, want) {
		t.Errorf("the log does not name the withheld %s: %s", kind, line)
	}
	ordinary := capture("ordinary-id")
	if !strings.Contains(ordinary, wantMsg) || !strings.Contains(ordinary, "ordinary-id") {
		t.Errorf("control run lost the message or the ordinary %s: %s", kind, ordinary)
	}
}

// TestWorkerLogsRenderAStoredProjectIDThroughTheTokenGuard drives every worker
// log line that carries a project id. An imported artifact can carry a
// token-shaped id, and these lines reach the MCP client's stderr.
func TestWorkerLogsRenderAStoredProjectIDThroughTheTokenGuard(t *testing.T) {
	pending := func() *mockStore {
		s := newMockStore()
		s.memories["mem-1"] = "a memory the index does not have yet"
		return s
	}
	cases := []struct {
		name, wantMsg string
		status        int
		store         func() memoryStore
		run           func(w *Worker, id string)
	}{
		{"recovered panic", "panic in embedding processProject, recovered", 200,
			func() memoryStore { return panickingListStore{newMockStore()} },
			func(w *Worker, id string) { w.safeProcessProject(context.Background(), id) }},
		{"list unembedded failed", "embed: list unembedded", 200,
			func() memoryStore { return failingListStore{newMockStore()} },
			func(w *Worker, id string) { w.EmbedPending(context.Background(), id, 10) }},
		{"failed embeds warn", "embedding failures this sweep", 503,
			func() memoryStore { return pending() },
			func(w *Worker, id string) { w.EmbedPending(context.Background(), id, 10) }},
		{"good batch", "embedding batch complete", 200,
			func() memoryStore { return pending() },
			func(w *Worker, id string) { w.EmbedPending(context.Background(), id, 10) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := embedStub(t, tc.status)
			assertLogGuard(t, "project_id", tc.wantMsg, func(logger *slog.Logger, id string) {
				tc.run(NewWorker(NewClient(srv.URL, "m", 3), tc.store(), logger, time.Hour, t.TempDir()), id)
			})
		})
	}
}
