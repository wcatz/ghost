package linking

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

func (p *panicOnceStore) MarkLinkScanned(ctx context.Context, memoryID string, scanned []float32) error {
	err := p.Store.MarkLinkScanned(ctx, memoryID, scanned)
	if err == nil && p.scanned != nil {
		select {
		case p.scanned <- memoryID:
		default:
		}
	}
	return err
}

// TestSweepOnceCapsRelatedDegree: scanning many spokes that each find the same
// hub must not let the hub accumulate unbounded inbound related links. The
// weakest links are invalidated so total live related degree stays at K.
func TestSweepOnceCapsRelatedDegree(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	dim := memory.MaxRelatedDegree + 15
	hubVec := make([]float32, dim)
	hubVec[0] = 1
	hub := addEmbedded(t, s, "hub", hubVec)

	nSpokes := memory.MaxRelatedDegree + 5
	spokes := make([]string, nSpokes)
	for i := 0; i < nSpokes; i++ {
		v := make([]float32, dim)
		v[0] = 0.8
		v[i+1] = 0.6
		spokes[i] = addEmbedded(t, s, fmt.Sprintf("spoke %d", i), v)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	w := NewWorker(s, logger, time.Minute, 0.70)
	// One sweep processes up to batchSize unscanned memories; all fit.
	w.SweepOnce(ctx)
	// Drain any remaining unscanned (e.g. if hub/spokes split across batches).
	w.SweepOnce(ctx)

	links, err := s.GetLinks(ctx, hub)
	if err != nil {
		t.Fatalf("GetLinks(hub): %v", err)
	}
	related := 0
	for _, l := range links {
		if l.Relation == "related" {
			related++
		}
	}
	if related > memory.MaxRelatedDegree {
		t.Fatalf("hub live related degree = %d, want <= %d", related, memory.MaxRelatedDegree)
	}
	if related == 0 {
		t.Fatal("hub has no related links; sweep did not link spokes")
	}
	// Spokes only relate to hub (mutual spoke cosine 0.64 < 0.70), so no spoke
	// can exceed the cap either.
	for _, id := range spokes {
		ls, err := s.GetLinks(ctx, id)
		if err != nil {
			t.Fatalf("GetLinks(spoke): %v", err)
		}
		n := 0
		for _, l := range ls {
			if l.Relation == "related" {
				n++
			}
		}
		if n > memory.MaxRelatedDegree {
			t.Fatalf("spoke %s live related degree = %d, want <= %d", id, n, memory.MaxRelatedDegree)
		}
	}
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

func TestSweepOnceTrimsExistingOverCapRelatedDegree(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := memory.NewStore(db, logger)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, testProject, filepath.Join(dir, "project"), "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	hub, err := s.Create(ctx, testProject, memory.Memory{
		Category: "fact", Content: "hub", Source: "manual", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create hub: %v", err)
	}
	others := make([]string, memory.MaxRelatedDegree+10)
	for i := range others {
		others[i], err = s.Create(ctx, testProject, memory.Memory{
			Category: "fact", Content: fmt.Sprintf("neighbor %d", i), Source: "manual", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("Create neighbor %d: %v", i, err)
		}
	}

	seed, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatalf("sql.Open seed: %v", err)
	}
	for i, other := range others {
		src, tgt := hub, other
		if src > tgt {
			src, tgt = tgt, src
		}
		strength := 0.99 - float32(i)*0.001
		if _, err := seed.Exec(`
			INSERT INTO memory_links (source_id, target_id, relation, strength, source)
			VALUES (?, ?, 'related', ?, 'auto')
		`, src, tgt, strength); err != nil {
			_ = seed.Close()
			t.Fatalf("seed link %d: %v", i, err)
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	w := NewWorker(s, logger, time.Minute, 0.70)
	w.SweepOnce(ctx)

	links, err := s.GetLinks(ctx, hub)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	kept := map[string]bool{}
	live := 0
	for _, l := range links {
		if l.Relation != "related" {
			continue
		}
		live++
		other := l.SourceID
		if other == hub {
			other = l.TargetID
		}
		kept[other] = true
	}
	if live != memory.MaxRelatedDegree {
		t.Fatalf("live related degree = %d, want %d", live, memory.MaxRelatedDegree)
	}
	for i := 0; i < memory.MaxRelatedDegree; i++ {
		if !kept[others[i]] {
			t.Errorf("strong link to neighbor %d was invalidated", i)
		}
	}
	for i := memory.MaxRelatedDegree; i < len(others); i++ {
		if kept[others[i]] {
			t.Errorf("weak link to neighbor %d survived", i)
		}
	}

	check, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open check: %v", err)
	}
	defer func() { _ = check.Close() }()
	var invalidated int
	if err := check.QueryRow(`
		SELECT COUNT(*) FROM memory_links
		WHERE relation = 'related' AND invalidated_at IS NOT NULL
		  AND (source_id = ? OR target_id = ?)
	`, hub, hub).Scan(&invalidated); err != nil {
		t.Fatalf("count invalidated: %v", err)
	}
	if invalidated != 10 {
		t.Fatalf("invalidated = %d, want 10", invalidated)
	}
}
