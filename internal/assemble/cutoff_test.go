package assemble

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The relevance cutoff (#954), on query-mode requests only: a relative-to-top
// fused-score cut that stops the answer where relevance falls off. Each test
// below is RED on a tree whose cutoff stage is a pass-through (the rows are not
// cut, or the top row is cut, or the pinned row is cut, or no verdict is filed),
// so the stage's four guarantees — fall off shortens the block, the top row is
// always kept, a pinned row is never cut, and `limit` still caps — are each
// pinned by a fixture that would otherwise pass.

// cutoffQueryRequest is a QUERY request with a total item cap and no slices, the
// shape ghost_memory_search sends, at one cutoff share. The cap is generous so it
// is the CUTOFF and not the budget doing the shortening in the fall-off tests.
func cutoffQueryRequest(cutoff float64) Request {
	req := baseRequest()
	req.Budget = Budget{MaxItems: 10}
	req.RelevanceCutoff = cutoff
	return req
}

// cutoffRun runs one candidate set through a query-mode request at one cutoff.
func cutoffRun(t *testing.T, cutoff float64, rows ...memory.Candidate) Result {
	t.Helper()
	return run(t, &fakeRetriever{set: setOf(rows...)}, cutoffQueryRequest(cutoff))
}

// cutoffDecision is the single dropped verdict the cutoff stage should hold for
// id, or nil.
func cutoffDecision(res Result, id string) *Decision {
	for i := range res.Trace.Decisions {
		d := &res.Trace.Decisions[i]
		if d.ID == id && d.Stage == stageCutoff {
			return d
		}
	}
	return nil
}

// TestCutoffStopsWhereRelevanceFallsOff is the shape #954 is about: five rows
// whose fused scores drop off a cliff after the second. The answer is the two
// rows above the cliff and every row below it is cut, each with its own
// relevance_cutoff decision — so a trace reader sees which rows went and why,
// and the retrieval record and explain inherit the same reason.
func TestCutoffStopsWhereRelevanceFallsOff(t *testing.T) {
	rows := []memory.Candidate{
		candidate("top", "proj", "fact", "the answer", 10),
		candidate("second", "proj", "fact", "still relevant", 7),
		candidate("third", "proj", "fact", "falling off", 4),
		candidate("fourth", "proj", "fact", "noise", 3),
		candidate("fifth", "proj", "fact", "noise", 2),
	}
	res := cutoffRun(t, 0.5, rows...)

	if ids := itemIDs(res.Items); !eq(ids, []string{"top", "second"}) {
		t.Fatalf("items = %v, want the two rows above the 0.50 cutoff: the answer stops where relevance falls off", ids)
	}
	for _, id := range []string{"third", "fourth", "fifth"} {
		d := cutoffDecision(res, id)
		if d == nil {
			t.Errorf("%s has no cutoff decision: the tail must be recorded with its reason", id)
			continue
		}
		if d.Reason != reasonRelevanceCutoff {
			t.Errorf("%s reason = %q, want %q", id, d.Reason, reasonRelevanceCutoff)
		}
	}
	// The kept rows carry no cutoff verdict at all — one verdict per row, and a
	// verdict on a kept row would count it as withheld.
	if d := cutoffDecision(res, "top"); d != nil {
		t.Errorf("the top row carries a cutoff decision: %+v", d)
	}
	if d := cutoffDecision(res, "second"); d != nil {
		t.Errorf("a row above the cutoff carries a cutoff decision: %+v", d)
	}
}

// TestTopRowIsKeptEvenWhenEveryRowIsWeak: an aggressive cutoff that would cut
// every non-top row still leaves the top one, so the answer is never emptied by
// relevance. A result rate below 1.000 on an answerable query is the regression
// this prevents, and it is the reason the cutoff can shorten a block but never
// delete it.
func TestTopRowIsKeptEvenWhenEveryRowIsWeak(t *testing.T) {
	rows := []memory.Candidate{
		candidate("top", "proj", "fact", "weak but first", 0.5),
		candidate("second", "proj", "fact", "weaker", 0.45),
		candidate("third", "proj", "fact", "weakest", 0.40),
	}
	res := cutoffRun(t, 0.95, rows...)

	if len(res.Items) == 0 {
		t.Fatal("the block is empty: an answerable query must keep its top row whatever the cutoff")
	}
	if ids := itemIDs(res.Items); len(ids) != 1 || ids[0] != "top" {
		t.Errorf("items = %v, want exactly the top row", ids)
	}
	if res.Outcome == OutcomeEmpty {
		t.Errorf("outcome = %q, want a non-empty answer: the top row is always kept", res.Outcome)
	}
}

// TestPinnedRowBelowTheCutoffIsKept: a pin is a slot guarantee, so a pinned row
// whose score is below the cutoff is still kept, and it is filed no cutoff
// verdict. The unpinned row beside it, below the same cutoff, is cut — which is
// what makes the exemption a rule about pins rather than a looser threshold.
func TestPinnedRowBelowTheCutoffIsKept(t *testing.T) {
	pinned := candidate("pinned", "proj", "fact", "below the cutoff but pinned", 4)
	pinned.Pinned = true
	rows := []memory.Candidate{
		candidate("top", "proj", "fact", "the answer", 10),
		candidate("mid", "proj", "fact", "above the cutoff", 7),
		pinned,
		candidate("tail", "proj", "fact", "below and unpinned", 3),
	}
	res := cutoffRun(t, 0.5, rows...)

	if !eq(itemIDs(res.Items), []string{"top", "mid", "pinned"}) {
		t.Errorf("items = %v, want the pinned row kept and the unpinned tail cut", itemIDs(res.Items))
	}
	if d := cutoffDecision(res, "pinned"); d != nil {
		t.Errorf("the pinned row carries a cutoff decision: a pin is a slot guarantee, %+v", d)
	}
	if d := cutoffDecision(res, "tail"); d == nil || d.Reason != reasonRelevanceCutoff {
		t.Errorf("the unpinned tail has no relevance_cutoff decision: %+v", d)
	}
}

// TestBoundStillCapsAfterTheCutoff: the cutoff shortens, it never lengthens, and
// `limit` is still the maximum. Six rows all above the cutoff are kept by it and
// then cut to three by the budget — filed as the budget's own cut, not the
// cutoff's, so the two removals keep their distinct reasons and remedies.
func TestBoundStillCapsAfterTheCutoff(t *testing.T) {
	rows := []memory.Candidate{
		candidate("r1", "proj", "fact", "row 1", 10),
		candidate("r2", "proj", "fact", "row 2", 9),
		candidate("r3", "proj", "fact", "row 3", 8),
		candidate("r4", "proj", "fact", "row 4", 7),
		candidate("r5", "proj", "fact", "row 5", 6),
		candidate("r6", "proj", "fact", "row 6", 5.5),
	}
	req := cutoffQueryRequest(0.5)
	req.Budget = Budget{MaxItems: 3}
	res := run(t, &fakeRetriever{set: setOf(rows...)}, req)

	if ids := itemIDs(res.Items); !eq(ids, []string{"r1", "r2", "r3"}) {
		t.Fatalf("items = %v, want the top 3: limit is the maximum after the cutoff", ids)
	}
	// The cutoff cut nothing here (every row cleared 0.50), so the removal of the
	// three below the cap is the budget's, and it names a different stage.
	if got := decisionsAt(res, stageCutoff); len(got) != 0 {
		t.Errorf("the cutoff filed %d decisions on a block nothing fell below its floor", len(got))
	}
	for _, id := range []string{"r4", "r5", "r6"} {
		found := false
		for _, d := range res.Trace.Decisions {
			if d.ID == id && !d.Kept && d.Stage == stageBudget {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was not cut by the budget stage, so limit is not capping", id)
		}
	}
}

// TestPassiveBlockIsByteIdenticalToTheRanking: the cutoff is a query-mode rule,
// so a passive block is untouched by it whatever the configured share — the same
// rows, the same order, the same verdicts a request with the cutoff off produces.
// It is the digit-level claim for the session-start and project-context surfaces,
// which are a digest and keep their slices.
func TestPassiveBlockIsByteIdenticalToTheRanking(t *testing.T) {
	rows := fallingOffRows()
	off := run(t, &fakeRetriever{set: setOf(rows...)}, func() Request {
		r := diversityRequest(5)
		r.RelevanceCutoff = 0
		return r
	}())
	on := run(t, &fakeRetriever{set: setOf(rows...)}, func() Request {
		r := diversityRequest(5)
		r.RelevanceCutoff = 0.5
		return r
	}())

	if !eq(itemIDs(on.Items), itemIDs(off.Items)) {
		t.Errorf("passive items differ with the cutoff on (%v) and off (%v)", itemIDs(on.Items), itemIDs(off.Items))
	}
	if on.Response != off.Response {
		t.Errorf("passive response differs with the cutoff on and off:\n on: %q\noff: %q", on.Response, off.Response)
	}
	// A passive request is a recorded pass-through at the cutoff stage: it ran, it
	// declined, and it filed no row verdict.
	if st := stageTraceFor(on, stageCutoff); st.In != st.Out || len(st.DroppedIDs) != 0 {
		t.Errorf("passive cutoff stage = in %d out %d dropped %v, want a pass-through", st.In, st.Out, st.DroppedIDs)
	}
	if got := decisionsAt(on, stageCutoff); len(got) != 0 {
		t.Errorf("the cutoff filed %d verdicts on a passive read: it is a query-mode rule", len(got))
	}
}

// TestDisabledCutoffIsByteIdenticalToTheRanking: 0 (or absent) leaves the cutoff
// off, and then a query-mode block is the retriever's order cut at the item cap —
// the same rows, the same verdicts and the same pass-through a pipeline whose
// stage was absent produces. This is the guarantee a machine without a configured
// cutoff relies on: it runs the historical answer, not a half-applied one.
func TestDisabledCutoffIsByteIdenticalToTheRanking(t *testing.T) {
	rows := fallingOffRows()
	res := cutoffRun(t, 0, rows...)

	limit := cutoffQueryRequest(0).Budget.MaxItems
	want := make([]string, 0, limit)
	for i := 0; i < limit && i < len(rows); i++ {
		want = append(want, rows[i].ID)
	}
	if ids := itemIDs(res.Items); !eq(ids, want) {
		t.Errorf("items = %v, want the ranking's own top %v: a disabled cutoff removes nothing", ids, want)
	}
	if got := decisionsAt(res, stageCutoff); len(got) != 0 {
		t.Errorf("the disabled cutoff filed %d decisions: it is off, so it removes nothing and records nothing", len(got))
	}
	if st := stageTraceFor(res, stageCutoff); st.In != st.Out || len(st.DroppedIDs) != 0 {
		t.Errorf("disabled cutoff stage = in %d out %d dropped %v, want a pass-through", st.In, st.Out, st.DroppedIDs)
	}
}

// TestExplainReportsTheCutRowsWithTheReason: explain is a projection of the same
// run, so a row the cutoff cut appears in it as not included, carrying the reason
// sentence — and, unlike the diversity deferral, this reason IS reachable from a
// shipped run, because the cutoff is a query rule and the payload is always a
// query.
func TestExplainReportsTheCutRowsWithTheReason(t *testing.T) {
	req := cutoffQueryRequest(0.5)
	req.Explain = true
	rows := fallingOffRows()
	res := run(t, &fakeRetriever{set: setOf(rows...)}, req)
	if res.Explain == nil {
		t.Fatal("no explain payload: explain is a projection of the same run")
	}
	var cut *memory.ExplainRow
	for i := range res.Explain.Rows {
		r := &res.Explain.Rows[i]
		if r.ID == tokeniseID(t, rows[2].ID) {
			cut = r
		}
	}
	if cut == nil {
		t.Fatalf("the cut row is absent from the explain payload: a row the pipeline saw should be reported as excluded")
	}
	if cut.Included {
		t.Error("the cut row is marked included: explain's included set must equal the answer")
	}
	if !strings.Contains(cut.Reason, "relevance cutoff") {
		t.Errorf("cut row reason = %q, want the relevance-cutoff sentence", cut.Reason)
	}
}

// fallingOffRows is five rows whose fused scores drop off after the second, the
// fixture the cutoff exists for.
func fallingOffRows() []memory.Candidate {
	return []memory.Candidate{
		candidate("top", "proj", "fact", "the answer", 10),
		candidate("second", "proj", "fact", "still relevant", 7),
		candidate("third", "proj", "fact", "falling off", 4),
		candidate("fourth", "proj", "fact", "noise", 3),
		candidate("fifth", "proj", "fact", "noise", 2),
	}
}

// tokeniseID renders an id the way explain does, so a lookup by the raw id works
// on the payload where every id has been through Token.
func tokeniseID(t *testing.T, id string) string {
	t.Helper()
	if got := Token(id); got != id {
		return got
	}
	return id
}
