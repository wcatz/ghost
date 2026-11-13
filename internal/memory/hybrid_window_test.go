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
