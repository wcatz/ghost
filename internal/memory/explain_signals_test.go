package memory

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The six tests below are issue #583's contract. They are written against the
// store's own search, not against explain's internals, so what they pin is the
// promise explain makes: every number in the payload is one the ranking path
// actually used, and a field the ranking path does not apply says so instead of
// carrying an invented contribution.

// explainStoreFor opens a file-backed store and registers the project the
// shared createTestMemory helper writes to, so a test that only reads scores
// does not pay for a shared fixture.
func explainStoreFor(t *testing.T, name string) (*Store, context.Context) {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), name+".sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, nil)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "test-proj", "/tmp/"+name, "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s, ctx
}

// explainRowByID indexes an explanation for lookup.
func explainRowByID(ex SearchExplain) map[string]ExplainRow {
	rows := make(map[string]ExplainRow, len(ex.Rows))
	for _, r := range ex.Rows {
		rows[r.ID] = r
	}
	return rows
}

// TestExplainScoreIsTheScoreTheRankingUsed is the load-bearing assertion of
// #583: explain must report the score the search ordered by, not a number it
// re-derived from the legs.
//
// The comparison is against Store.Candidates, which reaches the SAME ranking
// seam (fuseCandidatePool -> selectWindow -> decayRank -> demoteResults) through
// the other entry point, and whose Candidate.Base is documented as the fused
// score the window was cut on. Two independent callers of one seam must see one
// number: if explain re-derived it, a change to the fusion weights, the RRF
// constant, the vector floor or the status factor would move one and not the
// other, and the explanation would describe a ranking the search did not
// produce.
func TestExplainScoreIsTheScoreTheRankingUsed(t *testing.T) {
	store, ctx := setupTestStore(t)
	query := []float32{0.9, 0.2}

	// A resolved row, a _global row and a live project row, all matching, so
	// the status factor actually moves a score rather than being 1.0 by
	// accident. Without a demoted row this test would pass on a re-derivation
	// that forgot the factor entirely.
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	const needle = "the ledger replay job reconciles settled invoices nightly"
	live := createTestMemory(t, store, ctx, needle)
	resolved := createTestMemory(t, store, ctx, needle)
	global, err := store.Create(ctx, "_global", Memory{
		Category: "fact", Content: needle, Source: "manual", Importance: 0.8, Tags: []string{"test"},
	})
	if err != nil {
		t.Fatalf("Create(_global): %v", err)
	}
	if n, err := store.SetResolved(ctx, []string{resolved}); err != nil || n != 1 {
		t.Fatalf("SetResolved = (%d, %v), want (1, nil)", n, err)
	}
	for _, id := range []string{live, resolved, global} {
		if err := store.StoreEmbedding(ctx, id, query, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding(%s): %v", id, err)
		}
	}

	const limit = 5
	set, err := store.Candidates(ctx, CandidateRequest{
		ProjectID: "test-proj",
		Mode:      ProjectScoped,
		Query:     needle,
		QueryVec:  query,
		Condition: CondHybrid,
		Params:    DefaultSearchParams(),
		Now:       time.Now().UTC(),
		Fetch:     Fetch{Limit: limit},
	})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	ex, err := store.ExplainSearch(ctx, "test-proj", needle, query, limit)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}
	rows := explainRowByID(ex)

	cand := make(map[string]Candidate, len(set.Rows))
	for _, c := range set.Rows {
		cand[c.ID] = c
	}
	// The window the two entry points share, in the order they ranked it.
	window := set.Rows
	if len(window) > limit {
		window = window[:limit]
	}

	seen := 0
	for i, c := range window {
		row, ok := rows[c.ID]
		if !ok {
			t.Fatalf("row %s is in the ranked window but missing from the explanation", c.ID)
		}
		if !row.Included {
			t.Errorf("row %s ranked %d in the window but explain marks it excluded: %+v", c.ID, i+1, row)
			continue
		}
		seen++
		if row.Rank != i+1 {
			t.Errorf("row %s explain rank = %d, want %d — explain must report the order the search produced", c.ID, row.Rank, i+1)
		}
		// The fused base, before status demotion and decay, is what the
		// explanation publishes. Multiplying it by the factor the search
		// applied has to land on the score the window was actually cut on.
		got := row.RRFScore * row.StatusFactor
		if math.Abs(got-c.Base) > 1e-12 {
			t.Errorf("row %s: explain rrf_score*status_factor = %v, the score the window was cut on = %v "+
				"(rrf %v, status %v) — explain re-derived a number the ranking path did not produce",
				c.ID, got, c.Base, row.RRFScore, row.StatusFactor)
		}
		// And with decay the same product is the value decayRank ordered by.
		// The two paths read the wall clock a moment apart, so this one is a
		// tolerance rather than an equality.
		want := c.Score
		if math.Abs(got*row.DecayFactor-want) > 1e-9 {
			t.Errorf("row %s: explain rrf*status*decay = %v, the score the order was ranked on = %v (decay %v)",
				c.ID, got*row.DecayFactor, want, row.DecayFactor)
		}
	}
	if seen != len(window) {
		t.Fatalf("only %d of %d window rows were marked included", seen, len(window))
	}
	// At least one row has to be demoted, or the comparison above never left
	// the all-1.0 case where a re-derivation that ignored the factor would
	// still agree.
	if rows[resolved].StatusFactor == 1.0 || rows[global].StatusFactor == 1.0 {
		t.Fatalf("fixture did not produce a demoted row (resolved %v, _global %v): the score comparison is vacuous without one",
			rows[resolved].StatusFactor, rows[global].StatusFactor)
	}
}

// TestExplainScopeStateAgreesWithTheRowsSearchReturns closes the class of bug in
// #571 structurally: the scope verdict explain publishes is the one window
// selection applied, so the two cannot disagree. A row the search returns is
// scope-matched; a row that is not returns names scope as the reason.
func TestExplainScopeStateAgreesWithTheRowsSearchReturns(t *testing.T) {
	store, ctx := explainStoreFor(t, "explain-scope-signals")
	mk := func(content, environment string) string {
		t.Helper()
		id, err := store.Create(ctx, "test-proj", Memory{
			Category: "fact", Content: content, Source: "manual", Importance: 0.8,
			Tags: []string{"test"}, Scope: map[string]string{"environment": environment},
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", content, err)
		}
		return id
	}
	dev := mk("the ingest worker batches rows before writing", "development")
	prod := mk("the ingest worker batches rows before writing", "production")
	unscoped := createTestMemory(t, store, ctx, "the ingest worker batches rows before writing")

	want := map[string]string{"environment": "production"}
	final, err := store.SearchHybridScoped(ctx, "test-proj", "ingest worker batches rows", nil, 3, want)
	if err != nil {
		t.Fatalf("SearchHybridScoped: %v", err)
	}
	ex, err := store.ExplainSearchScoped(ctx, "test-proj", "ingest worker batches rows", nil, 3, want)
	if err != nil {
		t.Fatalf("ExplainSearchScoped: %v", err)
	}
	rows := explainRowByID(ex)
	if len(rows) != 3 {
		t.Fatalf("explanation carries %d rows, want 3 (one per scoped candidate)", len(rows))
	}

	// The keys compared are named, so a reader can see which axis decided.
	for id, row := range rows {
		if got := strings.Join(row.ScopeKeysCompared, ","); got != "environment" {
			t.Errorf("row %s scope_keys_compared = %q, want the requested key", id, got)
		}
	}

	returned := map[string]bool{}
	for _, m := range final {
		returned[m.ID] = true
	}
	for _, id := range []string{prod, unscoped} {
		row, ok := rows[id]
		if !ok {
			t.Fatalf("row %s missing from the explanation", id)
		}
		if !returned[id] {
			t.Errorf("row %s was not returned by the scoped search, so explain cannot mark it included", id)
		}
		if !row.Included || !row.ScopeMatched {
			t.Errorf("row %s is in the scoped result but explain reports included=%v scope_matched=%v: %+v",
				id, row.Included, row.ScopeMatched, row)
		}
	}
	devRow, ok := rows[dev]
	if !ok {
		t.Fatal("the out-of-scope candidate is missing from the explanation")
	}
	if devRow.ScopeMatched {
		t.Errorf("the development row reports scope_matched=true under a production request: %+v", devRow)
	}
	if devRow.Included || returned[dev] {
		t.Errorf("the development row is included (%v, search returned it: %v) under a production scope: %+v",
			devRow.Included, returned[dev], devRow)
	}
	if !strings.Contains(devRow.Reason, "scope") {
		t.Errorf("the development row's reason is %q, want the scope exclusion named", devRow.Reason)
	}
}

// TestExplainReportsValidityAndSaysNoPenaltyIsApplied: a row past its own
// validity window is still returned by the search — the ranking path does not
// read validity — so an explanation that stayed silent would let a caller read
// a stale row as a current one. The state is reported; the penalty is 0 because
// nothing in this path multiplies by it, and the note says who does drop the
// row instead.
func TestExplainReportsValidityAndSaysNoPenaltyIsApplied(t *testing.T) {
	store, ctx := explainStoreFor(t, "explain-validity")
	const needle = "the release train ships thursday at noon"
	expired := createTestMemory(t, store, ctx, needle)
	future := createTestMemory(t, store, ctx, needle)
	live := createTestMemory(t, store, ctx, needle)

	past := time.Now().UTC().Add(-48 * time.Hour).Format("2006-01-02 15:04:05")
	soon := time.Now().UTC().Add(48 * time.Hour).Format("2006-01-02 15:04:05")
	if _, err := store.db.ExecContext(ctx,
		`UPDATE memories SET valid_until = ? WHERE id = ?`, past, expired); err != nil {
		t.Fatalf("set valid_until: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`UPDATE memories SET valid_from = ? WHERE id = ?`, soon, future); err != nil {
		t.Fatalf("set valid_from: %v", err)
	}

	ex, err := store.ExplainSearch(ctx, "test-proj", needle, nil, 5)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}
	rows := explainRowByID(ex)
	for _, tc := range []struct{ id, want string }{
		{expired, ValidityExpired},
		{future, ValidityFuture},
		{live, ValidityUnset},
	} {
		row, ok := rows[tc.id]
		if !ok {
			t.Fatalf("row %s missing from the explanation", tc.id)
		}
		if row.ValidityState != tc.want {
			t.Errorf("row %s validity_state = %q, want %q", tc.id, row.ValidityState, tc.want)
		}
		if row.ValidityPenalty != 0 {
			t.Errorf("row %s validity_penalty = %v, want 0: nothing in the ranking path multiplies by validity, "+
				"so a non-zero value would be a contribution the search never made", tc.id, row.ValidityPenalty)
		}
	}
	if !hasExplainNote(ex.Notes, "validity") {
		t.Errorf("notes = %v, want one saying how validity is (not) applied", ex.Notes)
	}
}

// TestExplainProjectMatchDistinguishesASharedRow: a _global row is admitted by
// the legs' project predicate, so it can be INSIDE a project-scoped answer — and
// it must report project_match=false while saying which project it does belong
// to. Reporting it as a match would hide the one fact that explains why a shared
// row shows up in a project's results at all, and reporting it as excluded would
// contradict the window it is sitting in.
func TestExplainProjectMatchDistinguishesASharedRow(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	const needle = "the compaction schedule runs on the first sunday of each month"
	live := createTestMemory(t, store, ctx, needle)
	shared, err := store.Create(ctx, "_global", Memory{
		Category: "fact", Content: needle, Source: "manual", Importance: 0.8, Tags: []string{"test"},
	})
	if err != nil {
		t.Fatalf("Create(_global): %v", err)
	}
	// A second shared row, resolved, so the no-project case below can tell the
	// two halves of statusDemotionFactor apart instead of only checking the one
	// its fixture happens to exercise.
	sharedResolved, err := store.Create(ctx, "_global", Memory{
		Category: "fact", Content: needle, Source: "manual", Importance: 0.8, Tags: []string{"test"},
	})
	if err != nil {
		t.Fatalf("Create(_global, resolved): %v", err)
	}
	if n, err := store.SetResolved(ctx, []string{sharedResolved}); err != nil || n != 1 {
		t.Fatalf("SetResolved = (%d, %v), want (1, nil)", n, err)
	}
	for _, id := range []string{live, shared, sharedResolved} {
		if err := store.StoreEmbedding(ctx, id, []float32{0.8, 0.6}, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding(%s): %v", id, err)
		}
	}

	ex, err := store.ExplainSearch(ctx, "test-proj", needle, []float32{0.8, 0.6}, 5)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}
	rows := explainRowByID(ex)
	if !rows[shared].Included {
		t.Fatalf("the shared row is not in the answer, so this fixture cannot test an admitted "+
			"non-matching row: %+v", ex.Rows)
	}
	if rows[shared].ProjectMatch {
		t.Errorf("an admitted _global row reports project_match=true in a project search: %+v", rows[shared])
	}
	if rows[shared].RowProject != "_global" {
		t.Errorf("row_project = %q, want _global — the field that makes project_match checkable",
			rows[shared].RowProject)
	}
	if rows[shared].StatusFactor != globalDemotionFactor {
		t.Errorf("the shared row's status_factor = %v, want %v: that factor is what demotes it, and "+
			"project_match=false without it would be a claim nothing acted on",
			rows[shared].StatusFactor, globalDemotionFactor)
	}
	if !rows[live].ProjectMatch || rows[live].RowProject != "test-proj" {
		t.Errorf("the project's own row reports project_match=%v row_project=%q, want true / test-proj",
			rows[live].ProjectMatch, rows[live].RowProject)
	}

	// A search that names NO project. Both legs then run the shared-row
	// predicate literally, so only `_global` rows are candidates — this is not
	// the cross-project entry point (`SearchHybridAll`, which explain never
	// reaches), and the payload does not span projects. What it pins is
	// statusDemotionFactor's two halves at once, which is why the fixture holds
	// one resolved shared row beside the live one: the global demotion is guarded
	// on `searchProjectID != ""` and must not fire here, while the resolved
	// demotion carries no such guard and must.
	noProject, err := store.ExplainSearch(ctx, "", needle, []float32{0.8, 0.6}, 5)
	if err != nil {
		t.Fatalf("ExplainSearch(no project): %v", err)
	}
	rowsNoProject := explainRowByID(noProject)
	if len(noProject.Rows) == 0 {
		t.Fatal("a search naming no project returned nothing: the shared rows should still be candidates")
	}
	for _, row := range noProject.Rows {
		if !row.ProjectMatch {
			t.Errorf("a search naming no project reports row %s (%s) as project_match=false, but it "+
				"expressed no project of its own for a row to fail to match", row.ID, row.RowProject)
		}
	}
	if !rowsNoProject[sharedResolved].Included {
		t.Fatalf("the resolved shared row is missing from a search naming no project: %+v", noProject.Rows)
	}
	if got := rowsNoProject[sharedResolved].StatusFactor; got != resolvedDemotionFactor {
		t.Errorf("the resolved shared row's status_factor = %v, want %v: resolved demotion is not "+
			"project-scoped — it applies wherever the row is, which is why SearchHybridAll's own "+
			"comment says it demotes resolved rows there but never global ones", got, resolvedDemotionFactor)
	}
	if got := rowsNoProject[shared].StatusFactor; got != 1.0 {
		t.Errorf("a live shared row's status_factor = %v with no project being searched, want 1.0: the "+
			"global demotion exists to stop a shared row padding a PROJECT's results, and there is no "+
			"project here to pad", got)
	}
}

// TestExplainFloorDroppedSharedRowReportsNoAppliedDemotion: a candidate the
// vector floor removes never reaches fusion, so no demotion ever runs on it — and
// the floor site is therefore the only place it can be recorded.
//
// The assertion is that status_factor stays 1.0, which looks wrong until the
// alternative is priced: a hypothetical factor beside an rrf_score of 0 invites an
// agent to multiply a number that ranks nothing, and gives the field a second
// meaning inside one payload while the jsonschema description, docs/usage.md and
// the ExplainRow doc all say it is the factor the ranking used. The shared-row
// fact travels on row_project, and floor_dropped is what says nothing was applied
// — two fields that each mean one thing, rather than one that means two.
func TestExplainFloorDroppedSharedRowReportsNoAppliedDemotion(t *testing.T) {
	store, ctx := setupTestStore(t)
	if err := store.EnsureProject(ctx, "_global", "/global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	// A shared row the keyword leg cannot reach, embedded just above the floor
	// so the vector leg returns it and the floor then removes it. Nothing else
	// can put it in the pool, so the floor stamp is the only record of it.
	shared, err := store.Create(ctx, "_global", Memory{
		Category: "fact", Content: "postgres autovacuum thresholds for wraparound protection",
		Source: "manual", Importance: 0.8, Tags: []string{"test"},
	})
	if err != nil {
		t.Fatalf("Create(_global): %v", err)
	}
	if err := store.StoreEmbedding(ctx, shared, []float32{0.999, 0.0447}, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	// A live project row that matches the query, so the search has an answer and
	// the explanation is not one floor-dropped row.
	createTestMemory(t, store, ctx, "the compaction schedule runs on the first sunday of each month")
	store.SetVectorMinSimilarity(0.9999)

	ex, err := store.ExplainSearch(ctx, "test-proj", "compaction schedule sunday", []float32{1, 0}, 5)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}
	row, ok := explainRowByID(ex)[shared]
	if !ok {
		t.Fatalf("the floor-dropped shared row is missing from the explanation: %+v", ex.Rows)
	}
	if !row.FloorDropped {
		t.Fatalf("the shared row is not reported as floor-dropped, so this fixture is not exercising "+
			"the floor site: %+v", row)
	}
	if row.Included {
		t.Errorf("a floor-dropped row is reported included: %+v", row)
	}
	if !strings.Contains(row.Reason, "floor") {
		t.Errorf("reason = %q, want the floor named", row.Reason)
	}
	if row.ProjectMatch || row.RowProject != "_global" {
		t.Errorf("the floor-dropped shared row reports project_match=%v row_project=%q, want false / "+
			"_global: it belongs to no searched project however the floor judged it",
			row.ProjectMatch, row.RowProject)
	}
	// The one-meaning contract: no demotion ran, so the applied factor is 1.0,
	// and the shared-row fact is on row_project rather than smuggled into a
	// multiplier meant for a score that does not exist.
	if row.StatusFactor != 1.0 {
		t.Errorf("the floor-dropped row's status_factor = %v, want 1.0: no demotion was applied to it, "+
			"and reporting the factor it WOULD carry puts a multiplier beside an rrf_score of 0 — the field "+
			"is documented, in the jsonschema description and in docs/usage.md, as the factor the ranking used",
			row.StatusFactor)
	}
	if row.RRFScore != 0 {
		t.Errorf("a never-scored row reports rrf_score = %v, want 0: fusion gave it no score, and a "+
			"non-zero one would claim otherwise", row.RRFScore)
	}
	// And the payload must not claim a demotion it never applied. Every row
	// fusion actually scored here is a live project row, so nothing was
	// demoted, and the note telling a reader to multiply rrf_score by
	// status_factor would be a claim about a decision that did not happen.
	if hasExplainNote(ex.Notes, "status_factor is applied") {
		t.Errorf("notes = %v, want no status-demotion note: this payload demoted nothing, so the "+
			"sentence is a claim about a decision the ranking never made", ex.Notes)
	}
}

// TestExplainReportsConfidenceAndProvenanceAsNotApplied: both are readable
// columns and neither is multiplied by anything. Reporting the column alone
// would read as "this contributed 0.9 of score"; reporting a contribution would
// invent one. The contract is the column, an explicit zero delta and a note.
func TestExplainReportsConfidenceAndProvenanceAsNotApplied(t *testing.T) {
	store, ctx := explainStoreFor(t, "explain-confidence")
	id := createTestMemory(t, store, ctx, "the retry budget is three attempts per upstream call")
	conf := 0.42
	if _, err := store.db.ExecContext(ctx,
		`UPDATE memories SET confidence = ? WHERE id = ?`, conf, id); err != nil {
		t.Fatalf("set confidence: %v", err)
	}

	ex, err := store.ExplainSearch(ctx, "test-proj", "retry budget attempts upstream", nil, 5)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}
	row, ok := explainRowByID(ex)[id]
	if !ok {
		t.Fatalf("row %s missing from the explanation", id)
	}
	if row.Confidence == nil || math.Abs(*row.Confidence-conf) > 1e-12 {
		t.Errorf("confidence = %v, want the stored %v", row.Confidence, conf)
	}
	if row.ConfidenceContribution != 0 {
		t.Errorf("confidence_contribution = %v, want 0: no confidence multiplier exists in the ranking path",
			row.ConfidenceContribution)
	}
	if row.ProvenanceWeight != explainProvenanceOff {
		t.Errorf("provenance_weight = %q, want %q — the signal is off, not a number nobody multiplies by",
			row.ProvenanceWeight, explainProvenanceOff)
	}
	if row.ProvenanceContribution != 0 {
		t.Errorf("provenance_contribution = %v, want 0", row.ProvenanceContribution)
	}
	if !hasExplainNote(ex.Notes, "confidence") || !hasExplainNote(ex.Notes, "provenance") {
		t.Errorf("notes = %v, want one naming confidence and one naming provenance as not applied", ex.Notes)
	}
}

// TestExplainAttributesEachDemotionToASpecificMemory: a penalty count alone
// says the row moved and not what moved it. Each window-scoped demotion has to
// name the other memory that decided it, because that is the row the caller
// has to look at next.
func TestExplainAttributesEachDemotionToASpecificMemory(t *testing.T) {
	store, ctx := explainStoreFor(t, "explain-attribution")

	// A supersede pair: stale is replaced by fresh, and both must be in the
	// window for the penalty to exist at all.
	fresh := createTestMemory(t, store, ctx, "the archive bucket rotates to cold storage monthly")
	stale := createTestMemory(t, store, ctx, "the archive bucket rotates to cold storage monthly")
	if err := store.CreateLink(ctx, fresh, stale, "supersedes", 1, "llm"); err != nil {
		t.Fatalf("CreateLink(supersedes): %v", err)
	}
	// A near-duplicate pair, kept lexically distinct so neither folds into the
	// other on save.
	dupA := createTestMemory(t, store, ctx, "the cache warmer runs on the read replica every hour")
	dupB := createTestMemory(t, store, ctx, "the cache warmer runs on the read replica hourly")
	if err := store.CreateLink(ctx, dupA, dupB, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink(related): %v", err)
	}
	other := createTestMemory(t, store, ctx, "the cache warmer is drained before a deploy")

	ex, err := store.ExplainSearch(ctx, "test-proj", "archive bucket cache warmer replica", nil, 10)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}
	rows := explainRowByID(ex)

	if row := rows[stale]; !explainsAny(row.SupersededBy, fresh) {
		t.Errorf("the superseded row names %v, want the superseder %s — a penalty count without the "+
			"counterpart leaves the caller with nothing to read next", row.SupersededBy, fresh)
	}
	if row := rows[stale]; row.SupersedePenalty != 1 {
		t.Errorf("supersede_penalty = %d, want 1", row.SupersedePenalty)
	}

	// Exactly one member of the near-duplicate pair loses, and it must be the
	// one the search demoted, not an arbitrary one.
	loser := ""
	for _, id := range []string{dupA, dupB} {
		if rows[id].NearDuplicatePenalty == 0 {
			continue
		}
		if loser != "" {
			t.Fatalf("both members of the near-duplicate pair carry a penalty (%s and %s): the attribution "+
				"disagrees with the demotion, which sinks exactly one", loser, id)
		}
		loser = id
	}
	if loser == "" {
		t.Fatalf("no member of the near-duplicate pair carries a penalty: %+v", ex.Rows)
	}
	winner := map[string]string{dupA: dupB, dupB: dupA}[loser]
	if !explainsAny(rows[loser].NearDuplicateOf, winner) {
		t.Errorf("the demoted duplicate %s names %v, want the winner it lost to (%s)", loser,
			rows[loser].NearDuplicateOf, winner)
	}

	// A row neither edge touches carries no attribution at all, so a reader
	// can tell "nothing demoted this" from "something did and we cannot say
	// what".
	if row := rows[other]; len(row.SupersededBy) > 0 || len(row.NearDuplicateOf) > 0 {
		t.Errorf("an untouched row carries attribution %v / %v, want none", row.SupersededBy, row.NearDuplicateOf)
	}
	// And the two signals the ranking path does NOT apply are reported as
	// absent, with the reason in the notes.
	if !hasExplainNote(ex.Notes, "contradicts") {
		t.Errorf("notes = %v, want one saying a contradicts edge causes no penalty here", ex.Notes)
	}
	if !hasExplainNote(ex.Notes, "diversity") {
		t.Errorf("notes = %v, want one saying no per-bucket diversity cap is applied here", ex.Notes)
	}
}

// TestExplainTruncatesWithAnExplicitMarker: a pathological candidate set must
// not inflate the payload, and a reader must be able to see that it was cut
// rather than reading a short list as the whole truth. The included rows are
// the search's answer, so they are never the rows dropped.
func TestExplainTruncatesWithAnExplicitMarker(t *testing.T) {
	store, ctx := explainStoreFor(t, "explain-budget")
	// A corpus whose candidate union is larger than the row budget keeps. The
	// union is what both legs return, so it grows with the LIMIT rather than
	// with the corpus: the tool's own ceiling is a 100-row window, and each leg
	// then fetches 200, so a well-matched corpus really does produce a
	// four-hundred-row candidate set at the largest limit a caller can ask for.
	const limit = 100
	for i := range 2 * limit {
		if _, _, _, err := store.Upsert(ctx, "test-proj", "fact",
			"vaultwarden sync retries the unix socket path "+strconv.Itoa(i),
			"manual", 0.5, nil); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}

	// A row the KEYWORD leg cannot reach, embedded so the vector leg finds it at
	// cosine 1.0. It therefore wins the window on the fused score and sits LAST
	// in the candidate list (the list runs leg by leg), which is what makes "the
	// budget never drops a returned row" a guarantee rather than a lucky
	// ordering: a naive cut at the budget would drop it.
	lateWinner := createTestMemory(t, store, ctx, "an unrelated note about the postgres autovacuum schedule")
	if err := store.StoreEmbedding(ctx, lateWinner, []float32{1, 0}, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	ex, err := store.ExplainSearch(ctx, "test-proj", "vaultwarden sync unix socket", []float32{1, 0}, limit)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}
	if len(ex.Rows) == 0 {
		t.Fatal("no rows explained")
	}
	if ex.Truncation == nil {
		t.Fatalf("a %d-row payload from a corpus this size was returned whole: the budget is not applied", len(ex.Rows))
	}
	if ex.Truncation.MaxRows != explainMaxRows {
		t.Errorf("truncation max_rows = %d, want the documented budget %d", ex.Truncation.MaxRows, explainMaxRows)
	}
	if ex.Truncation.RowsOmitted <= 0 {
		t.Errorf("truncation rows_omitted = %d, want a positive count", ex.Truncation.RowsOmitted)
	}
	if len(ex.Rows) > explainMaxRows {
		t.Errorf("payload carries %d rows, over the documented budget of %d", len(ex.Rows), explainMaxRows)
	}
	// The marker is only honest if the rows it kept include the answer.
	if !hasExplainNote(ex.Notes, "truncated") {
		t.Errorf("notes = %v, want one naming the truncation", ex.Notes)
	}
	searched, err := store.SearchHybrid(ctx, "test-proj", "vaultwarden sync unix socket", []float32{1, 0}, limit)
	if err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}
	if len(searched) == 0 || searched[0].ID != lateWinner {
		t.Fatalf("fixture did not produce a vector-leg winner at rank 1 (got %d results, first %v): "+
			"the case this test exists for needs a returned row late in the candidate list",
			len(searched), searched)
	}
	kept := explainRowByID(ex)
	for _, m := range searched {
		row, ok := kept[m.ID]
		if !ok {
			t.Errorf("row %s was returned by the search but the truncated payload dropped it: "+
				"truncation may only spend the budget on candidates, never on the answer", m.ID)
			continue
		}
		if !row.Included {
			t.Errorf("row %s was returned by the search but kept as excluded", m.ID)
		}
	}
}

// hasExplainNote reports whether any note mentions substr.
// TestExplainDecayFactorIsTheRankingPathOwn pins the decay record, which is
// easy to leave untested and expensive when it goes: every other explain fixture
// seeds `fact` rows, and DecayFactor returns 1.0 for `fact` unconditionally, so
// deleting the whole decayRank trace block leaves the suite green. It also takes
// `searchTrace.now` with it — the clock the ranking ordered by — and explain then
// falls back to its own wall clock, which reports a different factor on every row
// old enough for the difference to show.
//
// The fixture therefore uses a decaying category and an old created_at, and
// checks both halves separately. The white-box half is the mutation guard: the
// ranking path must WRITE the factor and the clock onto the trace, because
// explain's recomputed fallback agrees with it at the same instant and the two
// can only be told apart by looking at what was recorded.
func TestExplainDecayFactorIsTheRankingPathOwn(t *testing.T) {
	store, ctx := setupTestStore(t)
	// `gotcha` falls to the default branch: tau 30, floor 0.15. At 100 days the
	// factor is 1/(1+100/30) = 0.2308, which no rounding-free comparison against
	// 1.0 can pass by accident. Create stamps created_at itself, so the age is
	// written directly — the same way the existing decay tests do it.
	aged, err := store.Create(ctx, "test-proj", Memory{
		Category: "gotcha", Content: "the archive bucket cache warmer needs the replica restarted first",
		Source: "manual", Importance: 0.8, Tags: []string{"test"},
	})
	if err != nil {
		t.Fatalf("Create(gotcha): %v", err)
	}
	created := time.Now().UTC().Add(-100 * 24 * time.Hour)
	if _, err := store.db.ExecContext(ctx, `UPDATE memories SET created_at = ? WHERE id = ?`,
		created.Format("2006-01-02 15:04:05"), aged); err != nil {
		t.Fatalf("set created_at: %v", err)
	}
	ex, err := store.ExplainSearch(ctx, "test-proj", "archive bucket cache warmer replica", []float32{0.8, 0.6}, 5)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}
	row, ok := explainRowByID(ex)[aged]
	if !ok {
		t.Fatalf("the gotcha row is missing from the explanation: %+v", ex.Rows)
	}
	if !row.Included {
		t.Fatalf("the gotcha row was excluded, so this fixture is not exercising the window: %+v", row)
	}

	// The public half: the number is this row's own decaying factor, and the two
	// halves of the report agree with each other — age_days feeds decay_factor, so
	// a reader can check one against the other rather than take either on trust.
	// The durable tier, because the fixture's rows are durable ones: the tier half
	// of the factor is 1.0 there, so this stays a check that age_days explains
	// decay_factor rather than one that also pins the session decay.
	want := DecayFactor("gotcha", RetentionProject, false, row.AgeDays)
	if row.DecayFactor != want {
		t.Errorf("decay_factor = %v, want DecayFactor(\"gotcha\", project, false, age_days=%v) = %v: the two must be "+
			"one number and its input, or neither is checkable", row.DecayFactor, row.AgeDays, want)
	}
	if row.DecayFactor >= 1.0 {
		t.Errorf("decay_factor = %v, want a factor below 1.0: this fixture is a `gotcha` row 100 days old, and "+
			"the whole point is that decay bites, so a 1.0 here means the fixture stopped reaching the branch "+
			"(a `fact` or `preference` category would return 1.0 unconditionally and make the rest vacuous)",
			row.DecayFactor)
	}
	if row.AgeDays < 99 || row.AgeDays > 101 {
		t.Errorf("age_days = %v, want ~100: a factor computed from a different age is a different number, and "+
			"this is what pins the clock the ranking measured against", row.AgeDays)
	}

	// The white-box half, and the one the mutation deletes. Read through the same
	// entry point explain uses, with the trace held by the test rather than
	// internal to ExplainSearch, so the assertion is on what the ranking path
	// WROTE rather than on what explain did with it.
	tr := newSearchTrace(nil)
	p := DefaultSearchParams()
	p.MinSimilarity = store.vectorMinSimilarityFloor()
	p.ProjectID = "test-proj"
	p.trace = tr
	fts, err := store.SearchFTS(ctx, "test-proj", "archive bucket cache warmer replica", 5*2)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	vec, err := store.SearchVector(ctx, "test-proj", []float32{0.8, 0.6}, 5*2)
	if err != nil {
		t.Fatalf("SearchVector: %v", err)
	}
	if _, err := store.fuseAndRank(ctx, fts, vec, 5, p); err != nil {
		t.Fatalf("fuseAndRank: %v", err)
	}
	tc := tr.row(aged)
	if tc == nil {
		t.Fatalf("the ranking path recorded no trace row for the gotcha candidate at all")
	}
	if tc.Decay == 0 {
		t.Error("the ranking path left tracedCandidate.Decay at zero: explain falls back to computing the factor " +
			"itself, which agrees today and silently stops agreeing the moment anything moves either clock")
	}
	// The two searches above ran some gap apart, so each measured the row's age
	// against its own `now`. The bound is therefore a CLOCK-GAP bound, not a
	// precision claim.
	//
	// The bound is 1e-3 days — 86 seconds. Two back-to-back searches cannot
	// plausibly be 86 seconds apart even on a loaded runner (CI's first attempt at
	// this test failed at a 12ms gap, which is what set the scale), while any
	// offset the code could actually produce is far above it: an hour is 1/24 =
	// 0.0417 days, 42x the bound.
	//
	// The FACTOR is bounded by 1/30 of that gap, and the 1/30 is derived rather
	// than chosen: this row is a `gotcha`, so DecayFactor takes its default
	// branch, 1/(1+ageDays/30), whose derivative is 1/30 at the origin and
	// decays from there — so a gap of g days moves the factor by at most g/30.
	// The 45-day pattern/architecture branch is gentler still. A future tau
	// shorter than 30 would need the constant re-derived, and the note below says
	// why that is a comment-level obligation and not a live risk.
	//
	// None of these comparisons is the guard. The mutation this exists for deletes
	// the trace block, which leaves the recorded factor and the clock at exactly
	// zero, and both of those are checked exactly, immediately below and above.
	// These two are supporting evidence that the values agree to within the gap.
	const (
		dayGap = 1e-3
		// tauDays is DecayFactor's default branch, in days.
		tauDays = 30.0
	)
	if d := math.Abs(tc.Decay - row.DecayFactor); d > dayGap/tauDays {
		t.Errorf("the factor the ranking recorded (%v) is not the one explain reported (%v): the payload must "+
			"carry the ranking's number, not one that happens to match it", tc.Decay, row.DecayFactor)
	}
	if tr.now.IsZero() {
		t.Error("searchTrace.now was never set: without it explain measures every row's age against its own " +
			"wall clock, and the factor it reports for an old row is then not the one that ranked anything")
	}
	if d := math.Abs(tc.AgeDays - row.AgeDays); d > dayGap {
		t.Errorf("the age the ranking recorded (%v) is not the one explain reported (%v): the clock is the "+
			"whole point, so the two must be the same measurement to within the gap between two searches",
			tc.AgeDays, row.AgeDays)
	}
}

// TestExplainNamesBothSidesOfTheKeywordReservation covers the one admission
// decision no score can explain: a keyword-only hit promoted into the window
// below the cut, taking a slot from a row that outscored it. Both sides are
// reported — keyword_reserved / took_slot_from on the promoted row,
// displaced_by on the loser — and without them "why is this row in the answer"
// has no answer, because its score is below the cut by construction.
//
// The fixture has to hit every guard at once, which is why this case had no test
// at all until now: the reservation needs limit ≥ 5 (for a non-zero slot count),
// len(pool) > width, a candidate with no vector leg at all, that candidate
// ranking inside the top limit/5 of the keyword leg, and a non-reserved row
// inside the window for it to evict. The keyword-only row carries NO embedding, so
// the vector leg cannot return it however close its text is.
func TestExplainNamesBothSidesOfTheKeywordReservation(t *testing.T) {
	store, ctx := setupTestStore(t)

	// The reserved row: it matches both query terms and they are rare, so BM25
	// puts it at the top of the keyword leg, but it has no embedding so the vector
	// leg never sees it. Its score is therefore a lone keyword term and it lands
	// below every dual-leg row.
	reserved, err := store.Create(ctx, "test-proj", Memory{
		Category: "fact", Content: "quasar calibration is a manual step",
		Source: "manual", Importance: 0.8, Tags: []string{"test"},
	})
	if err != nil {
		t.Fatalf("Create(reserved): %v", err)
	}
	// Twelve dual-leg rows, so the window (limit 10) is full before the
	// reservation runs and there is a non-reserved row in it to evict.
	dual := make([]string, 0, 12)
	for i := range 12 {
		id, err := store.Create(ctx, "test-proj", Memory{
			Category: "fact",
			Content:  fmt.Sprintf("the quasar calibration notes file %d covers stage %d", i, i),
			Source:   "manual", Importance: 0.8, Tags: []string{"test"},
		})
		if err != nil {
			t.Fatalf("Create(dual %d): %v", i, err)
		}
		// Every one of them close to the query, so they beat the keyword-only row
		// on the vector leg and fill the window.
		if err := store.StoreEmbedding(ctx, id, []float32{0.99, 0.14}, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding(dual %d): %v", i, err)
		}
		dual = append(dual, id)
	}

	// limit 10 gives slots = 2, so a keyword-only hit in the top two of the
	// keyword leg is reserved. Verified below rather than assumed.
	ex, err := store.ExplainSearch(ctx, "test-proj", "quasar calibration", []float32{1, 0}, 10)
	if err != nil {
		t.Fatalf("ExplainSearch: %v", err)
	}
	rows := explainRowByID(ex)

	res, ok := rows[reserved]
	if !ok {
		t.Fatalf("the keyword-only row is missing from the explanation, so this fixture is not reaching the "+
			"candidate set: %+v", ex.Rows)
	}
	if !res.KeywordReserved {
		t.Fatalf("the keyword-only row reports keyword_reserved=false with %d candidates, so the reservation "+
			"did not fire and the rest of this test would pass vacuously: %+v", len(ex.Rows), res)
	}
	if !res.Included {
		t.Errorf("a reserved row is not in the answer, which is the one thing the reservation is for: %+v", res)
	}
	if res.TookSlotFrom == "" {
		t.Errorf("keyword_reserved is set but took_slot_from is empty: the promoted row without the row it "+
			"displaced does not answer why it is in the window: %+v", res)
	}
	if res.VectorRank != -1 {
		t.Errorf("the reserved row reports vector_rank=%d, so the vector leg DID retrieve it and this fixture "+
			"is not exercising a keyword-only hit: %+v", res.VectorRank, res)
	}
	// Its score really is below the cut, which is the whole reason the field
	// exists: nothing in the number explains its presence.
	if res.Rank == 0 {
		t.Errorf("a reserved row reports rank 0: it is in the answer, so it must carry a 1-based rank: %+v", res)
	}

	// What the window holds, checked rather than assumed: still the 10 rows the
	// caller asked for, and exactly one of them is not a dual-leg row — the
	// keyword-only row the reservation promoted into it. Too few dual rows and the
	// pool is never wider than the window, the reservation cannot fire, and the
	// KeywordReserved guard above is then the only thing that would have said so.
	//
	// This does NOT count the exchanges. Every included row lands in exactly one
	// of dualIn/nonDualIn, so a third count would be arithmetic on the other two
	// rather than a new observation, and a second exchange would leave all three
	// unchanged. What pins the count of exchanges is the loop further down: no row
	// other than these two may report keyword_reserved or displaced_by.
	included, dualIn, nonDualIn := 0, 0, 0
	dualSet := make(map[string]bool, len(dual))
	for _, id := range dual {
		dualSet[id] = true
	}
	for _, r := range ex.Rows {
		if !r.Included {
			continue
		}
		included++
		if dualSet[r.ID] {
			dualIn++
		} else {
			nonDualIn++
		}
	}
	if included != 10 {
		t.Errorf("the window holds %d rows, want the 10 the caller asked for: the reservation moves a row in, "+
			"so a window that is not full means the pool was never wider than the window", included)
	}
	if nonDualIn != 1 {
		t.Errorf("the window holds %d dual-leg rows and %d that are not, want 9 and 1: the one that is not is "+
			"the reserved keyword-only row, so any other number means the window is not composed as the "+
			"reservation left it", dualIn, nonDualIn)
	}
	// The displaced row is one of the dual-leg rows and is genuinely out of the
	// window, which is what "it lost its slot" means. It is the row the eviction
	// chose, so the counts above already exclude it from the nine.
	if !dualSet[res.TookSlotFrom] {
		t.Errorf("took_slot_from names %s, which is not one of the %d dual-leg rows: the eviction target is "+
			"always an admitted row, so an id outside that set points at nothing this payload can explain",
			res.TookSlotFrom, len(dual))
	}
	if rows[res.TookSlotFrom].Included {
		t.Errorf("took_slot_from names %s, which is still in the window, so nothing was displaced", res.TookSlotFrom)
	}

	// The other side. A displaced row is not in the answer and says who took its
	// slot; that is the only place the loser's fate is recorded at all.
	loser, ok := rows[res.TookSlotFrom]
	if !ok {
		t.Fatalf("took_slot_from names row %s, which is not in the payload at all, so the id points at "+
			"nothing the reader can look up: %+v", res.TookSlotFrom, ex.Rows)
	}
	if loser.Included {
		t.Errorf("the row whose slot was taken is still included, so nothing was actually displaced: %+v", loser)
	}
	if loser.DisplacedBy != reserved {
		t.Errorf("the displaced row's displaced_by = %q, want %q: the two sides of one exchange must name each "+
			"other, or one of them is describing a different event", loser.DisplacedBy, reserved)
	}
	if loser.FloorDropped || loser.KeywordReserved {
		t.Errorf("the displaced row reports floor_dropped=%v keyword_reserved=%v; the eviction target is by "+
			"construction neither floor-dropped nor itself reserved, so one of those is wrong: %+v",
			loser.FloorDropped, loser.KeywordReserved, loser)
	}
	// And nothing else claims to be part of the exchange.
	for _, r := range ex.Rows {
		if r.ID == reserved || r.ID == loser.ID {
			continue
		}
		if r.DisplacedBy != "" {
			t.Errorf("row %s reports displaced_by=%q but the reservation names only one displaced row (%s): "+
				"the count of displaced rows is part of the claim", r.ID, r.DisplacedBy, loser.ID)
		}
		if r.KeywordReserved {
			t.Errorf("row %s reports keyword_reserved but only %s was reserved by the top-%d keyword rule",
				r.ID, reserved, 10/5)
		}
	}
}

// TestExplainSaysTheBaseIsUnweightedWhenTheVectorLegAddsNothing covers the note
// that tells a reader rrf_score is the UNWEIGHTED keyword base, from both of the
// ranking path's exits that can trigger it.
//
// The two causes have different remedies — no embedding to consult, or an
// embedding nothing was close enough to — and a reader cannot tell them apart from
// the numbers, so the note has to fire for each. It is a subtest pair rather than
// two functions because they are the same claim from the same flag, and the
// failure mode being guarded is precisely that one exit sets it and the other
// does not.
//
// The no-query-vector case is here because that path returns from searchHybridLegs
// BEFORE any floor is applied, so a verdict recorded only at the floor is a verdict
// it never produces. That was a live regression: the note vanished for exactly the
// deployments most likely to want it, since an absent or failing embedder is the
// common case and a small rrf_score there is a differently-weighted number rather
// than a weak match.
func TestExplainSaysTheBaseIsUnweightedWhenTheVectorLegAddsNothing(t *testing.T) {
	const wantBaseNote = "no vector matches survived"

	// Both cases assert the same three things: the note is present, the cause is
	// named by something (a note on one exit, floor_dropped on the other), and
	// rrf_score really is the plain keyword term the note claims — so the note
	// cannot pass by being decoration. causeNote is the note that SHOULD appear,
	// and empty means the cause is carried per-row and the named note must be
	// absent: a payload that blamed a missing embedder when the embedder worked
	// and the floor was strict would send an operator to the wrong setting.
	assertUnweighted := func(t *testing.T, ex SearchExplain, causeNote string) {
		t.Helper()
		if !hasExplainNote(ex.Notes, wantBaseNote) {
			t.Errorf("notes = %v, want the unweighted-base note: rrf_score here is the UNWEIGHTED keyword base, "+
				"and without the note a small one is indistinguishable from a weak vector match", ex.Notes)
		}
		if causeNote == "" {
			if hasExplainNote(ex.Notes, "no query embedding available") {
				t.Errorf("notes = %v, want NO missing-embedder note: this search had a query embedding and the "+
					"floor refused every match, so blaming the embedder sends the reader to the wrong setting",
					ex.Notes)
			}
		} else if !hasExplainNote(ex.Notes, causeNote) {
			t.Errorf("notes = %v, want the note naming the cause (%q) as well: a base note alone does not say "+
				"whether to configure an embedder or lower the floor", ex.Notes, causeNote)
		}
		var included int
		for _, row := range ex.Rows {
			if !row.Included {
				continue
			}
			included++
			// keywordOnlyParams leaves the FTS weight at 1 and zeroes the vector
			// weight, so the fused score is the plain 1/(K+rank+1) of the keyword
			// leg. The weighted hybrid form is a different number, and reporting it
			// is the failure the note exists to stop.
			want := 1.0 / (60 + float64(row.FTSRank) + 1)
			if math.Abs(row.RRFScore-want) > 1e-12 {
				t.Errorf("row %s reports rrf_score = %v, want 1/(60+%d+1) = %v: with no vector contribution "+
					"the base is the unweighted keyword term, and the note above claims exactly that",
					row.ID, row.RRFScore, row.FTSRank, want)
			}
			if row.VectorRank != -1 || row.VectorScore != -1 {
				t.Errorf("row %s reports vector_rank = %d and vector_score = %v, want the -1 sentinels: the "+
					"vector leg contributed nothing, which is a fact about the leg and not a zero score: %+v",
					row.ID, row.VectorRank, row.VectorScore, row)
			}
		}
		if included == 0 {
			t.Error("no row is included, so the base this note describes was never reported")
		}
	}

	t.Run("no query vector", func(t *testing.T) {
		store, ctx := setupTestStore(t)
		createTestMemory(t, store, ctx, "the compaction schedule runs on the first sunday of each month")

		// A nil query vector, not an empty slice: no embedder configured, or an
		// EmbedQuery that failed. Both reach searchHybridLegs this way.
		ex, err := store.ExplainSearch(ctx, "test-proj", "compaction schedule sunday", nil, 5)
		if err != nil {
			t.Fatalf("ExplainSearch(nil query vector): %v", err)
		}
		assertUnweighted(t, ex, "no query embedding available")
	})

	t.Run("the floor refused every match", func(t *testing.T) {
		store, ctx := setupTestStore(t)
		// An embedding exists and the vector leg returns the row, and the FLOOR is
		// what removes it — the other exit. The embedding is deliberately not the
		// query vector, so its cosine is 0.8 and the threshold can sit between the
		// two: an identical embedding would score 1.0 and no floor above 1 would be
		// a threshold anyone would configure.
		id := createTestMemory(t, store, ctx, "the compaction schedule runs on the first sunday of each month")
		if err := store.StoreEmbedding(ctx, id, []float32{0.8, 0.6}, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
		store.SetVectorMinSimilarity(0.9) // above the 0.8 cosine

		ex, err := store.ExplainSearch(ctx, "test-proj", "compaction schedule sunday", []float32{1, 0}, 5)
		if err != nil {
			t.Fatalf("ExplainSearch: %v", err)
		}
		// Guard the fixture: the floor must be what emptied the leg, or this
		// subtest is the nil case wearing a different name.
		row, ok := explainRowByID(ex)[id]
		if !ok {
			t.Fatalf("the matched row is missing from the explanation: %+v", ex.Rows)
		}
		if !row.FloorDropped || row.FloorScore <= 0 || row.FloorScore >= 0.9 {
			t.Fatalf("the row reports floor_dropped=%v floor_score=%v, want true and a cosine under the 0.9 "+
				"threshold: the FLOOR is what must have emptied the leg here, or this is not the second exit: %+v",
				row.FloorDropped, row.FloorScore, row)
		}
		// vector_rank is -1 here, and deliberately: the floor strips the term
		// before fusion, so the row reaches the window on its keyword score alone
		// and carries no vector rank. That is the same -1 the no-embedder case
		// reports, which is precisely why floor_dropped and floor_score are what
		// distinguish the two, and why this subtest checks the cause per-row.
		if row.VectorRank != -1 {
			t.Errorf("vector_rank = %d, want -1: the floor removes the term before fusion, so the row is scored "+
				"on its keyword leg alone", row.VectorRank)
		}
		assertUnweighted(t, ex, "")
	})
}

// TestExplainReportsHowLongTheScopeKeyListReallyWas covers the cap on
// scope_keys_compared.
//
// The scope object is caller-supplied and as unbounded as a JSON object, so the
// list of keys the narrowing compared is capped at 16. A cap on a diagnostic list
// is fine; a SILENT one is not, because the payload would report scope_matched as
// the verdict the narrowing reached over the caller's whole key set while naming
// sixteen of them, and the reader could not tell. The count beside the list is
// what makes the pair honest — which is the convention the attribution lists
// beside superseded_by and near_duplicate_of already follow, and which this cap's
// own comment claimed while not doing.
func TestExplainReportsHowLongTheScopeKeyListReallyWas(t *testing.T) {
	store, ctx := setupTestStore(t)
	createTestMemory(t, store, ctx, "the compaction schedule runs on the first sunday of each month")

	// More keys than the cap allows, so the cut actually engages. Twenty is
	// comfortably past 16 and still a small enough scope object to read.
	scope := make(map[string]string, 20)
	for i := range 20 {
		scope[fmt.Sprintf("zone-%02d", i)] = fmt.Sprintf("rack-%02d", i)
	}

	ex, err := store.ExplainSearchScoped(ctx, "test-proj", "compaction schedule sunday", []float32{0.8, 0.6}, 5, scope)
	if err != nil {
		t.Fatalf("ExplainSearchScoped: %v", err)
	}
	if len(ex.Rows) == 0 {
		t.Fatal("a scoped search returned no rows, so there is no row to read the fields off")
	}
	row := ex.Rows[0]

	if len(row.ScopeKeysCompared) != maxScopeKeys {
		t.Errorf("scope_keys_compared names %d keys, want the %d the cap allows: the cap is a rendering "+
			"budget and the fixture is past it, so a different number means the cap is not where it is documented",
			len(row.ScopeKeysCompared), maxScopeKeys)
	}
	if row.ScopeKeysComparedTotal != 20 {
		t.Errorf("scope_keys_compared_total = %d, want 20: the count beside a capped list is the only thing "+
			"that makes the cut visible, and it must be the real length rather than the rendered one",
			row.ScopeKeysComparedTotal)
	}
	// A short scope must NOT report a cap it did not apply, and the total has to
	// equal the list — otherwise the count is decoration.
	small := map[string]string{"zone-01": "rack-01"}
	smallEx, err := store.ExplainSearchScoped(ctx, "test-proj", "compaction schedule sunday", []float32{0.8, 0.6}, 5, small)
	if err != nil {
		t.Fatalf("ExplainSearchScoped(one key): %v", err)
	}
	if len(smallEx.Rows) == 0 {
		t.Fatal("the one-key scoped search returned no rows")
	}
	smallRow := smallEx.Rows[0]
	if smallRow.ScopeKeysComparedTotal != 1 || len(smallRow.ScopeKeysCompared) != 1 {
		t.Errorf("a one-key scope reports total %d and %d named key(s), want 1 and 1: with nothing cut the count "+
			"must be the list's own length, or a reader cannot tell a capped list from a complete one",
			smallRow.ScopeKeysComparedTotal, len(smallRow.ScopeKeysCompared))
	}
}

func hasExplainNote(notes []string, substr string) bool {
	for _, n := range notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

// explainsAny reports whether ids names want.
func explainsAny(ids []string, want string) bool {
	s := append([]string(nil), ids...)
	sort.Strings(s)
	i := sort.SearchStrings(s, want)
	return i < len(s) && s[i] == want
}
