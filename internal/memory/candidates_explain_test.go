package memory

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// These tests pin the store half of explain: Candidates, asked to explain,
// returns the facts the ranking stages stamped as they decided (RankFacts), and
// the pool candidates the returned rows do not carry (Excluded, FloorDropped).
// The assembler projects them into the payload; what is pinned here is that
// every number it will read is one the ranking used, and that asking for the
// record changes nothing about the retrieval.

// explainedCandidates runs one retrieval with the record requested.
func explainedCandidates(t *testing.T, s *Store, ctx context.Context, project, query string, vec []float32, limit int, scope map[string]string) *CandidateSet {
	t.Helper()
	set, err := s.Candidates(ctx, explainRequest(project, query, vec, limit, scope, true))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	return set
}

func explainRequest(project, query string, vec []float32, limit int, scope map[string]string, explain bool) CandidateRequest {
	cond := CondHybrid
	if vec == nil {
		cond = CondFTSOnly
	}
	mode := ProjectScoped
	if project == "" {
		mode = GlobalOnly
	}
	return CandidateRequest{
		ProjectID: project,
		Mode:      mode,
		Query:     query,
		QueryVec:  vec,
		Scope:     scope,
		Condition: cond,
		Params:    DefaultSearchParams(),
		Now:       time.Now().UTC(),
		Fetch:     Fetch{FTSTopK: limit * 2, VectorTopK: limit * 2, Limit: limit},
		Explain:   explain,
	}
}

func rowByID(set *CandidateSet, id string) (Candidate, bool) {
	for _, c := range set.Rows {
		if c.ID == id {
			return c, true
		}
	}
	return Candidate{}, false
}

func inMemories(ms []Memory, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

// TestCandidatesExplainChangesNothingAboutTheRetrieval is the proof the flag is
// a request to record and not a knob: the same request with and without it
// returns the same rows in the same order with the same scores, the same leg
// statuses and the same edges. A scope, a status demotion and a near-duplicate
// pair are all in play so the flag has every chance to move something.
func TestCandidatesExplainChangesNothingAboutTheRetrieval(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	const needle = "the ledger replay job reconciles settled invoices nightly"
	scoped := func(content, env string) string {
		id, err := store.Create(ctx, "test-proj", Memory{Category: "fact", Content: content, Source: "manual",
			Importance: 0.8, Scope: map[string]string{"environment": env}})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return id
	}
	prod := scoped(needle, "production")
	scoped(needle+" in development", "development")
	resolved := createTestMemory(t, store, ctx, needle)
	if n, err := store.SetResolved(ctx, []string{resolved}); err != nil || n != 1 {
		t.Fatalf("SetResolved = (%d, %v)", n, err)
	}
	if _, err := store.Create(ctx, "_global", Memory{Category: "fact", Content: needle, Source: "manual", Importance: 0.7}); err != nil {
		t.Fatalf("Create(_global): %v", err)
	}
	dupA := createTestMemory(t, store, ctx, "the cache warmer runs on the read replica every hour for ledger replay")
	dupB := createTestMemory(t, store, ctx, "the cache warmer runs on the read replica hourly for ledger replay")
	if err := store.CreateLink(ctx, dupA, dupB, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	for _, id := range []string{prod, resolved, dupA, dupB} {
		if err := store.StoreEmbedding(ctx, id, []float32{0.9, 0.2}, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
	}

	for _, scope := range []map[string]string{nil, {"environment": "production"}} {
		now := time.Now().UTC()
		plainReq := explainRequest("test-proj", needle+" ledger replay", []float32{0.9, 0.2}, 3, scope, false)
		explainReq := plainReq
		explainReq.Explain = true
		plainReq.Now, explainReq.Now = now, now

		plain, err := store.Candidates(ctx, plainReq)
		if err != nil {
			t.Fatalf("Candidates(plain): %v", err)
		}
		explained, err := store.Candidates(ctx, explainReq)
		if err != nil {
			t.Fatalf("Candidates(explain): %v", err)
		}
		if len(plain.Rows) == 0 {
			t.Fatalf("scope %v: fixture returned no rows", scope)
		}
		if !reflect.DeepEqual(plain.Rows, explained.Rows) {
			t.Errorf("scope %v: explain changed the rows:\nplain:   %+v\nexplain: %+v", scope, plain.Rows, explained.Rows)
		}
		if !reflect.DeepEqual(plain.Legs, explained.Legs) || !reflect.DeepEqual(plain.Edges, explained.Edges) ||
			plain.EdgesStatus != explained.EdgesStatus || plain.Widened != explained.Widened || plain.Unrecorded != explained.Unrecorded {
			t.Errorf("scope %v: explain changed the leg statuses or the edges", scope)
		}
		if plain.RankFacts != nil || plain.Excluded != nil || plain.FloorDropped != nil {
			t.Errorf("scope %v: a plain retrieval carries explain-only facts: the production path must record nothing", scope)
		}
		if explained.RankFacts == nil {
			t.Errorf("scope %v: an explain retrieval carries no ranking facts", scope)
		}
	}
}

// TestFloorDroppedCandidatesAreReported: a candidate the vector floor removes
// outright (the keyword leg never reached it) is carried in FloorDropped with
// the cosine that removed it and no demotion recorded, and a dual-leg row whose
// vector term alone was cut stays a candidate on its keyword term.
func TestFloorDroppedCandidatesAreReported(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	shared, err := store.Create(ctx, "_global", Memory{Category: "fact",
		Content: "postgres autovacuum thresholds for wraparound protection", Source: "manual", Importance: 0.8})
	if err != nil {
		t.Fatalf("Create(_global): %v", err)
	}
	if err := store.StoreEmbedding(ctx, shared, []float32{0.999, 0.0447}, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	dual := createTestMemory(t, store, ctx, "the compaction schedule runs on the first sunday of each month")
	if err := store.StoreEmbedding(ctx, dual, []float32{0.8, 0.6}, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	store.SetVectorMinSimilarity(0.9999)

	set := explainedCandidates(t, store, ctx, "test-proj", "compaction schedule sunday", []float32{1, 0}, 5, nil)
	if !inMemories(set.FloorDropped, shared) {
		t.Fatalf("the floor-dropped shared row is not in FloorDropped: %+v", set.FloorDropped)
	}
	f := set.RankFacts[shared]
	if f == nil || !f.FloorDropped || f.FloorScore <= 0 {
		t.Fatalf("floor-dropped row fact = %+v, want floor_dropped with the cosine", f)
	}
	if f.StatusFactor != 1.0 || f.Base != 0 {
		t.Errorf("a never-scored row reports status_factor=%v base=%v, want 1.0 and 0: no demotion ran on it", f.StatusFactor, f.Base)
	}
	if f.ProjectMatch || f.RowProject != "_global" {
		t.Errorf("floor-dropped shared row project_match=%v row_project=%q, want false / _global", f.ProjectMatch, f.RowProject)
	}
	if _, ok := rowByID(set, shared); ok {
		t.Errorf("the floor-dropped row was returned in Rows")
	}
	d := set.RankFacts[dual]
	if d == nil || !d.FloorDropped || d.FTSRank < 0 || d.Base <= 0 {
		t.Errorf("the dual-leg row's fact = %+v, want floor_dropped on its vector term and a keyword-only score", d)
	}
	if _, ok := rowByID(set, dual); !ok {
		t.Errorf("a dual-leg row whose vector term the floor cut must stay in the answer on its keyword term")
	}
}

// TestRankFactsAttributeEachDemotionToASpecificMemory: a penalty count says the
// row moved; the fact names the memory that decided it.
func TestRankFactsAttributeEachDemotionToASpecificMemory(t *testing.T) {
	store, ctx := setupTestStore(t)
	fresh := createTestMemory(t, store, ctx, "the archive bucket rotates to cold storage monthly")
	stale := createTestMemory(t, store, ctx, "the archive bucket rotates to cold storage monthly")
	if err := store.CreateLink(ctx, fresh, stale, "supersedes", 1, "llm"); err != nil {
		t.Fatalf("CreateLink(supersedes): %v", err)
	}
	dupA := createTestMemory(t, store, ctx, "the cache warmer runs on the read replica every hour")
	dupB := createTestMemory(t, store, ctx, "the cache warmer runs on the read replica hourly")
	if err := store.CreateLink(ctx, dupA, dupB, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink(related): %v", err)
	}
	other := createTestMemory(t, store, ctx, "the cache warmer is drained before a deploy")

	set := explainedCandidates(t, store, ctx, "test-proj", "archive bucket cache warmer replica", nil, 10, nil)
	if f := set.RankFacts[stale]; f == nil || f.SupersedePenalty != 1 || len(f.SupersededBy) != 1 || f.SupersededBy[0] != fresh {
		t.Errorf("superseded row fact = %+v, want penalty 1 naming %s", f, fresh)
	}
	loser := ""
	for _, id := range []string{dupA, dupB} {
		if set.RankFacts[id].NearDuplicatePenalty == 0 {
			continue
		}
		if loser != "" {
			t.Fatalf("both members of the near-duplicate pair carry a penalty")
		}
		loser = id
	}
	if loser == "" {
		t.Fatalf("no member of the near-duplicate pair carries a penalty")
	}
	winner := map[string]string{dupA: dupB, dupB: dupA}[loser]
	if f := set.RankFacts[loser]; f.NearDuplicatePenalty != 1 || len(f.NearDuplicateOf) != 1 || f.NearDuplicateOf[0] != winner {
		t.Errorf("near-duplicate loser fact = %+v, want penalty 1 naming %s", f, winner)
	}
	if f := set.RankFacts[other]; f.SupersedePenalty != 0 || f.NearDuplicatePenalty != 0 || len(f.SupersededBy) > 0 || len(f.NearDuplicateOf) > 0 {
		t.Errorf("an untouched row carries attribution: %+v", f)
	}
}

// TestExplainIsRefusedWhereThereIsNoRankingToExplain: a historical read ranks
// nothing and a passive retrieval scores nothing, so the store refuses both
// rather than returning explain fields it never computed.
func TestExplainIsRefusedWhereThereIsNoRankingToExplain(t *testing.T) {
	store, ctx := setupTestStore(t)
	asOf := explainRequest("test-proj", "anything", nil, 5, nil, true)
	at := asOf.Now.Add(-time.Hour)
	asOf.AsOf = &at
	if _, err := store.Candidates(ctx, asOf); err == nil {
		t.Error("explain with as_of was served; a historical read has no ranking to project")
	}
	passive := explainRequest("test-proj", "", nil, 5, nil, true)
	passive.Passive = []SlicePolicy{{Bucket: "test-proj", Order: "decay", OverFetch: 5, ItemCap: 5}}
	if _, err := store.Candidates(ctx, passive); err == nil {
		t.Error("explain with no query was served; a passive retrieval scores no candidate")
	}
}

// TestClampScopeKeysReportsTheRealLength: the cap is a rendering budget, so the
// count beside it is the real length and equals the list when nothing was cut.
func TestClampScopeKeysReportsTheRealLength(t *testing.T) {
	long := make([]string, 20)
	for i := range long {
		long[i] = fmt.Sprintf("zone-%02d", i)
	}
	names, total := ClampScopeKeys(long)
	if len(names) != maxScopeKeys || total != 20 {
		t.Errorf("ClampScopeKeys(20 keys) = %d named, total %d, want %d and 20", len(names), total, maxScopeKeys)
	}
	names[0] = "changed"
	if long[0] == "changed" {
		t.Error("ClampScopeKeys returned the caller's own backing array")
	}
	names, total = ClampScopeKeys([]string{"zone-01"})
	if len(names) != 1 || total != 1 {
		t.Errorf("ClampScopeKeys(1 key) = %d named, total %d, want 1 and 1", len(names), total)
	}
}
