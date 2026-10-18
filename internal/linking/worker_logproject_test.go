package linking

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// scriptedLinkStore is a linkStore whose answers are fixed, so a test can reach
// a worker log line without a database. A nil listErr and one pending id makes a
// sweep link that memory to a neighbour.
type scriptedLinkStore struct {
	listErr error
}

func (s scriptedLinkStore) ListProjects(context.Context) ([]memory.Project, error) { return nil, nil }

func (s scriptedLinkStore) UnscannedEmbeddedMemoryIDs(context.Context, string, int) ([]string, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return []string{"mem-a"}, nil
}

func (scriptedLinkStore) GetEmbedding(context.Context, string) ([]float32, error) {
	return []float32{1, 0, 0}, nil
}

func (scriptedLinkStore) GetByIDs(_ context.Context, ids []string) ([]memory.Memory, error) {
	return []memory.Memory{{ID: ids[0]}}, nil
}

func (scriptedLinkStore) SearchVectorScoped(context.Context, string, []float32, int, map[string]string) ([]memory.ScoredMemory, error) {
	return []memory.ScoredMemory{{MemoryID: "mem-b", Score: 0.99}}, nil
}

func (scriptedLinkStore) CreateLink(context.Context, string, string, string, float32, string) error {
	return nil
}

func (scriptedLinkStore) MarkLinkScanned(context.Context, string, []float32) error { return nil }

// TestWorkerLogsRenderAStoredProjectIDThroughTheTokenGuard drives both log lines
// the linking worker writes with a project id: the failed unscanned-id list and
// the batch-complete line. A stored project id is not token-checked on import,
// so an imported artifact can carry a token-shaped one, and these lines reach
// the MCP client's stderr.
//
// Each case asserts both halves: the token-shaped id never appears, and an
// ordinary id still does, so a renderer that withheld everything fails the
// control.
func TestWorkerLogsRenderAStoredProjectIDThroughTheTokenGuard(t *testing.T) {
	// Built from parts so this file holds no literal of the token's shape: "ghp_"
	// plus 36 characters, which the token guard names as a GitHub token.
	tokenID := "gh" + "p_" + strings.Repeat("a", 36)
	const withheld = "<project_id withheld: it holds a GitHub personal access token>"

	cases := []struct {
		name    string
		wantMsg string
		store   scriptedLinkStore
	}{
		{"list unscanned failed", "linking: list unscanned", scriptedLinkStore{listErr: errors.New("list failed")}},
		{"batch complete", "linking: batch complete", scriptedLinkStore{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := func(projectID string) string {
				var logged bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
				NewWorker(tc.store, logger, time.Hour, 0.70).processProject(context.Background(), projectID)
				return logged.String()
			}

			line := capture(tokenID)
			if !strings.Contains(line, tc.wantMsg) {
				t.Fatalf("the run never logged %q: %q", tc.wantMsg, line)
			}
			if strings.Contains(line, tokenID) {
				t.Errorf("a log line repeats the token-shaped project id: %s", line)
			}
			if !strings.Contains(line, withheld) {
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
