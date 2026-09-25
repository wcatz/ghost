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

// TestFuseAndRankBackfillStaysInScope is the interaction between the two
// behaviours that meet in this file. Scope is narrowed from the candidate pool
// before the window is cut, and a selected row that disappears before hydration
// is backfilled from that same pool — so the backfill has to be narrowed too.
// Filtering only the window would let an out-of-scope row return in the deleted
// row's place, which is the one thing the narrowing exists to prevent.
func TestFuseAndRankBackfillStaysInScope(t *testing.T) {
	store, ctx := setupTestStore(t)
	createScoped := func(content, environment string) string {
		t.Helper()
		id, err := store.Create(ctx, "test-proj", Memory{
			Category: "fact",
			Content:  content,
			Source:   "tool",
			Scope:    map[string]string{"environment": environment},
		})
		if err != nil {
			t.Fatalf("create %s memory: %v", environment, err)
		}
		return id
	}

	// The one in-scope row ranks first, so it is the one selected and then
	// deleted; the two development rows rank below it and are what a backfill
	// ignoring scope would return in its place.
	inScope := createScoped("in scope candidate", "production")
	vec := []ScoredMemory{{MemoryID: inScope, Score: 0.9, Scope: map[string]string{"environment": "production"}}}
	for i, score := range []float32{0.8, 0.7} {
		id := createScoped(fmt.Sprintf("out of scope candidate %d", i), "development")
		vec = append(vec, ScoredMemory{MemoryID: id, Score: score, Scope: map[string]string{"environment": "development"}})
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
	p.Scope = map[string]string{"environment": "production"}
	got, err := store.fuseAndRank(ctx, nil, vec, 3, p)
	if err != nil {
		t.Fatalf("fuseAndRank: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d results (%v), want none: the only in-scope candidate was deleted, and "+
			"an out-of-scope row must not backfill it", len(got), got)
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

// TestFuseAndSelectWindowAppliesScope covers the fusion path directly, which
// the tool-level tests cannot reach: they run against a store with no
// embeddings, so they take the FTS-only fallback and never enter fusion. The
// development rows are vector-only, so this also proves the vector leg carries
// each candidate's scope into selection rather than relying on a second lookup
// or on scope already present in the FTS result.
func TestFuseAndSelectWindowAppliesScope(t *testing.T) {
	prod := map[string]string{"environment": "production"}
	fts := []Memory{{ID: "p1", Content: "production one", Scope: prod}}
	vec := []ScoredMemory{
		{MemoryID: "d1", Score: 0.9, Scope: map[string]string{"environment": "development"}},
		{MemoryID: "d2", Score: 0.8, Scope: map[string]string{"environment": "development"}},
		{MemoryID: "d3", Score: 0.7, Scope: map[string]string{"environment": "development"}},
		{MemoryID: "p1", Score: 0.1, Scope: prod},
	}

	p := DefaultSearchParams()
	p.Scope = prod
	// limit 2 with three development rows ranked above the production one: the
	// window can only contain p1 if the ineligible rows are dropped from the
	// combined pool before the cut, not filtered after it.
	got := FuseAndSelectWindow(fts, vec, 2, p)
	if len(got.IDs) != 1 || got.IDs[0] != "p1" {
		t.Errorf("window = %v, want [p1] — the three development vector candidates would otherwise "+
			"fill both slots before scope narrows the pool", got.IDs)
	}

	// And with no scope set, selection is unchanged.
	plain := FuseAndSelectWindow(fts, vec, 2, DefaultSearchParams())
	if len(plain.IDs) != 2 {
		t.Errorf("unscoped window = %v, want 2 ids", plain.IDs)
	}
}

// TestFuseAndSelectWindowScopeKeepsUnmentionedRows: silence about a requested
// key is not disagreement, so a row with no scope at all stays eligible.
func TestFuseAndSelectWindowScopeKeepsUnmentionedRows(t *testing.T) {
	fts := []Memory{{ID: "u1", Content: "general knowledge"}}
	vec := []ScoredMemory{{MemoryID: "u1", Score: 0.5}}
	p := DefaultSearchParams()
	p.Scope = map[string]string{"environment": "production"}

	got := FuseAndSelectWindow(fts, vec, 5, p)
	if len(got.IDs) != 1 || got.IDs[0] != "u1" {
		t.Errorf("window = %v, want [u1] — an unscoped row is not in conflict with a requested scope", got.IDs)
	}
}

func TestSearchHybridScopedFTSOnlyUsesSelectionSeam(t *testing.T) {
	store, ctx := setupTestStore(t)
	createScoped := func(content, environment string) string {
		t.Helper()
		id, err := store.Create(ctx, "test-proj", Memory{
			Category: "fact",
			Content:  content,
			Source:   "tool",
			Scope:    map[string]string{"environment": environment},
		})
		if err != nil {
			t.Fatalf("create %s memory: %v", environment, err)
		}
		return id
	}
	for _, content := range []string{
		"database configuration pooling timeout retry",
		"database configuration isolation repeatable read",
		"database configuration index tuning planner",
	} {
		createScoped(content, "development")
	}
	want := createScoped("database configuration replication lag failover quorum elections together with enough neutral detail to rank below the shorter rows", "production")

	got, err := store.SearchHybridScoped(ctx, "test-proj", "database configuration", nil, 2, map[string]string{"environment": "production"})
	if err != nil {
		t.Fatalf("SearchHybridScoped: %v", err)
	}
	if len(got) != 1 || got[0].ID != want {
		t.Fatalf("scoped FTS-only results = %v, want only %s", got, want)
	}
}

func TestSearchHybridScopedCarriesVectorOnlyScope(t *testing.T) {
	store, ctx := setupTestStore(t)
	createScoped := func(content, environment string) string {
		t.Helper()
		id, err := store.Create(ctx, "test-proj", Memory{
			Category: "fact",
			Content:  content,
			Source:   "tool",
			Scope:    map[string]string{"environment": environment},
		})
		if err != nil {
			t.Fatalf("create %s memory: %v", environment, err)
		}
		return id
	}

	want := createScoped("database configuration production", "production")
	for i, score := range []float32{0.9, 0.8, 0.7} {
		id := createScoped(fmt.Sprintf("semantic-only development candidate %d", i), "development")
		if err := store.StoreEmbedding(ctx, id, []float32{score, 1 - score}, "test-model"); err != nil {
			t.Fatalf("store development embedding %d: %v", i, err)
		}
	}
	if err := store.StoreEmbedding(ctx, want, []float32{0.1, 0.99}, "test-model"); err != nil {
		t.Fatalf("store production embedding: %v", err)
	}

	got, err := store.SearchHybridScoped(ctx, "test-proj", "database configuration", []float32{1, 0}, 2, map[string]string{"environment": "production"})
	if err != nil {
		t.Fatalf("SearchHybridScoped: %v", err)
	}
	if len(got) != 1 || got[0].ID != want {
		t.Fatalf("scoped hybrid results = %v, want only %s; vector-only scope did not reach selection", got, want)
	}
}

func TestSearchHybridScopedUsesConfiguredVectorFloor(t *testing.T) {
	store, ctx := setupTestStore(t)
	weak, err := store.Create(ctx, "test-proj", Memory{Category: "fact", Content: "semantic-only weak match", Source: "tool"})
	if err != nil {
		t.Fatalf("create weak memory: %v", err)
	}
	strong, err := store.Create(ctx, "test-proj", Memory{
		Category: "fact",
		Content:  "database configuration replication failover",
		Source:   "tool",
		Scope:    map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create strong memory: %v", err)
	}
	if err := store.StoreEmbedding(ctx, weak, []float32{0.8, 0.6}, "test-model"); err != nil {
		t.Fatalf("store weak embedding: %v", err)
	}
	if err := store.StoreEmbedding(ctx, strong, []float32{1, 0}, "test-model"); err != nil {
		t.Fatalf("store strong embedding: %v", err)
	}
	store.SetVectorMinSimilarity(0.9)

	got, err := store.SearchHybridScoped(ctx, "test-proj", "database configuration", []float32{1, 0}, 2, map[string]string{"environment": "production"})
	if err != nil {
		t.Fatalf("SearchHybridScoped: %v", err)
	}
	if len(got) != 1 || got[0].ID != strong {
		t.Fatalf("results = %v, want only %s; the scoped entry point bypassed the configured vector floor", got, strong)
	}
}
