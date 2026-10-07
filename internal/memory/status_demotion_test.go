package memory

import (
	"context"
	"testing"
)

// Issue #559: `ghost_memory_search` ranked resolved memories and unrelated
// `_global` rows equally with live project memories. The fix is a
// multiplicative demotion applied inside the fusion seam, before the window
// cut, so a live project memory both outranks them and takes the
// slot a raw-score cut would have given them. Nothing here filters by rule —
// the demotion only scales a score — so a demoted row's fate is the window's
// ordinary rank question: TestResolvedMemoryStaysSearchable pins the case
// where it is the only match and must come back, TestStatusDemotionOwnsTheWindowCut
// the case where a live row takes its slot instead.
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

	set := explainedCandidates(t, store, ctx, "test-proj", needle, nil, 10, nil)
	rows := set.RankFacts

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
			t.Fatalf("%s has no ranking fact", tc.name)
		}
		if row.StatusFactor != tc.want {
			t.Errorf("%s status_factor = %v, want %v", tc.name, row.StatusFactor, tc.want)
		}
		if _, ok := rowByID(set, tc.id); !ok {
			t.Errorf("%s was not returned, but the search returns it", tc.name)
		}
	}
	if len(set.Rows) == 0 || set.Rows[0].ID != liveID {
		t.Errorf("first row = %v, want the live row: the demotion must sink the resolved and shared rows below it", set.Rows)
	}
}

// TestKeywordReservationSkipsDemotedRows: the keyword reservation admitted
// rows by raw FTS rank alone, so a demoted row in the top `limit/5` still
// took a window slot and evicted a live project row that the demoted pool
// would have kept. Reservation reads raw rank and demotion writes the fused
// score, so the two disagreed exactly when a demoted row led the keyword leg.
// The fix is that a row whose status factor is below 1 is never reserved.
//
// limit 5 is the smallest window that reserves anything (limit/5 = 1, and
// below five the reservation is skipped entirely), the demoted row carries
// the highest importance so FTS ranks it first, and seven identical rows
// overflow the five-slot window — which is the only time reservation runs at
// all (len(pool) > width).
func TestKeywordReservationSkipsDemotedRows(t *testing.T) {
	const needle = "temporal workflow replay fails the activity heartbeats"

	for _, tc := range []struct {
		name   string
		demote func(t *testing.T, store *Store, ctx context.Context) string
	}{
		{
			name: "_global row",
			demote: func(t *testing.T, store *Store, ctx context.Context) string {
				t.Helper()
				if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
					t.Fatalf("EnsureProject(_global): %v", err)
				}
				return createStatusMemory(t, store, ctx, "_global", needle, 0.9)
			},
		},
		{
			name: "resolved row",
			demote: func(t *testing.T, store *Store, ctx context.Context) string {
				t.Helper()
				id := createStatusMemory(t, store, ctx, "test-proj", needle, 0.9)
				if n, err := store.SetResolved(ctx, []string{id}); err != nil || n != 1 {
					t.Fatalf("SetResolved = (%d, %v), want (1, nil)", n, err)
				}
				return id
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, ctx := setupTestStore(t)
			demotedID := tc.demote(t, store, ctx)

			// Six live rows behind the demoted one. A plain cut of the
			// demoted pool returns the five highest-importance live rows;
			// the sixth (importance 0.3) is the row the buggy reservation
			// evicts to make room for the demoted rank-1 keyword hit.
			live := make([]string, 0, 6)
			for _, importance := range []float32{0.8, 0.7, 0.6, 0.5, 0.4, 0.3} {
				live = append(live, createStatusMemory(t, store, ctx, "test-proj", needle, importance))
			}

			// nil query vector: the keyword leg alone drives the window, so
			// every candidate is keyword-only and eligible for reservation.
			results, err := store.SearchHybrid(ctx, "test-proj", needle, nil, 5)
			if err != nil {
				t.Fatalf("SearchHybrid: %v", err)
			}
			pos := positions(results)

			for _, id := range live[:5] {
				if _, ok := pos[id]; !ok {
					t.Errorf("live project memory missing from the window of %d: %s — the reserved "+
						"slot went to a demoted row", len(results), contents(results))
				}
			}
			if got, ok := pos[demotedID]; ok {
				t.Errorf("demoted row returned at position %d while a live row was evicted: %s — "+
					"a row whose status factor is below 1 must never be reserved",
					got, contents(results))
			}
		})
	}
}

// TestSearchDemotesVectorOnlyRows: the vector leg carries each row's status so
// a semantic-only match cannot escape the demotion. Every row below shares no
// keyword with the query, so the FTS leg never retrieves them — the vector
// branch of fusion (projectID/resolved read from ScoredMemory) is the only
// thing that can tell a live row from a _global or resolved one here.
//
// Both demoted rows are given the better raw standing (closer to the query
// vector than the live row), the same convention as every other case in this
// file: without the vector status fields the demoted rows rank first and this
// test fails.
func TestSearchDemotesVectorOnlyRows(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}

	const query = "kubernetes readiness probe timeouts"
	globalID := createStatusMemory(t, store, ctx, "_global", "quokka wallaby numbat exhibit", 0.9)
	resolvedID := createStatusMemory(t, store, ctx, "test-proj", "zebra okapi tapir enclosure", 0.9)
	liveID := createStatusMemory(t, store, ctx, "test-proj", "axolotl narwhal manatee habitat", 0.8)

	queryVec := []float32{1, 0}
	for _, row := range []struct {
		id  string
		vec []float32
	}{
		{globalID, []float32{1, 0.05}},
		{resolvedID, []float32{1, 0.1}},
		{liveID, []float32{1, 0.3}},
	} {
		if err := store.StoreEmbedding(ctx, row.id, row.vec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding(%s): %v", row.id, err)
		}
	}
	if n, err := store.SetResolved(ctx, []string{resolvedID}); err != nil || n != 1 {
		t.Fatalf("SetResolved = (%d, %v), want (1, nil)", n, err)
	}

	results, err := store.SearchHybrid(ctx, "test-proj", query, queryVec, 10)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	pos := positions(results)
	requireBothPresent(t, pos, globalID, resolvedID, liveID)

	if pos[liveID] > pos[globalID] || pos[liveID] > pos[resolvedID] {
		t.Errorf("live row at %d, _global at %d, resolved at %d — a vector-only match outranked "+
			"the live project row: the vector leg must carry each row's status into fusion (results = %s)",
			pos[liveID], pos[globalID], pos[resolvedID], contents(results))
	}
}

// TestKeywordOnlyGlobalHitReturnsWhenWindowHasRoom: the reservation gate must
// not turn the demotion into a lookup filter. The `_global` row below is
// keyword-only — never embedded, so only the FTS leg can reach it — and it is
// the best keyword hit. In a hybrid search with room for every candidate it
// must still come back, ranked below the live rows like every other demoted
// row; the gate only ever bites when the window is full, which is the case
// TestKeywordReservationSkipsDemotedRows pins from the other side.
func TestKeywordOnlyGlobalHitReturnsWhenWindowHasRoom(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}

	const needle = "the shared opsign runbook forbids failover drills"
	globalID := createStatusMemory(t, store, ctx, "_global", needle, 0.9)

	// Live rows the vector leg reaches too, so the search is genuinely
	// hybrid (full RRF parameters) rather than the keyword-only fallback.
	queryVec := []float32{1, 0}
	live := make([]string, 0, 3)
	for i, importance := range []float32{0.8, 0.7, 0.6} {
		id := createStatusMemory(t, store, ctx, "test-proj", needle, importance)
		if err := store.StoreEmbedding(ctx, id, []float32{1, 0.1 * float32(i+1)}, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding live %d: %v", i, err)
		}
		live = append(live, id)
	}

	// limit 10 over four candidates: no cut runs, so the only thing that
	// could have dropped the _global row is a status rule.
	results, err := store.SearchHybrid(ctx, "test-proj", needle, queryVec, 10)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	pos := positions(results)
	requireBothPresent(t, pos, globalID, live[0], live[1], live[2])
	if _, ok := pos[globalID]; !ok {
		return // requireBothPresent already reported the missing row
	}

	for _, id := range live {
		if pos[globalID] < pos[id] {
			t.Errorf("_global row at position %d outranked a live project memory at %d — the demoted row must "+
				"still rank below the live ones when both come back (results = %s)",
				pos[globalID], pos[id], contents(results))
		}
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
		out += ExplainSnippet(m.Content, 40)
	}
	if out == "" {
		return "<none>"
	}
	return out
}
