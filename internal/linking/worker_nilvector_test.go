package linking

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// nilVectorStore is a linkStore whose single source memory has no usable
// vector — the state memory.Store reports for a row recorded in another vector
// space (see Store.GetEmbedding). It stands in for any linkStore
// implementation that can hand back no vector, and records whether the worker
// spent the memory's one scan slot on the attempt.
type nilVectorStore struct {
	searched  atomic.Int32
	marked    atomic.Int32
	created   atomic.Int32
	memoryID  string
	scopedHit bool
}

func (s *nilVectorStore) ListProjects(context.Context) ([]memory.Project, error) {
	return []memory.Project{{ID: testProject, Name: testProject}}, nil
}

func (s *nilVectorStore) UnscannedEmbeddedMemoryIDs(context.Context, string, int) ([]string, error) {
	return []string{s.memoryID}, nil
}

func (s *nilVectorStore) GetEmbedding(context.Context, string) ([]float32, error) {
	return nil, nil // no usable vector: another vector space, or not embedded yet
}

func (s *nilVectorStore) GetByIDs(context.Context, []string) ([]memory.Memory, error) {
	return []memory.Memory{{ID: s.memoryID, Category: "fact", Content: "some memory"}}, nil
}

func (s *nilVectorStore) SearchVectorScoped(context.Context, string, []float32, int, map[string]string) ([]memory.ScoredMemory, error) {
	s.searched.Add(1)
	return nil, nil
}

func (s *nilVectorStore) CreateLink(context.Context, string, string, string, float32, string) error {
	s.created.Add(1)
	return nil
}

func (s *nilVectorStore) MarkLinkScanned(context.Context, string) error {
	s.marked.Add(1)
	return nil
}

// TestSweepOnceLeavesMemoryUnscannedWithoutUsableVector: a memory whose vector
// is not in this process's space has nothing to compare, so the pass produces no
// edge. It must therefore NOT be marked scanned — that slot is per memory, and
// spending it here would mean the link is never built once the embedding worker
// finally rewrites the vector.
func TestSweepOnceLeavesMemoryUnscannedWithoutUsableVector(t *testing.T) {
	store := &nilVectorStore{memoryID: "mem-1"}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	w := NewWorker(store, logger, time.Minute, 0.70)

	w.SweepOnce(context.Background())

	if got := store.marked.Load(); got != 0 {
		t.Errorf("MarkLinkScanned called %d times, want 0: the memory's scan slot must survive a pass that had no vector to compare", got)
	}
	if got := store.created.Load(); got != 0 {
		t.Errorf("CreateLink called %d times without a query vector, want 0", got)
	}
	if got := store.searched.Load(); got != 0 {
		t.Errorf("SearchVectorScoped called %d times with no vector, want 0", got)
	}
}
