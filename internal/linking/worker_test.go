package linking

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
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
	return addEmbeddedScoped(t, s, content, vec, nil)
}

func addEmbeddedScoped(t *testing.T, s *memory.Store, content string, vec []float32, scope map[string]string) string {
	t.Helper()
	ctx := context.Background()
	id, err := s.Create(ctx, testProject, memory.Memory{
		Category: "fact", Content: content, Source: "manual", Importance: 0.7, Scope: scope,
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

// TestSweepOnceFindsCompatibleNeighbourBelowConflictingOnes covers the
// interaction between the scope guard and the neighbor budget. SearchVector
// truncates to the limit it is given, so applying the scope check after that cut
// spends the budget on rows that can never be linked: here seven
// scope-conflicting neighbours outrank the one compatible row, and with a fetch
// of maxCandidates+1 the compatible row is never examined. The source is marked
// scanned once the sweep succeeds, so it is never reconsidered — the
// same-scope duplicate stays unlinked, and therefore undemoted.
func TestSweepOnceFindsCompatibleNeighbourBelowConflictingOnes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Source and the compatible neighbour are nearest to each other in angle;
	// the seven conflicting rows sit between them, all above the 0.70
	// threshold, so nothing but the ordering decides the outcome.
	source := addEmbeddedScoped(t, s, "production pooling timeout", []float32{1, 0}, map[string]string{"environment": "production"})
	compatible := addEmbeddedScoped(t, s, "production replication failover", []float32{0.9, 0.44}, map[string]string{"environment": "production"})
	conflicting := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		conflicting = append(conflicting, addEmbeddedScoped(t, s,
			fmt.Sprintf("development candidate %d", i),
			[]float32{1, 0.02 * float32(i+1)},
			map[string]string{"environment": "development"}))
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	NewWorker(s, logger, time.Minute, 0.70).SweepOnce(ctx)

	links, err := s.GetLinks(ctx, source)
	if err != nil {
		t.Fatalf("GetLinks(source): %v", err)
	}
	got := map[string]bool{}
	for _, l := range links {
		other := l.SourceID
		if other == source {
			other = l.TargetID
		}
		got[other] = true
	}
	if !got[compatible] {
		t.Errorf("source is not linked to the compatible neighbour: the scope filter ran after the "+
			"candidate cut, so the budget was spent on %d conflicting rows ranked above it", len(conflicting))
	}
	for _, id := range conflicting {
		if got[id] {
			t.Errorf("source linked to scope-conflicting memory %s", id)
		}
	}
}

// TestSweepOnceStillCapsNeighborCount guards the budget the oversample could
// otherwise spend: the fetch looks deeper to find compatible rows, but a memory
// still gets at most maxCandidates related edges. Before the fetch limit was
// widened this fell out of the truncation for free; it is now an explicit cap.
func TestSweepOnceStillCapsNeighborCount(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	source := addEmbeddedScoped(t, s, "production pooling timeout", []float32{1, 0}, map[string]string{"environment": "production"})
	for i := 0; i < maxCandidates+3; i++ {
		addEmbeddedScoped(t, s, fmt.Sprintf("production neighbour %d", i),
			[]float32{1, 0.02 * float32(i+1)}, map[string]string{"environment": "production"})
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	NewWorker(s, logger, time.Minute, 0.70).SweepOnce(ctx)

	links, err := s.GetLinks(ctx, source)
	if err != nil {
		t.Fatalf("GetLinks(source): %v", err)
	}
	if len(links) != maxCandidates {
		t.Errorf("source has %d links, want the %d-neighbour cap to still apply: %+v", len(links), maxCandidates, links)
	}
}

func TestSweepOnceDoesNotLinkScopeConflictingMemories(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	dev := addEmbeddedScoped(t, s, "development database uses SQLite", []float32{1, 0, 0.1}, map[string]string{"environment": "development"})
	prod := addEmbeddedScoped(t, s, "production database uses SQLite", []float32{1, 0.1, 0}, map[string]string{"environment": "production"})

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	NewWorker(s, logger, time.Minute, 0.70).SweepOnce(ctx)

	for _, id := range []string{dev, prod} {
		links, err := s.GetLinks(ctx, id)
		if err != nil {
			t.Fatalf("GetLinks(%s): %v", id, err)
		}
		if len(links) != 0 {
			t.Errorf("scope-conflicting memory %s received related links: %+v", id, links)
		}
	}
}

// panicOnceStore wraps a real *memory.Store, panicking exactly once — on the
// first call to ListProjects — before delegating to the real implementation
// on every call after that. It also observes MarkLinkScanned so the test can
// detect (without sleeping) that a later sweep tick actually completed
// processing, proving the worker loop is still alive rather than merely
// proving recover() appears somewhere in the source.
type panicOnceStore struct {
	*memory.Store
	fired   atomic.Bool
	scanned chan string
}

func (p *panicOnceStore) ListProjects(ctx context.Context) ([]memory.Project, error) {
	if p.fired.CompareAndSwap(false, true) {
		panic("simulated panic in ListProjects")
	}
	return p.Store.ListProjects(ctx)
}

func (p *panicOnceStore) MarkLinkScanned(ctx context.Context, memoryID string) error {
	err := p.Store.MarkLinkScanned(ctx, memoryID)
	if err == nil && p.scanned != nil {
		select {
		case p.scanned <- memoryID:
		default:
		}
	}
	return err
}

// TestRun_SurvivesPanicInTickerSweep proves Run's ticker branch keeps firing
// after a panic inside SweepOnce (via ListProjects). Before the fix, the
// first tick's panic is unrecovered anywhere in the goroutine's call stack,
// which crashes the whole process; the fix must keep later ticks scanning
// normally.
func TestRun_SurvivesPanicInTickerSweep(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := addEmbedded(t, s, "SQLite WAL journal mode", []float32{1, 0, 0.1})

	store := &panicOnceStore{Store: s, scanned: make(chan string, 1)}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	w := NewWorker(store, logger, 20*time.Millisecond, 0.70)

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		w.Run(runCtx)
		close(done)
	}()

	select {
	case got := <-store.scanned:
		if got != id {
			t.Fatalf("scanned %q, want %q", got, id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not mark the memory scanned after a panic on the first sweep tick — ticker loop likely died")
	}

	cancel()
	<-done
}
