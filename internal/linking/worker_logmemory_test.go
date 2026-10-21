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

// tokenShapedMemoryID builds a value the token guard recognises as a GitHub
// personal access token ("ghp_" plus 36 characters) from parts, so this file
// holds no literal of that shape.
func tokenShapedMemoryID() string {
	return "gh" + "p_" + strings.Repeat("a", 36)
}

const withheldGitHubMemoryID = "<memory_id withheld: it holds a GitHub personal access token>"

// scriptedLinkStoreMem is a linkStore whose answers are fixed, so a test can reach
// a worker log line without a database. A nil listErr and one pending id makes a
// sweep link that memory to a neighbour.
type scriptedLinkStoreMem struct {
	listErr         error
	getEmbeddingErr error
	getByIDsErr     error
	searchErr       error
	createLinkErr   error
	markScannedErr  error
	embedding       []float32
	sourceMemories  []memory.Memory
	candidates      []memory.ScoredMemory
}

func (s scriptedLinkStoreMem) ListProjects(context.Context) ([]memory.Project, error) {
	return nil, nil
}

func (s scriptedLinkStoreMem) UnscannedEmbeddedMemoryIDs(context.Context, string, int) ([]string, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return []string{"mem-a"}, nil
}

func (s scriptedLinkStoreMem) GetEmbedding(ctx context.Context, memoryID string) ([]float32, error) {
	if s.getEmbeddingErr != nil {
		return nil, s.getEmbeddingErr
	}
	if s.embedding != nil {
		return s.embedding, nil
	}
	return []float32{1, 0, 0}, nil
}

func (s scriptedLinkStoreMem) GetByIDs(_ context.Context, ids []string) ([]memory.Memory, error) {
	if s.getByIDsErr != nil {
		return nil, s.getByIDsErr
	}
	if s.sourceMemories != nil {
		return s.sourceMemories, nil
	}
	return []memory.Memory{{ID: ids[0]}}, nil
}

func (s scriptedLinkStoreMem) SearchVectorScoped(context.Context, string, []float32, int, map[string]string) ([]memory.ScoredMemory, error) {
	if s.searchErr != nil {
		return nil, s.searchErr
	}
	if s.candidates != nil {
		return s.candidates, nil
	}
	return []memory.ScoredMemory{{MemoryID: "mem-b", Score: 0.99}}, nil
}

func (s scriptedLinkStoreMem) CreateLink(context.Context, string, string, string, float32, string) error {
	return s.createLinkErr
}

func (s scriptedLinkStoreMem) MarkLinkScanned(context.Context, string, []float32) error {
	return s.markScannedErr
}

// TestWorkerLogsRenderAStoredMemoryIDThroughTheTokenGuard drives every log line
// the linking worker writes with a memory id: the get embedding debug line, the
// no usable vector debug line, the get source scope debug line, the source
// memory disappeared debug line, the search debug line, the create link debug
// line, and the mark scanned error line. A stored memory id is not token-checked
// on import, so an imported artifact can carry a token-shaped one, and these
// lines reach the MCP client's stderr.
//
// Each case asserts both halves: the token-shaped id never appears, and an
// ordinary id still does, so a renderer that withheld everything fails the
// control.
func TestWorkerLogsRenderAStoredMemoryIDThroughTheTokenGuard(t *testing.T) {
	tokenID := tokenShapedMemoryID()

	cases := []struct {
		name    string
		wantMsg string
		store   scriptedLinkStoreMem
	}{
		{
			name:    "get embedding error",
			wantMsg: "linking: get embedding",
			store:   scriptedLinkStoreMem{getEmbeddingErr: errors.New("embedding fetch failed")},
		},
		{
			name:    "no usable vector",
			wantMsg: "linking: no usable vector yet",
			store:   scriptedLinkStoreMem{embedding: []float32{}},
		},
		{
			name:    "get source scope error",
			wantMsg: "linking: get source scope",
			store:   scriptedLinkStoreMem{getByIDsErr: errors.New("get by ids failed")},
		},
		{
			name:    "source memory disappeared",
			wantMsg: "linking: source memory disappeared",
			store:   scriptedLinkStoreMem{sourceMemories: []memory.Memory{}},
		},
		{
			name:    "search error",
			wantMsg: "linking: search",
			store:   scriptedLinkStoreMem{searchErr: errors.New("search failed")},
		},
		{
			name:    "create link error",
			wantMsg: "linking: create link",
			store:   scriptedLinkStoreMem{createLinkErr: errors.New("create link failed")},
		},
		{
			name:    "mark scanned error",
			wantMsg: "linking: mark scanned",
			store:   scriptedLinkStoreMem{markScannedErr: errors.New("mark scanned failed")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := func(memoryID string) string {
				var logged bytes.Buffer
				logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
				// Override the memory ID in the store to return our test ID
				NewWorker(&memoryIDStoreMem{store: tc.store, memoryID: memoryID}, logger, time.Hour, 0.70).
					processProject(context.Background(), "proj-1")
				return logged.String()
			}

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
		})
	}
}

// memoryIDStoreMem wraps a scriptedLinkStoreMem to return a specific memory ID.
type memoryIDStoreMem struct {
	store    scriptedLinkStoreMem
	memoryID string
}

func (m *memoryIDStoreMem) ListProjects(ctx context.Context) ([]memory.Project, error) {
	return m.store.ListProjects(ctx)
}

func (m *memoryIDStoreMem) UnscannedEmbeddedMemoryIDs(ctx context.Context, projectID string, limit int) ([]string, error) {
	return []string{m.memoryID}, nil
}

func (m *memoryIDStoreMem) GetEmbedding(ctx context.Context, memoryID string) ([]float32, error) {
	return m.store.GetEmbedding(ctx, memoryID)
}

func (m *memoryIDStoreMem) GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error) {
	return m.store.GetByIDs(ctx, ids)
}

func (m *memoryIDStoreMem) SearchVectorScoped(ctx context.Context, projectID string, queryVec []float32, limit int, scope map[string]string) ([]memory.ScoredMemory, error) {
	return m.store.SearchVectorScoped(ctx, projectID, queryVec, limit, scope)
}

func (m *memoryIDStoreMem) CreateLink(ctx context.Context, sourceID, targetID, relation string, strength float32, source string) error {
	return m.store.CreateLink(ctx, sourceID, targetID, relation, strength, source)
}

func (m *memoryIDStoreMem) MarkLinkScanned(ctx context.Context, memoryID string, scanned []float32) error {
	return m.store.MarkLinkScanned(ctx, memoryID, scanned)
}
