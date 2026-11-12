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
