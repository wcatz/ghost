package memory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
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

// TestCandidatesCarriesTheEvidenceCounts: the assembler's only route to the store
// is this one read, so the support counts have to arrive with the rows. A row
// whose counts read zero when the memory has evidence would make the trace report
// "no recorded evidence" about a memory two agents had already reported — a false
// claim about the corpus, produced by a read that silently did nothing.
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
	if _, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact",
		"the readiness probe gate misconfiguration is in the api deployment, per the runbook", "mcp", 0.5, nil,
		Provenance{Agent: "codex", SessionID: "ses_b"}); err != nil {
		t.Fatalf("second Upsert: %v", err)
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

// TestCandidatesWindowMatchesSearchHybrid is the equivalence the seam depends
// on: with no predicate, the rows a candidate request returns, closed to the
// requested window, must be exactly what the production search returns. The
// corpus is larger than the window so the set is genuinely widened, which is
// what makes this a test of the tail rather than of a pool that happened to
// fit.
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

// TestCandidatesKeepsTheNegativeRetrievalContract: query mode admits resolved
// rows and demotes them, supersede demotion sinks a replaced row below its
// replacement, and a _global row stays reachable from a project search. A
// candidate path that changed any of those would change what search returns.
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

// TestCandidatesReportsAHydrationFailure: a read that fails is an error, not an
// empty window. The tail is hydrated by a second, independent read, so a
// transient failure on the window read that the tail read survives would
// otherwise yield a candidate set missing the whole selected window — and
// "no matching memories" for a store that plainly has matches.
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

// TestCandidatesReturnsUntrimmedWidenedSet: stages 2-8 need rows the window
// would have cut, or a predicate can only ever remove from the answer.
func TestCandidatesReturnsUntrimmedWidenedSet(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i := range 12 {
		makeMemory(t, s, "vector search window candidate number "+string(rune('a'+i)))
	}
	req := candidateRequest("vector search window candidate", 3, time.Now().UTC())

	set, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(set.Rows) <= 3 {
		t.Fatalf("candidate set has %d rows: the window of 3 was closed before returning, so no filter can backfill", len(set.Rows))
	}
	if !set.Widened {
		t.Error("Widened = false for a set larger than the requested window")
	}
	// The tail is capped at the window width: no closure the assembler can
	// apply admits more rows than the window it was given.
	if got := len(set.Rows) - 3; got > 3 {
		t.Errorf("tail has %d rows, want at most the window width (3)", got)
	}
}

// TestCandidatesAppliesConfiguredVectorFloor: the Store owns the floor, so a
// candidate request cannot bypass search.min_similarity the way an unset
// SearchParams.MinSimilarity could.
func TestCandidatesAppliesConfiguredVectorFloor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	s.SetVectorMinSimilarity(0.5)

	strong := makeMemory(t, s, "kubernetes readiness probe misconfiguration")
	weak := makeMemory(t, s, "zzz unrelated quantum banana syntax")
	if err := s.StoreEmbedding(ctx, strong, []float32{1, 0, 0}, "test"); err != nil {
		t.Fatalf("StoreEmbedding strong: %v", err)
	}
	if err := s.StoreEmbedding(ctx, weak, []float32{0.1, 0.99, 0}, "test"); err != nil {
		t.Fatalf("StoreEmbedding weak: %v", err)
	}

	req := candidateRequest("kubernetes readiness probe", 10, time.Now().UTC())
	req.QueryVec = []float32{1, 0, 0}
	req.Condition = CondHybrid
	// The caller leaves the floor unset in its params; the Store's configured
	// value is the one that has to apply.
	req.Params.MinSimilarity = 0

	set, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	for _, c := range set.Rows {
		if c.ID == weak {
			t.Errorf("weak candidate %s survived the configured floor of 0.5", weak)
		}
	}
	if !set.Legs["vector"].Applicable {
		t.Error("vector leg not marked applicable for a hybrid request")
	}
}

// TestCandidatesNarrowsScopeInsideTheWindow: scope is narrowed from the fused
// pool before the window is cut, not applied to the finished window. A request
// whose eligible row ranks below the window's fill must still get it — and it
// would not if the window were allowed to fill with contradicting rows, because
// the backfill that replaces them is bounded.
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

// TestCandidatesUsesTheBoundNow: the candidate path must not read the wall
// clock. A request clock one day after a row's created_at has to produce an
// age of one day, whatever time the test happens to run.
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
	if want := DecayFactor(row.Category, row.Pinned, row.AgeDays); row.Decay != want {
		t.Errorf("decay = %v, want %v (the factor the bound clock implies)", row.Decay, want)
	}
	if row.Score != row.Base*row.Decay {
		t.Errorf("score = %v, want base*decay = %v", row.Score, row.Base*row.Decay)
	}
}

// TestCandidatesOrdersDecayAgainstTheBoundClock is the ordering half of the
// bound clock. Decay reorders the window, so a request clock that disagrees
// with the wall clock reorders it differently: the row that is fresh relative
// to the request must outrank the stronger hit that is ancient relative to it,
// and it cannot if the candidate path reads the wall clock instead.
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

// TestCandidatesCarriesScoringFacts: the trace copies these, so they have to be
// the numbers fusion actually used, including the -1 that distinguishes "this
// leg did not retrieve the row" from a first place.
func TestCandidatesCarriesScoringFacts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	hit := makeMemory(t, s, "sqlite wal mode checkpoint starvation")
	miss := makeMemory(t, s, "zzz unrelated quantum banana syntax")
	// cosine 0.6 with the query, and content that shares no term with it, so
	// the row can only arrive through the vector leg.
	if err := s.StoreEmbedding(ctx, miss, []float32{0.6, 0.8, 0}, "test"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}

	req := candidateRequest("sqlite wal mode checkpoint", 10, time.Now().UTC())
	req.QueryVec = []float32{1, 0, 0}
	req.Condition = CondHybrid
	set, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	byID := map[string]Candidate{}
	for _, c := range set.Rows {
		byID[c.ID] = c
	}
	row, ok := byID[hit]
	if !ok {
		t.Fatalf("keyword hit %s missing: %v", hit, candidateIDs(t, set))
	}
	if row.FTSRank != 0 {
		t.Errorf("fts rank = %d, want 0 for the only keyword hit", row.FTSRank)
	}
	if row.VectorRank != -1 || row.VectorScore != -1 {
		t.Errorf("vector facts = %d at %v, want -1 at -1 for a leg that did not retrieve it", row.VectorRank, row.VectorScore)
	}
	if row.Base <= 0 {
		t.Errorf("base = %v, want a positive fused score", row.Base)
	}
	if row.ID != hit || row.Content == "" {
		t.Error("candidate does not carry the hydrated memory")
	}
	if byID[miss].VectorScore <= 0 {
		t.Error("vector-only candidate did not carry its cosine")
	}
}

// TestCandidatesScansValidityColumns: the columns exist and are readable, so
// the retrieval DTO carries them. No MCP writer sets them yet, which is why the
// test writes them the way ImportMemory and RestoreSnapshot do.
func TestCandidatesScansValidityColumns(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := makeMemory(t, s, "retention policy for the nightly backup window")

	from, until, verified := "2026-01-01", "2026-12-31 23:59:59", "2026-09-20 08:00:00"
	if _, err := s.db.ExecContext(ctx,
		`UPDATE memories SET valid_from = ?, valid_until = ?, verified_at = ? WHERE id = ?`,
		from, until, verified, id); err != nil {
		t.Fatalf("write validity columns: %v", err)
	}

	set, err := s.Candidates(ctx, candidateRequest("retention policy nightly backup", 10, time.Now().UTC()))
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
		t.Fatalf("row %s missing: %v", id, candidateIDs(t, set))
	}
	if row.ValidFrom == nil || *row.ValidFrom != from {
		t.Errorf("valid_from = %v, want %q", row.ValidFrom, from)
	}
	if row.ValidUntil == nil || *row.ValidUntil != until {
		t.Errorf("valid_until = %v, want %q", row.ValidUntil, until)
	}
	if row.VerifiedAt == nil || *row.VerifiedAt != verified {
		t.Errorf("verified_at = %v, want %q", row.VerifiedAt, verified)
	}

	// Every retrieval path that hydrates a row has to carry the columns, or a
	// validity claim would be visible to one surface and invisible to another.
	byIDs, err := s.GetByIDs(ctx, []string{id})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(byIDs) != 1 || byIDs[0].ValidFrom == nil {
		t.Errorf("GetByIDs dropped the validity columns: %+v", byIDs)
	}
	fts, err := s.SearchFTS(ctx, testProject, "retention policy", 10)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	if len(fts) != 1 || fts[0].ValidUntil == nil {
		t.Errorf("SearchFTS dropped the validity columns: %+v", fts)
	}
}

// TestCandidateEmbedsMemoryWithoutRedeclaringValidity: Candidate embeds
// Memory, and a redeclared field would silently shadow the embedded one — a
// validity claim read from the wrong field. The collision is rejected here
// rather than diagnosed later in a trace.
func TestCandidateEmbedsMemoryWithoutRedeclaringValidity(t *testing.T) {
	ct := reflect.TypeFor[Candidate]()
	for i := range ct.NumField() {
		f := ct.Field(i)
		if f.Anonymous {
			continue
		}
		switch f.Name {
		case "ValidFrom", "ValidUntil", "VerifiedAt", "Scope", "Confidence", "Agent":
			t.Errorf("Candidate declares %s; the value must come from the embedded Memory", f.Name)
		}
	}
	mt := reflect.TypeFor[Memory]()
	for _, name := range []string{"ValidFrom", "ValidUntil", "VerifiedAt"} {
		f, ok := mt.FieldByName(name)
		if !ok {
			t.Errorf("Memory has no %s field", name)
			continue
		}
		if f.Type.Kind() != reflect.Pointer {
			t.Errorf("Memory.%s is %v, want a pointer so nil means no claim", name, f.Type)
		}
	}
}

// TestCandidatesHonoursTheConfiguredEmbeddingIdentity: the store's snapshot
// copy has to carry every knob that steers retrieval, and the embedding identity
// is the one that decides whether a stored vector may be scored at all. A
// candidate set built without it would rank the foreign-space vectors the real
// search excludes, so a search would return rows no caller could reproduce.
func TestCandidatesHonoursTheConfiguredEmbeddingIdentity(t *testing.T) {
	store, ctx := setupTestStore(t)
	store.SetEmbeddingIdentity(identityCurrent)
	current, stale := seedIdentityRows(t, store, ctx)

	// The query term is in the current row's content only, so the stale row can
	// reach the candidate set through the vector leg and nothing else: what this
	// asserts is the leg's verdict, not the keyword leg's.
	req := candidateRequest("configured", 10, time.Now().UTC())
	req.ProjectID = "test-proj" // the project setupTestStore seeds
	req.QueryVec = []float32{1, 0, 0}
	req.Condition = CondHybrid
	set, err := store.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	for _, c := range set.Rows {
		if c.ID == stale {
			t.Errorf("candidate %s carries a vector from another space: the snapshot store lost the "+
				"configured embedding identity, so its vector leg scored a vector production search excludes", stale)
		}
	}
	if got := set.Legs["vector"]; got.Applicable && !got.Attempted {
		t.Error("vector leg reported applicable but not attempted for a hybrid request with a query vector")
	}
	if !containsID(set, current) {
		t.Errorf("candidates = %v, want the current-identity row %s", candidateIDs(t, set), current)
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

// TestCandidatesReportsLegStatuses: absence is only claimable when every
// applicable leg ran, and a truncated leg is not complete coverage. The
// statuses are what makes that decidable downstream.
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

// TestCandidatesReportsEdgesAndTheirStatus: stages 5 and 6 read the edge set,
// and a failed lookup has to be visible rather than silently empty.
func TestCandidatesReportsEdgesAndTheirStatus(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "sqlite wal mode checkpoint starvation")
	b := makeMemory(t, s, "sqlite wal mode checkpoint after a restart")
	if err := s.CreateLink(ctx, a, b, "contradicts", 1, "manual"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	set, err := s.Candidates(ctx, candidateRequest("sqlite wal mode checkpoint", 10, time.Now().UTC()))
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if set.EdgesStatus.Status != "ok" {
		t.Errorf("edge status = %+v, want ok", set.EdgesStatus)
	}
	if len(set.Edges) != 1 {
		t.Fatalf("edges = %+v, want the contradiction between the two candidates", set.Edges)
	}
	e := set.Edges[0]
	if (e.From != a || e.To != b) && (e.From != b || e.To != a) {
		t.Errorf("edge %+v does not join the two candidates", e)
	}
	if e.Relation != "contradicts" {
		t.Errorf("relation = %q, want contradicts", e.Relation)
	}

	// A query that matches nothing links nothing, which is not a failure.
	empty, err := s.Candidates(ctx, candidateRequest("no such term anywhere in the store", 10, time.Now().UTC()))
	if err != nil {
		t.Fatalf("Candidates empty: %v", err)
	}
	if empty.EdgesStatus.Status != "unavailable" {
		t.Errorf("edge status = %+v, want unavailable for a successful query with no edges", empty.EdgesStatus)
	}
}

// TestCandidatesRejectsInvalidRequests: the retriever validates what it is
// asked to do, because it is reachable directly as well as through the
// assembler.
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

// TestCandidatesGlobalOnlyForAnUnresolvedProject: search keeps today's
// fallback — an unresolved project still reaches the global rows rather than
// failing or searching everything.
func TestCandidatesGlobalOnlyForAnUnresolvedProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	if _, err := s.Create(ctx, "_global", Memory{
		Category: "convention", Content: "unattributed commits belong to the committer",
		Source: "manual", Importance: 0.7,
	}); err != nil {
		t.Fatalf("Create global: %v", err)
	}
	makeMemory(t, s, "unattributed commits are forbidden in this project")

	req := candidateRequest("unattributed commits", 10, time.Now().UTC())
	req.Mode = GlobalOnly
	req.ProjectID = ""

	set, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(set.Rows) != 1 {
		t.Fatalf("global-only search returned %d rows, want only the global one: %v", len(set.Rows), candidateIDs(t, set))
	}
	if set.Rows[0].ProjectID != "_global" {
		t.Errorf("row project = %q, want _global", set.Rows[0].ProjectID)
	}
}

// TestCandidatesAllProjectsSeesEveryProject: the cross-project mode has no
// project predicate at all.
func TestCandidatesAllProjectsSeesEveryProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "second", "/tmp/second", "second"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := s.Create(ctx, "second", Memory{
		Category: "fact", Content: "cross project sqlite wal mode note", Source: "manual", Importance: 0.7,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	makeMemory(t, s, "cross project sqlite wal mode note for the first project")

	req := candidateRequest("cross project sqlite wal mode", 10, time.Now().UTC())
	req.Mode = AllProjects
	req.ProjectID = ""

	set, err := s.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(set.Rows) != 2 {
		t.Errorf("all-projects search returned %d rows, want both projects: %v", len(set.Rows), candidateIDs(t, set))
	}
}

// TestNewStoreWarnsWhenItHasNoReadHandle: the production DSN issues
// BEGIN IMMEDIATE, so a candidate transaction on the primary handle takes the
// write lock. A store without an injected read handle says so once, rather
// than taking it quietly on every search.
func TestNewStoreWarnsWhenItHasNoReadHandle(t *testing.T) {
	var logs strings.Builder
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, logger)
	if s.readDB != nil {
		t.Fatal("NewStore invented a read handle")
	}
	if _, err := s.Candidates(context.Background(), candidateRequest("anything", 5, time.Now().UTC())); err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if !strings.Contains(logs.String(), "read-only handle") {
		t.Errorf("no warning about the missing read handle:\n%s", logs.String())
	}

	// With a read handle the snapshot never touches the primary connection, so
	// there is nothing to warn about.
	path := filepath.Join(t.TempDir(), "ghost.db")
	primary, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = primary.Close() })
	readDB, err := OpenReadDB(path)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	t.Cleanup(func() { _ = readDB.Close() })
	var quiet strings.Builder
	withRead := NewStoreWithRead(primary, readDB, slog.New(slog.NewTextHandler(&quiet, &slog.HandlerOptions{Level: slog.LevelWarn})))
	if _, err := withRead.Candidates(context.Background(), candidateRequest("anything", 5, time.Now().UTC())); err != nil {
		t.Fatalf("Candidates with read handle: %v", err)
	}
	if quiet.Len() != 0 {
		t.Errorf("a store with a read handle warned: %s", quiet.String())
	}
}

// TestCandidatesRunsOnTheInjectedReadHandle: with a read handle, the candidate
// transaction is taken on that handle, so a write transaction already open on
// the primary handle does not block it and the snapshot is the committed one.
// This is the guarantee a single-connection primary handle cannot give.
func TestCandidatesRunsOnTheInjectedReadHandle(t *testing.T) {
	if _, err := OpenReadDB(":memory:"); err == nil {
		t.Fatal("OpenReadDB accepted :memory: — a second connection cannot see a private in-memory database")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "ghost.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	readDB, err := OpenReadDB(path)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	t.Cleanup(func() { _ = readDB.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewStoreWithRead(db, readDB, logger)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, testProject, "/tmp/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	makeMemory(t, s, "sqlite wal mode checkpoint starvation")

	// Hold an open write transaction on the primary handle: it owns the only
	// connection there, and with _txlock=immediate it owns the write lock.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO memories (project_id, category, content, source, importance, tags)
		 VALUES (?, 'fact', 'uncommitted row', 'manual', 0.7, '[]')`, testProject); err != nil {
		t.Fatalf("insert in tx: %v", err)
	}

	done := make(chan *CandidateSet, 1)
	errc := make(chan error, 1)
	go func() {
		set, err := s.Candidates(ctx, candidateRequest("sqlite wal mode checkpoint", 10, time.Now().UTC()))
		if err != nil {
			errc <- err
			return
		}
		done <- set
	}()
	select {
	case err := <-errc:
		t.Fatalf("Candidates on the read handle failed while a write transaction was open: %v", err)
	case set := <-done:
		if len(set.Rows) != 1 {
			t.Errorf("rows = %v, want the committed row only: the snapshot must not include the open transaction", candidateIDs(t, set))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Candidates blocked: the candidate transaction did not use the injected read handle")
	}
}

// TestOpenReadDBRefusesAMissingDatabase: a diagnostic read must not create the
// file it is reporting about.
func TestOpenReadDBRefusesAMissingDatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "ghost.db")
	if _, err := OpenReadDB(missing); !errors.Is(err, ErrNoDatabase) {
		t.Fatalf("OpenReadDB(missing) error = %v, want ErrNoDatabase", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Error("OpenReadDB created the database it was asked to report about")
	}
}

// TestStoreCloseClosesTheInjectedReadHandle: the read handle is a second
// database connection this Store opened, so closing the Store has to close it.
// Leaving it open holds a WAL reader for the life of the process, which is
// what the read handle exists to avoid holding during a retrieval.
func TestStoreCloseClosesTheInjectedReadHandle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ghost.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	readDB, err := OpenReadDB(path)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}

	s := NewStoreWithRead(db, readDB, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := readDB.PingContext(context.Background()); err == nil {
		t.Error("the injected read handle is still open after Store.Close")
	}
}

// TestCandidatesErrorsWhenEveryApplicableLegFails: a total retrieval failure is
// an error, not an empty set. The zero CandidateSet is reserved for the case
// where one leg failed and another completed — a partial retrieval, which the
// caller reports as incomplete. When nothing ran, there is no result to report
// at all, and returning an empty set would let a broken keyword index read as
// "this store has no memory matching that".
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

// TestCandidatesReturnsStatusesForAPartialLegFailure: one leg failed and the
// other completed with nothing, so the caller can say "the search was
// incomplete" rather than "nothing matched". This is the case the zero set is
// for, and it must not be an error.
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

// TestCandidatesWarnsOncePerRetiredIdentity: the foreign-vector warning is
// gated so a re-embed — which lasts many queries — reports each retired
// identity once per process rather than once per search. The gate is shared
// state, so the snapshot store Candidates builds has to carry it: a store with
// no gate reports every time, and since the formatted search path now runs
// through Candidates, every search after a model change would repeat the line.
func TestCandidatesWarnsOncePerRetiredIdentity(t *testing.T) {
	store, ctx := setupTestStore(t)
	var logs strings.Builder
	store.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	store.SetEmbeddingIdentity(identityCurrent)
	current, _ := seedIdentityRows(t, store, ctx)
	_ = current

	req := candidateRequest("configured", 10, time.Now().UTC())
	req.ProjectID = "test-proj"
	req.QueryVec = []float32{1, 0, 0}
	req.Condition = CondHybrid
	for range 3 {
		if _, err := store.Candidates(ctx, req); err != nil {
			t.Fatalf("Candidates: %v", err)
		}
	}

	if n := strings.Count(logs.String(), "another vector space"); n != 1 {
		t.Errorf("foreign-vector warning appeared %d times over three searches, want 1: "+
			"the snapshot store is not sharing the process's warning gate", n)
	}
}

// TestLegFactsAreNotLeftOverFromAnEarlierSearch: the vector scan's snapshot is
// pooled and reused across queries, and the leg's facts now travel on it. A count
// that survives the reuse would be reported as this search's: a leg that skipped
// a retired identity once would keep claiming it skipped rows for every later
// query, which is the difference between "your model changed" and "your model
// changed, on every search since".
func TestLegFactsAreNotLeftOverFromAnEarlierSearch(t *testing.T) {
	store, ctx := setupTestStore(t)
	store.SetEmbeddingIdentity(identityCurrent)
	seedIdentityRows(t, store, ctx) // one vector in the configured space, one retired

	req := candidateRequest("configured", 10, time.Now().UTC())
	req.ProjectID = "test-proj"
	req.QueryVec = []float32{1, 0, 0}
	req.Condition = CondHybrid
	first, err := store.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates (mixed identities): %v", err)
	}
	if first.Legs["vector"].DimMismatch == 0 {
		t.Fatal("precondition: the first search should have skipped the retired vector")
	}

	// Retire the odd one out: the same search, now with nothing to skip.
	if _, err := store.db.ExecContext(ctx, "DELETE FROM memory_embeddings WHERE model = ?", identityStale); err != nil {
		t.Fatalf("delete the stale vector: %v", err)
	}
	second, err := store.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("Candidates (one identity): %v", err)
	}
	if got := second.Legs["vector"].DimMismatch; got != 0 {
		t.Errorf("the second search reports %d skipped rows, want 0: the fact is left over from the pooled snapshot", got)
	}
}

// TestTheSnapshotStoreRunsItsVectorLegThroughTheStoresScratch: the corpus-sized
// snapshot is recycled through a store's scratch, and the store Candidates builds
// for one retrieval is a bare literal. With no scratch of its own it takes the
// pre-#560 path and allocates a snapshot per query, handing it to a pool that is
// garbage the moment the retrieval returns -- reintroducing on the live search path,
// at every search, the cost the scratch exists to avoid.
//
// The assertion is that the store's own allocation counter moved, which is the one
// part of this that is guaranteed. Two adjacent properties are NOT, and the first
// version of this test asserted them and failed in CI for exactly that reason:
// a sync.Pool's contents are per-processor and are dropped at every collection, so
// neither "the pool is empty" nor "five further searches allocate nothing" is a
// property a test can hold -- a collection in the window re-allocates, whichever
// store is asking. What cannot vary is whether the retrieval went through the real
// store's scratch at all: a store with no scratch of its own never touches this
// counter, so a counter that never moves is the defect, deterministically.
func TestTheSnapshotStoreRunsItsVectorLegThroughTheStoresScratch(t *testing.T) {
	store, ctx := setupTestStore(t)
	for i, content := range []string{"vector search corpus row one", "vector search corpus row two"} {
		id := createTestMemory(t, store, ctx, content)
		vec := []float32{0.8 - float32(i)*0.1, 0.6, 0}
		if err := store.StoreEmbedding(ctx, id, vec, identityCurrent); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
	}

	req := candidateRequest("vector search corpus", 10, time.Now().UTC())
	req.ProjectID = "test-proj"
	req.QueryVec = []float32{1, 0, 0}
	req.Condition = CondHybrid

	// Drain first. Create's near-duplicate probes borrow and return a corpus
	// snapshot, so a pool that still holds one would let the retrieval recycle it
	// without allocating, and the counter would not move for a reason that has
	// nothing to do with the seam. Each Get on an empty pool allocates, which is
	// fine: the baseline is read after this, not before.
	for range 8 {
		if v := store.scratch.pool.Get(); v != nil {
			_ = v
		}
	}
	before := store.scratch.allocationCount()
	if _, err := store.Candidates(ctx, req); err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	if after := store.scratch.allocationCount(); after <= before {
		t.Errorf("the store's corpus-scratch allocations did not move (%d -> %d) across a retrieval, "+
			"so the snapshot store ran its vector leg without this store's scratch: a corpus-sized "+
			"allocation per query that nothing recycles", before, after)
	}
}
