package assemble

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The no-answer bar (#955), on query-mode requests only: when the best vector
// cosine among the rows a block would carry is below one absolute bar, the block
// is withheld and the answer says nothing cleared it. Each test is RED on a tree
// whose step is a pass-through, so the guarantees are pinned separately: a weak
// block is withheld with the stated reason and the score, an answerable block is
// untouched, a pinned row is never withheld, an off bar changes nothing, and a
// block that cannot be judged (no vector leg, no cosine) is never refused.

// cosRow is a candidate carrying the vector leg's cosine. The keyword rank is
// well outside the keyword arm so only the cosine decides anything here.
func cosRow(id string, cosine float64) memory.Candidate {
	c := candidate(id, "proj", "fact", "memory "+id, 0.5)
	c.FTSRank, c.VectorRank, c.VectorScore = 6, 0, cosine
	return c
}

func noAnswerRequest(bar float64) Request {
	req := baseRequest()
	req.Condition = CondHybrid
	req.QueryVec = []float32{0.1, 0.2, 0.3}
	req.Budget = Budget{MaxItems: 10}
	req.NoAnswerCosine = bar
	return req
}

func noAnswerRun(t *testing.T, bar float64, set *memory.CandidateSet) Result {
	t.Helper()
	return run(t, &fakeRetriever{set: set}, noAnswerRequest(bar))
}

// TestNoAnswerWithholdsABlockWhoseBestMatchIsBelowTheBar: no row reaches the bar,
// so the block comes back empty with the stated reason, a sentence that shows the
// best score and the bar, a machine line carrying the reason, and one decision per
// withheld row at the step.
func TestNoAnswerWithholdsABlockWhoseBestMatchIsBelowTheBar(t *testing.T) {
	res := noAnswerRun(t, 0.6, hybridSet(cosRow("a", 0.55), cosRow("b", 0.50), cosRow("c", 0.41)))

	if len(res.Items) != 0 {
		t.Fatalf("items = %v, want none: the best match (0.55) is below the 0.60 bar", itemIDs(res.Items))
	}
	if res.Outcome != OutcomeEmpty || res.Reason != reasonNothingClearedBar {
		t.Fatalf("outcome = %q reason = %q, want empty / %q", res.Outcome, res.Reason, reasonNothingClearedBar)
	}
	for _, want := range []string{"No memory answers this", "nothing cleared the bar", "0.550", "0.600", "context.no_answer_cosine"} {
		if !strings.Contains(res.Abstention, want) {
			t.Errorf("abstention %q is missing %q", res.Abstention, want)
		}
	}
	if !strings.Contains(res.Machine, "reason="+reasonNothingClearedBar) {
		t.Errorf("machine line %q does not carry the reason", res.Machine)
	}
	if !strings.Contains(res.Response, "No memory answers this") {
		t.Errorf("the response does not state the abstention:\n%s", res.Response)
	}
	got := decisionsAt(res, stageNoAnswer)
	if len(got) != 3 {
		t.Fatalf("no-answer decisions = %d, want one per withheld row (3)", len(got))
	}
	for _, d := range got {
		if d.Kept || d.Reason != reasonNothingClearedBar {
			t.Errorf("decision %+v, want a drop with reason %q", d, reasonNothingClearedBar)
		}
	}
	if st := stageTraceFor(res, stageNoAnswer); st.In != 3 || st.Out != 0 || len(st.DroppedIDs) != 3 {
		t.Errorf("stage trace = in %d out %d dropped %v, want 3 in, 0 out", st.In, st.Out, st.DroppedIDs)
	}
}

// TestNoAnswerLeavesAnAnswerableBlockAlone: the best match clears the bar, so the
// block is byte-identical to the one a request with the bar off produces.
func TestNoAnswerLeavesAnAnswerableBlockAlone(t *testing.T) {
	rows := []memory.Candidate{cosRow("a", 0.71), cosRow("b", 0.50), cosRow("c", 0.41)}
	on := noAnswerRun(t, 0.6, hybridSet(rows...))
	off := noAnswerRun(t, 0, hybridSet(rows...))

	if !eq(itemIDs(on.Items), []string{"a", "b", "c"}) {
		t.Fatalf("items = %v, want every row: the best match clears the bar", itemIDs(on.Items))
	}
	if on.Response != off.Response || on.Machine != off.Machine {
		t.Errorf("an answerable block differs with the bar on and off:\n on: %q\noff: %q", on.Response, off.Response)
	}
	if got := decisionsAt(on, stageNoAnswer); len(got) != 0 {
		t.Errorf("the step filed %d decisions on a block that cleared the bar", len(got))
	}
}

// TestNoAnswerNeverWithholdsAPinnedRow: a pin is a slot guarantee. The pinned row
// stays and is not judged, and the unpinned rows below the bar go.
func TestNoAnswerNeverWithholdsAPinnedRow(t *testing.T) {
	pinned := cosRow("pinned", 0.10)
	pinned.Pinned = true
	res := noAnswerRun(t, 0.6, hybridSet(pinned, cosRow("a", 0.55), cosRow("b", 0.50)))

	if !eq(itemIDs(res.Items), []string{"pinned"}) {
		t.Fatalf("items = %v, want only the pinned row", itemIDs(res.Items))
	}
	if res.Outcome == OutcomeEmpty {
		t.Errorf("outcome = %q: a block that still holds a pinned row is not empty", res.Outcome)
	}
	for _, d := range decisionsAt(res, stageNoAnswer) {
		if d.ID == "pinned" {
			t.Errorf("the pinned row carries a no-answer decision: %+v", d)
		}
	}
	if got := decisionsAt(res, stageNoAnswer); len(got) != 2 {
		t.Errorf("no-answer decisions = %d, want the two unpinned rows", len(got))
	}
}

// TestNoAnswerPinnedRowsDoNotRescueTheBlock: a pinned row's own cosine says
// nothing about whether the rest of the block answers the query, so a strong
// pinned row does not keep weak neighbours in.
func TestNoAnswerPinnedRowDoesNotKeepWeakRowsIn(t *testing.T) {
	pinned := cosRow("pinned", 0.95)
	pinned.Pinned = true
	res := noAnswerRun(t, 0.6, hybridSet(pinned, cosRow("a", 0.55)))
	if !eq(itemIDs(res.Items), []string{"pinned"}) {
		t.Errorf("items = %v, want the pinned row alone", itemIDs(res.Items))
	}
}

// TestDisabledNoAnswerIsByteIdenticalToTheRanking: 0 leaves the step off, and a
// weak block comes back whole, with no verdict and no decision, exactly as a
// pipeline without the step returns it.
func TestDisabledNoAnswerIsByteIdenticalToTheRanking(t *testing.T) {
	res := noAnswerRun(t, 0, hybridSet(cosRow("a", 0.30), cosRow("b", 0.20)))
	if !eq(itemIDs(res.Items), []string{"a", "b"}) {
		t.Fatalf("items = %v, want the ranking's own rows: a disabled bar removes nothing", itemIDs(res.Items))
	}
	if got := decisionsAt(res, stageNoAnswer); len(got) != 0 {
		t.Errorf("the disabled step filed %d decisions", len(got))
	}
	if st := stageTraceFor(res, stageNoAnswer); st.In != st.Out || len(st.DroppedIDs) != 0 || len(st.Notes) != 0 {
		t.Errorf("disabled stage = %+v, want a silent pass-through", st)
	}
	for _, banned := range []string{"no-answer", "No memory answers this", reasonNothingClearedBar} {
		if strings.Contains(res.Response, banned) {
			t.Errorf("the disabled bar left %q in the response:\n%s", banned, res.Response)
		}
	}
}

// TestNoAnswerIsAQueryModeRule: a passive request carries no query, so the bar is
// a recorded pass-through whatever its value and the block is byte-identical.
func TestNoAnswerIsAQueryModeRule(t *testing.T) {
	rows := []memory.Candidate{cosRow("a", 0.30), cosRow("b", 0.20)}
	mk := func(bar float64) Result {
		req := diversityRequest(5)
		req.NoAnswerCosine = bar
		return run(t, &fakeRetriever{set: hybridSet(rows...)}, req)
	}
	on, off := mk(0.9), mk(0)
	if on.Response != off.Response || !eq(itemIDs(on.Items), itemIDs(off.Items)) {
		t.Errorf("a passive block differs with the bar on and off:\n on: %q\noff: %q", on.Response, off.Response)
	}
	if st := stageTraceFor(on, stageNoAnswer); st.In != st.Out || len(st.DroppedIDs) != 0 || len(st.Notes) != 0 {
		t.Errorf("passive stage = %+v, want a silent pass-through", st)
	}
}

// TestNoAnswerDoesNotJudgeWhatItCannotCompare: with no vector leg in play, or
// with no row carrying a cosine, there is nothing to measure the bar against, so
// the block passes through. Refusing every answer on a machine with no embedder
// would be the rule misreading its blind spot as a verdict.
func TestNoAnswerDoesNotJudgeWhatItCannotCompare(t *testing.T) {
	rows := []memory.Candidate{cosRow("a", 0.10), cosRow("b", 0.05)}

	t.Run("vector leg not applicable", func(t *testing.T) {
		res := noAnswerRun(t, 0.6, ftsOnlySet(rows...))
		if len(res.Items) != 2 {
			t.Errorf("items = %v, want both rows: no vector leg means no cosine to judge", itemIDs(res.Items))
		}
	})
	t.Run("no legs reported", func(t *testing.T) {
		res := noAnswerRun(t, 0.6, setOf(rows...))
		if len(res.Items) != 2 {
			t.Errorf("items = %v, want both rows", itemIDs(res.Items))
		}
	})
	t.Run("vector leg failed", func(t *testing.T) {
		set := hybridSet(rows...)
		set.Legs["vector"] = memory.LegStatus{Applicable: true, Attempted: true, Available: false, Err: "boom"}
		res := noAnswerRun(t, 0.6, set)
		if len(res.Items) != 2 {
			t.Errorf("items = %v, want both rows: a failed leg is an incident, not a verdict", itemIDs(res.Items))
		}
	})
	t.Run("sentinel only", func(t *testing.T) {
		// -1 is "the vector leg did not retrieve this row", not a low cosine.
		res := noAnswerRun(t, 0.6, hybridSet(cosRow("a", -1), cosRow("b", -1)))
		if len(res.Items) != 2 {
			t.Errorf("items = %v, want both rows: no row carries a cosine", itemIDs(res.Items))
		}
	})
	t.Run("sentinel rows do not count as weak", func(t *testing.T) {
		res := noAnswerRun(t, 0.6, hybridSet(cosRow("a", -1), cosRow("b", 0.7)))
		if len(res.Items) != 2 {
			t.Errorf("items = %v, want both rows: the one cosine clears the bar", itemIDs(res.Items))
		}
	})
}

// TestNoAnswerDoesNotExemptKeywordReservedRows: the reservation puts the best
// keyword hits in every window, so exempting them would keep rows on exactly the
// near-miss queries the rule exists for.
func TestNoAnswerDoesNotExemptKeywordReservedRows(t *testing.T) {
	reserved := cosRow("reserved", 0.30)
	reserved.KeywordReserved = true
	res := noAnswerRun(t, 0.6, hybridSet(cosRow("a", 0.55), reserved))
	if len(res.Items) != 0 {
		t.Errorf("items = %v, want none: a keyword-reserved row below the bar is withheld too", itemIDs(res.Items))
	}
}

// TestNoAnswerBarIsStrictlyBelow: a best match exactly on the bar clears it, so a
// block is withheld only when nothing reached the bar.
func TestNoAnswerBarIsStrictlyBelow(t *testing.T) {
	res := noAnswerRun(t, 0.5, hybridSet(cosRow("a", 0.5)))
	if len(res.Items) != 1 {
		t.Errorf("items = %v, want the row: a cosine equal to the bar clears it", itemIDs(res.Items))
	}
}

// TestExplainReportsTheNoAnswerRows: explain is a projection of the same run, so a
// withheld row is an excluded row carrying the no-answer sentence.
func TestExplainReportsTheNoAnswerRows(t *testing.T) {
	req := noAnswerRequest(0.6)
	req.Explain = true
	res := run(t, &fakeRetriever{set: hybridSet(cosRow("a", 0.55))}, req)
	if res.Explain == nil || len(res.Explain.Rows) == 0 {
		t.Fatal("no explain rows: a withheld row is still a row the pipeline saw")
	}
	for _, r := range res.Explain.Rows {
		if r.Included {
			t.Errorf("row %s is included: the block was withheld", r.ID)
		}
		if !strings.Contains(r.Reason, "no-answer bar") {
			t.Errorf("row %s reason = %q, want the no-answer sentence", r.ID, r.Reason)
		}
	}
}

// TestNoAnswerBarIsValidated: a bar no cosine can be is refused on the request
// itself, as the other relevance parameters are.
func TestNoAnswerBarIsValidated(t *testing.T) {
	for _, bad := range []float64{-0.1, 1.1} {
		req := noAnswerRequest(bad)
		if _, err := Run(t.Context(), &fakeRetriever{set: hybridSet()}, req); err == nil {
			t.Errorf("Run accepted NoAnswerCosine %v", bad)
		}
	}
}
