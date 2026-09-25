package memory

import (
	"fmt"
	"testing"
)

// TestHybridWindowAdmitsAnFTSOnlyHit is issue #543.
//
// The fused window was chosen by a pure score cut, and RRF weights make that
// cut unable to admit a keyword-only hit whenever the vector leg fills the
// window: a top FTS row scores 0.3/61 ≈ 0.0049, while even the weakest vector
// candidate in a full window scores 0.7/80 ≈ 0.0088. So the vector leg's
// 20th-best row always outranked the keyword leg's best one, and an exact
// identifier match — the thing FTS exists for — could not reach the results no
// matter how exactly it matched.
//
// The target below has no embedding at all, so it is reachable only through
// the keyword leg. It must still be in the window.
func TestHybridWindowAdmitsAnFTSOnlyHit(t *testing.T) {
	store, ctx := setupTestStore(t)

	// Twelve vector-reachable memories, none of which mention the identifier.
	// They are what fills the window when the fused score is simply cut.
	queryVec := []float32{0.9, 0.1}
	for i := 0; i < 12; i++ {
		id := createTestMemory(t, store, ctx, fmt.Sprintf("scheduler throughput measurement alpha %d beta gamma", i))
		if err := store.StoreEmbedding(ctx, id, queryVec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding filler %d: %v", i, err)
		}
	}

	// The keyword-only hit: no embedding stored, so the vector leg cannot see it.
	needle := createTestMemory(t, store, ctx, "the listener on 8090 timed out, who owns it")

	results, err := store.SearchHybrid(ctx, "test-proj", "the listener on 8090 timed out, who owns it", queryVec, 10)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results, got none")
	}

	pos := -1
	for i, m := range results {
		if m.ID == needle {
			pos = i
			break
		}
	}
	// Admission is the whole guarantee, so that is all this pins. Position is
	// left to the fused score: stronger interventions were built and measured
	// (see FuseAndSelectWindow), and both cost more than they bought.
	if pos < 0 {
		t.Fatalf("the only keyword-reachable memory was dropped from the window: %d results returned, none of them the FTS-only hit", len(results))
	}
}

// TestHybridWindowReservationIsBounded: reserving keyword slots must not turn
// into a keyword-only ranking. A memory that matches BOTH legs outranks a
// keyword-only row, and a limit smaller than the reservation must not admit
// more rows than were asked for.
func TestHybridWindowReservationIsBounded(t *testing.T) {
	store, ctx := setupTestStore(t)

	both := createTestMemory(t, store, ctx, "the listener on 8090 timed out, who owns it")
	if err := store.StoreEmbedding(ctx, both, []float32{0.9, 0.1}, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	for i := 0; i < 8; i++ {
		id := createTestMemory(t, store, ctx, fmt.Sprintf("scheduler throughput measurement alpha %d beta gamma", i))
		if err := store.StoreEmbedding(ctx, id, []float32{0.9, 0.1}, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding filler %d: %v", i, err)
		}
	}

	results, err := store.SearchHybrid(ctx, "test-proj", "the listener on 8090 timed out, who owns it", []float32{0.9, 0.1}, 10)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected results, got none")
	}
	// Matching both legs accumulates both weighted contributions, so it must
	// still be first: a reservation that outranked it would be inverting the
	// evidence rather than balancing it.
	if results[0].ID != both {
		t.Errorf("first result = %s, want the memory matching both legs (%s)", results[0].ID, both)
	}
}

// TestHybridWindowNeverReturnsMoreThanLimit: the reservation adds rows to the
// window, so it must not add rows past the caller's limit.
func TestHybridWindowNeverReturnsMoreThanLimit(t *testing.T) {
	store, ctx := setupTestStore(t)

	for i := 0; i < 20; i++ {
		id := createTestMemory(t, store, ctx, fmt.Sprintf("scheduler throughput measurement alpha %d beta gamma", i))
		if err := store.StoreEmbedding(ctx, id, []float32{0.9, 0.1}, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding filler %d: %v", i, err)
		}
	}
	createTestMemory(t, store, ctx, "the listener on 8090 timed out, who owns it")

	for _, limit := range []int{1, 3, 5, 10} {
		results, err := store.SearchHybrid(ctx, "test-proj", "the listener on 8090 timed out, who owns it", []float32{0.9, 0.1}, limit)
		if err != nil {
			t.Fatalf("SearchHybrid(limit=%d): %v", limit, err)
		}
		if len(results) > limit {
			t.Errorf("limit=%d returned %d results", limit, len(results))
		}
	}
}

func TestFuseAndSelectWindowSkipsFTSReservationWhenWeightZero(t *testing.T) {
	fts := []Memory{{ID: "keyword-only"}}
	vec := make([]ScoredMemory, 0, 20)
	for i := 0; i < 20; i++ {
		vec = append(vec, ScoredMemory{MemoryID: fmt.Sprintf("vector-%02d", i), Score: float32(20 - i)})
	}
	p := DefaultSearchParams()
	p.FTSWeight = 0
	p.VecWeight = 1

	window := FuseAndSelectWindow(fts, vec, 10, p)
	for _, id := range window.IDs {
		if id == "keyword-only" {
			t.Fatal("zero FTS weight still reserved a keyword-only candidate")
		}
	}
}

func TestFuseAndRankBackfillsDeletedCandidate(t *testing.T) {
	store, ctx := setupTestStore(t)
	vec := make([]ScoredMemory, 0, 6)
	ids := make([]string, 0, 6)
	for i := 0; i < 6; i++ {
		id := createTestMemory(t, store, ctx, fmt.Sprintf("candidate %d", i))
		ids = append(ids, id)
		vec = append(vec, ScoredMemory{MemoryID: id, Score: float32(6 - i)})
	}

	beforeHybridHydrateFn.Store(func(selected []string) {
		if len(selected) == 0 {
			return
		}
		if err := store.Delete(ctx, selected[0]); err != nil {
			t.Fatalf("delete selected candidate: %v", err)
		}
	})
	t.Cleanup(func() { beforeHybridHydrateFn.Store(func([]string) {}) })

	p := DefaultSearchParams()
	p.DecayEnabled = false
	got, err := store.fuseAndRank(ctx, nil, vec, 5, p)
	if err != nil {
		t.Fatalf("fuseAndRank: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d results after selected-row deletion, want 5", len(got))
	}
	for _, want := range ids[1:] {
		found := false
		for _, memory := range got {
			if memory.ID == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("lower-ranked candidate %s was not backfilled", want)
		}
	}
}

// TestFuseAndSelectWindowWidth pins the function's own contract, which the
// search path cannot show: decayRank trims the window again on its way out, so
// an over-wide return would be invisible through SearchHybrid even though the
// function is what owns window selection.
func TestFuseAndSelectWindowWidth(t *testing.T) {
	mk := func(n int) ([]Memory, []ScoredMemory) {
		fts := make([]Memory, 0, n)
		vec := make([]ScoredMemory, 0, n)
		for i := 0; i < n; i++ {
			fts = append(fts, Memory{ID: fmt.Sprintf("f%02d", i)})
			vec = append(vec, ScoredMemory{MemoryID: fmt.Sprintf("v%02d", i)})
		}
		return fts, vec
	}

	t.Run("never wider than limit", func(t *testing.T) {
		fts, vec := mk(40)
		for _, limit := range []int{1, 3, 10} {
			if got := len(FuseAndSelectWindow(fts, vec, limit, DefaultSearchParams()).IDs); got > limit {
				t.Errorf("limit=%d returned %d ids", limit, got)
			}
		}
	})

	t.Run("decay reselect keeps the wider pool", func(t *testing.T) {
		fts, vec := mk(40)
		p := DefaultSearchParams()
		p.DecayReselect = true
		p.DecayEnabled = true
		if got := len(FuseAndSelectWindow(fts, vec, 10, p).IDs); got != 20 {
			t.Errorf("decay reselect returned %d ids, want the 2x pool it narrows from", got)
		}
	})

	t.Run("no limit yields no window", func(t *testing.T) {
		fts, vec := mk(5)
		if got := FuseAndSelectWindow(fts, vec, 0, DefaultSearchParams()); len(got.IDs) != 0 {
			t.Errorf("limit 0 returned %d ids", len(got.IDs))
		}
	})

	t.Run("ordering is deterministic across calls", func(t *testing.T) {
		fts, vec := mk(20)
		first := FuseAndSelectWindow(fts, vec, 10, DefaultSearchParams()).IDs
		for i := 0; i < 5; i++ {
			got := FuseAndSelectWindow(fts, vec, 10, DefaultSearchParams()).IDs
			for j := range got {
				if got[j] != first[j] {
					t.Fatalf("call %d differed at %d: %s vs %s", i, j, got[j], first[j])
				}
			}
		}
	})
}
