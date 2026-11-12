package assemble

import (
	"context"
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
// best score and the bar, a machine line carrying the reason.
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

// TestNoAnswerDoesNotJudgeWhatItCannotCompare: with no vector leg in play, or with
// no row carrying a cosine, there is nothing to measure the bar against, so the
// block passes through.
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

// cosineRetriever is a retriever that can also read a row's own stored cosine, as
// *memory.Store can. A row absent from `stored` has no embedding.
type cosineRetriever struct {
	fakeRetriever
	stored map[string]float32
}

func (c *cosineRetriever) EmbeddingCosines(_ context.Context, ids []string, _ []float32) (map[string]float32, error) {
	out := map[string]float32{}
	for _, id := range ids {
		if v, ok := c.stored[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

// TestNoAnswerJudgesAKeywordOnlyRowByItsStoredCosine: the vector leg's list does
// not carry every embedded row (a keyword hit rides a reserved slot whatever its
// cosine), so a keyword-only row is judged by its own stored cosine.
func TestNoAnswerJudgesAKeywordOnlyRowByItsStoredCosine(t *testing.T) {
	kw := cosRow("kw", -1)
	kw.FTSRank = 0
	r := &cosineRetriever{fakeRetriever: fakeRetriever{set: hybridSet(kw, cosRow("v1", 0.50))}, stored: map[string]float32{"kw": 0.30}}
	res := run(t, r, noAnswerRequest(0.62))
	if len(res.Items) != 0 || res.Reason != reasonNothingClearedBar {
		t.Errorf("items = %v reason = %q, want the whole block withheld: both rows are embedded and weak", itemIDs(res.Items), res.Reason)
	}
}

// TestNoAnswerJudgesAKeywordOnlyRowWhoseStoredCosineIsNegative: a cosine is in
// [-1, 1], so a stored cosine that is genuinely negative is a JUDGED (very weak)
// match and not "no cosine".
func TestNoAnswerJudgesAKeywordOnlyRowWhoseStoredCosineIsNegative(t *testing.T) {
	kw := cosRow("kw", -1)
	kw.FTSRank = 0
	r := &cosineRetriever{fakeRetriever: fakeRetriever{set: hybridSet(kw)}, stored: map[string]float32{"kw": -0.25}}
	res := run(t, r, noAnswerRequest(0.62))
	if len(res.Items) != 0 || res.Reason != reasonNothingClearedBar {
		t.Errorf("items = %v reason = %q, want the row withheld: its stored cosine (-0.25) is a judgeable value below the bar", itemIDs(res.Items), res.Reason)
	}
	if !strings.Contains(res.Abstention, "-0.250") {
		t.Errorf("abstention %q does not state the negative score that was judged", res.Abstention)
	}
	if got := decisionsAt(res, stageNoAnswer); len(got) != 1 || got[0].Kept {
		t.Errorf("no-answer decisions = %+v, want one drop", got)
	}
}

// TestNoAnswerDoesNotClaimAnAbstentionForABlockALaterStageEmptied: the bar kept the
// one row it could not judge, so it did not withhold the block. When the response
// byte cap then removes that last row the empty block is the cap's doing, and it
// caller makes: only a Measure caller's fit can still empty a block (#1483).
func TestNoAnswerDoesNotClaimAnAbstentionForABlockALaterStageEmptied(t *testing.T) {
	kw := cosRow("kw", -1)
	kw.FTSRank = 0
	kw.Content = strings.Repeat("x", 400)
	r := &cosineRetriever{fakeRetriever: fakeRetriever{set: hybridSet(kw, cosRow("v", 0.30))}, stored: map[string]float32{}}
	req := noAnswerRequest(0.62)
	// Under the row's own size, measured by a caller's own Budget.Measure: only that
	// fit cuts the last row.
	req.Budget.MaxBytes = 300
	req.Budget.Measure = rowsOnlyMeasure
	res := run(t, r, req)
	if len(res.Items) != 0 {
		t.Fatalf("items = %v, want the cap to have emptied the block", itemIDs(res.Items))
	}
	if res.Reason != reasonAllOverBudget {
		t.Errorf("reason = %q, want all_over_budget: the bar kept a row, so it did not withhold the block", res.Reason)
	}
	if strings.Contains(res.Abstention, "nothing cleared the bar") {
		t.Errorf("abstention = %q: the bar did not empty the block, so it may not claim the answer", res.Abstention)
	}
	if !strings.Contains(res.Abstention, "response budget") {
		t.Errorf("abstention = %q, want the sentence to name the response cap that emptied the block", res.Abstention)
	}
	// The fixture is only worth the assertions above if the bar really did fire:
	// the breakdown names the row it withheld and the row the cap removed, apart.
	notes := strings.Join(res.Notes, "\n")
	if !strings.Contains(notes, "no_answer 1") || !strings.Contains(notes, "response_fit 1") {
		t.Errorf("breakdown note = %q, want the bar's row and the cap's row named apart", notes)
	}
}

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

func TestNoAnswerBarOffForAnUnmeasuredModelIsRecordedInTheTrace(t *testing.T) {
	rows := []memory.Candidate{cosRow("a", 0.30)}
	req := noAnswerRequest(0)
	req.NoAnswerBarNote = "no_answer bar off: unmeasured embedding model"
	res := run(t, &fakeRetriever{set: hybridSet(rows...)}, req)
	plain := noAnswerRun(t, 0, hybridSet(rows...))

	if st := stageTraceFor(res, stageNoAnswer); len(st.Notes) != 1 || !strings.Contains(st.Notes[0], "unmeasured embedding model") {
		t.Errorf("stage notes = %v, want the unmeasured-model note", st.Notes)
	}
	if res.Response != plain.Response || len(res.Items) != 1 {
		t.Errorf("the note changed the block:\n%q\nvs\n%q", res.Response, plain.Response)
	}
}
