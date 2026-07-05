package linking

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

const testProject = "test-project"

func testStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := memory.NewStore(db, logger)
	if err := s.EnsureProject(context.Background(), testProject, "/tmp/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s
}

func addEmbedded(t *testing.T, s *memory.Store, content string, vec []float32) string {
	t.Helper()
	ctx := context.Background()
	id, err := s.Create(ctx, testProject, memory.Memory{
		Category: "fact", Content: content, Source: "manual", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.StoreEmbedding(ctx, id, vec, "test"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	return id
}

func TestSweepOnceLinksSimilarMemories(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// a and b nearly parallel (cosine ~0.99); c orthogonal.
	a := addEmbedded(t, s, "SQLite WAL journal mode", []float32{1, 0, 0.1})
	b := addEmbedded(t, s, "SQLite busy timeout pragma", []float32{1, 0.1, 0})
	c := addEmbedded(t, s, "totally different topic", []float32{0, 1, 0})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	w := NewWorker(s, logger, time.Minute, 0.70)
	w.SweepOnce(ctx)

	linksA, err := s.GetLinks(ctx, a)
	if err != nil {
		t.Fatalf("GetLinks(a): %v", err)
	}
	if len(linksA) != 1 {
		t.Fatalf("a: got %d links, want 1 (a-b only): %+v", len(linksA), linksA)
	}
	other := linksA[0].SourceID
	if other == a {
		other = linksA[0].TargetID
	}
	if other != b {
		t.Errorf("a linked to %s, want %s", other, b)
	}

	linksC, err := s.GetLinks(ctx, c)
	if err != nil {
		t.Fatalf("GetLinks(c): %v", err)
	}
	if len(linksC) != 0 {
		t.Fatalf("c: got %d links, want 0 (below threshold): %+v", len(linksC), linksC)
	}

	// All embedded memories are now scanned — second sweep is a no-op.
	ids, err := s.UnscannedEmbeddedMemoryIDs(ctx, testProject, 10)
	if err != nil {
		t.Fatalf("UnscannedEmbeddedMemoryIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("got %d unscanned after sweep, want 0", len(ids))
	}
}
