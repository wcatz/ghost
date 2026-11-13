package memory

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// #559: resolved and project-scoped _global rows are demoted inside fusion.
// #1771: vector leg per-bucket depth keeps project rows from crowding.

func createStatusMemory(t *testing.T, store *Store, ctx context.Context, projectID, content string, importance float32) string {
	t.Helper()
	id, err := store.Create(ctx, projectID, Memory{Category: "gotcha", Content: content, Source: "manual", Importance: importance, Tags: []string{"test"}})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

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
			t.Errorf("missing %s", id)
		}
	}
}

func TestSearchDemotesResolvedBelowLive(t *testing.T) {
	store, ctx := setupTestStore(t)
	const needle = "kubernetes readiness probe times out during a rolling rollout"
	resolvedID := createStatusMemory(t, store, ctx, "test-proj", needle, 0.9)
	liveID := createStatusMemory(t, store, ctx, "test-proj", needle, 0.8)
	if n, err := store.SetResolved(ctx, []string{resolvedID}); err != nil || n != 1 {
		t.Fatalf("SetResolved: %d %v", n, err)
	}
	results, err := store.SearchHybrid(ctx, "test-proj", needle, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	pos := positions(results)
	requireBothPresent(t, pos, resolvedID, liveID)
	if pos[liveID] > pos[resolvedID] {
		t.Errorf("live=%d resolved=%d %s", pos[liveID], pos[resolvedID], contents(results))
	}
}

func TestSearchDemotesGlobalRowBelowProjectMemory(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatal(err)
	}
	const needle = "helmfile applies the sops secrets before the chart upgrade"
	globalID := createStatusMemory(t, store, ctx, "_global", needle, 0.9)
	liveID := createStatusMemory(t, store, ctx, "test-proj", needle, 0.8)
	results, err := store.SearchHybrid(ctx, "test-proj", needle, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	pos := positions(results)
	requireBothPresent(t, pos, globalID, liveID)
	if pos[liveID] > pos[globalID] {
		t.Errorf("live=%d global=%d %s", pos[liveID], pos[globalID], contents(results))
	}
}

func TestStatusDemotionOwnsTheWindowCut(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatal(err)
	}
	const needle = "postgres connection pool is exhausted by the migration job"
	for _, importance := range []float32{0.9, 0.8, 0.7} {
		createStatusMemory(t, store, ctx, "_global", needle, importance)
	}
	liveID := createStatusMemory(t, store, ctx, "test-proj", needle, 0.6)
	results, err := store.SearchHybrid(ctx, "test-proj", needle, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := positions(results)[liveID]; !ok {
		t.Errorf("live missing: %s", contents(results))
	}
}

func TestResolvedMemoryStaysSearchable(t *testing.T) {
	store, ctx := setupTestStore(t)
	const needle = "the ghost sqlite pool is capped at one connection"
	resolvedID := createStatusMemory(t, store, ctx, "test-proj", needle, 0.7)
	if n, err := store.SetResolved(ctx, []string{resolvedID}); err != nil || n != 1 {
		t.Fatalf("SetResolved: %d %v", n, err)
	}
	results, err := store.SearchHybrid(ctx, "test-proj", needle, nil, 10)
	if err != nil || len(results) == 0 || results[0].ID != resolvedID {
		t.Fatalf("results=%s err=%v", contents(results), err)
	}
}

func TestCrossProjectSearchDoesNotDemoteGlobalRows(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatal(err)
	}
	const needle = "supersession rewrites the opcert rotation runbook"
	globalID := createStatusMemory(t, store, ctx, "_global", needle, 0.9)
	createStatusMemory(t, store, ctx, "test-proj", needle, 0.8)
	results, err := store.SearchHybridAll(ctx, needle, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	pos := positions(results)
	requireBothPresent(t, pos, globalID)
	if pos[globalID] != 0 {
		t.Errorf("global rank=%d %s", pos[globalID], contents(results))
	}
}

func TestExplainReportsStatusFactor(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatal(err)
	}
	const needle = "the reflection phase bills the calling harness subscription"
	resolvedID := createStatusMemory(t, store, ctx, "test-proj", needle, 0.9)
	liveID := createStatusMemory(t, store, ctx, "test-proj", needle, 0.8)
	globalID := createStatusMemory(t, store, ctx, "_global", needle, 0.7)
	if n, err := store.SetResolved(ctx, []string{resolvedID}); err != nil || n != 1 {
		t.Fatalf("SetResolved: %d %v", n, err)
	}
	set := explainedCandidates(t, store, ctx, "test-proj", needle, nil, 10, nil)
	for _, tc := range []struct {
		id   string
		want float64
	}{
		{liveID, 1.0}, {resolvedID, resolvedDemotionFactor}, {globalID, globalDemotionFactor},
	} {
		row, ok := set.RankFacts[tc.id]
		if !ok || row.StatusFactor != tc.want {
			t.Fatalf("%s ok=%v got=%v want=%v", tc.id, ok, row.StatusFactor, tc.want)
		}
		if _, ok := rowByID(set, tc.id); !ok {
			t.Fatalf("missing row %s", tc.id)
		}
	}
	if len(set.Rows) == 0 || set.Rows[0].ID != liveID {
		t.Fatalf("first=%v", set.Rows)
	}
}

func TestKeywordReservationSkipsDemotedRows(t *testing.T) {
	const needle = "temporal workflow replay fails the activity heartbeats"
	for _, tc := range []struct {
		name   string
		demote func(t *testing.T, store *Store, ctx context.Context) string
	}{
		{"_global row", func(t *testing.T, store *Store, ctx context.Context) string {
			if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
				t.Fatal(err)
			}
			return createStatusMemory(t, store, ctx, "_global", needle, 0.9)
		}},
		{"resolved row", func(t *testing.T, store *Store, ctx context.Context) string {
			id := createStatusMemory(t, store, ctx, "test-proj", needle, 0.9)
			if n, err := store.SetResolved(ctx, []string{id}); err != nil || n != 1 {
				t.Fatalf("SetResolved: %d %v", n, err)
			}
			return id
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, ctx := setupTestStore(t)
			demotedID := tc.demote(t, store, ctx)
			live := make([]string, 0, 6)
			for _, importance := range []float32{0.8, 0.7, 0.6, 0.5, 0.4, 0.3} {
				live = append(live, createStatusMemory(t, store, ctx, "test-proj", needle, importance))
			}
			results, err := store.SearchHybrid(ctx, "test-proj", needle, nil, 5)
			if err != nil {
				t.Fatal(err)
			}
			pos := positions(results)
			for _, id := range live[:5] {
				if _, ok := pos[id]; !ok {
					t.Fatalf("missing live %s in %s", id, contents(results))
				}
			}
			if got, ok := pos[demotedID]; ok {
				t.Fatalf("demoted at %d in %s", got, contents(results))
			}
		})
	}
}

func TestSearchDemotesVectorOnlyRows(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatal(err)
	}
	const query = "kubernetes readiness probe timeouts"
	globalID := createStatusMemory(t, store, ctx, "_global", "quokka wallaby numbat exhibit", 0.9)
	resolvedID := createStatusMemory(t, store, ctx, "test-proj", "zebra okapi tapir enclosure", 0.9)
	liveID := createStatusMemory(t, store, ctx, "test-proj", "axolotl narwhal manatee habitat", 0.8)
	queryVec := []float32{1, 0}
	for _, row := range []struct {
		id  string
		vec []float32
	}{{globalID, []float32{1, 0.05}}, {resolvedID, []float32{1, 0.1}}, {liveID, []float32{1, 0.3}}} {
		if err := store.StoreEmbedding(ctx, row.id, row.vec, "test-model"); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := store.SetResolved(ctx, []string{resolvedID}); err != nil || n != 1 {
		t.Fatalf("SetResolved: %d %v", n, err)
	}
	results, err := store.SearchHybrid(ctx, "test-proj", query, queryVec, 10)
	if err != nil {
		t.Fatal(err)
	}
	pos := positions(results)
	requireBothPresent(t, pos, globalID, resolvedID, liveID)
	if pos[liveID] > pos[globalID] || pos[liveID] > pos[resolvedID] {
		t.Fatalf("live=%d global=%d resolved=%d %s", pos[liveID], pos[globalID], pos[resolvedID], contents(results))
	}
}

func TestKeywordOnlyGlobalHitReturnsWhenWindowHasRoom(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatal(err)
	}
	const needle = "the shared opsign runbook forbids failover drills"
	globalID := createStatusMemory(t, store, ctx, "_global", needle, 0.9)
	queryVec := []float32{1, 0}
	live := make([]string, 0, 3)
	for i, importance := range []float32{0.8, 0.7, 0.6} {
		id := createStatusMemory(t, store, ctx, "test-proj", needle, importance)
		if err := store.StoreEmbedding(ctx, id, []float32{1, 0.1 * float32(i+1)}, "test-model"); err != nil {
			t.Fatal(err)
		}
		live = append(live, id)
	}
	results, err := store.SearchHybrid(ctx, "test-proj", needle, queryVec, 10)
	if err != nil {
		t.Fatal(err)
	}
	pos := positions(results)
	requireBothPresent(t, pos, globalID, live[0], live[1], live[2])
	for _, id := range live {
		if pos[globalID] < pos[id] {
			t.Fatalf("global=%d live=%d %s", pos[globalID], pos[id], contents(results))
		}
	}
}

func TestVectorSearchDoesNotCrowdProjectRows(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		id := createStatusMemory(t, store, ctx, "_global", fmt.Sprintf("unrelated global note %d", i), 0.7)
		if err := store.StoreEmbedding(ctx, id, []float32{1, 0.01 * float32(i+1)}, "test-model"); err != nil {
			t.Fatal(err)
		}
	}
	projectID := createStatusMemory(t, store, ctx, "test-proj", "my project helm release workflow", 0.8)
	if err := store.StoreEmbedding(ctx, projectID, []float32{1, 0.05}, "test-model"); err != nil {
		t.Fatal(err)
	}
	results, _, err := store.searchVectorLegAt(ctx, "test-proj", []float32{1, 0}, 4, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range results {
		if r.MemoryID == projectID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("project row missing: %v", results)
	}
}

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
