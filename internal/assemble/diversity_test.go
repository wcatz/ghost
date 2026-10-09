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
// for the project under test and no total cap, so stage 8's only trim is the
// slice cap and the rows diversity moves are the rows the budget then cuts. The
// share is a passive-read rule, so this is the shape every fixture below asks in.
func diversityRequest(window int) Request {
	req := baseRequest()
	req.Query = ""
	req.Source = SourceSessionStart
	req.Budget = Budget{Slices: []Slice{{Bucket: "proj", MaxItems: window, ClampBytes: 200}}}
	return req
}

// diversityQueryRequest is the same window as a QUERY, which is the shape
// `ghost_memory_search` sends: a total item cap and no slices. The share does not
// apply to it, so this is the request the two query-mode tests use to pin that.
func diversityQueryRequest(window int) Request {
	req := baseRequest()
	req.Budget = Budget{MaxItems: window}
	return req
}

// overfullOneCategoryWindow is the fixture both query-mode tests use: four rows
// of one category followed by two of another, at a window of four. It OVERFLOWS,
// which is the one case where a share could act — and on a passive read it does,
// which is what makes it a fixture that can tell the two modes apart. A fixture
// that fitted would pass on any tree.
func overfullOneCategoryWindow() []memory.Candidate {
	return []memory.Candidate{
		catCandidate("a1", "gotcha", 0.90),
		catCandidate("a2", "gotcha", 0.80),
		catCandidate("a3", "gotcha", 0.70),
		catCandidate("a4", "gotcha", 0.60),
		catCandidate("b1", "preference", 0.55),
		catCandidate("b2", "preference", 0.50),
	}
}

// The query-mode half of the rule. A query is a relevance question and the
// ranking is the whole answer to it, so stage 7 declines it: the row that
// answered the question must not move behind another row because of what it is
// ABOUT. Both tests are RED on a branch that shares the window on a query, and
// both are the byte-identity claim for `ghost_memory_search`.

// TestQueryModeLeavesAnOverflowingWindowExactlyAsRanked: the same over-full,
// one-category window that a passive read mixes is left in the ranking's own
// order on a query-mode read. The budget's top four is the answer, and stage 7
// records no verdict of any kind — not a deferral, not a backfill.
func TestQueryModeLeavesAnOverflowingWindowExactlyAsRanked(t *testing.T) {
	rows := overfullOneCategoryWindow()
	res := run(t, &fakeRetriever{set: setOf(rows...)}, diversityQueryRequest(diversityWindow))

	if ids := itemIDs(res.Items); !eq(ids, []string{"a1", "a2", "a3", "a4"}) {
		t.Errorf("items = %v, want the ranking's own top %d: a query is ranked for "+
			"relevance and the share must not move the row that answered it", ids, diversityWindow)
	}
	for _, d := range decisionsAt(res, stageDiversity) {
		t.Errorf("stage %s filed a verdict on a query-mode read: %+v", stageDiversity, d)
	}
	if st := stageTraceFor(res, stageDiversity); len(st.DroppedIDs) != 0 {
		t.Errorf("stage %s dropped %v on a query-mode read", stageDiversity, st.DroppedIDs)
	}
	if st := stageTraceFor(res, stageDiversity); st.In != len(rows) || st.Out != len(rows) {
		t.Errorf("stage %s saw %d rows and left %d, want %d and %d: it removed nothing",
			stageDiversity, st.In, st.Out, len(rows), len(rows))
	}
}

// TestQueryModeOverflowIsByteIdenticalToTheRanking: the observable answer of an
// overflowing query-mode read is the retriever's order cut at the item cap —
// the same rows, the same order, the same verdicts and the same per-stage counts
// a tree whose stage 7 was a pass-through produces. The ONE thing that moves is
// the stage's own diagnostic sentence, which no surface renders: the search
// surface prints no notes for a non-empty answer, and `assemblerNotes` writes
// them only for an empty one.
func TestQueryModeOverflowIsByteIdenticalToTheRanking(t *testing.T) {
	rows := overfullOneCategoryWindow()
	res := run(t, &fakeRetriever{set: setOf(rows...)}, diversityQueryRequest(diversityWindow))

	want := make([]string, diversityWindow)
	for i := range want {
		want[i] = rows[i].ID
	}
	if ids := itemIDs(res.Items); !eq(ids, want) {
		t.Errorf("items = %v, want %v", ids, want)
	}
	// Every row the budget cut keeps the budget's own verdict, because no stage
	// deferred it first: `b1` and `b2` are outside the window by the ranking, not
	// by a share.
	for _, id := range []string{"b1", "b2"} {
		verdicts := 0
		for _, d := range res.Trace.Decisions {
			if d.ID == id && !d.Kept {
				verdicts++
				if d.Stage != stageBudget {
					t.Errorf("%s = %+v, want the budget's own cut", id, d)
				}
			}
		}
		if verdicts != 1 {
			t.Errorf("%s carries %d dropped verdicts, want exactly one (the budget's)", id, verdicts)
		}
	}
	// The stage list, and every other stage's counts, are what the pass-through
	// produced: stage 7 is present, ran, and changed nothing.
	gotStages := make([]string, 0, len(res.Trace.Stages))
	for _, st := range res.Trace.Stages {
		gotStages = append(gotStages, st.Stage)
		if st.Stage == stageDiversity && (st.In != st.Out || len(st.DroppedIDs) != 0) {
			t.Errorf("stage %s = in %d out %d dropped %v, want a pass-through",
				stageDiversity, st.In, st.Out, st.DroppedIDs)
		}
	}
	wantStages := []string{"validity", "predicates", "provenance", "conflicts", "dedup", "diversity", "budget", "render", "response_fit"}
	if !eq(gotStages, wantStages) {
		t.Errorf("stage list = %v, want %v", gotStages, wantStages)
	}
}

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

// TestDiversityLeavesAOneCategoryWindowAlone: five rows, one category, a window
// of four and a share of two. Rows three and four are inside the window, so the
// share defers them, and no other category exists to take the slots they
// vacated, so the backfill returns them: the same four rows in the same order,
// and no drop. The stage is recorded as having moved rows and kept them, which
// is the difference between a deferral that mattered and one that did not.
func TestDiversityLeavesAOneCategoryWindowAlone(t *testing.T) {
	res := diversityRun(t, diversityWindow,
		catCandidate("a1", "gotcha", 0.90),
		catCandidate("a2", "gotcha", 0.85),
		catCandidate("a3", "gotcha", 0.80),
		catCandidate("a4", "gotcha", 0.75),
		catCandidate("a5", "gotcha", 0.70),
	)

	if ids := itemIDs(res.Items); !eq(ids, []string{"a1", "a2", "a3", "a4"}) {
		t.Errorf("items = %v, want the ranking's own top four: a single category "+
			"leaves the stage nothing to divide", ids)
	}
	st := stageTraceFor(res, stageDiversity)
	if len(st.DroppedIDs) != 0 {
		t.Errorf("deferred = %v, want nothing: every deferred row came back", st.DroppedIDs)
	}
	// The two rows the share moved and returned are recorded as considered.
	kept := 0
	for _, d := range decisionsAt(res, stageDiversity) {
		if d.Kept {
			kept++
		}
	}
	if kept != 2 {
		t.Errorf("%d keeps at %s, want one for each of the two rows that were "+
			"deferred and readmitted", kept, stageDiversity)
	}
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

// TestDiversityBackfillsWhenTheOtherCategoriesRunOut: five rows of one category
// and one of another, at a window of four and a share of two. The other category
// cannot fill the two slots the share vacated, so the deferred rows come back in
// their original order and the window is still full. A rule that dropped them
// instead would return a shorter answer than the caller's own cap allows.
func TestDiversityBackfillsWhenTheOtherCategoriesRunOut(t *testing.T) {
	res := diversityRun(t, diversityWindow,
		catCandidate("a1", "gotcha", 0.90),
		catCandidate("a2", "gotcha", 0.80),
		catCandidate("a3", "gotcha", 0.70),
		catCandidate("a4", "gotcha", 0.60),
		catCandidate("a5", "gotcha", 0.50),
		catCandidate("b1", "preference", 0.40),
	)

	// a3 is the first deferred row, so it is the one that comes back: original
	// order, not the highest-scoring of the deferred ones.
	if ids := itemIDs(res.Items); !eq(ids, []string{"a1", "a2", "b1", "a3"}) {
		t.Errorf("items = %v, want [a1 a2 b1 a3]: a1 and a2 fill the gotcha "+
			"share, b1 takes a second category's slot, and the window is "+
			"completed by the first deferred row", ids)
	}
	// The row that stayed behind the window is the later one, and it kept its own
	// order: the deferral moved a4 down and did not sort it. a5 was already
	// behind the window when the walk reached it, so it was never a candidate
	// for a deferral — moving it to just after the window would have promoted it.
	st := stageTraceFor(res, stageDiversity)
	if !eq(st.DroppedIDs, []string{"a4"}) {
		t.Errorf("deferred = %v, want [a4]", st.DroppedIDs)
	}
	// And the row that came back is recorded as one that was considered, not as
	// one that was deferred.
	for _, d := range decisionsAt(res, stageDiversity) {
		if d.ID != "a3" {
			continue
		}
		if !d.Kept || d.Reason != reasonDiversityBackfilled {
			t.Errorf("a3 was readmitted into the window, so the trace must not "+
				"carry a %s decision for it: %+v", reasonDiversityDeferred, d)
		}
	}
	var backfilled bool
	for _, d := range res.Trace.Decisions {
		if d.ID == "a3" && d.Stage == stageDiversity {
			backfilled = d.Kept
			if d.Reason != reasonDiversityBackfilled {
				t.Errorf("a3's decision reason = %q, want %q", d.Reason, reasonDiversityBackfilled)
			}
		}
	}
	if !backfilled {
		t.Errorf("a3 was deferred and then readmitted, and the trace holds no keep for it")
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

// TestDiversityKeepsEachCategoryInItsOwnOrder: two categories interleaved by
// rank, one of them over the share. The answer holds the survivors in the rank
// order the retriever gave them, and the rows the share moved keep their
// relative order behind the window. A stage that re-ranked — by score, by
// category, by anything — would show up here.
func TestDiversityKeepsEachCategoryInItsOwnOrder(t *testing.T) {
	res := diversityRun(t, diversityWindow,
		catCandidate("a1", "gotcha", 0.90),
		catCandidate("b1", "preference", 0.88),
		catCandidate("a2", "gotcha", 0.86),
		catCandidate("a3", "gotcha", 0.84),
		catCandidate("a4", "gotcha", 0.82),
		catCandidate("b2", "preference", 0.80),
	)

	if ids := itemIDs(res.Items); !eq(ids, []string{"a1", "b1", "a2", "b2"}) {
		t.Errorf("items = %v, want [a1 b1 a2 b2]: the survivors keep the rank "+
			"order they were handed in", ids)
	}
	// The share moved a3, which was inside the window; a4 was already behind it
	// and was left where the ranking put it. Neither swapped with the other, and
	// neither moved ahead of a row that outranked it.
	if st := stageTraceFor(res, stageDiversity); !eq(st.DroppedIDs, []string{"a3"}) {
		t.Errorf("deferred = %v, want [a3]", st.DroppedIDs)
	}
	// Within each category the survivors are still in the order they arrived:
	// a1 before a2, b1 before b2.
	order := map[string]int{}
	for i, it := range res.Items {
		order[it.ID] = i
	}
	if order["a1"] > order["a2"] || order["b1"] > order["b2"] {
		t.Errorf("items = %v reordered rows inside a category", itemIDs(res.Items))
	}
}

// TestDiversityIsANoOpWhenEverythingFits: three candidates under a window of
// four. Nothing was over the share, so the stage is a recorded pass-through: the
// same rows in the same order, no per-row verdict, and no drop. This is what
// makes the change safe for every block that never had to choose.
func TestDiversityIsANoOpWhenEverythingFits(t *testing.T) {
	rows := []memory.Candidate{
		catCandidate("a1", "gotcha", 0.90),
		catCandidate("a2", "gotcha", 0.80),
		catCandidate("b1", "preference", 0.70),
	}
	res := run(t, &fakeRetriever{set: setOf(rows...)}, diversityRequest(diversityWindow))

	if ids := itemIDs(res.Items); !eq(ids, []string{"a1", "a2", "b1"}) {
		t.Errorf("items = %v, want the candidate order unchanged", ids)
	}
	st := stageTraceFor(res, stageDiversity)
	if st.In != len(rows) || st.Out != len(rows) {
		t.Errorf("stage %s saw %d rows and left %d, want %d and %d", stageDiversity, st.In, st.Out, len(rows), len(rows))
	}
	if len(st.DroppedIDs) != 0 {
		t.Errorf("stage %s dropped %v with nothing over the share", stageDiversity, st.DroppedIDs)
	}
	for _, d := range decisionsAt(res, stageDiversity) {
		t.Errorf("stage %s filed a verdict with nothing to defer: %+v", stageDiversity, d)
	}
	// The word that says it ran: a stage that changed nothing and a stage that
	// was skipped are different statements, and the note is which one this is.
	all := strings.Join(st.Notes, " ")
	if !strings.Contains(all, "no row was deferred") {
		t.Errorf("stage notes = %q, want the recorded no-op", all)
	}
}

// TestDiversityIsANoOpWhenTheRequestStatesNoItemCap: a passive read bounded only
// by bytes has no window for a share to divide, so the stage declines rather than
// inventing one from the retrieval ceiling. The answer is the budget stage's to
// bound. A passive slice must still state an OVER-FETCH — the read cannot be
// unbounded on a path that runs at every session start — so the shape under test
// is a slice whose fetch is named and whose block is not.
func TestDiversityIsANoOpWhenTheRequestStatesNoItemCap(t *testing.T) {
	rows := []memory.Candidate{
		catCandidate("a1", "gotcha", 0.90),
		catCandidate("a2", "gotcha", 0.80),
		catCandidate("a3", "gotcha", 0.70),
	}
	req := baseRequest()
	req.Query = ""
	req.Source = SourceSessionStart
	req.Budget = Budget{Slices: []Slice{{Bucket: "proj", OverFetch: 8, MaxBytes: 4000}}}
	res := run(t, &fakeRetriever{set: setOf(rows...)}, req)

	if ids := itemIDs(res.Items); !eq(ids, []string{"a1", "a2", "a3"}) {
		t.Errorf("items = %v, want all three rows: a byte-only passive block has no "+
			"item window to divide", ids)
	}
	// The guard's whole observable effect is what it records: the stage says it
	// declined rather than dividing a window the caller never stated.
	all := strings.Join(stageTraceFor(res, stageDiversity).Notes, " ")
	if !strings.Contains(all, "no item cap") {
		t.Errorf("stage notes = %q, want the recorded decline naming the missing item cap", all)
	}
	for _, d := range decisionsAt(res, stageDiversity) {
		t.Errorf("stage %s filed a verdict: %+v", stageDiversity, d)
	}
}

// TestDiversityRecordsEveryDeferralInTheTraceAndExplain: each deferred row gets
// its own decision naming its stage and reason, and the rows it moved do not
// appear in the answer without an explanation. The mirror half: a row the stage
// moved and then readmitted is IN the answer and is not reported as deferred.
// TestDiversityRecordsEveryDeferralInTheTrace: each deferred row gets its own
// decision naming its stage, its reason and the row's own project, and the rows
// it moved do not appear in the answer without a record of why. The mirror half
// is below: a row the stage moved and then readmitted is IN the answer and is
// recorded as considered, not as deferred.
//
// The explain half of this used to live here and is now a renderer test, because
// a passive read cannot ask for an explanation: `validateRequest` refuses
// `Explain` without a query, and the share is a passive-read rule, so no shipped
// run can carry the reason into a search payload. The sentence itself is pinned
// by TestExplainReasonNamesTheShareARowHit so it cannot rot while unreachable.
func TestDiversityRecordsEveryDeferralInTheTrace(t *testing.T) {
	rows := []memory.Candidate{
		catCandidate("a1", "gotcha", 0.90),
		catCandidate("a2", "gotcha", 0.80),
		catCandidate("a3", "gotcha", 0.70),
		catCandidate("a4", "gotcha", 0.60),
		catCandidate("b1", "preference", 0.55),
		catCandidate("b2", "preference", 0.50),
	}
	res := run(t, &fakeRetriever{set: setOf(rows...)}, diversityRequest(diversityWindow))

	got := decisionsAt(res, stageDiversity)
	if len(got) != 2 {
		t.Fatalf("%d decisions at %s, want one per deferred row: %+v", len(got), stageDiversity, got)
	}
	for i, want := range []string{"a3", "a4"} {
		d := got[i]
		if d.ID != want || d.Kept || d.Reason != reasonDiversityDeferred {
			t.Errorf("decision %d = %+v, want %s/%s/%s", i, d, want, reasonDiversityDeferred, "not kept")
		}
		if d.Stage != stageDiversity {
			t.Errorf("decision %d names stage %q, want %q", i, d.Stage, stageDiversity)
		}
		if d.ProjectID != "proj" {
			t.Errorf("decision %d names project %q, want the row's own", i, d.ProjectID)
		}
	}
	if st := stageTraceFor(res, stageDiversity); !eq(st.DroppedIDs, []string{"a3", "a4"}) {
		t.Errorf("stage %s dropped %v, want [a3 a4]", stageDiversity, st.DroppedIDs)
	}
	if res.Explain != nil {
		t.Error("a request that did not ask for a projection carries one")
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

// TestDiversityLeavesTheSessionStartBlockAlone: the passive shape bounds the
// block with per-slice caps and no total, so the window is their sum. Diversity
// still divides it, and the block it produces is the one the slices would have
// produced anyway — which is the property that lets the stage ship on a surface
// whose only bound is a per-bucket cap.
func TestDiversityLeavesTheSessionStartBlockAlone(t *testing.T) {
	f := &fakeRetriever{set: passiveSet(
		projectCandidate("p1", 0.9), projectCandidate("p2", 0.8),
		projectCandidate("p3", 0.7), projectCandidate("p4", 0.6),
		globalCandidate("g1", 0.5), globalCandidate("g2", 0.4), globalCandidate("g3", 0.3),
	)}
	res := run(t, f, passiveRequest())

	if ids := itemIDs(res.Items); !eq(ids, []string{"p1", "p2", "p3", "g1", "g2"}) {
		t.Errorf("items = %v, want the per-slice caps' own answer", ids)
	}
	// The rows the share moved are explained by the deferral that moved them: the
	// stage 8 cut that followed is not filed as a second verdict for it, so one
	// row is counted once — in the breakdown, in the bucket tally and in the
	// retrieval record.
	dropped := map[string]int{}
	for _, d := range res.Trace.Decisions {
		if d.Kept {
			continue
		}
		dropped[d.ID]++
		if d.ID != "p4" {
			continue
		}
		if d.Reason != reasonDiversityDeferred {
			t.Errorf("p4 = %+v, want its one drop verdict to be the deferral", d)
		}
	}
	if n := dropped["p4"]; n != 1 {
		t.Errorf("p4 carries %d drop verdicts, want exactly one", n)
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

// TestDiversityHalfOfAWindowRoundsUp: a window of five has a share of three, not
// two. Rounding down would let a category fill three of five and still be
// reported as within its share.
func TestDiversityHalfOfAWindowRoundsUp(t *testing.T) {
	res := diversityRun(t, 5,
		catCandidate("a1", "gotcha", 0.90),
		catCandidate("a2", "gotcha", 0.85),
		catCandidate("a3", "gotcha", 0.80),
		catCandidate("a4", "gotcha", 0.75),
		catCandidate("a5", "gotcha", 0.70),
		catCandidate("b1", "preference", 0.65),
		catCandidate("b2", "preference", 0.60),
	)
	if ids := itemIDs(res.Items); !eq(ids, []string{"a1", "a2", "a3", "b1", "b2"}) {
		t.Errorf("items = %v, want three gotcha rows (the rounded-up share) "+
			"and the two preference rows", ids)
	}
	if st := stageTraceFor(res, stageDiversity); !eq(st.DroppedIDs, []string{"a4", "a5"}) {
		t.Errorf("deferred = %v, want [a4 a5]", st.DroppedIDs)
	}
}

// TestDiversityRunsOnEveryPassiveSurface: the stage is on the shared pipeline and
// the gate is the request's mode rather than its Source, so every passive surface
// inherits it whatever it calls itself. A per-surface rule would be a second
// implementation, and this asserts the one place the rule lives by running the
// same passive fixture under each source — including SourceSearch, which is
// passive here because the QUERY is what decides the mode, not the label. The
// query-mode half of the same claim is the two TestQueryMode* tests.
func TestDiversityRunsOnEveryPassiveSurface(t *testing.T) {
	rows := []memory.Candidate{
		catCandidate("a1", "gotcha", 0.90),
		catCandidate("a2", "gotcha", 0.85),
		catCandidate("a3", "gotcha", 0.80),
		catCandidate("a4", "gotcha", 0.75),
		catCandidate("b1", "preference", 0.70),
	}
	for _, source := range []Source{SourceSearch, SourceSessionStart, SourceProjectCtx} {
		req := diversityRequest(diversityWindow)
		req.Source = source
		res := run(t, &fakeRetriever{set: setOf(rows...)}, req)
		// a1 and a2 fill the gotcha share, b1 takes a second category's slot, and
		// the window is completed by the first deferred row: four slots, four
		// rows, whatever the share did to which of them they are.
		if ids := itemIDs(res.Items); !eq(ids, []string{"a1", "a2", "b1", "a3"}) {
			t.Errorf("source %s: items = %v, want the same shared rule every "+
				"other passive surface gets", source, ids)
		}
	}
}

// TestDiversityIsDeterministic: the walk reads the retriever's order and one map
// of category counts, so two runs over the same set hold the same rows. A stage
// that consulted a map for the row order would drift between runs.
func TestDiversityIsDeterministic(t *testing.T) {
	rows := []memory.Candidate{
		catCandidate("a1", "gotcha", 0.90),
		catCandidate("b1", "preference", 0.88),
		catCandidate("a2", "gotcha", 0.86),
		catCandidate("a3", "gotcha", 0.84),
		catCandidate("b2", "preference", 0.82),
		catCandidate("a4", "gotcha", 0.80),
	}
	first := diversityRun(t, diversityWindow, rows...)
	for i := 0; i < 3; i++ {
		again := diversityRun(t, diversityWindow, rows...)
		if itemIDs(again.Items) == nil || len(again.Items) != len(first.Items) {
			t.Fatalf("run %d produced a different number of items", i)
		}
		for j := range first.Items {
			if again.Items[j].ID != first.Items[j].ID {
				t.Fatalf("run %d reordered the block: %v against %v", i,
					itemIDs(again.Items), itemIDs(first.Items))
			}
		}
	}
}
