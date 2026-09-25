package memory

import (
	"context"
	"fmt"
	"testing"
)

// scopedVectorFixture builds `conflicts` development-scoped memories ranked
// above `eligible` production-scoped ones, plus a production-scoped source that
// is its own nearest neighbour. Every embedding is distinct, because a tie would
// leave the ranking up to sort.Slice and make the assertions depend on it.
func scopedVectorFixture(t *testing.T, conflicts, eligible int) (*Store, context.Context, string) {
	t.Helper()
	store, ctx := setupTestStore(t)

	add := func(content, environment string, vec []float32) string {
		t.Helper()
		id, err := store.Create(ctx, "test-proj", Memory{
			Category: "fact",
			Content:  content,
			Source:   "manual",
			Scope:    map[string]string{"environment": environment},
		})
		if err != nil {
			t.Fatalf("Create %q: %v", content, err)
		}
		if err := store.StoreEmbedding(ctx, id, vec, "test"); err != nil {
			t.Fatalf("StoreEmbedding %q: %v", content, err)
		}
		return id
	}

	source := add("production source", "production", []float32{1, 0})
	// Conflicting rows sit just off the source vector, so they outrank the
	// eligible ones without tying anything.
	for i := 0; i < conflicts; i++ {
		add(fmt.Sprintf("development row %d", i), "development",
			[]float32{1, 0.005 * float32(i+1)})
	}
	for i := 0; i < eligible; i++ {
		add(fmt.Sprintf("production row %d", i), "production",
			[]float32{0.9, 0.44 + 0.01*float32(i)})
	}
	return store, ctx, source
}

// TestSearchVectorScopedFiltersBeforeTheLimit is the property the linking worker
// depends on, and the reason it is a separate method rather than a post-filter at
// the call site. SearchVector truncates to its limit, so filtering afterwards
// hands back rows the caller may not use: with a limit of 2 and one eligible row
// behind eight conflicting ones, the unscoped call returns two development rows
// and the eligible row is nowhere in the result, at any limit a caller could
// guess.
func TestSearchVectorScopedFiltersBeforeTheLimit(t *testing.T) {
	store, ctx, source := scopedVectorFixture(t, 8, 2)
	prod := map[string]string{"environment": "production"}

	// Sanity: the fixture only demonstrates anything if the unscoped call really
	// does fill its limit with rows the scoped call must drop.
	unscoped, err := store.SearchVector(ctx, "test-proj", []float32{1, 0}, 2)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	conflicting := 0
	for _, sm := range unscoped {
		if sm.MemoryID != source && ScopesConflict(prod, sm.Scope) {
			conflicting++
		}
	}
	if conflicting == 0 {
		t.Fatalf("unscoped SearchVector(limit=2) returned no scope-conflicting row "+
			"(%v), so this fixture does not exercise the cut", unscoped)
	}

	scoped, err := store.SearchVectorScoped(ctx, "test-proj", []float32{1, 0}, 2, prod)
	if err != nil {
		t.Fatalf("SearchVectorScoped: %v", err)
	}
	if len(scoped) != 2 {
		t.Fatalf("got %d candidates, want 2 — the limit must count eligible rows", len(scoped))
	}
	for _, sm := range scoped {
		if sm.MemoryID != source && ScopesConflict(prod, sm.Scope) {
			t.Errorf("SearchVectorScoped returned scope-conflicting candidate %s", sm.MemoryID)
		}
	}
}

// TestSearchVectorScopedNilScopeMatchesSearchVector pins the search path: hybrid
// retrieval and the benchmark harness call SearchVector, and this refactor must
// not move a single ranked row for them.
func TestSearchVectorScopedNilScopeMatchesSearchVector(t *testing.T) {
	store, ctx, _ := scopedVectorFixture(t, 8, 3)

	want, err := store.SearchVector(ctx, "test-proj", []float32{1, 0}, 5)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	for _, scope := range []map[string]string{nil, {}} {
		got, err := store.SearchVectorScoped(ctx, "test-proj", []float32{1, 0}, 5, scope)
		if err != nil {
			t.Fatalf("SearchVectorScoped(%v): %v", scope, err)
		}
		if len(got) != len(want) {
			t.Fatalf("scope %v returned %d candidates, want the %d SearchVector returns", scope, len(got), len(want))
		}
		for i := range want {
			if got[i].MemoryID != want[i].MemoryID || got[i].Score != want[i].Score {
				t.Errorf("scope %v row %d = %s/%v, want %s/%v", scope, i,
					got[i].MemoryID, got[i].Score, want[i].MemoryID, want[i].Score)
			}
		}
	}
}
