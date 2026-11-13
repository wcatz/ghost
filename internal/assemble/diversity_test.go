package assemble

// Stage 7, the diversity stage (#927). A small digest is easy to fill with rows
// from one category while the next-ranked rows of every other category are cut,
// so the rule here is a per-category SHARE of the window rather than a filter:
// within the window no category may take more than half the slots, rounded up.
//
// THE SHARE IS A PASSIVE-READ RULE, and that is the first thing to know about the
// fixtures below: every one of them asks through a passive request. A query is a
// relevance question and the ranking is the whole answer to it, so moving the row
// that answered it behind another row because of what it is ABOUT is the stage
// answering a question nobody asked. A passive read asks no question and hands a
// model everything worth knowing, where breadth is the point. The two
// TestQueryMode* tests pin the query half: an overflowing query-mode read is left
// exactly as ranked, with no verdict of any kind at this stage.
//
// The properties these tests hold, each with a mutation that kills it:
//
//   - a window one category filled is mixed, up to the share, with the
//     next-ranked rows of other categories;
//   - a share is a deferral and never a deletion: when the other categories run
//     out of rows the deferred rows come back in their original order and the
//     window is still full;
//   - a pinned row is never deferred, and it still counts toward its category's
//     share;
//   - a request whose candidates fit under the window records a no-op, so a
//     block that never had to choose is unchanged;
//   - a query-mode request records the same no-op, whatever its window holds.
//
// The trace and the reason sentence are pinned together, because a deferral a
// reader cannot see is indistinguishable from a row that was never a candidate.

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// diversityWindow is the item cap every fixture asks for. The candidate set is
// handed to the stages untrimmed — that is the retriever's contract — so the
// number of rows below it is what makes the stage act, and a fixture built at
// the cap itself would test nothing but the budget stage.
const diversityWindow = 4

// diversityRequest is a PASSIVE request whose item cap IS the window: one slice
// for the project under test and no total cap, so stage 9's only trim is the
// slice cap and the rows diversity moves are the rows the budget then cuts. The
// share is a passive-read rule, so this is the shape every fixture below asks in.
func diversityRequest(window int) Request {
	req := baseRequest()
	req.Query = ""
	req.Source = SourceSessionStart
	req.Budget = Budget{Slices: []Slice{{Bucket: "proj", MaxItems: window, ClampBytes: 200}}}
	return req
}

// diversityBucketedRequest is the shape the real session start sends: a project
// slice and a `_global` slice, with no total cap, so each bucket's own slice cap
// is what stage 9 enforces over the rows it admits. The share divides each
// bucket by its own cap, which is what the two-bucket tests pin.
func diversityBucketedRequest(projCap, globalCap int) Request {
	req := diversityRequest(projCap)
	req.Budget = Budget{Slices: []Slice{
		{Bucket: "proj", MaxItems: projCap, ClampBytes: 200},
		{Bucket: memory.GlobalProjectID, MaxItems: globalCap, ClampBytes: 300},
	}}
	return req
}

// bucketedFact is a row in the named category in the named bucket.
func bucketedFact(id, bucket, category string, score float64) memory.Candidate {
	return candidate(id, bucket, category, "a "+bucket+" "+category+" memory "+id, score)
}

// renderedVerdictsAreHonest is the invariant the per-bucket window exists for: a
// row the answer RENDERS carries no drop verdict, and the bucket tally the passive
// header reads counts exactly the rows it renders. Before the per-bucket window a
// deferred row could be admitted by stage 9 with its deferral verdict still on it,
// which is a header-honesty break — the block shows a row the header says it
// withheld.
func renderedVerdictsAreHonest(t *testing.T, res Result) {
	t.Helper()
	shown := map[string]bool{}
	perBucket := map[string]int{}
	for _, it := range res.Items {
		shown[it.ID] = true
		perBucket[it.Bucket]++
	}
	for _, d := range res.Trace.Decisions {
		if d.Kept || !shown[d.ID] {
			continue
		}
		t.Errorf("row %s is RENDERED but carries a drop verdict: %+v", d.ID, d)
	}
	for bucket, n := range perBucket {
		tally := CountsFor(res.Trace, bucket, n)
		// NOT `tally.Shown != n`: CountsFor copies the `shown` argument it is
		// handed straight into BucketTally.Shown, so that comparison can never
		// fail. What the helper checks instead is derived from TWO records and
		// so can: stage 7's own DroppedIDs, and its per-row verdicts. They are
		// written by different code paths and a withdrawal rewrites both, so a
		// row that left only one of them shows up here.
		var stageDropped []string
		for _, st := range res.Trace.Stages {
			if st.Stage == stageDiversity {
				stageDropped = st.DroppedIDs
			}
		}
		for _, id := range stageDropped {
			verdict := false
			for _, d := range res.Trace.Decisions {
				if d.ID == id && d.Stage == stageDiversity && !d.Kept {
					verdict = true
				}
			}
			if !verdict {
				t.Errorf("bucket %s: stage %s lists %s as dropped and no decision says so", bucket, stageDiversity, id)
			}
			if shown[id] {
				t.Errorf("bucket %s: stage %s lists %s as dropped and the answer renders it", bucket, stageDiversity, id)
			}
		}
		if tally.Withheld < 0 || tally.RankedOut < 0 || tally.Deduped < 0 {
			t.Errorf("bucket %s: the tally holds a negative count: %+v", bucket, tally)
		}
	}
}

// The query-mode half of the rule. A query is a relevance question and the
// ranking is the whole answer to it, so stage 7 declines it: the row that
// answered the question must not move behind another row because of what it is
// ABOUT. Both tests are RED on a branch that shares the window on a query, and
// both are the byte-identity claim for `ghost_memory_search`.

// catCandidate is a candidate in the named category. Content names the category
// so a failure message says which row it was, and ids carry the category so a
// mis-ordered block is readable in a diff.
func catCandidate(id, category string, score float64) memory.Candidate {
	return candidate(id, "proj", category, "a memory about "+category+" "+id, score)
}

func pinnedCatCandidate(id, category string, score float64) memory.Candidate {
	c := catCandidate(id, category, score)
	c.Pinned = true
	return c
}

// diversityRun runs one candidate set at the given window and returns the
// result, so a test can assert on the answer and on the same run's trace.
func diversityRun(t *testing.T, window int, rows ...memory.Candidate) Result {
	t.Helper()
	return run(t, &fakeRetriever{set: setOf(rows...)}, diversityRequest(window))
}

// stageTraceFor is the trace entry for one stage, or a zero value.
func stageTraceFor(res Result, stage string) StageTrace {
	for _, st := range res.Trace.Stages {
		if st.Stage == stage {
			return st
		}
	}
	return StageTrace{}
}

// decisionsAt is every decision one stage filed, in the order it filed them.
func decisionsAt(res Result, stage string) []Decision {
	var out []Decision
	for _, d := range res.Trace.Decisions {
		if d.Stage == stage {
			out = append(out, d)
		}
	}
	return out
}

// TestDiversityMixesAWindowOneCategoryFilled: four rows of one category are
// followed by rows of another, the window is four and the share is two, so the
// answer holds two of each rather than four of the first. This is the whole
// issue: a block filled by one category while other relevant rows are cut.
func TestDiversityMixesAWindowOneCategoryFilled(t *testing.T) {
	res := diversityRun(t, diversityWindow,
		catCandidate("a1", "gotcha", 0.90),
		catCandidate("a2", "gotcha", 0.80),
		catCandidate("a3", "gotcha", 0.70),
		catCandidate("a4", "gotcha", 0.60),
		catCandidate("b1", "preference", 0.55),
		catCandidate("b2", "preference", 0.50),
	)

	if ids := itemIDs(res.Items); !eq(ids, []string{"a1", "a2", "b1", "b2"}) {
		t.Errorf("items = %v, want two gotcha rows and two preference rows, "+
			"in the rank order the retriever returned them in", ids)
	}
	// The window is mixed, never shrunk: four slots held four rows.
	if len(res.Items) != diversityWindow {
		t.Errorf("%d items, want the full window of %d", len(res.Items), diversityWindow)
	}
}

// TestDiversityNeverDefersAPinnedRow: the pinned gotcha row is the third of its
// category in a window of four with a share of two. The share is already full
// when the walk reaches it, and the row stays anyway; the unpinned fourth row of
// the same category is the one that moves. A pin is a slot guarantee (#936), so
// a stage that cut one would take the guarantee back with a deferral.
func TestDiversityNeverDefersAPinnedRow(t *testing.T) {
	res := diversityRun(t, diversityWindow,
		catCandidate("a1", "gotcha", 0.90),
		catCandidate("a2", "gotcha", 0.85),
		pinnedCatCandidate("a3", "gotcha", 0.80),
		catCandidate("a4", "gotcha", 0.75),
		catCandidate("b1", "preference", 0.70),
		catCandidate("b2", "preference", 0.65),
	)

	if ids := itemIDs(res.Items); !eq(ids, []string{"a1", "a2", "a3", "b1"}) {
		t.Errorf("items = %v, want [a1 a2 a3 b1]: the pinned a3 stays over the "+
			"share and the unpinned a4 is the row that moves", ids)
	}
	for _, d := range decisionsAt(res, stageDiversity) {
		if d.ID == "a3" {
			t.Errorf("a3 is pinned and must never be deferred: %+v", d)
		}
	}
	if st := stageTraceFor(res, stageDiversity); !eq(st.DroppedIDs, []string{"a4"}) {
		t.Errorf("deferred = %v, want [a4]", st.DroppedIDs)
	}
}

// TestExplainReasonNamesTheShareARowHit: the sentence a passive deferral renders
// if a projection ever reads it. It is asked of the renderer directly, because
// the shipped path cannot reach it — `Request.Explain` is refused without a query
// and the share is a passive-read rule — and a sentence nothing exercises is a
// sentence that rots. It names the category, the share and the window, and states
// the RULE rather than the count the answer holds: when the other categories run
// out of rows the deferred ones come back, so a block can carry more of one
// category than the share and a sentence claiming otherwise would contradict the
// block above it.
func TestExplainReasonNamesTheShareARowHit(t *testing.T) {
	p := &pipeline{
		passive:  true,
		deferred: map[string]diversityDeferral{"a3": {category: "gotcha", share: 2}},
		dropped:  map[string]string{"a3": reasonDiversityDeferred},
		req:      diversityRequest(diversityWindow),
	}
	got := p.explainReason("a3", memory.ExplainRow{}, nil)
	for _, want := range []string{"diversity", "gotcha", "2", "4"} {
		if !strings.Contains(got, want) {
			t.Errorf("reason = %q, want it to name %q", got, want)
		}
	}
	// The fallback, for a row whose deferral the pipeline did not record.
	p2 := &pipeline{passive: true, dropped: map[string]string{"a3": reasonDiversityDeferred}}
	if got := p2.explainReason("a3", memory.ExplainRow{}, nil); !strings.Contains(got, "share") {
		t.Errorf("reason = %q, want it to name the share", got)
	}
}

// TestDiversityDefersNothingWhenTheShareCoversTheWindow: a window of one admits
// one row, so the share is one and the row that takes it is the top one. The
// half-window arithmetic must never round a share down to zero.
func TestDiversityDefersNothingWhenTheShareCoversTheWindow(t *testing.T) {
	res := diversityRun(t, 1,
		catCandidate("a1", "gotcha", 0.90),
		catCandidate("b1", "preference", 0.80),
	)
	if ids := itemIDs(res.Items); !eq(ids, []string{"a1"}) {
		t.Errorf("items = %v, want the top row", ids)
	}
	// The second row was already outside the window, so nothing was deferred.
	if st := stageTraceFor(res, stageDiversity); len(st.DroppedIDs) != 0 {
		t.Errorf("deferred = %v, want nothing: a row below the window is not deferred", st.DroppedIDs)
	}
}

// The per-bucket half of the rule. A sliced passive budget states a cap PER
// BUCKET and stage 9 enforces each one over the rows it admits, so a share that
// divides ONE window across all the buckets would let one bucket's share evict
// another bucket's row — the very row stage 9 then keeps, leaving a deferral
// verdict on a row in the answer and a header that says it was withheld.

// TestDiversityNeverDefersARowAnotherBucketsCapKeeps: the reproduction from
// review. A `_global` slice capped at 2 and a project slice capped at 3, three
// global `fact` rows ranked above two project `fact` rows and a project
// `decision` row. The whole set shares ONE 5-row window in the old code, so the
// third global row takes a `fact` slot and a project row is deferred — and stage
// 8 then keeps it under the project cap, so the trace calls a RENDERED row
// withheld. Both buckets overflow their own cap here — the `_global` bucket holds
// three rows under a two-row one — but no bucket's share evicts a row another
// bucket's cap keeps, so the answer is the one the slices alone would produce.
func TestDiversityNeverDefersARowAnotherBucketsCapKeeps(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(
		bucketedFact("g1", memory.GlobalProjectID, "fact", 0.9),
		bucketedFact("g2", memory.GlobalProjectID, "fact", 0.8),
		bucketedFact("g3", memory.GlobalProjectID, "fact", 0.7),
		bucketedFact("p1", "proj", "fact", 0.6),
		bucketedFact("p2", "proj", "fact", 0.5),
		bucketedFact("p3", "proj", "decision", 0.4),
	)}
	res := run(t, f, diversityBucketedRequest(3, 2))

	if ids := itemIDs(res.Items); !eq(ids, []string{"g1", "g2", "p1", "p2", "p3"}) {
		t.Errorf("items = %v, want [g1 g2 p1 p2 p3] — the answer the two slices "+
			"alone produce: no bucket's share evicts a row another bucket's cap keeps", ids)
	}
	renderedVerdictsAreHonest(t, res)
}

// TestTrimWithdrawsADeferralForARowItKeeps: the call site, asked of directly.
// `trim` is where membership is decided, so it is where a deferral on a kept row
// has to be reversed — and the direct test above cannot reach that, because the
// per-bucket window is what keeps a deferred row away from a kept row. This one
// hands `trim` the one state it refuses to produce: a row stage 7 deferred that
// the budget keeps.
func TestTrimWithdrawsADeferralForARowItKeeps(t *testing.T) {
	req := diversityRequest(diversityWindow)
	p := &pipeline{
		passive:   true,
		req:       req,
		dropped:   map[string]string{"a3": reasonDiversityDeferred},
		deferred:  map[string]diversityDeferral{"a3": {category: "gotcha", share: 2, slots: 4}},
		droppedBy: map[string]int{stageDiversity: 1},
		trace:     newTrace(req, &memory.CandidateSet{}),
	}
	rows := []memory.Candidate{catCandidate("a1", "gotcha", 0.9), catCandidate("a3", "gotcha", 0.7)}
	items := []Item{itemOf(rows[0]), itemOf(rows[1])}
	p.trace.decide("a3", "proj", stageDiversity, reasonDiversityDeferred, 0.7)

	// a3 is deferred and the budget still keeps it: the state stage 7's own
	// windows make unreachable, and the reversal is what makes it survivable.
	keepRow := []bool{true, true}
	kept, _, dropped := trim(rows, items, keepRow, nil, p, "budget")
	if len(kept) != 2 {
		t.Fatalf("trim kept %d rows, want both", len(kept))
	}
	if len(dropped) != 0 {
		t.Errorf("trim reports %v as removed, want nothing: it kept every row", dropped)
	}
	if _, still := p.dropped["a3"]; still {
		t.Error("a3 is still marked dropped, so the bucket tally and the retrieval record would count a rendered row as withheld")
	}
	if n := p.droppedBy[stageDiversity]; n != 0 {
		t.Errorf("stage %s still counts %d removals", stageDiversity, n)
	}
	for _, d := range p.trace.Decisions {
		if d.ID == "a3" && !d.Kept {
			t.Errorf("a3 = %+v, want the deferral withdrawn", d)
		}
	}
}

// sizedCandidate is a candidate whose stored content is exactly `size` bytes, so
// a slice byte cap lands where the test says it lands. The score is its rank.
func sizedCandidate(id, category string, size int, score float64) memory.Candidate {
	if size < len(id) {
		size = len(id)
	}
	return candidate(id, "proj", category, id+strings.Repeat(".", size-len(id)), score)
}

// TestTrimWithdrawsADeferralForEveryRowItKeeps: a slice byte cap is what makes
// this reachable, and it is reachable often enough to need the rule stated.
// Stage 7 leaves a deferred row BEHIND the window, and the budget stage then cuts
// the rows in front of it — a byte cap cuts them while the item count is still
// under its cap, so a deferred row is readmitted. Every such row must lose its
// deferral verdict: the answer renders it, and the trace, `dropped`,
// `droppedBy[diversity]`, the bucket tally and the stage record would all say it
// was withheld.
//
// The fixture is the one a review reproduced: five 25-byte fact rows and three
// 75-byte decision rows under a 6-row window and a 200-byte slice cap. The share
// holds three facts and all three decisions, deferring a4 and a5; the byte cap
// then keeps a1, a2, a3 and b1, cuts b2 and b3, and reads a4 and a5.
func TestTrimWithdrawsADeferralForEveryRowItKeeps(t *testing.T) {
	req := diversityRequest(6)
	req.Budget.Slices[0].MaxBytes = 200
	res := run(t, &fakeRetriever{set: setOf(
		sizedCandidate("a1", "fact", 25, 0.90),
		sizedCandidate("a2", "fact", 25, 0.85),
		sizedCandidate("a3", "fact", 25, 0.80),
		sizedCandidate("a4", "fact", 25, 0.75),
		sizedCandidate("a5", "fact", 25, 0.70),
		sizedCandidate("b1", "decision", 75, 0.65),
		sizedCandidate("b2", "decision", 75, 0.60),
		sizedCandidate("b3", "decision", 75, 0.55),
	)}, req)

	if ids := itemIDs(res.Items); !eq(ids, []string{"a1", "a2", "a3", "b1", "a4", "a5"}) {
		t.Fatalf("items = %v, want [a1 a2 a3 b1 a4 a5]", ids)
	}
	// Both deferred rows came back, so neither keeps a deferral verdict, neither
	// is counted as withheld, and the stage record agrees.
	renderedVerdictsAreHonest(t, res)
	for _, d := range res.Trace.Decisions {
		if d.Kept || d.Reason != reasonDiversityDeferred {
			continue
		}
		t.Fatalf("%s = %+v, want no deferral verdict on any rendered row", d.ID, d)
	}
	if st := stageTraceFor(res, stageDiversity); len(st.DroppedIDs) != 0 {
		t.Errorf("stage %s still lists %v as dropped", stageDiversity, st.DroppedIDs)
	}
	for _, st := range res.Trace.Stages {
		if st.Stage != stageDiversity {
			continue
		}
		for _, note := range st.Notes {
			if strings.Contains(note, "deferred 2") {
				t.Errorf("the stage note still claims 2 deferrals: %q", note)
			}
		}
	}
	tally := CountsFor(res.Trace, "proj", len(res.Items))
	if tally.Shown != len(res.Items) {
		t.Errorf("the tally says %d shown, the answer renders %d", tally.Shown, len(res.Items))
	}
	if tally.Withheld != 0 {
		t.Errorf("the tally counts %d withheld rows, want 0: every row the answer carries is rendered", tally.Withheld)
	}
}

// TestDiversityNoteDoesNotClaimAFitItCannotShow: the no-op sentence reports only
// the population it measured. `in` is every candidate — including rows in buckets
// the windows skipped — while the window is the cap, so a request whose slices
// carry only byte caps, or one bucket with no item cap beside a capped one, can
// reach the no-deferral note with more candidates than the window admits. The
// sentence must not then claim they all fit.
func TestDiversityNoteDoesNotClaimAFitItCannotShow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		req        Request
		rows       []memory.Candidate
		wantFit    bool
		wantHonest bool
	}{
		{
			name: "everything fits", req: diversityRequest(4),
			rows:       []memory.Candidate{catCandidate("a1", "gotcha", 0.9), catCandidate("a2", "gotcha", 0.8)},
			wantFit:    true,
			wantHonest: true,
		},
		{
			name: "a byte-only slice and a total cap", req: func() Request {
				r := baseRequest()
				r.Query, r.Source = "", SourceSessionStart
				r.Budget = Budget{MaxItems: 2, Slices: []Slice{{Bucket: "proj", MaxBytes: 4000, OverFetch: 8}}}
				return r
			}(),
			rows:       []memory.Candidate{catCandidate("a1", "gotcha", 0.9), catCandidate("a2", "gotcha", 0.8), candidate("b1", "_global", "fact", "a global fact b1", 0.7)},
			wantHonest: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, &fakeRetriever{set: setOf(tc.rows...)}, tc.req)
			notes := strings.Join(stageTraceFor(res, stageDiversity).Notes, " ")
			if !strings.Contains(notes, "no row was deferred") {
				t.Errorf("notes = %q, want the recorded no-op", notes)
			}
			if !tc.wantFit && strings.Contains(notes, "candidates fit under it") {
				t.Errorf("notes = %q, claim a fit for a population it did not measure", notes)
			}
			if !strings.Contains(notes, "per-category share") {
				t.Errorf("notes = %q, want the share stated", notes)
			}
		})
	}
}
