package assemble

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/memory"
)

// fakeRetriever serves a fixed candidate set and records the request it was
// handed, so a test can assert both what the assembler selected and what it
// asked the store for.
type fakeRetriever struct {
	set  *memory.CandidateSet
	req  memory.CandidateRequest
	err  error
	sets int
}

func (f *fakeRetriever) Candidates(_ context.Context, req memory.CandidateRequest) (*memory.CandidateSet, error) {
	f.req = req
	f.sets++
	if f.err != nil {
		return nil, f.err
	}
	return f.set, nil
}

func candidate(id, projectID, category, content string, score float64) memory.Candidate {
	return memory.Candidate{
		Memory: memory.Memory{
			ID: id, ProjectID: projectID, Category: category, Content: content,
			CreatedAt: "2026-09-01 12:00:00", Importance: 0.7,
		},
		Base: score, Decay: 1.0, Score: score,
	}
}

// scopedCandidate is a candidate whose memory states a scope, which is what
// makes it contradict a request for a different value.
func scopedCandidate(id string, scope map[string]string, score float64) memory.Candidate {
	c := candidate(id, "proj", "fact", "database configuration "+id, score)
	c.Scope = scope
	return c
}

func setOf(rows ...memory.Candidate) *memory.CandidateSet {
	return &memory.CandidateSet{Rows: rows}
}

func baseRequest() Request {
	return Request{
		ProjectID: "proj",
		Query:     "database configuration",
		Source:    SourceSearch,
		Budget:    Budget{MaxItems: 2},
		Condition: CondFTSOnly,
		Now:       time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC),
	}
}

func run(t *testing.T, r Retriever, req Request) Result {
	t.Helper()
	res, err := Run(context.Background(), r, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func itemIDs(items []Item) []string {
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}

func eq(a, b []string) bool {
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

// TestCategoryFiltersBeforeWindowClosure is issue #573's remaining half. The
// category post-filter ran after the result window was closed, so a matching
// row ranked below the window was never seen and the tool reported absence
// while the memory existed. The filter has to run over the widened candidate
// set, before stage 8 closes the window.
func TestCategoryFiltersBeforeWindowClosure(t *testing.T) {
	// Ten rows outrank the one that matches the filter; the caller's window is
	// two, so the eleventh row can only be reached by filtering before closure.
	rows := []memory.Candidate{
		candidate("A1", "proj", "gotcha", "database configuration pooling", 0.9),
		candidate("A2", "proj", "gotcha", "database configuration retry", 0.8),
		candidate("A3", "proj", "gotcha", "database configuration cache", 0.7),
		candidate("B1", "proj", "gotcha", "database configuration vacuum", 0.6),
		candidate("B2", "proj", "gotcha", "database configuration index", 0.5),
		candidate("B3", "proj", "fact", "database configuration collation", 0.4),
	}
	r := &fakeRetriever{set: setOf(rows...)}
	req := baseRequest()
	req.Category = "fact"

	res := run(t, r, req)

	if !eq(itemIDs(res.Items), []string{"B3"}) {
		t.Errorf("category must be applied before the window closes, so the only "+
			"matching row is admitted from beyond it; got %v", itemIDs(res.Items))
	}
	if res.Outcome != OutcomeAnswerable {
		t.Errorf("outcome = %q, want answerable: a matching row exists", res.Outcome)
	}
}

// TestScopeFiltersBeforeWindowClosure: the same closure rule for scope, which
// the fusion seam already narrows before the cut. The assembler owns the
// verdict and records it; the row must still be admitted from the widened set.
func TestScopeFiltersBeforeWindowClosure(t *testing.T) {
	rows := []memory.Candidate{
		scopedCandidate("dev1", map[string]string{"environment": "development"}, 0.9),
		scopedCandidate("dev2", map[string]string{"environment": "development"}, 0.8),
		scopedCandidate("prod1", map[string]string{"environment": "production"}, 0.7),
		// Silence is not disagreement: a row that names no environment applies
		// everywhere and must survive a production request.
		candidate("general", "proj", "fact", "database configuration in general", 0.6),
	}
	r := &fakeRetriever{set: setOf(rows...)}
	req := baseRequest()
	req.Scope = map[string]string{"environment": "production"}

	res := run(t, r, req)

	if !eq(itemIDs(res.Items), []string{"prod1", "general"}) {
		t.Errorf("scope must exclude the contradicting rows, admit the matching one, and keep an "+
			"unscoped row; got %v", itemIDs(res.Items))
	}
}

// TestNoPredicateKeepsRetrieverOrderAndClosesWindow: with no predicate the
// assembler must not reorder anything — the retriever's order already carries
// the keyword reservation, status demotion, decay and both demotions — and
// stage 8 must close the window at the request's limit.
func TestNoPredicateKeepsRetrieverOrderAndClosesWindow(t *testing.T) {
	r := &fakeRetriever{set: setOf(
		candidate("A1", "proj", "fact", "one", 0.9),
		candidate("A2", "proj", "fact", "two", 0.8),
		candidate("A3", "proj", "fact", "three", 0.7),
	)}
	res := run(t, r, baseRequest())

	if !eq(itemIDs(res.Items), []string{"A1", "A2"}) {
		t.Errorf("items = %v, want the first two candidates in the retriever's order", itemIDs(res.Items))
	}
}

// TestWideningForCategoryPredicateIsVisibleInTheRequest records the retrieval
// policy the old tool applied by hand: a category post-filter needs room
// beyond the requested window, so the retriever is asked for a wider one while
// the closure stays at the caller's limit.
func TestWideningForCategoryPredicateIsVisibleInTheRequest(t *testing.T) {
	r := &fakeRetriever{set: setOf(candidate("A1", "proj", "fact", "one", 0.9))}
	req := baseRequest()
	req.Budget = Budget{MaxItems: 10}
	req.Category = "gotcha"

	run(t, r, req)

	if r.req.Fetch.Limit != 30 {
		t.Errorf("retrieval window = %d, want 30 (the request's limit widened 3x for a category predicate)", r.req.Fetch.Limit)
	}
	if r.req.Fetch.FTSTopK != 60 || r.req.Fetch.VectorTopK != 60 {
		t.Errorf("leg depth = fts %d / vector %d, want 60 each (window * 2, as every production search fetches)", r.req.Fetch.FTSTopK, r.req.Fetch.VectorTopK)
	}
}

// TestNoCategoryPredicateKeepsRequestedWindow: without a category the window
// is the caller's limit, so a plain search retrieves and returns exactly what
// it always did.
func TestNoCategoryPredicateKeepsRequestedWindow(t *testing.T) {
	r := &fakeRetriever{set: setOf(candidate("A1", "proj", "fact", "one", 0.9))}
	req := baseRequest()
	req.Budget = Budget{MaxItems: 10}

	run(t, r, req)

	if r.req.Fetch.Limit != 10 || r.req.Fetch.FTSTopK != 20 {
		t.Errorf("retrieval window = %d (legs %d), want 10 (legs 20)", r.req.Fetch.Limit, r.req.Fetch.FTSTopK)
	}
}

// TestWideningIsBoundedButTheBudgetIsNot: the category widening is capped so a
// large limit cannot buy a disproportionately deep fetch, and the cap applies to
// the widening alone. A caller whose own budget exceeds the cap still gets a
// window that can fill that budget — the next consumer of this seam (session
// start) will ask for more than 100, and a window smaller than its budget would
// silently under-fill every block.
func TestWideningIsBoundedButTheBudgetIsNot(t *testing.T) {
	tests := []struct {
		name     string
		budget   int
		category string
		want     int
	}{
		{"category widens within the cap", 10, "gotcha", 30},
		{"category widening is capped", 100, "gotcha", 100},
		{"a budget above the cap is not narrowed", 250, "", 250},
		{"a budget above the cap is not narrowed by a category either", 250, "gotcha", 250},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRetriever{set: setOf(candidate("A1", "proj", "fact", "one", 0.9))}
			req := baseRequest()
			req.Budget = Budget{MaxItems: tc.budget}
			req.Category = tc.category

			run(t, r, req)

			if r.req.Fetch.Limit != tc.want {
				t.Errorf("retrieval window = %d, want %d", r.req.Fetch.Limit, tc.want)
			}
			if r.req.Fetch.Limit < req.Budget.MaxItems {
				t.Errorf("retrieval window %d is smaller than the budget %d: the closure could never be filled",
					r.req.Fetch.Limit, req.Budget.MaxItems)
			}
		})
	}
}

func TestRunRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Request)
		want string
	}{
		{"all-zero budget", func(r *Request) { r.Budget = Budget{} }, "budget"},
		{"unknown source", func(r *Request) { r.Source = "nonsense" }, "source"},
		{"vector only without a vector", func(r *Request) { r.Condition = CondVectorOnly }, "vector"},
		{"zero clock", func(r *Request) { r.Now = time.Time{} }, "Now"},
		{"project context without a project", func(r *Request) { r.Source = SourceProjectCtx; r.Query = ""; r.ProjectID = "" }, "project"},
		{"unknown condition", func(r *Request) { r.Condition = "magic" }, "condition"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := baseRequest()
			tc.mut(&req)
			_, err := Run(context.Background(), &fakeRetriever{set: setOf()}, req)
			if err == nil {
				t.Fatalf("Run accepted an invalid request (%s)", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestRunRejectsNegativeBudget is the "all-zero budget is rejected" rule's
// other half: a negative cap is not a way to ask for no rows.
func TestRunRejectsNegativeBudget(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = -1
	if _, err := Run(context.Background(), &fakeRetriever{set: setOf()}, req); err == nil {
		t.Fatal("Run accepted a negative item budget")
	}
}

// TestRunPassesRequestThroughUnchanged pins the mapping from Request to
// CandidateRequest: the clock is the caller's, the condition picks the legs,
// and the caller's unresolved params reach the store, which is what applies the
// configured vector floor.
func TestRunPassesRequestThroughUnchanged(t *testing.T) {
	r := &fakeRetriever{set: setOf()}
	req := baseRequest()
	req.Condition = CondHybrid
	req.QueryVec = []float32{0.1, 0.2}
	req.Scope = map[string]string{"environment": "staging"}
	params := &memory.SearchParams{FTSWeight: 0.5, VecWeight: 0.5, RRFK: 30}
	req.Params = params
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	req.Now = now

	run(t, r, req)

	if !r.req.Now.Equal(now) {
		t.Errorf("Now = %v, want the caller's %v: the candidate path must not read the wall clock", r.req.Now, now)
	}
	if r.req.Condition != memory.CondHybrid {
		t.Errorf("condition = %q, want hybrid", r.req.Condition)
	}
	if len(r.req.QueryVec) != 2 || r.req.Params.RRFK != 30 || r.req.Params.FTSWeight != 0.5 {
		t.Errorf("candidate request lost the caller's vector or params: %+v", r.req)
	}
	if r.req.Mode != memory.ProjectScoped {
		t.Errorf("mode = %q, want scoped for a resolved project", r.req.Mode)
	}
	if len(r.req.Scope) != 1 || r.req.Scope["environment"] != "staging" {
		t.Errorf("scope = %v, want the caller's scope", r.req.Scope)
	}
}

// TestUnresolvedSearchIsGlobalOnly: a search whose project name did not resolve
// keeps today's behaviour — global memories are still reachable — rather than
// becoming an error or a cross-project search.
func TestUnresolvedSearchIsGlobalOnly(t *testing.T) {
	r := &fakeRetriever{set: setOf()}
	req := baseRequest()
	req.ProjectID = ""

	run(t, r, req)

	if r.req.Mode != memory.GlobalOnly {
		t.Errorf("mode = %q, want global_only for an unresolved project", r.req.Mode)
	}
}

func TestAllProjectsSourcesUseAllProjects(t *testing.T) {
	for _, src := range []Source{SourceAllProjects, SourceBench} {
		r := &fakeRetriever{set: setOf()}
		req := baseRequest()
		req.Source = src
		req.ProjectID = ""

		run(t, r, req)

		if r.req.Mode != memory.AllProjects {
			t.Errorf("source %q with an empty project: mode = %q, want all_projects", src, r.req.Mode)
		}
	}
}

// TestNilQueryVectorSkipsTheVectorLeg: a nil vector is legal and means the
// vector leg is not attempted, which is not the same as asking for vector-only
// retrieval (an error).
func TestNilQueryVectorSkipsTheVectorLeg(t *testing.T) {
	r := &fakeRetriever{set: setOf(candidate("A1", "proj", "fact", "one", 0.9))}
	req := baseRequest()
	req.Condition = CondHybrid
	req.QueryVec = nil

	res := run(t, r, req)

	if len(r.req.QueryVec) != 0 {
		t.Errorf("candidate request carried a query vector the caller did not supply: %v", r.req.QueryVec)
	}
	if len(res.Items) != 1 {
		t.Errorf("items = %v, want the FTS-only result", itemIDs(res.Items))
	}
}

// TestRetrieverErrorIsReturned: a retrieval failure is an error, never an empty
// outcome that reads as "nothing exists".
func TestRetrieverErrorIsReturned(t *testing.T) {
	boom := errors.New("leg exploded")
	_, err := Run(context.Background(), &fakeRetriever{err: boom}, baseRequest())
	if !errors.Is(err, boom) {
		t.Fatalf("Run error = %v, want the retriever's error", err)
	}
}

// TestEmptyResultCarriesAReason: an empty outcome names why, from the closed
// reason set, so a caller can tell an empty store from a filtered one.
func TestEmptyResultCarriesAReason(t *testing.T) {
	r := &fakeRetriever{set: setOf()}
	res := run(t, r, baseRequest())

	if res.Outcome != OutcomeEmpty {
		t.Errorf("outcome = %q, want empty", res.Outcome)
	}
	if res.Reason != "no_candidates" {
		t.Errorf("reason = %q, want no_candidates", res.Reason)
	}
}

func TestFilteredToEmptyResultNamesTheFilter(t *testing.T) {
	tests := []struct {
		name  string
		mut   func(*Request)
		build func() *memory.CandidateSet
		want  string
	}{
		{
			name: "category", want: "all_out_of_category",
			mut: func(r *Request) { r.Category = "gotcha" },
			build: func() *memory.CandidateSet {
				return setOf(candidate("A1", "proj", "fact", "one", 0.9))
			},
		},
		{
			name: "scope", want: "all_out_of_scope",
			mut: func(r *Request) { r.Scope = map[string]string{"environment": "production"} },
			build: func() *memory.CandidateSet {
				rows := []memory.Candidate{{
					Memory: memory.Memory{
						ID: "dev", ProjectID: "proj", Category: "fact", Content: "one",
						CreatedAt: "2026-09-01 12:00:00", Scope: map[string]string{"environment": "development"},
					},
					Base: 0.9, Decay: 1.0, Score: 0.9,
				}}
				return setOf(rows...)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := baseRequest()
			tc.mut(&req)
			res := run(t, &fakeRetriever{set: tc.build()}, req)
			if res.Outcome != OutcomeEmpty || res.Reason != tc.want {
				t.Errorf("outcome/reason = %q/%q, want empty/%s", res.Outcome, res.Reason, tc.want)
			}
		})
	}
}

// TestValidityDropsRowsOutsideTheClock: stage 2 drops a row whose window has
// closed or has not opened, and records why.
func TestValidityDropsRowsOutsideTheClock(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	expired, starts, openStart, openEnd, verified := "2026-01-01 00:00:00", "2026-12-01 00:00:00",
		"2026-09-01 00:00:00", "2026-12-31", "2026-09-20 08:00:00"
	rows := []memory.Candidate{
		candidate("old", "proj", "fact", "expired row", 0.9),
		candidate("new", "proj", "fact", "future row", 0.8),
		candidate("live", "proj", "fact", "open row", 0.7),
	}
	rows[0].ValidUntil = &expired
	rows[1].ValidFrom = &starts
	rows[2].ValidFrom = &openStart
	rows[2].ValidUntil = &openEnd
	rows[2].VerifiedAt = &verified

	req := baseRequest()
	req.Budget.MaxItems = 10
	req.Now = now

	res := run(t, &fakeRetriever{set: setOf(rows...)}, req)

	if !eq(itemIDs(res.Items), []string{"live"}) {
		t.Errorf("items = %v, want only the row inside its validity window", itemIDs(res.Items))
	}
	if res.Items[0].ValidityState != "valid" {
		t.Errorf("validity state = %q, want valid", res.Items[0].ValidityState)
	}
}

// TestUnparseableValidityIsUnsetAndReported: SQLite stores these as
// unconstrained text, so a value Ghost cannot read is not a claim. It must not
// silently read as valid, and it must be reported.
func TestUnparseableValidityIsUnsetAndReported(t *testing.T) {
	garbage := "sometime last spring"
	rows := []memory.Candidate{candidate("odd", "proj", "fact", "one", 0.9)}
	rows[0].ValidUntil = &garbage
	req := baseRequest()
	req.Budget.MaxItems = 10

	res := run(t, &fakeRetriever{set: setOf(rows...)}, req)

	if len(res.Items) != 1 {
		t.Fatalf("an unreadable validity value must not drop the row: %v", itemIDs(res.Items))
	}
	if res.Items[0].ValidityState != "unset" || res.Items[0].ValidUntil != nil {
		t.Errorf("validity state = %q / until = %v, want unset / nil", res.Items[0].ValidityState, res.Items[0].ValidUntil)
	}
	if !hasNote(res.Notes, "validity_unparseable") {
		t.Errorf("notes %v do not report the unreadable value", res.Notes)
	}
}

// TestProvenanceMultiplierIsInert: stage 4 ships pinned at 1.0, so a seeded
// confidence value must not change the order.
func TestProvenanceMultiplierIsInert(t *testing.T) {
	low, high := 0.1, 0.95
	rows := []memory.Candidate{
		candidate("A1", "proj", "fact", "first", 0.9),
		candidate("A2", "proj", "fact", "second", 0.8),
	}
	rows[0].Confidence = &low
	rows[1].Confidence = &high
	req := baseRequest()
	req.Budget.MaxItems = 10

	res := run(t, &fakeRetriever{set: setOf(rows...)}, req)

	if !eq(itemIDs(res.Items), []string{"A1", "A2"}) {
		t.Errorf("items = %v, want the retriever's order unchanged while the multiplier is pinned at 1.0", itemIDs(res.Items))
	}
	for id, sig := range res.Trace.Signals {
		if sig.ProvenanceWeight != "1.0" || sig.ConfidenceContribution != 0 || sig.ProvenanceContribution != 0 {
			t.Errorf("row %s: provenance weight %q with contributions %v/%v, want 1.0 and zero while inert",
				id, sig.ProvenanceWeight, sig.ConfidenceContribution, sig.ProvenanceContribution)
		}
	}
}

// TestTraceRecordsEveryStage: the trace is recorded unconditionally and names
// the stages the request ran, with the rows each one saw and dropped.
func TestTraceRecordsEveryStage(t *testing.T) {
	rows := []memory.Candidate{
		candidate("A1", "proj", "gotcha", "pooling", 0.9),
		candidate("A2", "proj", "gotcha", "retry", 0.8),
		candidate("B1", "proj", "fact", "vacuum", 0.7),
	}
	req := baseRequest()
	req.Category = "fact"

	res := run(t, &fakeRetriever{set: setOf(rows...)}, req)

	want := []string{"validity", "predicates", "provenance", "conflicts", "dedup", "diversity", "budget", "render"}
	got := make([]string, 0, len(res.Trace.Stages))
	for _, st := range res.Trace.Stages {
		got = append(got, st.Stage)
	}
	if !eq(got, want) {
		t.Fatalf("stages = %v, want %v", got, want)
	}
	predicates := res.Trace.Stages[1]
	if predicates.In != 3 || predicates.Out != 1 {
		t.Errorf("predicates stage in/out = %d/%d, want 3/1", predicates.In, predicates.Out)
	}
	if !eq(predicates.DroppedIDs, []string{"A1", "A2"}) {
		t.Errorf("dropped ids = %v, want the two non-matching rows", predicates.DroppedIDs)
	}
	if len(res.Trace.Decisions) != 2 {
		t.Errorf("decisions = %d, want one per excluded row", len(res.Trace.Decisions))
	}
	for _, d := range res.Trace.Decisions {
		if d.Stage != "predicates" || d.Kept || d.Reason != "category_mismatch" || d.ID == "" {
			t.Errorf("decision %+v does not explain the exclusion", d)
		}
	}
}

// TestTraceCarriesCandidateFacts: the trace copies the retriever's scoring
// facts instead of re-deriving them, which is what keeps an explanation from
// drifting from the ranking it explains.
func TestTraceCarriesCandidateFacts(t *testing.T) {
	rows := []memory.Candidate{candidate("A1", "proj", "fact", "one", 0.9)}
	rows[0].FTSRank = 2
	rows[0].VectorRank = -1
	rows[0].VectorScore = 0.42
	rows[0].Base = 0.9
	rows[0].Decay = 0.75
	rows[0].Score = 0.675
	rows[0].AgeDays = 3.5
	req := baseRequest()
	req.Budget.MaxItems = 10

	res := run(t, &fakeRetriever{set: setOf(rows...)}, req)

	sig, ok := res.Trace.Signals["A1"]
	if !ok {
		t.Fatalf("trace has no signals for the admitted row: %+v", res.Trace.Signals)
	}
	if sig.FTSRank != 2 || sig.VectorRank != -1 || sig.VectorScore != 0.42 {
		t.Errorf("ranks = %d/%d at %v, want 2/-1 at 0.42", sig.FTSRank, sig.VectorRank, sig.VectorScore)
	}
	if sig.Base != 0.9 || sig.DecayFactor != 0.75 || sig.AgeDays != 3.5 {
		t.Errorf("scoring facts = base %v decay %v age %v, want 0.9/0.75/3.5", sig.Base, sig.DecayFactor, sig.AgeDays)
	}
	if !sig.ProjectMatch {
		t.Error("project verdict not recorded for a row the retriever returned for this project")
	}
}

// TestBudgetTrimIsRecordedAgainstTheSlice: stage 8 is the window closure, and
// the trace has to say which rows it cut.
func TestBudgetTrimIsRecordedAgainstTheSlice(t *testing.T) {
	r := &fakeRetriever{set: setOf(
		candidate("A1", "proj", "fact", "one", 0.9),
		candidate("A2", "proj", "fact", "two", 0.8),
		candidate("A3", "proj", "fact", "three", 0.7),
	)}
	res := run(t, r, baseRequest())

	var budget *StageTrace
	for i, st := range res.Trace.Stages {
		if st.Stage == "budget" {
			budget = &res.Trace.Stages[i]
		}
	}
	if budget == nil {
		t.Fatal("no budget stage in the trace")
	}
	if budget.In != 3 || budget.Out != 2 || !eq(budget.DroppedIDs, []string{"A3"}) {
		t.Errorf("budget stage = in %d out %d dropped %v, want 3/2/[A3]", budget.In, budget.Out, budget.DroppedIDs)
	}
}

// TestSliceBudgetsMembershipPerBucket: a slice cap is a per-bucket membership
// budget, and a row's bucket is the project it belongs to.
func TestSliceBudgetsMembershipPerBucket(t *testing.T) {
	rows := []memory.Candidate{
		candidate("p1", "proj", "fact", "one", 0.9),
		candidate("p2", "proj", "fact", "two", 0.8),
		candidate("g1", "_global", "fact", "three", 0.7),
		candidate("g2", "_global", "fact", "four", 0.6),
	}
	req := baseRequest()
	req.Budget = Budget{Slices: []Slice{{Bucket: "proj", MaxItems: 1}, {Bucket: "_global", MaxItems: 1}}}

	res := run(t, &fakeRetriever{set: setOf(rows...)}, req)

	if !eq(itemIDs(res.Items), []string{"p1", "g1"}) {
		t.Errorf("items = %v, want one row per bucket", itemIDs(res.Items))
	}
}

// TestSliceClampPreservesUTF8Boundaries: a presentation clamp cuts on a rune
// boundary, so a multi-byte memory is never left half a character.
func TestSliceClampPreservesUTF8Boundaries(t *testing.T) {
	rows := []memory.Candidate{candidate("A1", "proj", "fact", "héllo wörld — ok", 0.9)}
	req := baseRequest()
	// Two bytes lands inside "é" (h is one byte, é is two), so a naive byte
	// cut would emit half a character.
	req.Budget = Budget{MaxItems: 1, Slices: []Slice{{Bucket: "proj", ClampBytes: 2}}}

	res := run(t, &fakeRetriever{set: setOf(rows...)}, req)

	got := res.Items[0].Content
	if !strings.HasPrefix("héllo wörld — ok", got) {
		t.Fatalf("clamped content %q is not a prefix of the original", got)
	}
	if got != "h" {
		t.Errorf("clamped content = %q, want %q (2 bytes on a rune boundary)", got, "h")
	}
	if !utf8.ValidString(got) {
		t.Errorf("clamped content %q is not valid UTF-8", got)
	}
	if res.Items[0].Bytes != len(got) {
		t.Errorf("item bytes = %d, want the clamped content's %d", res.Items[0].Bytes, len(got))
	}
}

// TestItemCarriesTheParsedOutputFields: the shared item is what both surfaces
// render, so the four timestamps and the scope must be parsed output values
// rather than the stored strings.
func TestItemCarriesTheParsedOutputFields(t *testing.T) {
	resolved := "2026-09-20 08:00:00"
	validFrom := "2026-09-01"
	validUntil := "2026-10-01"
	verified := "2026-09-25 09:30:00"
	row := candidate("A1", "proj", "fact", "one", 0.9)
	row.ResolvedAt = &resolved
	row.ValidFrom = &validFrom
	row.ValidUntil = &validUntil
	row.VerifiedAt = &verified
	row.Agent = "opencode"
	row.Scope = map[string]string{"environment": "production"}
	row.Tags = []string{"db"}
	row.Pinned = true
	req := baseRequest()
	req.Budget.MaxItems = 10

	res := run(t, &fakeRetriever{set: setOf(row)}, req)

	it := res.Items[0]
	if it.ResolvedAt == nil || !it.ResolvedAt.Equal(time.Date(2026, 9, 20, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("resolved_at = %v, want the parsed timestamp", it.ResolvedAt)
	}
	if it.ValidFrom == nil || it.ValidFrom.Day() != 1 || it.ValidUntil == nil || it.ValidUntil.Month() != time.October {
		t.Errorf("validity timestamps = %v / %v, want both parsed (date-only layout included)", it.ValidFrom, it.ValidUntil)
	}
	if it.VerifiedAt == nil || it.VerifiedAt.Hour() != 9 {
		t.Errorf("verified_at = %v, want the parsed timestamp", it.VerifiedAt)
	}
	if it.Bucket != "proj" || it.Agent != "opencode" || len(it.Scope) != 1 || !it.Pinned || len(it.Tags) != 1 {
		t.Errorf("item lost a carried field: %+v", it)
	}
	if it.Score != 0.9 {
		t.Errorf("score = %v, want the retriever's 0.9", it.Score)
	}
}

// TestRowWithoutAValidityClaimKeepsItsPlace: a row that states no validity is
// unset, not expired and not unverified — "unset" is the honest reading of a
// column nobody wrote, and it must not cost the row its place.
func TestRowWithoutAValidityClaimKeepsItsPlace(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 10
	res := run(t, &fakeRetriever{set: setOf(candidate("A1", "proj", "fact", "one", 0.9))}, req)

	if len(res.Items) != 1 || res.Items[0].ValidityState != "unset" {
		t.Errorf("items = %+v, want the row kept and labelled unset", res.Items)
	}
}

// TestPassiveModeIsNotImplementedYet: an empty query selects passive retrieval,
// which arrives with the session-start migration. It must fail as an error
// rather than return an empty result that reads as an empty store.
func TestPassiveModeIsNotImplementedYet(t *testing.T) {
	r := &fakeRetriever{set: setOf()}
	req := baseRequest()
	req.Query = ""
	req.Source = SourceSessionStart

	_, err := Run(context.Background(), r, req)
	if err == nil {
		t.Fatal("Run accepted a passive request it cannot serve")
	}
	if r.sets != 0 {
		t.Error("Run called the retriever for a request it should have rejected first")
	}
}

// TestNotesAreBounded: diagnostic notes are bounded so a pathological row
// count cannot inflate a response.
func TestNotesAreBounded(t *testing.T) {
	rows := make([]memory.Candidate, 0, 40)
	for i := range 40 {
		garbage := "not a timestamp"
		row := candidate(string(rune('A'+i%26))+string(rune('a'+i/26)), "proj", "fact", "one", 0.5)
		row.ValidFrom = &garbage
		rows = append(rows, row)
	}
	req := baseRequest()
	req.Budget.MaxItems = 10

	res := run(t, &fakeRetriever{set: setOf(rows...)}, req)

	if len(res.Notes) == 0 {
		t.Fatal("no notes recorded for 40 unreadable validity values")
	}
	if total := joinedLen(res.Notes); total > defaultMaxNotesBytes {
		t.Errorf("notes total %d bytes, want at most %d", total, defaultMaxNotesBytes)
	}
	for _, n := range res.Notes {
		if len(n) > defaultMaxNoteBytes {
			t.Errorf("note %q is %d bytes, want at most %d", n, len(n), defaultMaxNoteBytes)
		}
	}
}

func hasNote(notes []string, want string) bool {
	for _, n := range notes {
		if strings.Contains(n, want) {
			return true
		}
	}
	return false
}

func joinedLen(notes []string) int {
	total := 0
	for _, n := range notes {
		total += len(n) + 1
	}
	return total
}

// TestPartialLegFailureReportsRetrievalFailed: one applicable leg errored and
// the other completed with no rows. That is a partial retrieval, so the result
// is empty with the reason `retrieval_failed` — not `no_candidates`, which
// claims nothing matched over complete coverage, and not a success.
func TestPartialLegFailureReportsRetrievalFailed(t *testing.T) {
	set := setOf()
	set.Legs = map[string]memory.LegStatus{
		"fts":    {Applicable: true, Attempted: true, Available: false, Err: "no such table: memories_fts"},
		"vector": {Applicable: true, Attempted: true, Available: true},
	}
	res := run(t, &fakeRetriever{set: set}, baseRequest())

	if res.Outcome != OutcomeEmpty {
		t.Errorf("outcome = %q, want empty", res.Outcome)
	}
	if res.Reason != "retrieval_failed" {
		t.Errorf("reason = %q, want retrieval_failed: an errored leg means the search was incomplete", res.Reason)
	}
	if !hasNote(res.Notes, "memories_fts") {
		t.Errorf("notes %v do not name the leg that failed", res.Notes)
	}
}

// TestTotalLegFailureIsAnErrorNotAnOutcome: when every applicable leg errored
// there is no retrieval at all, and the retriever returns that as an error. Run
// must pass it through rather than rendering an empty block, which is the
// difference between "nothing matched" and "nothing was searched".
func TestTotalLegFailureIsAnErrorNotAnOutcome(t *testing.T) {
	boom := errors.New("every applicable retrieval leg failed")
	if _, err := Run(context.Background(), &fakeRetriever{err: boom}, baseRequest()); !errors.Is(err, boom) {
		t.Fatalf("Run error = %v, want the retriever's error", err)
	}
}

// TestResolvedMarkerFollowsTheColumnNotItsParsing: the [resolved] marker is a
// statement about the row — this memory was resolved — so it must depend on
// resolved_at being set, not on its text being readable. SQLite wrote that
// column, so a value this build cannot parse still means a resolved row, and
// dropping the marker would tell a reader the opposite of what the store says.
func TestResolvedMarkerFollowsTheColumnNotItsParsing(t *testing.T) {
	unreadable := "whenever the reviewer got to it"
	rows := []memory.Candidate{candidate("A1", "proj", "fact", "one", 0.9)}
	rows[0].ResolvedAt = &unreadable
	req := baseRequest()
	req.Budget.MaxItems = 10

	res := run(t, &fakeRetriever{set: setOf(rows...)}, req)

	if !strings.Contains(res.Items[0].Line(), "[resolved]") {
		t.Errorf("line = %q, want the resolved marker: resolved_at is set, whatever its text says", res.Items[0].Line())
	}
	if res.Items[0].ResolvedAt == nil {
		t.Error("ResolvedAt is nil, so every consumer that reads the column to decide whether a row is resolved would call it live")
	}
}

// TestResolvedMarkerIsAbsentWhenTheColumnIsNull: the other half. A row that was
// never resolved must not be marked, and nil is the only reading that says so.
func TestResolvedMarkerIsAbsentWhenTheColumnIsNull(t *testing.T) {
	res := run(t, &fakeRetriever{set: setOf(candidate("A1", "proj", "fact", "one", 0.9))}, baseRequest())
	if strings.Contains(res.Items[0].Line(), "[resolved]") {
		t.Errorf("line = %q, want no marker for a row with no resolved_at", res.Items[0].Line())
	}
}

// TestScopeContradictsMatchesTheStoreRule pins ScopeContradicts to the rule the
// rest of the system already applies. It exists here (Decision 5) so the
// assembler's stage 3 and the context metric's contamination arm share one leaf
// rather than each carrying a copy that can drift — which makes "identical" a
// claim that has to be checked, not one that follows from naming. Every case
// below is a disagreement or a silence, and a divergence on any of them would
// mean a memory the linker considers in scope is excluded from a block, or a
// contradicting one is measured as clean.
func TestScopeContradictsMatchesTheStoreRule(t *testing.T) {
	cases := []struct {
		name         string
		have, want   map[string]string
		contradicted bool
	}{
		{"both unscoped", nil, nil, false},
		{"unscoped row, scoped request", nil, map[string]string{"environment": "production"}, false},
		{"scoped row, unscoped request", map[string]string{"environment": "production"}, nil, false},
		{"same value", map[string]string{"environment": "production"}, map[string]string{"environment": "production"}, false},
		{"different value", map[string]string{"environment": "development"}, map[string]string{"environment": "production"}, true},
		{"row silent on the requested key", map[string]string{"component": "api"}, map[string]string{"environment": "production"}, false},
		{"row names another key with a different value", map[string]string{"component": "worker"}, map[string]string{"environment": "production"}, false},
		{"one shared key agrees, another disagrees", map[string]string{"environment": "production", "component": "worker"}, map[string]string{"environment": "production", "component": "api"}, true},
		{"one shared key agrees, another unmentioned by the request", map[string]string{"environment": "production", "component": "api"}, map[string]string{"environment": "production"}, false},
		{"empty value still counts as a claim", map[string]string{"environment": ""}, map[string]string{"environment": "production"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ScopeContradicts(tc.have, tc.want); got != tc.contradicted {
				t.Errorf("ScopeContradicts(%v, %v) = %v, want %v", tc.have, tc.want, got, tc.contradicted)
			}
			// The same question the other way round: memory.ScopeMatches asks
			// "may this row be used", so a contradiction is its negation.
			if matches := memory.ScopeMatches(tc.have, tc.want); matches == tc.contradicted {
				t.Errorf("ScopeContradicts(%v, %v) = %v but memory.ScopeMatches says %v: the two rules disagree",
					tc.have, tc.want, tc.contradicted, matches)
			}
		})
	}
}

// rows2items projects candidates onto their ids, so a test can compare the
// retriever's set before and after the pipeline.
func rows2items(rows []memory.Candidate) []Item {
	items := make([]Item, len(rows))
	for i, c := range rows {
		items[i] = Item{ID: c.ID}
	}
	return items
}

// TestStagesDoNotMutateTheRetrievedSet: the candidate set is the retriever's
// return value, and the retriever contract says it is the widened untrimmed
// result. A stage that filters by compacting in place corrupts it: drop the
// first row and the set reads as duplicated and stale entries. Nothing in this
// package reads it after stage 1 today, so only a caller holding the same set
// would notice — which is precisely the contract being broken.
func TestStagesDoNotMutateTheRetrievedSet(t *testing.T) {
	expired := "2020-01-01 00:00:00"
	rows := []memory.Candidate{
		candidate("expired", "proj", "fact", "expired row", 0.9),
		candidate("live1", "proj", "fact", "first live row", 0.8),
		candidate("live2", "proj", "fact", "second live row", 0.7),
	}
	rows[0].ValidUntil = &expired
	set := setOf(rows...)
	before := itemIDs(rows2items(set.Rows))

	req := baseRequest()
	req.Budget.MaxItems = 10
	run(t, &fakeRetriever{set: set}, req)

	if after := itemIDs(rows2items(set.Rows)); !eq(after, before) {
		t.Errorf("the retrieved set changed from %v to %v: a stage filtered by compacting in place", before, after)
	}
}

// TestOriginLabelIsScopedToTheGlobalProject: the legacy-seed correction is only
// a correction for the shipped global rule. A project row that happens to hold
// the same sentence is the user's own material, and labelling it builtin both
// misattributes it and strips the "no agent recorded" marker that tells a reader
// the row is theirs. The shared renderer is the surface that gets this wrong if
// anyone applies the correction by content alone.
func TestOriginLabelIsScopedToTheGlobalProject(t *testing.T) {
	seed := "NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user."

	// source=manual is the pre-v15 shape: a build that wrote the shipped seed
	// before the provenance columns existed recorded it as direct user material.
	globalRow := candidate("G1", "_global", "preference", seed, 0.9)
	globalRow.Source = "manual"
	projectRow := candidate("P1", "proj", "preference", seed, 0.9)
	projectRow.Source = "manual"

	req := baseRequest()
	req.Budget.MaxItems = 10
	res := run(t, &fakeRetriever{set: setOf(globalRow, projectRow)}, req)

	lines := map[string]string{}
	for _, it := range res.Items {
		lines[it.ID] = it.Line()
	}
	if !strings.Contains(lines["G1"], "source=builtin") {
		t.Errorf("global seed row = %q, want the builtin label: the correction exists for exactly this row", lines["G1"])
	}
	if strings.Contains(lines["P1"], "source=builtin") {
		t.Errorf("project row = %q, want no origin label: a project row carrying the seed text is the user's own material", lines["P1"])
	}
}

// TestTraceDoesNotReportAKeptRowAsExcluded: Decision is "one row's fate at one
// stage", and it is what an explain projection reads as "the reason this
// candidate was left out". A row that survived stage 2 with an unreadable
// validity value was recorded through the same path as a dropped one, so the
// trace claimed a row in the answer was excluded from it — the one thing a
// consumer of the trace cannot be allowed to get wrong.
func TestTraceDoesNotReportAKeptRowAsExcluded(t *testing.T) {
	garbage := "sometime last spring"
	rows := []memory.Candidate{candidate("KEPT", "proj", "fact", "one", 0.9)}
	rows[0].ValidUntil = &garbage
	req := baseRequest()
	req.Budget.MaxItems = 10

	res := run(t, &fakeRetriever{set: setOf(rows...)}, req)

	if len(res.Items) != 1 {
		t.Fatalf("the row was dropped, so there is nothing to check: %v", itemIDs(res.Items))
	}
	for _, d := range res.Trace.Decisions {
		if d.ID == "KEPT" && !d.Kept {
			t.Errorf("trace records %s as excluded at %s (%s) while it is in the answer",
				d.ID, d.Stage, d.Reason)
		}
	}
	var kept, excluded int
	for _, d := range res.Trace.Decisions {
		if d.Kept {
			kept++
		} else {
			excluded++
		}
	}
	if kept != 1 || excluded != 0 {
		t.Errorf("decisions = %d kept / %d excluded, want 1/0: an unreadable value is recorded, not an exclusion", kept, excluded)
	}
}
