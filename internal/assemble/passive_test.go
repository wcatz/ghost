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

// TestRunPassiveCapsEachBucketIndependently: the per-slice caps are the whole
// bound, and they are independent — 3 project and 2 global, not 3 and 2 out of 5.
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
// relevance claim to fail a floor.
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

// TestRunPassiveEmptyByExclusionUsesTheExclusionWording is the other half of the
// pair above: a passive window that came back non-empty and was emptied by stage 2
// IS an exclusion.
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

// TestRunPassiveStillFiltersValidity: stage 2 is the same stage the search path
// runs, and this is the membership change the migration buys.
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

// TestRunPassiveBackfillsFromTheTailBehindThePool: the store's two-pass selection
// keeps a POOL of twice the item cap and returns everything past it, undemoted, as
// the backfill supply.
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

// TestRunPassiveProjectlessIsGlobalOnly: a session start in an unmatched directory
// is a supported state, and it reads the global rows.
func TestRunPassiveProjectlessIsGlobalOnly(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(globalCandidate("g1", 0.9))}
	req := passiveRequest()
	req.ProjectID = ""
	// A projectless session start reads ONLY the global bucket.
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
// is the only thing standing between a request and a project it never named.
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

func stampPtr(s string) *string { return &s }

// TestRunPassiveRefusesAHistoricalRead: Run dispatches on AsOf before Query, so a
// request carrying both would be answered by the historical path with its policies
// discarded and its window replaced.
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
	// The LEG sentence, not the shared one.
	if strings.Contains(res.Abstention, "vector leg could not run") {
		t.Errorf("the abstention reports a leg failure for a set a stage emptied — the leg never caused this: %q", res.Abstention)
	}
}

// TestRunPassiveRefusesACategoryPredicate is the filter the passive fetch cannot
// honour. `passiveKeyScanSQL` binds `WHERE project_id = ? AND resolved_at IS NULL`
// plus scope — there is no category and no tier in its SQL — while the query path
// the cut stays reachable (that widening is what closed #573).
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

// TestRunPassiveMachineLineSaysNotAppliedEvenWithACosine: the passive arm is
// checked FIRST, so a retriever that reported a vector leg as `ok` — or a caller
// that set AbstainCosine.
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

func pinnedCandidate(c memory.Candidate) memory.Candidate {
	c.Pinned = true
	return c
}

// TestRunPassiveCountsThePinnedRowsACapCut: more pinned rows than the slice cap
// are cut by the budget stage like any other row, and the trace says how many of
// the cut rows were pinned.
func TestRunPassiveCountsThePinnedRowsACapCut(t *testing.T) {
	var rows []memory.Candidate
	for _, id := range []string{"p1", "p2", "p3", "p4", "p5"} {
		c := projectCandidate(id, 0.5)
		c.FetchedBy = "proj"
		rows = append(rows, pinnedCandidate(c))
	}
	u := projectCandidate("u1", 0.9)
	u.FetchedBy = "proj"
	rows = append(rows, u)
	f := &fakeRetriever{set: passiveSet(rows...)}
	res := run(t, f, passiveRequest())
	if got := res.Trace.PinnedCut["proj"]; got != 2 {
		t.Errorf("PinnedCut[proj] = %d, want 2 (5 pinned rows against a cap of 3)", got)
	}
	if got := res.Trace.PinnedCut["_global"]; got != 0 {
		t.Errorf("PinnedCut[_global] = %d, want 0", got)
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

func TestRunPassiveAddsThePinnedRowsBeyondTheWindow(t *testing.T) {
	var rows []memory.Candidate
	for _, id := range []string{"p1", "p2", "p3", "p4"} {
		c := projectCandidate(id, 0.5)
		c.FetchedBy = "proj"
		rows = append(rows, pinnedCandidate(c))
	}
	set := passiveSet(rows...)
	set.PinnedBeyond = map[string]int{"proj": 3}
	res := run(t, &fakeRetriever{set: set}, passiveRequest())
	if got := res.Trace.PinnedCut["proj"]; got != 4 {
		t.Errorf("PinnedCut[proj] = %d, want 4 (1 cut in the window, 3 beyond it)", got)
	}
	if got := CountsFor(res.Trace, "proj", 3).PinnedCut; got != 4 {
		t.Errorf("BucketTally.PinnedCut = %d, want 4", got)
	}
}

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
