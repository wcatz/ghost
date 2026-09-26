package memory

import (
	"context"
	"strings"
	"testing"
)

// Issue #559: `ghost_memory_search` ranked resolved memories and unrelated
// `_global` rows equally with live project memories. The fix is a
// multiplicative demotion applied inside the fusion seam, before the window
// cut, so a comparable live project memory both outranks them and takes the
// slot a raw-score cut would have given them. Nothing is filtered: a demoted
// row still wins when it is the best thing that matched.
//
// Every case below gives the demoted row the *better* raw standing (identical
// wording, higher importance, so FTS lists it first), which makes the expected
// order deterministic and makes the test fail without the demotion rather than
// depend on SQLite's tie order.

// createStatusMemory inserts content under projectID with an explicit
// importance, so the test controls the FTS tie-break that decides which of two
// equally-matching rows the raw ranking puts first.
func createStatusMemory(t *testing.T, store *Store, ctx context.Context, projectID, content string, importance float32) string {
	t.Helper()
	id, err := store.Create(ctx, projectID, Memory{
		Category:   "gotcha",
		Content:    content,
		Source:     "manual",
		Importance: importance,
		Tags:       []string{"test"},
	})
	if err != nil {
		t.Fatalf("Create(%s, %q): %v", projectID, content, err)
	}
	return id
}

// positions returns the index of each memory in results, so assertions can
// speak about rank instead of slicing.
func positions(results []Memory) map[string]int {
	pos := make(map[string]int, len(results))
	for i, m := range results {
		pos[m.ID] = i
	}
	return pos
}

func requireBothPresent(t *testing.T, pos map[string]int, want ...string) {
	t.Helper()
	for _, id := range want {
		if _, ok := pos[id]; !ok {
			t.Errorf("memory %s missing from the results — demotion must rank a row down, never filter it out", id)
		}
	}
}

// TestSearchDemotesResolvedBelowLive: an equally matching resolved memory must
// rank below the live project memory, and must still be returned.
func TestSearchDemotesResolvedBelowLive(t *testing.T) {
	store, ctx := setupTestStore(t)

	const needle = "kubernetes readiness probe times out during a rolling rollout"
	resolvedID := createStatusMemory(t, store, ctx, "test-proj", needle, 0.9)
	liveID := createStatusMemory(t, store, ctx, "test-proj", needle, 0.8)
	if n, err := store.SetResolved(ctx, []string{resolvedID}); err != nil || n != 1 {
		t.Fatalf("SetResolved = (%d, %v), want (1, nil)", n, err)
	}

	results, err := store.SearchHybrid(ctx, "test-proj", needle, nil, 10)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	pos := positions(results)
	requireBothPresent(t, pos, resolvedID, liveID)

	// The two rows match the query identically; only importance separates
	// them, and it puts the resolved row first. Losing that lead is the fix.
	if pos[liveID] > pos[resolvedID] {
		t.Errorf("resolved memory ranks above the live one: live at %d, resolved at %d — "+
			"resolved results = %s", pos[liveID], pos[resolvedID], contents(results))
	}
}

// TestSearchDemotesGlobalRowBelowProjectMemory: a project-scoped search must
// rank an equally matching `_global` row below the project's own memory, and
// must still return it.
func TestSearchDemotesGlobalRowBelowProjectMemory(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}

	const needle = "helmfile applies the sops secrets before the chart upgrade"
	globalID := createStatusMemory(t, store, ctx, "_global", needle, 0.9)
	liveID := createStatusMemory(t, store, ctx, "test-proj", needle, 0.8)

	results, err := store.SearchHybrid(ctx, "test-proj", needle, nil, 10)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	pos := positions(results)
	requireBothPresent(t, pos, globalID, liveID)

	if pos[liveID] > pos[globalID] {
		t.Errorf("_global memory ranks above the project's own: project at %d, _global at %d — "+
			"results = %s", pos[liveID], pos[globalID], contents(results))
	}
}

// TestStatusDemotionOwnsTheWindowCut: the factor has to be applied before the
// cut, not just to the finished window. The legs fetch limit*2 rows, so with a
// window of two this pool holds three _global rows above the project's own
// memory — a post-cut reorder would never get the project row into a window it
// was never selected for, only rearrange rows that already were.
func TestStatusDemotionOwnsTheWindowCut(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}

	const needle = "postgres connection pool is exhausted by the migration job"
	for _, importance := range []float32{0.9, 0.8, 0.7} {
		createStatusMemory(t, store, ctx, "_global", needle, importance)
	}
	// Lowest importance: FTS lists it last, so a raw cut of the four fetched
	// rows to a window of two drops it.
	liveID := createStatusMemory(t, store, ctx, "test-proj", needle, 0.6)

	results, err := store.SearchHybrid(ctx, "test-proj", needle, nil, 2)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	pos := positions(results)
	if _, ok := pos[liveID]; !ok {
		t.Errorf("the project's own memory never entered the window of %d: %s — "+
			"the demotion must own the cut, not just the order of what survived it", len(results), contents(results))
	}
}

// TestResolvedMemoryStaysSearchable: demotion must never become a filter.
// A resolved memory is the only match, so it is the answer.
func TestResolvedMemoryStaysSearchable(t *testing.T) {
	store, ctx := setupTestStore(t)

	const needle = "the ghost sqlite pool is capped at one connection"
	resolvedID := createStatusMemory(t, store, ctx, "test-proj", needle, 0.7)
	if n, err := store.SetResolved(ctx, []string{resolvedID}); err != nil || n != 1 {
		t.Fatalf("SetResolved = (%d, %v), want (1, nil)", n, err)
	}

	results, err := store.SearchHybrid(ctx, "test-proj", needle, nil, 10)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(results) == 0 || results[0].ID != resolvedID {
		t.Errorf("results = %s, want the resolved memory first — it is the only match", contents(results))
	}
}

// TestCrossProjectSearchDoesNotDemoteGlobalRows pins the condition on the
// `_global` factor: it fires when a specific project is searched, where the
// shared row has the project's own memories to lose to. A cross-project
// search has no "elsewhere", so `_global` keeps its raw standing.
func TestCrossProjectSearchDoesNotDemoteGlobalRows(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}

	const needle = "supersession rewrites the opcert rotation runbook"
	globalID := createStatusMemory(t, store, ctx, "_global", needle, 0.9)
	createStatusMemory(t, store, ctx, "test-proj", needle, 0.8)

	results, err := store.SearchHybridAll(ctx, needle, nil, 10)
	if err != nil {
		t.Fatalf("SearchHybridAll: %v", err)
	}
	pos := positions(results)
	requireBothPresent(t, pos, globalID)
	if pos[globalID] != 0 {
		t.Errorf("_global ranks %d in a cross-project search, want 0 — the project-scoped "+
			"factor must not fire when no project is being searched (results = %s)",
			pos[globalID], contents(results))
	}
}

// TestExplainReportsStatusFactor: explain reuses the search's own membership
// and ranking seam, so it has to report the factor that actually ranked each
// row — a demoted result whose demotion is invisible in the diagnosis is the
// bug this issue describes, restated for the debug path.
func TestExplainReportsStatusFactor(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}

	const needle = "the reflection phase bills the calling harness subscription"
	resolvedID := createStatusMemory(t, store, ctx, "test-proj", needle, 0.9)
	liveID := createStatusMemory(t, store, ctx, "test-proj", needle, 0.8)
	globalID := createStatusMemory(t, store, ctx, "_global", needle, 0.7)
	if n, err := store.SetResolved(ctx, []string{resolvedID}); err != nil || n != 1 {
		t.Fatalf("SetResolved = (%d, %v), want (1, nil)", n, err)
	}

	ex, err := store.ExplainSearchScoped(ctx, "test-proj", needle, nil, 10, nil)
	if err != nil {
		t.Fatalf("ExplainSearchScoped: %v", err)
	}
	rows := make(map[string]ExplainRow, len(ex.Rows))
	for _, row := range ex.Rows {
		rows[row.ID] = row
	}

	for _, tc := range []struct {
		name string
		id   string
		want float64
	}{
		{"live project row", liveID, 1.0},
		{"resolved row", resolvedID, resolvedDemotionFactor},
		{"project-scoped _global row", globalID, globalDemotionFactor},
	} {
		row, ok := rows[tc.id]
		if !ok {
			t.Fatalf("%s missing from the explanation: %+v", tc.name, ex.Rows)
		}
		if row.StatusFactor != tc.want {
			t.Errorf("%s status_factor = %v, want %v", tc.name, row.StatusFactor, tc.want)
		}
		if !row.Included {
			t.Errorf("%s reported excluded at rank %d, want included — the search returned it", tc.name, row.Rank)
		}
	}
	if rows[liveID].Rank != 1 {
		t.Errorf("live row rank = %d, want 1 — explain must report the same order the demotion produced", rows[liveID].Rank)
	}

	sawNote := false
	for _, note := range ex.Notes {
		if strings.Contains(note, "status_factor") {
			sawNote = true
		}
	}
	if !sawNote {
		t.Errorf("explanation demoted rows without disclosing the factor: %v", ex.Notes)
	}
}

// contents renders result content for failure messages, truncated so a failing
// assertion stays readable.
func contents(results []Memory) string {
	out := ""
	for i, m := range results {
		if i > 0 {
			out += " | "
		}
		out += explainSnippet(m.Content, 40)
	}
	if out == "" {
		return "<none>"
	}
	return out
}
