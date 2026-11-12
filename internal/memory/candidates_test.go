package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// candidateRequest is the smallest query-mode request the tests need: one
// project, one condition, no scope, and an explicit clock.
func candidateRequest(query string, limit int, now time.Time) CandidateRequest {
	return CandidateRequest{
		ProjectID: testProject,
		Mode:      ProjectScoped,
		Query:     query,
		Condition: CondFTSOnly,
		Params:    DefaultSearchParams(),
		Now:       now,
		Fetch:     Fetch{FTSTopK: limit * 2, VectorTopK: limit * 2, Limit: limit},
	}
}

func candidateIDs(t *testing.T, set *CandidateSet) []string {
	t.Helper()
	ids := make([]string, len(set.Rows))
	for i, c := range set.Rows {
		ids[i] = c.ID
	}
	return ids
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestProjectSearchIsNotCrowdedOutByGlobalRows: a project search whose default
// limit is met by short _global rows that share the query term must still return
// the project's own matching rows. Before the fix, the leg cut the union of the
// two buckets at limit*2 by bm25, so ten short global rows filled the depth and
// the project's rows never became candidates (#1771).
func TestProjectSearchIsNotCrowdedOutByGlobalRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, err := s.Create(ctx, "_global", Memory{
			Category:   "preference",
			Content:    fmt.Sprintf("Prefer helm charts over kustomize for service %d.", i),
			Source:     "manual",
			Importance: 0.7,
		}); err != nil {
			t.Fatalf("Create global %d: %v", i, err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := s.Create(ctx, testProject, Memory{
			Category:   "gotcha",
			Content:    fmt.Sprintf("The helm release for service %d must be rolled back with the documented procedure after a failed deploy.", i),
			Source:     "manual",
			Importance: 0.7,
		}); err != nil {
			t.Fatalf("Create project %d: %v", i, err)
		}
	}

	now := time.Now().UTC()
	set, err := s.Candidates(ctx, candidateRequest("helm", 5, now))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	projectCount := 0
	for _, c := range set.Rows {
		if c.ProjectID == testProject {
			projectCount++
		}
	}
	if projectCount == 0 {
		t.Fatalf("no project row among the %d candidates: %v", len(set.Rows), candidateIDs(t, set))
	}
}

func TestCandidatesCarriesTheEvidenceCounts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	corroborated, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact",
		"the readiness probe gate misconfiguration is in the api deployment", "mcp", 0.5, nil,
		Provenance{Agent: "claude-code", SessionID: "ses_a"})
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	restated, foldedInto, _, err := s.UpsertWithProvenance(ctx, testProject, "fact",
		"the readiness probe gate misconfiguration is in the api deployment, per the runbook", "mcp", 0.5, nil,
		Provenance{Agent: "codex", SessionID: "ses_b"})
	if err != nil {
		t.Fatalf("second Upsert: %v", err)
	}
	if foldedInto != corroborated {
		t.Fatalf("the second save folded into %q, want the corroborated row %s", foldedInto, corroborated)
	}
	// The restatement is kept as its own row behind a duplicate edge, and the
	// candidate read keeps one row of such a pair: by rank when both were saved
	// in the same second, the NEWER one when created_at differs
	// (earlierRowLoses). created_at has one-second resolution, so two saves that
	// straddle a second boundary would drop the corroborated row. Pinning the
	// copy to the same second keeps the rank verdict this test reads.
	if _, err := s.db.Exec(
		`UPDATE memories SET created_at = (SELECT created_at FROM memories WHERE id = ?) WHERE id = ?`,
		corroborated, restated,
	); err != nil {
		t.Fatalf("pin the copy's created_at: %v", err)
	}
	if _, err := s.db.Exec(
		`UPDATE memory_provenance SET verified_at = datetime('now') WHERE memory_id = ? AND agent = 'codex'`,
		corroborated,
	); err != nil {
		t.Fatalf("stamp a verification: %v", err)
	}
	unattributed, duplicateOf, _, err := s.Upsert(ctx, testProject, "fact",
		"readiness probe timeout budget for the control plane", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert(unattributed): %v", err)
	}
	if duplicateOf != "" {
		t.Fatalf("the third save folded into %s, so it is no longer the unattributed row the fixture needs", duplicateOf)
	}

	set, err := s.Candidates(ctx, candidateRequest("readiness probe", 5, now))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	counts := map[string]EvidenceCounts{}
	for _, c := range set.Rows {
		counts[c.ID] = c.Evidence
	}
	// A row missing from the set would read as zero counts below, so its absence
	// is reported as what it is.
	for _, id := range []string{corroborated, unattributed} {
		if _, ok := counts[id]; !ok {
			dropped := map[string][]string{}
			for _, d := range set.DroppedLosers {
				dropped[d.ID] = d.LostTo
			}
			t.Fatalf("row %s is not in the candidate set %v (dropped, with what each lost to: %v)", id, candidateIDs(t, set), dropped)
		}
	}
	if got := counts[corroborated]; got.Observations != 2 || got.Verified != 1 {
		t.Errorf("the corroborated row's counts = %+v, want 2 observations and 1 verified", got)
	}
	if got := counts[unattributed]; got.Observations != 1 || got.Verified != 0 {
		t.Errorf("the unattributed row's counts = %+v, want its own single unverified observation", got)
	}
	// Every other row the set returned is cross-checked against the store, so a
	// read that reported a plausible-looking zero for a row it never looked up
	// cannot pass.
	for _, c := range set.Rows {
		want, err := s.MemoryEvidenceCounts(ctx, c.ID)
		if err != nil {
			t.Fatalf("MemoryEvidenceCounts(%s): %v", c.ID, err)
		}
		if c.Evidence != want {
			t.Errorf("row %s reported %+v, want the store's %+v", c.ID, c.Evidence, want)
		}
	}
}

func TestCandidatesWindowMatchesSearchHybrid(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	if _, err := s.Create(ctx, "_global", Memory{
		Category: "convention", Content: "kubernetes readiness probe is checked before rollout",
		Source: "manual", Importance: 0.7,
	}); err != nil {
		t.Fatalf("Create global: %v", err)
	}
	for _, content := range []string{
		"kubernetes readiness probe misconfiguration on the api deployment",
		"kubernetes readiness probe tuning for the worker pool",
		"kubernetes readiness probe failure page timeout seconds",
		"kubernetes readiness probe failure page timeout after seconds",
		"kubernetes readiness probe timeout budget for the control plane",
		"kubernetes readiness probe rollout gate documentation",
	} {
		makeMemory(t, s, content)
	}

	q := "kubernetes readiness probe"
	const window = 3
	direct, err := s.SearchHybridScoped(ctx, testProject, q, nil, window, nil)
	if err != nil {
		t.Fatalf("SearchHybridScoped: %v", err)
	}
	if len(direct) != window {
		t.Fatalf("precondition: production search returned %d rows, want %d", len(direct), window)
	}
	want := make([]string, len(direct))
	for i, m := range direct {
		want[i] = m.ID
	}

	set, err := s.Candidates(ctx, candidateRequest(q, window, now))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if !set.Widened {
		t.Errorf("a candidate set of %d rows for a window of %d must report Widened", len(set.Rows), window)
	}
	if got := candidateIDs(t, set)[:window]; !sameIDs(got, want) {
		t.Errorf("candidate window = %v, want the production search's %v", got, want)
	}
}

func TestCandidatesKeepsTheNegativeRetrievalContract(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	global, err := s.Create(ctx, "_global", Memory{
		Category: "convention", Content: "gizmo rotation is announced in the ops channel",
		Source: "manual", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create global: %v", err)
	}
	stale := makeMemory(t, s, "gizmo rotation uses the legacy calendar")
	fresh := makeMemory(t, s, "gizmo rotation uses the ops calendar")
	if err := s.CreateLink(ctx, fresh, stale, "supersedes", 1, "manual"); err != nil {
		t.Fatalf("CreateLink supersedes: %v", err)
	}
	resolved := makeMemory(t, s, "gizmo rotation reminder for the on-call engineer")
	if _, err := s.SetResolved(ctx, []string{resolved}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}

	set, err := s.Candidates(ctx, candidateRequest("gizmo rotation", 10, time.Now().UTC()))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	ids := candidateIDs(t, set)
	if len(ids) != 4 {
		t.Fatalf("candidates = %v, want all four gizmo rows", ids)
	}
	rank := map[string]int{}
	for i, id := range ids {
		rank[id] = i
	}
	if rank[stale] < rank[fresh] {
		t.Errorf("superseded row %s ranked above its replacement %s", stale, fresh)
	}
	if _, ok := rank[global]; !ok {
		t.Errorf("global row %s missing: a project search still reaches _global", global)
	}
	if _, ok := rank[resolved]; !ok {
		t.Errorf("resolved row %s missing: query mode admits resolved rows, demoted rather than excluded", resolved)
	}
	// A resolved row goes below the project's own live memory, which is what
	// the status factor is for. (The superseded row is demoted by its own
	// rule and may sit lower still; that is a different contract.)
	if rank[resolved] < rank[fresh] {
		t.Errorf("resolved row %s ranked %d, above the live row %s at %d: status demotion owns membership, not only order",
			resolved, rank[resolved], fresh, rank[fresh])
	}
	if rank[global] < rank[fresh] {
		t.Errorf("_global row %s ranked %d, above the live project row %s at %d",
			global, rank[global], fresh, rank[fresh])
	}
}

func TestCandidatesReportsAHydrationFailure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := range 4 {
		makeMemory(t, s, "hydratable keyword row "+string(rune('a'+i)))
	}
	// The legs have already run when the seam fires, so failing the context
	// there fails exactly the hydration read — which is the read whose silent
	// failure this test is about.
	failCtx, cancel := context.WithCancel(ctx)
	beforeHybridHydrateFn.Store(func([]string) { cancel() })
	defer beforeHybridHydrateFn.Store(func([]string) {})

	_, err := s.Candidates(failCtx, candidateRequest("hydratable keyword row", 3, time.Now().UTC()))
	if err == nil {
		t.Fatal("Candidates hid a failed read behind an empty result")
	}
	if !strings.Contains(err.Error(), "hydrate window") {
		t.Errorf("error = %q, want it to name the hydration step", err)
	}
}

func TestCandidatesNarrowsScopeInsideTheWindow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i, content := range []string{
		"sprocket calibration development alpha",
		"sprocket calibration development bravo",
		"sprocket calibration development charlie",
		"sprocket calibration development delta",
	} {
		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: content, Source: "manual", Importance: 0.7,
			Scope: map[string]string{"environment": "development"},
		})
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		_ = id
	}
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "sprocket calibration production echo with a longer sentence to rank it lower",
		Source: "manual", Importance: 0.7,
		Scope: map[string]string{"environment": "production"},
	}); err != nil {
		t.Fatalf("Create production: %v", err)
	}

	req := candidateRequest("sprocket calibration", 2, time.Now().UTC())
	// A deeper keyword leg than the window needs: the eligible row has to be
	// inside the fetched set and outside the window plus its bounded backfill,
	// which is the position #573 was about. The assembler asks for twice the
	// window, which is not deep enough to reach a row this far down, so the
	// narrowing inside the window is what makes it reachable at all.
	req.Fetch.FTSTopK = 10
	req.Scope = map[string]string{"environment": "production"}
	set, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	ids := candidateIDs(t, set)
	if len(ids) == 0 {
		t.Fatal("no candidates: an eligible row ranked below the window was never reached")
	}
	first, err := s.GetByIDs(ctx, []string{ids[0]})
	if err != nil || len(first) != 1 {
		t.Fatalf("GetByIDs: %v %d", err, len(first))
	}
	if first[0].Scope["environment"] != "production" {
		t.Errorf("first candidate is scoped %v: the window filled with contradicting rows, "+
			"so the eligible one had to come from a bounded backfill", first[0].Scope)
	}
}

func TestCandidatesUsesTheBoundNow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id := makeMemory(t, s, "sqlite wal mode checkpoint starvation")
	created := time.Now().UTC().Add(-100 * 24 * time.Hour)
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET created_at = ? WHERE id = ?`,
		created.Format("2006-01-02 15:04:05"), id); err != nil {
		t.Fatalf("set created_at: %v", err)
	}

	bound := created.Add(24 * time.Hour)
	set, err := s.Candidates(ctx, candidateRequest("sqlite wal mode checkpoint", 10, bound))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	var row *Candidate
	for i := range set.Rows {
		if set.Rows[i].ID == id {
			row = &set.Rows[i]
		}
	}
	if row == nil {
		t.Fatalf("row %s missing from the candidate set", id)
	}
	if row.AgeDays < 0.9 || row.AgeDays > 1.1 {
		t.Errorf("age = %.2f days against the bound clock, want ~1: the candidate path read the wall clock", row.AgeDays)
	}
	if want := DecayFactor(row.Category, row.Content, row.Retention, row.Pinned, row.AgeDays); row.Decay != want {
		t.Errorf("decay = %v, want %v (the factor the bound clock implies)", row.Decay, want)
	}
	if row.Score != row.Base*row.Decay {
		t.Errorf("score = %v, want base*decay = %v", row.Score, row.Base*row.Decay)
	}
}

func TestCandidatesOrdersDecayAgainstTheBoundClock(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A request clock far in the past, so the wall clock would make every row
	// ancient and the decay factor would floor for both.
	bound := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	// Category decision, because that is one of the categories that decay; a
	// fact never decays and the test would prove nothing.
	mk := func(content string) string {
		id, err := s.Create(ctx, testProject, Memory{
			Category: "decision", Content: content, Source: "manual", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return id
	}
	old := mk("gizmo alpha")
	recent := mk("gizmo alpha plus a much longer tail of words so bm25 ranks it below the short one")
	for id, age := range map[string]time.Duration{old: 200 * 24 * time.Hour, recent: 24 * time.Hour} {
		if _, err := s.db.ExecContext(ctx, `UPDATE memories SET created_at = ? WHERE id = ?`,
			bound.Add(-age).Format("2006-01-02 15:04:05"), id); err != nil {
			t.Fatalf("set created_at for %s: %v", id, err)
		}
	}

	set, err := s.Candidates(ctx, candidateRequest("gizmo alpha", 10, bound))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	ids := candidateIDs(t, set)
	if len(ids) != 2 {
		t.Fatalf("candidates = %v, want both rows", ids)
	}
	if ids[0] != recent {
		t.Errorf("order = %v, want the row that is fresh against the request clock first: "+
			"decay ranked against a different time than the request named", ids)
	}
}

func TestCandidatesMarksARowTheVectorLegCouldNotSee(t *testing.T) {
	store, ctx := setupTestStore(t)
	store.SetEmbeddingIdentity(identityCurrent)

	embedded := createTestMemory(t, store, ctx, "the pool timeout knob is set in the config file")
	absent := createTestMemory(t, store, ctx, "the pool timeout is read from the environment")
	foreign := createTestMemory(t, store, ctx, "the pool timeout has a default of five seconds")
	// The correct identity, the wrong width. VectorIdentity only carries the
	// dimension when the embedding reports one, so an identity match is not
	// evidence of a usable vector: the leg compares the blob's own length
	// against the query and skips this row exactly as it skips the foreign one.
	wide := createTestMemory(t, store, ctx, "the pool timeout is doubled on retry")
	if err := store.StoreEmbedding(ctx, embedded, []float32{1, 0, 0}, identityCurrent); err != nil {
		t.Fatalf("StoreEmbedding(embedded): %v", err)
	}
	if err := store.StoreEmbedding(ctx, foreign, []float32{1, 0, 0}, identityStale); err != nil {
		t.Fatalf("StoreEmbedding(foreign): %v", err)
	}
	if err := store.StoreEmbedding(ctx, wide, []float32{1, 0, 0, 0}, identityCurrent); err != nil {
		t.Fatalf("StoreEmbedding(wide): %v", err)
	}

	req := candidateRequest("pool timeout", 10, time.Now().UTC())
	req.ProjectID = "test-proj"
	req.QueryVec = []float32{1, 0, 0}
	req.Condition = CondHybrid
	set, err := store.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	seen := make(map[string]bool, len(set.Rows))
	for _, c := range set.Rows {
		seen[c.ID] = true
	}
	for _, id := range []string{embedded, absent, foreign, wide} {
		if !seen[id] {
			t.Fatalf("candidate %s missing from the set: %v", id, candidateIDs(t, set))
		}
	}
	for _, c := range set.Rows {
		want := c.ID != embedded
		if c.Unembedded != want {
			t.Errorf("candidate %s (embedded=%v) Unembedded = %v, want %v",
				c.ID, c.ID == embedded, c.Unembedded, want)
		}
	}
}

func containsID(set *CandidateSet, id string) bool {
	for _, c := range set.Rows {
		if c.ID == id {
			return true
		}
	}
	return false
}

func TestCandidatesReportsLegStatuses(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A corpus larger than the leg depth truncates the keyword leg.
	for i := range 6 {
		makeMemory(t, s, "sqlite wal mode checkpoint note "+string(rune('a'+i)))
	}
	set, err := s.Candidates(ctx, candidateRequest("sqlite wal mode checkpoint note", 2, time.Now().UTC()))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	fts, ok := set.Legs["fts"]
	if !ok {
		t.Fatalf("no fts leg status: %+v", set.Legs)
	}
	if !fts.Applicable || !fts.Attempted || !fts.Available {
		t.Errorf("fts leg = %+v, want applicable, attempted and available", fts)
	}
	if !fts.Truncated {
		t.Error("fts leg served more rows than it was asked for, or fewer — either way Truncated is wrong here")
	}
	if fts.CoverageComplete {
		t.Error("a truncated leg cannot be complete coverage")
	}
	vec, ok := set.Legs["vector"]
	if !ok {
		t.Fatalf("no vector leg status: %+v", set.Legs)
	}
	if vec.Applicable {
		t.Error("vector leg marked applicable for an FTS-only request")
	}
	if vec.Attempted || vec.Available {
		t.Errorf("vector leg = %+v, want not attempted for an FTS-only request", vec)
	}

	// Hybrid with a query vector: both legs applicable, and the leg reports
	// what it did and did not look at. The coverage reconciliation stays
	// unclaimed in v1 — see the LegStatus doc — so what is asserted here is
	// that the leg ran, and that it makes no coverage claim it cannot support.
	withVec := candidateRequest("sqlite wal mode checkpoint note", 10, time.Now().UTC())
	withVec.Condition = CondHybrid
	withVec.QueryVec = []float32{1, 0, 0}
	set, err = s.Candidates(ctx, withVec)
	if err != nil {
		t.Fatalf("Candidates hybrid: %v", err)
	}
	vec = set.Legs["vector"]
	if !vec.Applicable || !vec.Attempted || !vec.Available {
		t.Errorf("vector leg = %+v, want applicable, attempted and available", vec)
	}
	if vec.DimMismatch != 0 {
		t.Errorf("dim mismatch = %d, want 0: every stored embedding has the query's dimensions", vec.DimMismatch)
	}
	if vec.CoverageComplete {
		t.Error("a leg that cannot see unembedded rows must not claim complete coverage, and v1 does not pay for the counts that would let it")
	}
	if vec.Expected != 0 || vec.Indexed != 0 || vec.Unembedded != 0 {
		t.Errorf("coverage counts = %d/%d/%d, want zero: nothing reconciles them yet, and two COUNT(*) scans per search is not worth a field with no reader",
			vec.Expected, vec.Indexed, vec.Unembedded)
	}
}

func TestCandidatesRejectsInvalidRequests(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("zero clock", func(t *testing.T) {
		req := candidateRequest("anything", 10, time.Time{})
		if _, err := s.Candidates(ctx, req); err == nil {
			t.Error("Candidates accepted a zero clock")
		}
	})
	t.Run("unknown mode", func(t *testing.T) {
		req := candidateRequest("anything", 10, now)
		req.Mode = "sideways"
		if _, err := s.Candidates(ctx, req); err == nil {
			t.Error("Candidates accepted an unknown project mode")
		}
	})
	t.Run("vector only without a vector", func(t *testing.T) {
		req := candidateRequest("anything", 10, now)
		req.Condition = CondVectorOnly
		if _, err := s.Candidates(ctx, req); err == nil {
			t.Error("Candidates accepted vector-only with no query vector")
		}
	})
	t.Run("passive retrieval", func(t *testing.T) {
		req := candidateRequest("", 10, now)
		req.Passive = []SlicePolicy{{Bucket: "project"}}
		_, err := s.Candidates(ctx, req)
		if err == nil {
			t.Fatal("Candidates accepted a passive request it does not serve yet")
		}
		if !strings.Contains(err.Error(), "passive") {
			t.Errorf("error %q does not say passive retrieval is unimplemented", err)
		}
	})
}

func TestCandidatesErrorsWhenEveryApplicableLegFails(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	makeMemory(t, s, "kubernetes readiness probe misconfiguration")

	// Both retrieval tables gone: neither leg can run.
	for _, table := range []string{"memories_fts", "memory_embeddings"} {
		if _, err := s.db.ExecContext(ctx, "DROP TABLE "+table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}

	set, err := s.Candidates(ctx, candidateRequest("kubernetes readiness probe", 10, time.Now().UTC()))
	if err == nil {
		t.Fatalf("Candidates returned a set with %d rows instead of an error: set=%+v", len(set.Rows), set.Legs)
	}
	if !strings.Contains(err.Error(), "retrieval") {
		t.Errorf("error = %q, want it to name the retrieval failure", err)
	}
}

func TestCandidatesReturnsStatusesForAPartialLegFailure(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	makeMemory(t, s, "kubernetes readiness probe misconfiguration")

	// Only the keyword index is gone: the vector leg can still run, and finds
	// nothing because no embedding exists.
	if _, err := s.db.ExecContext(ctx, "DROP TABLE memories_fts"); err != nil {
		t.Fatalf("drop memories_fts: %v", err)
	}
	req := candidateRequest("kubernetes readiness probe", 10, time.Now().UTC())
	req.Condition = CondHybrid
	req.QueryVec = []float32{1, 0, 0}

	set, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: a partial failure must return statuses, not an error: %v", err)
	}
	if len(set.Rows) != 0 {
		t.Errorf("rows = %d, want none", len(set.Rows))
	}
	if set.Legs["fts"].Err == "" {
		t.Error("the failed leg reports no error, so the caller cannot tell an incomplete search from an empty one")
	}
	if !set.Legs["vector"].Attempted || !set.Legs["vector"].Available {
		t.Errorf("vector leg = %+v, want it attempted and available: it ran, it simply found nothing", set.Legs["vector"])
	}
}

func TestCandidatesCountsTheRowsTheKeywordLegWithheld(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

	expired := "2020-01-01 00:00:00"
	future := "2099-01-01 00:00:00"
	for _, m := range []struct {
		content    string
		validUntil *string
		validFrom  *string
	}{
		{"the kubernetes upgrade runbook is retired", &expired, nil},
		{"the kubernetes upgrade rehearsal is not yet in force", nil, &future},
		{"the kubernetes upgrade lunch menu", nil, nil},
	} {
		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: m.content, Source: "manual",
			Importance: 0.7, ValidUntil: m.validUntil, ValidFrom: m.validFrom,
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", m.content, err)
		}
		// The third row is resolved, so it stays in the keyword leg's population
		// (which filters resolved rows later, not in its own statement) and must
		// still be counted when it is out of its window.
		if m.content == "the kubernetes upgrade lunch menu" {
			if _, err := s.db.Exec(`UPDATE memories SET valid_until = '2019-01-01 00:00:00', resolved_at = datetime('now') WHERE id = ?`, id); err != nil {
				t.Fatalf("retire and resolve the third row: %v", err)
			}
		}
	}
	// A row of another project that matches: the project clause is the leg's own,
	// so this one is not the leg's to withhold.
	other := "other-project"
	if err := s.EnsureProject(ctx, other, "/src/other", other); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := s.Create(ctx, other, Memory{
		Category: "fact", Content: "the kubernetes upgrade runbook is retired", Source: "manual",
		Importance: 0.7, ValidUntil: &expired,
	}); err != nil {
		t.Fatalf("Create in another project: %v", err)
	}

	// Every match in this project is out of its window, so the leg returns
	// nothing and the count is the three of them.
	set, err := s.Candidates(ctx, candidateRequest("kubernetes upgrade runbook", 5, now))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(set.Rows) != 0 {
		t.Fatalf("rows = %v, want none: every match is out of its window", candidateIDs(t, set))
	}
	if set.ValidityExcluded != 3 {
		t.Errorf("ValidityExcluded = %d, want 3: two rows whose window closed (one of them resolved), one whose "+
			"window has not opened — all three over this project's own rows", set.ValidityExcluded)
	}

	// A query that matches nothing at all is left at zero: nothing was withheld,
	// which is what keeps `no_candidates` and `all_invalid` distinguishable.
	none, err := s.Candidates(ctx, candidateRequest("postgres connection pooling", 5, now))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if none.ValidityExcluded != 0 {
		t.Errorf("ValidityExcluded = %d for a query that matched nothing, want 0: a count of withheld rows "+
			"would report rows that never matched", none.ValidityExcluded)
	}
}

// TestCandidatesCountsTheRowsTheVectorLegWithheld is the semantic counterpart of
// the keyword count above, and the half of #1053 the semantic-only and hybrid
func TestCandidatesCountsTheRowsTheVectorLegWithheld(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	expired := "2020-01-01 00:00:00"

	add := func(content string, validUntil *string, vec []float32) {
		t.Helper()
		id, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: content, Source: "manual",
			Importance: 0.7, ValidUntil: validUntil,
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", content, err)
		}
		if err := s.StoreEmbedding(ctx, id, vec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding(%q): %v", content, err)
		}
	}
	// Two expired rows whose embeddings ARE the query vector: the cosine pass
	// scores both above the floor and withholds both.
	add("the quarterly banana harvest is in the ledger", &expired, []float32{1, 0, 0})
	add("the annual banana harvest is in the ledger", &expired, []float32{1, 0, 0})

	req := CandidateRequest{
		ProjectID: testProject,
		Mode:      ProjectScoped,
		Query:     "kubernetes upgrade runbook",
		QueryVec:  []float32{1, 0, 0},
		Condition: CondVectorOnly,
		Params:    DefaultSearchParams(),
		Now:       now,
		Fetch:     Fetch{FTSTopK: 10, VectorTopK: 10, Limit: 5},
	}
	set, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(set.Rows) != 0 {
		t.Fatalf("rows = %v, want none: every vector match is out of its window", candidateIDs(t, set))
	}
	if set.ValidityExcluded != 2 {
		t.Errorf("ValidityExcluded = %d, want 2: both above-floor matches were withheld, so the empty "+
			"reason must not read as `no_candidates`", set.ValidityExcluded)
	}

	// A live row on the same vector makes the leg non-empty, so the count must
	// stay zero: the emptiness rule is what stops it claiming a leg that found a
	// match withheld rows.
	add("the monthly banana harvest is in the ledger", nil, []float32{1, 0, 0})
	set, err = s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(set.Rows) == 0 {
		t.Fatal("rows empty after a valid match was added; the fixture is wrong")
	}
	if set.ValidityExcluded != 0 {
		t.Errorf("ValidityExcluded = %d on a non-empty leg, want 0: the count is taken only when the "+
			"leg comes back empty", set.ValidityExcluded)
	}
}

// TestVectorLegIsNotRunWhenItSkippedEveryRow: a vector leg that skipped every
// row it read computed no cosine, so it did not run. Reporting it attempted and
// available printed `vector:ok` for a search the vector side never answered,
// and the advice to retry a surprising miss on `vector:not_run` never applied to
// the state a model or width change leaves the store in until the worker has
// re-embedded it. A leg that scored at least one row, and a leg with nothing to
// read at all, stay as they were.
