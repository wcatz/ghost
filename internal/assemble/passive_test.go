package assemble

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// passiveRequest is the session-start shape: no query, two bucket policies, and
// no total cap. The per-slice caps are the block's whole bound, which is why
// MaxItems and MaxBytes are both 0.
func passiveRequest() Request {
	return Request{
		ProjectID: "proj",
		Query:     "",
		Source:    SourceSessionStart,
		Condition: CondFTSOnly,
		Now:       time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
		Budget: Budget{
			Slices: []Slice{
				{Bucket: "proj", MaxItems: 3, ClampBytes: 200, DropDemotedLosers: false},
				{Bucket: "_global", MaxItems: 2, ClampBytes: 300, DropDemotedLosers: true},
			},
		},
	}
}

func passiveSet(rows ...memory.Candidate) *memory.CandidateSet {
	return &memory.CandidateSet{
		Rows: rows,
		Legs: map[string]memory.LegStatus{
			"fts":    {},
			"vector": {},
		},
	}
}

func projectCandidate(id string, score float64) memory.Candidate {
	return candidate(id, "proj", "fact", "a project memory "+id, score)
}

func globalCandidate(id string, score float64) memory.Candidate {
	return candidate(id, "_global", "preference", "a global memory "+id, score)
}

// TestRunServesPassiveSessionStart is the RED test: an empty query on a
// session-start source with populated slice policies is assembled rather than
// refused. validateRequest is what rejects it today.
func TestRunServesPassiveSessionStart(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(
		projectCandidate("p1", 0.9), projectCandidate("p2", 0.8), projectCandidate("p3", 0.7),
		projectCandidate("p4", 0.6),
		globalCandidate("g1", 0.5), globalCandidate("g2", 0.4), globalCandidate("g3", 0.3),
	)}
	res := run(t, f, passiveRequest())
	if len(res.Items) == 0 {
		t.Fatal("passive Run admitted no rows")
	}
}

// TestRunPassiveCapsEachBucketIndependently: the per-slice caps are the whole
// bound, and they are independent — 3 project and 2 global, not 3 and 2 out of
// 5. This is the shape that a total cap would break, and it is why the
// session-start budget states MaxItems 0.
func TestRunPassiveCapsEachBucketIndependently(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(
		projectCandidate("p1", 0.9), projectCandidate("p2", 0.8), projectCandidate("p3", 0.7),
		projectCandidate("p4", 0.6), projectCandidate("p5", 0.5),
		globalCandidate("g1", 0.4), globalCandidate("g2", 0.3), globalCandidate("g3", 0.2),
	)}
	res := run(t, f, passiveRequest())
	counts := map[string]int{}
	for _, it := range res.Items {
		counts[it.Bucket]++
	}
	if counts["proj"] != 3 {
		t.Errorf("project bucket: got %d items, want the slice cap of 3 (all: %v)", counts["proj"], itemIDs(res.Items))
	}
	if counts["_global"] != 2 {
		t.Errorf("_global bucket: got %d items, want the slice cap of 2 (all: %v)", counts["_global"], itemIDs(res.Items))
	}
}

// TestRunPassiveIsNeverWeak: a passive retrieval has no query, so there is no
// relevance claim to fail a floor. `weak` means "we found rows and they were
// not good enough", which is a statement about a query this surface never
// received — so an answerable passive block carries `not_applicable` instead.
func TestRunPassiveIsNeverWeak(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(projectCandidate("p1", 0.9))}
	res := run(t, f, passiveRequest())
	if res.Outcome == OutcomeWeak {
		t.Errorf("passive outcome is %q/%q: a session start has no query to be weak against", res.Outcome, res.Reason)
	}
	if res.Reason != reasonNotApplicable {
		t.Errorf("passive reason: got %q, want %q", res.Reason, reasonNotApplicable)
	}
	if res.Abstention != "" {
		t.Errorf("an answerable passive block withholds nothing, so Abstention must be empty; got %q", res.Abstention)
	}
}

// TestRunPassiveEmptyNamesTheWindowNotTheStore: `no_memories` describes an empty
// over-fetched window. It must not read as "this store holds no memories",
// because the passive path never counts the corpus — the two-pass selection and
// the caps mean the block was never a census.
func TestRunPassiveEmptyNamesTheWindowNotTheStore(t *testing.T) {
	f := &fakeRetriever{set: passiveSet()}
	res := run(t, f, passiveRequest())
	if res.Outcome != OutcomeEmpty {
		t.Errorf("outcome: got %q, want %q", res.Outcome, OutcomeEmpty)
	}
	if res.Reason != reasonNoMemories {
		t.Errorf("reason: got %q, want %q", res.Reason, reasonNoMemories)
	}
	// The design spec's copy principle for this reason: it describes the empty
	// window, not store-wide absence. Two wordings are therefore refusals, and
	// each is a different lie:
	//
	//   - "no matching memories found" claims the store holds none, which is a
	//     census this path never took.
	//   - "no sufficiently trustworthy memory found" claims rows were found and
	//     rejected, which is the EXCLUSION wording. Nothing was excluded here —
	//     the window came back empty — so the sentence would invent a rejection
	//     that never happened, and send the reader to a filter that removed
	//     nothing.
	if strings.Contains(strings.ToLower(res.Abstention), "no matching memories") {
		t.Errorf("abstention claims store-wide absence, which a passive window cannot support: %q", res.Abstention)
	}
	if strings.Contains(res.Abstention, "sufficiently trustworthy") {
		t.Errorf("abstention uses the exclusion wording, but nothing was excluded — the window was empty: %q", res.Abstention)
	}
	if !strings.Contains(res.Abstention, "window") {
		t.Errorf("abstention must name the window it observed: %q", res.Abstention)
	}
}

// TestRunPassiveEmptyByExclusionUsesTheExclusionWording is the other half of the
// pair above: a passive window that came back non-empty and was emptied by
// stage 2 IS an exclusion, and must say so in the words every other exclusion
// reason uses. The distinction is the whole reason no_memories is its own reason
// rather than a synonym for no_candidates.
func TestRunPassiveEmptyByExclusionUsesTheExclusionWording(t *testing.T) {
	expired := projectCandidate("p_exp", 0.9)
	expired.ValidUntil = stampPtr("2026-01-01 00:00:00")
	f := &fakeRetriever{set: passiveSet(expired)}
	res := run(t, f, passiveRequest())
	if res.Outcome != OutcomeEmpty {
		t.Fatalf("outcome: got %q, want %q", res.Outcome, OutcomeEmpty)
	}
	if res.Reason != reasonAllInvalid {
		t.Errorf("reason: got %q, want %q — a window that was emptied by a stage is not `no_memories`", res.Reason, reasonAllInvalid)
	}
	if !strings.Contains(strings.ToLower(res.Abstention), "no sufficiently trustworthy memory found") {
		t.Errorf("an exclusion-caused empty must use the shared exclusion wording: %q", res.Abstention)
	}
}

// TestRunPassiveReportsTheFloorAsNotApplied: no leg ran, so no arm held a value.
// The machine line has to say the arm was not applied, or a passive verdict reads
// as a measured one.
func TestRunPassiveReportsTheFloorAsNotApplied(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(projectCandidate("p1", 0.9))}
	res := run(t, f, passiveRequest())
	if !strings.Contains(res.Machine, "abstain_cosine=not_applied") {
		t.Errorf("machine line: got %q, want abstain_cosine=not_applied", res.Machine)
	}
	if res.Trace.Floors.FTSApplied {
		t.Error("FTSApplied: a passive retrieval ran no keyword leg, so no arm applied")
	}
	if res.Trace.Floors.VectorApplied {
		t.Error("VectorApplied: a passive retrieval ran no vector leg, so no arm applied")
	}
}

// TestRunPassiveStillFiltersValidity: stage 2 is the same stage the search path
// runs, and this is the membership change the migration buys. An expired row and
// a not-yet-valid row are both excluded from the injected block, which the
// loaders never did.
func TestRunPassiveStillFiltersValidity(t *testing.T) {
	expired := projectCandidate("p_exp", 0.9)
	expired.ValidUntil = stampPtr("2026-01-01 00:00:00")
	future := projectCandidate("p_fut", 0.95)
	future.ValidFrom = stampPtr("2027-01-01 00:00:00")
	live := projectCandidate("p_live", 0.5)
	f := &fakeRetriever{set: passiveSet(expired, future, live)}
	res := run(t, f, passiveRequest())
	if got := itemIDs(res.Items); len(got) != 1 || got[0] != "p_live" {
		t.Errorf("items: got %v, want only p_live — validity filtering must run on the passive path too", got)
	}
}

// TestRunPassiveCarriesScopeIntoTheItem: the row's scope reaches the shared
// renderer, which is what puts a session-start row on the same footing as a
// search result.
func TestRunPassiveCarriesScopeIntoTheItem(t *testing.T) {
	c := scopedCandidate("p_scoped", map[string]string{"area": "payments"}, 0.9)
	f := &fakeRetriever{set: passiveSet(c)}
	res := run(t, f, passiveRequest())
	if len(res.Items) != 1 {
		t.Fatalf("items: got %d, want 1", len(res.Items))
	}
	if got := ScopeLabel(res.Items[0].Scope); !strings.Contains(got, "area=payments") {
		t.Errorf("scope label: got %q, want it to name area=payments", got)
	}
}

// TestRunPassiveRetriesTheWindowWhenScopeExcludesEveryRow: a predicate that
// removes a window row must be backfilled from the widened set, or a session
// start would report an absence the store does not have. This is the seam's
// whole reason for returning an untrimmed candidate set.
func TestRunPassiveRetriesTheWindowWhenScopeExcludesEveryRow(t *testing.T) {
	// Every row the window would have closed on contradicts the request's
	// scope; only a deeper row matches. A cap applied before the predicate
	// would return nothing.
	var rows []memory.Candidate
	for i := 0; i < 6; i++ {
		c := scopedCandidate("p_bad", map[string]string{"area": "storage"}, 0.9-float64(i)*0.01)
		c.ID = "p_bad" + string(rune('a'+i))
		rows = append(rows, c)
	}
	match := scopedCandidate("p_match", map[string]string{"area": "payments"}, 0.1)
	rows = append(rows, match)
	f := &fakeRetriever{set: passiveSet(rows...)}

	req := passiveRequest()
	req.Scope = map[string]string{"area": "payments"}
	res := run(t, f, req)
	if len(res.Items) != 1 || res.Items[0].ID != "p_match" {
		t.Errorf("items: got %v, want only the scoped match — the predicate must be applied over the widened set", itemIDs(res.Items))
	}
}

// TestRunPassiveBackfillsFromTheTailBehindThePool: the store's two-pass
// selection keeps a POOL of twice the item cap and returns everything past it,
// undemoted, as the backfill supply. Only the pool is "selected", so a stage
// that drops pool rows has to be able to reach the tail or the block is a hole —
// and the hole is worse than a short block, because the caller reads the block
// as everything there was.
//
// The fixture puts the retired rows where the pool is and the live rows past it,
// and the shape is what makes the property checkable: stage 8's slice cap is two
// rows, so a window that closed BEFORE validity would admit two retired rows and
// then empty itself, reporting `no memories` for a project that holds four. It
// reaches the tail only because every stage that can affect membership runs
// before the closure, which is the reason the store returns an untrimmed set at
// all.
//
// This is the assembler's half of the property; the store's half — that the tail
// is returned at all, in the policy's order — is pinned in
// `internal/memory`'s passive tests, because a fake retriever cannot see it.
func TestRunPassiveBackfillsFromTheTailBehindThePool(t *testing.T) {
	const itemCap = 2
	expired := "2026-01-01 00:00:00"
	rows := make([]memory.Candidate, 0, 12)
	// The pool: twice the item cap, every row retired.
	for i := range 2 * itemCap {
		c := projectCandidate("pool"+string(rune('a'+i)), 0.9-float64(i)*0.01)
		c.ValidUntil = &expired
		rows = append(rows, c)
	}
	// The tail: live rows behind the pool, in the order the policy returned them.
	for i := range 4 {
		rows = append(rows, projectCandidate("tail"+string(rune('a'+i)), 0.4-float64(i)*0.01))
	}

	req := passiveRequest()
	req.Budget.Slices[0].MaxItems = itemCap
	res := run(t, &fakeRetriever{set: passiveSet(rows...)}, req)

	want := []string{"taila", "tailb"}
	if got := itemIDs(res.Items); !eq(got, want) {
		t.Errorf("items = %v, want %v: stage 2 dropped every selected row, so the block can only be filled from the "+
			"backfill supply the widened set carries", got, want)
	}
	if res.Outcome != OutcomeAnswerable {
		t.Errorf("outcome = %q/%q, want answerable: the bucket holds four live rows", res.Outcome, res.Reason)
	}
}

// TestRunPassiveMapsSlicesOntoPolicies: the retriever needs the bucket's
// over-fetch, order, floor and threshold, and the budget is where the caller
// states them. A slice that did not reach the retriever would leave the passive
// fetch with no policy to run.
func TestRunPassiveMapsSlicesOntoPolicies(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(projectCandidate("p1", 0.9))}
	req := passiveRequest()
	req.Budget.Slices[0].OverFetch = 45
	req.Budget.Slices[0].TwoPass = true
	req.Budget.Slices[0].Order = "decay"
	req.Budget.Slices[0].BehaviorFloor = 2
	req.Budget.Slices[0].DemotionThreshold = 0.9
	run(t, f, req)

	if len(f.req.Passive) != 2 {
		t.Fatalf("passive policies: got %d, want one per slice (2)", len(f.req.Passive))
	}
	byBucket := map[string]memory.SlicePolicy{}
	for _, p := range f.req.Passive {
		byBucket[p.Bucket] = p
	}
	proj, ok := byBucket["proj"]
	if !ok {
		t.Fatalf("no policy for the project bucket: %+v", f.req.Passive)
	}
	if proj.OverFetch != 45 {
		t.Errorf("project OverFetch: got %d, want 45", proj.OverFetch)
	}
	if !proj.TwoPass || proj.BehaviorFloor != 2 || proj.Order != "decay" {
		t.Errorf("project policy lost its two-pass selection: %+v", proj)
	}
	g := byBucket["_global"]
	if !g.DropDemotedLosers {
		t.Error("_global policy must carry DropDemotedLosers: the hook drops global near-duplicate losers")
	}
}

// TestRunPassiveForwardsScopeToTheRetriever: the scope narrows the FETCH, not
// only the rows it returns, because the over-fetch chooses which rows are read
// at all. A row the session excluded must not spend any of the window.
func TestRunPassiveForwardsScopeToTheRetriever(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(projectCandidate("p1", 0.9))}
	req := passiveRequest()
	req.Scope = map[string]string{"area": "payments"}
	run(t, f, req)
	if f.req.Scope["area"] != "payments" {
		t.Errorf("retriever scope: got %v, want area=payments", f.req.Scope)
	}
}

// TestRunPassiveRejectsAPolicyWithNoOverFetch: an unbounded passive fetch is a
// store scan, and this path runs on every session start. A slice with no
// over-fetch and no cap is refused rather than fetched.
func TestRunPassiveRejectsAPolicyWithNoOverFetch(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(projectCandidate("p1", 0.9))}
	req := passiveRequest()
	req.Budget.Slices = []Slice{{Bucket: "proj", ClampBytes: 200}}
	_, err := Run(context.Background(), f, req)
	if err == nil {
		t.Fatal("a passive slice with neither a cap nor an over-fetch must be refused: it bounds nothing and would fetch the store")
	}
}

// TestRunPassiveProjectlessIsGlobalOnly: a session start in an unmatched
// directory is a supported state, and it reads the global rows. The mode is
// GlobalOnly rather than ProjectScoped so no row can be pulled in by an empty
// project id.
func TestRunPassiveProjectlessIsGlobalOnly(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(globalCandidate("g1", 0.9))}
	req := passiveRequest()
	req.ProjectID = ""
	// A projectless session start reads ONLY the global bucket, and that is now a
	// REFUSAL rather than an assertion about Mode: on a passive read the slice
	// bucket IS the project predicate, so a "proj" slice left here would read that
	// project through the real store even though the request named no project.
	// The previous version of this test kept the "proj" slice and passed only
	// because the fake retriever ignores the request it is given.
	req.Budget.Slices = []Slice{{
		Bucket: memory.GlobalProjectID, MaxItems: 2, OverFetch: 16,
		ClampBytes: 300, Order: "pinned_importance_updated",
		DemotionThreshold: 0.85, DropDemotedLosers: true,
	}}
	res := run(t, f, req)
	if res.Trace.Mode != string(memory.GlobalOnly) {
		t.Errorf("mode: got %q, want %q", res.Trace.Mode, memory.GlobalOnly)
	}
	if len(res.Items) != 1 {
		t.Errorf("a projectless session start must still admit global rows; got %v", itemIDs(res.Items))
	}
	if got := f.req.Passive; len(got) != 1 || got[0].Bucket != memory.GlobalProjectID {
		t.Errorf("policies sent: %+v, want exactly the _global bucket", got)
	}
}

// TestRunPassiveRefusesASliceNamingAnotherProject is the mismatch refusal, and it
// is the only thing standing between a request and a project it never named. On a
// passive read the bucket IS the project predicate — the store binds it as the
// WHERE clause and does not consult Mode — so a slice pointing elsewhere reads
// that project, and stage 3 only RECORDS the mismatch as
// Signals[id].ProjectMatch=false, which nothing refuses and no surface projects.
func TestRunPassiveRefusesASliceNamingAnotherProject(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(projectCandidate("p1", 0.9))}
	req := passiveRequest()
	req.Budget.Slices[0].Bucket = "some-other-project"
	_, err := Run(context.Background(), f, req)
	if err == nil {
		t.Fatal("a passive slice naming a project the request did not must be refused")
	}
	if !strings.Contains(err.Error(), "project predicate") {
		t.Errorf("the refusal must say why the bucket is load-bearing; got %v", err)
	}
	if f.sets != 0 {
		t.Errorf("the retriever was called %d times for a request this seam refused", f.sets)
	}
	// The _global bucket is always legitimate beside a project, because a session
	// start reads both and the global rows are not "another project".
	ok := passiveRequest()
	ok.Budget.Slices[1].Bucket = memory.GlobalProjectID
	run(t, &fakeRetriever{set: passiveSet(globalCandidate("g1", 0.9))}, ok)
}

// TestRunPassiveQueryModeIgnoresTheDropPolicyInTheNote is the shape of the other
// half: a query-mode request that sets DropDemotedLosers must not be told a
// memory was dropped, because nothing was — the policies never reach a query
// retrieval and the fusion path only reorders.
func TestRunPassiveQueryModeIgnoresTheDropPolicyInTheNote(t *testing.T) {
	f := &fakeRetriever{set: setOf(candidate("c1", "proj", "fact", "a row", 0.9))}
	req := baseRequest()
	req.Budget = Budget{MaxItems: 2, Slices: []Slice{{
		Bucket: "proj", MaxItems: 2, DropDemotedLosers: true, DemotionThreshold: 0.9,
	}}}
	res := run(t, f, req)
	if containsNote(res.Notes, "losers are REMOVED") {
		t.Errorf("a query-mode request must not be told losers were removed: no policy reaches a query retrieval, "+
			"and the fusion path only reorders (%v)", res.Notes)
	}
	if !containsNote(res.Notes, "no source policy drops losers") {
		t.Errorf("and it must say the drop does not apply here: %v", res.Notes)
	}
}

// TestRunQueryModeIsUnaffectedByThePassiveBranch: passive support must not
// change the search path. A search request with no query is still refused, and
// one with a query gets no passive policies.
func TestRunQueryModeIsUnaffectedByThePassiveBranch(t *testing.T) {
	f := &fakeRetriever{set: setOf(candidate("c1", "proj", "fact", "x", 0.9))}
	run(t, f, baseRequest())
	if len(f.req.Passive) != 0 {
		t.Errorf("a query-mode request must send no passive policies; got %+v", f.req.Passive)
	}

	// A search source with an empty query is still refused: the passive
	// policies are a session-start statement, and serving a search with none
	// would answer it with an empty set that reads as an empty store.
	bad := baseRequest()
	bad.Query = ""
	bad.Source = SourceSearch
	if _, err := Run(context.Background(), f, bad); err == nil {
		t.Error("a search request with an empty query and no policies must still be refused")
	}
}

func stampPtr(s string) *string { return &s }

// TestRunPassiveRefusesAHistoricalRead: Run dispatches on AsOf before Query, so
// a request carrying both would be answered by the historical path with its
// policies discarded and its window replaced — while Run still reported
// `not_applicable` and added the historical qualifier. Nothing refused the
// combination, so the caller would have been told a block neither policy
// describes.
func TestRunPassiveRefusesAHistoricalRead(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(projectCandidate("p1", 0.9))}
	req := passiveRequest()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	req.AsOf = &at
	_, err := Run(context.Background(), f, req)
	if err == nil {
		t.Fatal("a passive request carrying an as_of must be refused")
	}
	if !strings.Contains(err.Error(), "passive") {
		t.Errorf("the refusal must name the passive shape; got %v", err)
	}
}

// TestRunPassiveAnExclusionOutranksALegFact is the precedence rule, built on the
// shape the review names: a passive set whose only row is removed by stage 2, read
// by a retriever that reports the vector leg APPLICABLE but not attempted.
//
// The query path deliberately consults the stages first (see the fall-through
// comment on the `len(p.set.Rows) > 0` block): naming a stage that removed rows is
// the fact the caller can act on, and naming a leg instead would send it to an
// embedder that was never the reason there is no answer. The passive branch
// returned the two leg reasons BEFORE that check, so it inverted the precedence
// the shared path is built to keep — and rendered a passive block with "The query
// was not wrong", a sentence about a query and an embedder for a surface that had
// neither.
func TestRunPassiveAnExclusionOutranksALegFact(t *testing.T) {
	expired := projectCandidate("p_exp", 0.9)
	expired.ValidUntil = stampPtr("2026-01-01 00:00:00")
	f := &fakeRetriever{set: &memory.CandidateSet{
		Rows: []memory.Candidate{expired},
		Legs: map[string]memory.LegStatus{
			"fts":    {Applicable: true, Attempted: true, Available: true},
			"vector": {Applicable: true, Attempted: false},
		},
	}}
	res := run(t, f, passiveRequest())
	if res.Reason != reasonAllInvalid {
		t.Errorf("reason: got %q, want %q — a stage emptied this set, and that outranks a leg that never ran, "+
			"exactly as it does on the query path", res.Reason, reasonAllInvalid)
	}
	// The LEG sentence, not the shared one. "The query was not wrong" is the
	// trailing clause of every exclusion reason including main's `all_invalid`, so
	// asserting on it would fail a correct fix; what is wrong here is claiming a
	// vector leg and a keyword leg ran, on a surface that ran neither.
	if strings.Contains(res.Abstention, "vector leg could not run") {
		t.Errorf("the abstention reports a leg failure for a set a stage emptied — the leg never caused this: %q", res.Abstention)
	}
}

// TestRunPassiveAnEmptyWindowStillNamesTheLeg is the other half, and it is what
// keeps the precedence above from swallowing the leg reasons: when NO stage
// removed anything and the set was empty coming in, the leg facts are the only
// thing left to explain it, and they must survive. Both arms are asserted by the
// one pair because fixing the precedence by deleting the leg checks would make the
// first pass and this one fail.
func TestRunPassiveAnEmptyWindowStillNamesTheLeg(t *testing.T) {
	f := &fakeRetriever{set: &memory.CandidateSet{
		Rows: nil,
		Legs: map[string]memory.LegStatus{
			"fts":    {Applicable: true, Attempted: true, Available: true},
			"vector": {Applicable: true, Attempted: false},
		},
	}}
	res := run(t, f, passiveRequest())
	if res.Reason != reasonVectorUnavailable {
		t.Errorf("reason: got %q, want %q — with no stage to blame, a leg that never ran is the only fact left",
			res.Reason, reasonVectorUnavailable)
	}
}

// TestRunPassiveRefusesACategoryPredicate is the filter the passive fetch cannot
// honour. `passiveFetchSQL` binds `WHERE project_id = ? AND resolved_at IS NULL`
// plus scope — there is no category and no tier in its SQL — while the query path
// WIDENS the window when either is set, precisely so a matching row ranked below
// the cut stays reachable (that widening is what closed #573).
//
// The passive branch returns before that widening, so a passive request carrying
// either predicate is accepted, stage 3 then drops every non-matching row out of a
// window that was never widened, and the answer is `all_out_of_category` — a
// short block, or none — while the store holds exactly the rows the caller asked
// for, just below the over-fetch cut. The caller's category is not a filter that
// arrived too late to apply; it never applied at all, and a block that cannot be
// backfilled from a filter the fetch never bound is a false negative with a
// confident reason attached.
func TestRunPassiveRefusesACategoryPredicate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply func(*Request)
		want  string
	}{
		{"category", func(r *Request) { r.Category = "gotcha" }, "Category"},
		{"retention", func(r *Request) { r.Retention = memory.RetentionPersistent }, "Retention"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRetriever{set: passiveSet(projectCandidate("p1", 0.9))}
			req := passiveRequest()
			tc.apply(&req)
			_, err := Run(context.Background(), f, req)
			if err == nil {
				t.Fatalf("a passive request carrying %s must be refused, not served from a window nothing widened",
					tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal must name the predicate it cannot honour; got %v", err)
			}
			if !strings.Contains(err.Error(), "passive") {
				t.Errorf("the refusal must name the passive shape; got %v", err)
			}
			if f.sets != 0 {
				t.Errorf("the retriever was called %d times for a request this seam refused", f.sets)
			}
		})
	}
}

// TestRunPassiveTraceReportsTheWindowItActuallyRead: Trace.Limit is a checkable
// artifact — the next PR projects it — so on a passive run it has to be the width
// the store was asked for. It was the sum of the slice ITEM CAPS, which is how
// many rows the block admits and not how many were read: a 45+16 over-fetch
// reported 5.
func TestRunPassiveTraceReportsTheWindowItActuallyRead(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(projectCandidate("p1", 0.9))}
	req := passiveRequest()
	req.Budget.Slices[0].OverFetch = 45
	req.Budget.Slices[1].OverFetch = 16
	res := run(t, f, req)
	if got, want := res.Trace.Limit, 61; got != want {
		t.Errorf("Trace.Limit: got %d, want %d — it is the window the retriever was asked for, not the rows the block admits", got, want)
	}
}

// TestRunPassiveHasNoCeilingDisclosure: the disclosure fires when the pipeline
// chose the window itself. A passive window is stated per bucket by the
// over-fetch, so on a passive run the note would name a 100-row window that no
// passive read ever asked for, and — for a budget bounded only by per-slice
// MaxBytes — would tell the reader the block is bounded by a byte cap that is
// real while pointing at a window that is not.
func TestRunPassiveHasNoCeilingDisclosure(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(projectCandidate("p1", 0.9))}
	req := passiveRequest()
	// No item bound anywhere (itemBound 0) AND a passive window that happens to
	// total the documented ceiling of 100 — which is the only shape that can
	// trigger the disclosure, and therefore the only one a test of its absence may
	// use. A window of 61 leaves `retrievalWindow == maxRetrievalWindow` false
	// for the wrong reason, which would make this test pass against the guard
	// having been deleted.
	req.Budget.Slices = []Slice{
		{Bucket: "proj", MaxBytes: 400, OverFetch: 100, DemotionThreshold: 0.9},
	}
	// The control: the SAME budget shape on the query path does carry the note.
	// Without it this test could pass because the note is broken everywhere, which
	// would make the absence it claims to be measuring an artefact.
	control := baseRequest()
	control.Budget = Budget{MaxBytes: 400, Slices: []Slice{{Bucket: "proj", MaxBytes: 400, OverFetch: 100}}}
	controlRes := run(t, &fakeRetriever{set: setOf(candidate("c1", "proj", "fact", "x", 0.9))}, control)
	if !containsNote(controlRes.Notes, "retrieval_window_capped") {
		t.Fatalf("the control did not produce the disclosure, so this test cannot detect its absence: %v", controlRes.Notes)
	}

	res := run(t, f, req)
	if containsNote(res.Notes, "retrieval_window_capped") {
		t.Errorf("a passive window is stated by the over-fetch, not chosen by the pipeline, so it carries no ceiling disclosure: %v", res.Notes)
	}
}

// TestRunPassiveDedupNoteAgreesWithThePolicy: stage 6's note used to say "no
// source policy drops losers on this surface yet", which is false for exactly the
// surface it was written for — a `_global` slice with DropDemotedLosers has just
// had them removed by the retriever.
func TestRunPassiveDedupNoteAgreesWithThePolicy(t *testing.T) {
	dropping := &fakeRetriever{set: passiveSet(globalCandidate("g1", 0.9))}
	res := run(t, dropping, passiveRequest())
	if !containsNote(res.Notes, "REMOVED") {
		t.Errorf("a bucket whose policy drops losers must say so: %v", res.Notes)
	}
	if containsNote(res.Notes, "no source policy drops losers") {
		t.Errorf("the note claims no policy drops losers while one just did: %v", res.Notes)
	}

	// A request with no dropping policy keeps the old sentence, so the two are
	// distinguishable and the new one is not simply always printed.
	plain := passiveRequest()
	plain.Budget.Slices[1].DropDemotedLosers = false
	kept := &fakeRetriever{set: passiveSet(globalCandidate("g1", 0.9))}
	res = run(t, kept, plain)
	if !containsNote(res.Notes, "no source policy drops losers") {
		t.Errorf("with no dropping policy the note must say so: %v", res.Notes)
	}
}

// TestRunPassiveMachineLineSaysNotAppliedEvenWithACosine: the passive arm is
// checked FIRST, so a retriever that reported a vector leg as `ok` — or a caller
// that set AbstainCosine — cannot make the line print a threshold that no cosine
// produced. Unreachable through *memory.Store today, which is exactly why it
// needs a test: the next retriever is not obliged to leave the leg statuses at
// their zero values.
func TestRunPassiveMachineLineSaysNotAppliedEvenWithACosine(t *testing.T) {
	set := passiveSet(projectCandidate("p1", 0.9))
	set.Legs["vector"] = memory.LegStatus{Applicable: true, Attempted: true, Available: true}
	f := &fakeRetriever{set: set}
	req := passiveRequest()
	req.AbstainCosine = 0.6
	res := run(t, f, req)
	if !strings.Contains(res.Machine, "abstain_cosine=not_applied") {
		t.Errorf("machine line: got %q, want abstain_cosine=not_applied — a passive block has no query to compare a cosine against", res.Machine)
	}
}

func containsNote(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// TestRunPassiveTheTraceAgreesWithTheResponseOnBothArms is the half of the floor
// contract the machine line already had and the trace did not.
//
// `fitResponse` sets VectorApplied from the leg status and the configured arm, so
// a retriever reporting the vector leg `ok` on a passive request left the trace
// claiming a cosine arm applied to a block the response says was never judged. The
// machine line is checked first precisely so it cannot be contradicted this way;
// this is the same property on the artifact the next PR projects into `explain`.
func TestRunPassiveTheTraceAgreesWithTheResponseOnBothArms(t *testing.T) {
	set := passiveSet(projectCandidate("p1", 0.9))
	set.Legs["fts"] = memory.LegStatus{Applicable: true, Attempted: true, Available: true}
	set.Legs["vector"] = memory.LegStatus{Applicable: true, Attempted: true, Available: true}
	f := &fakeRetriever{set: set}
	req := passiveRequest()
	req.AbstainCosine = 0.6 // the arm is configured
	res := run(t, f, req)

	if res.Trace.Floors.FTSApplied || res.Trace.Floors.VectorApplied {
		t.Errorf("both arms ran per the retriever, but a passive block has no query to compare against: FTSApplied=%v VectorApplied=%v "+
			"while the response says %q", res.Trace.Floors.FTSApplied, res.Trace.Floors.VectorApplied, res.Machine)
	}
	// The configured arm is still RECORDED, so a reader can tell "not applied"
	//	// from "not configured" — the same three-state distinction the machine line makes.
	if !res.Trace.Floors.VectorArmOn {
		t.Error("VectorArmOn: the arm was configured, so clearing its application must not erase the configuration")
	}
	if res.Trace.Floors.VectorCosine != 0.6 {
		t.Errorf("VectorCosine: got %v, want the configured 0.6 — the threshold is a fact about the request whatever the verdict", res.Trace.Floors.VectorCosine)
	}
}

// TestRunPassiveDedupNoteDoesNotClaimARemovalThatDidNotHappen keeps the stage-6
// note a statement about policy. The stage cannot know whether the retriever
// removed anything — it did that over a window whose edges this pipeline never
// saw — so a note claiming a removal for every `_global` slice that sets the flag
// would report a prediction, on the common occasion that the window held no
// near-duplicate edge at all.
//
// The control is the point: a retriever that DID drop a loser (fewer rows in than
// the window would admit, with the edge present) must not change the wording,
// because the note is not about the count.
func TestRunPassiveDedupNoteDoesNotClaimARemovalThatDidNotHappen(t *testing.T) {
	// Nothing was removed: the retriever returned everything, as a window with no
	// near-duplicate edge does.
	clean := &fakeRetriever{set: passiveSet(globalCandidate("g1", 0.9), globalCandidate("g2", 0.8))}
	note := passiveDedupNote(t, run(t, clean, passiveRequest()))
	if strings.Contains(note, "was REMOVED") || strings.Contains(note, "was removed") {
		t.Errorf("the note claims a removal on a window where none was evidenced: %q", note)
	}
	if !strings.Contains(note, "losers are REMOVED") {
		t.Errorf("the note must still state the policy; got %q", note)
	}

	// A retriever that DID drop a loser changes nothing, because the note is not a
	// count. If this ever wants to differ, it has to read the trace for it.
	dropped := &fakeRetriever{set: passiveSet(globalCandidate("g1", 0.9))}
	note = passiveDedupNote(t, run(t, dropped, passiveRequest()))
	if !strings.Contains(note, "losers are REMOVED") {
		t.Errorf("the note states the policy, not the outcome, so it is the same either way: %q", note)
	}
}

// TestRunPassiveDedupNoteDeniesTheDropWhenNoPolicyAsks: the other half — a
// request with no dropping policy must not be told a drop happened, and the two
// sentences have to be distinguishable so the first is not simply always printed.
func TestRunPassiveDedupNoteDeniesTheDropWhenNoPolicyAsks(t *testing.T) {
	plain := passiveRequest()
	plain.Budget.Slices[1].DropDemotedLosers = false
	f := &fakeRetriever{set: passiveSet(globalCandidate("g1", 0.9))}
	note := passiveDedupNote(t, run(t, f, plain))
	if !strings.Contains(note, "no source policy drops losers") {
		t.Errorf("with no dropping policy the note must say so: %q", note)
	}
}

// passiveDedupNote pulls the stage-6 sentence out of the notes, so the assertion
// is on THAT sentence rather than on a neighbour that happens to share a word.
func passiveDedupNote(t *testing.T, res Result) string {
	t.Helper()
	for _, n := range res.Notes {
		if strings.Contains(n, "near-duplicate reordering") {
			return n
		}
	}
	t.Fatalf("no near-duplicate note in %v", res.Notes)
	return ""
}

// TestRunPassiveRejectsASliceThatBoundsBytesButNotTheFetch: a MaxBytes-only
// slice bounds how large the ANSWER may be, which is not the same as bounding how
// much is READ — and the read is what must not be unbounded on a path that runs at
// every session start.
//
// Accepting it would also be incoherent downstream, which is the sharper form of
// the same point: `passivePolicies` falls back to MaxItems for the over-fetch, so
// such a slice would reach the store asking for a window of 0 and be refused
// THERE, with a message about the store's contract for a request this seam had
// already declared valid. The error a caller gets is the one that names the
// missing bound.
func TestRunPassiveRejectsASliceThatBoundsBytesButNotTheFetch(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(projectCandidate("p1", 0.9))}
	req := passiveRequest()
	req.Budget.Slices = []Slice{{Bucket: "proj", MaxBytes: 400, DemotionThreshold: 0.9}}
	_, err := Run(context.Background(), f, req)
	if err == nil {
		t.Fatal("a slice bounding only the answer's bytes must still be refused: it bounds nothing about the read")
	}
	if !strings.Contains(err.Error(), "OverFetch") {
		t.Errorf("the refusal must name the bound that is missing; got %v", err)
	}
	if f.sets != 0 {
		t.Errorf("the retriever was called %d times for a request this seam refused", f.sets)
	}
}

// TestAPassiveAbstentionPromisesNoNoteItCannotSend is a review should-fix, and the
// sentence is the defect rather than the pipeline.
//
// `Result.Abstention` is bytes a CALLER renders, and the callers do not agree on
// what comes with it. The search surface writes `assemblerNotes(res.Notes)` after
// the abstention, so "The note below breaks the removals down per stage" is
// there true. The passive block does not: `ghost_project_context`, the
// `ghost://project/{id}/context` resource and the `recall_project` prompt return
// `res.Abstention` as the ENTIRE answer, and render `Result.Notes` never. A
// passive block whose rows were all withheld as out of date shipped:
//
//	…withheld as out of date, their validity windows having closed or not yet
//	opened. The note below breaks the removals down per stage. The block was not
//	empty before that — the answer is withheld, not absent. Call
//	ghost_memories_list to see them, still marked with the window they carry.
//
// which points an agent at a breakdown that is not in the payload. The promise
// cannot be kept by the sentence, because the assembler does not own the
// rendering, so it is dropped from the PASSIVE half only — the search half keeps
// it, where the renderer honours it, and the control below proves that rather
// than asserting it.
//
// The abstention is still the same sentence otherwise: it keeps the shared
// exclusion wording pinned by the test above, and it keeps the clause that
// distinguishes "found and withheld" from "not there", which is the part a
// caller needs.
func TestAPassiveAbstentionPromisesNoNoteItCannotSend(t *testing.T) {
	expired := projectCandidate("p_exp", 0.9)
	expired.ValidUntil = stampPtr("2026-01-01 00:00:00")
	res := run(t, &fakeRetriever{set: passiveSet(expired)}, passiveRequest())
	if res.Outcome != OutcomeEmpty || res.Reason != reasonAllInvalid {
		t.Fatalf("fixture: got outcome %q reason %q, want empty/all_invalid", res.Outcome, res.Reason)
	}
	if !strings.Contains(strings.ToLower(res.Abstention), "no sufficiently trustworthy memory found") {
		t.Fatalf("fixture: the abstention must be the all_invalid sentence under test: %q", res.Abstention)
	}
	lower := strings.ToLower(res.Abstention)
	for _, promise := range []string{"note below", "breaks the removals down", "per stage"} {
		if strings.Contains(lower, promise) {
			t.Errorf("the passive abstention promises %q, and a caller that renders the abstention alone "+
				"(ghost_project_context, the project-context resource, recall_project) sends no note:\n%s",
				promise, res.Abstention)
		}
	}
	// The half that makes the abstention useful has to survive the removal: this is
	// an EXCLUSION, and a reader who is not told the rows were found cannot tell it
	// from a window that was empty.
	if !strings.Contains(lower, "withheld as out of date") {
		t.Errorf("the passive abstention lost the clause saying the rows were found and withheld:\n%s", res.Abstention)
	}

	// The control: the SEARCH sentence keeps the promise, and there it is true —
	// the response really does carry the breakdown it points at.
	row := projectCandidate("p_exp", 0.9)
	row.ValidUntil = stampPtr("2020-01-01 00:00:00")
	sres := run(t, &fakeRetriever{set: ftsOnlySet(row)}, baseRequest())
	if sres.Reason != reasonAllInvalid {
		t.Fatalf("control fixture: got reason %q, want %q", sres.Reason, reasonAllInvalid)
	}
	if !strings.Contains(sres.Response, "The note below") {
		t.Errorf("control: the search response lost its promise, which is the half that is true:\n%s", sres.Response)
	}
	if !strings.Contains(sres.Response, "(Note:") {
		t.Errorf("control: the search response promises a breakdown and then does not send one:\n%s", sres.Response)
	}
}
