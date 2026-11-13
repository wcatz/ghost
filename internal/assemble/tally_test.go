package assemble

import (
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestCountsForSplitsBucketFates pins the one table session-start reads: a
// bucket's rows, as the trace records them, split into what was shown, what the
// budget ranked out, and what a stage withheld.
func TestCountsForSplitsBucketFates(t *testing.T) {
	trace := &Trace{Decisions: []Decision{
		{ID: "a1", ProjectID: "p1", Stage: stageValidity, Reason: "expired", Before: 0.9},
		{ID: "a2", ProjectID: "p1", Stage: stageValidity, Reason: "expired", Before: 0.8},
		{ID: "a3", ProjectID: "p1", Stage: stageBudget, Reason: "slice_item_cap", Before: 0.5},
		{ID: "a4", ProjectID: "p1", Stage: stageResponseFit, Reason: reasonBudgetDropped, Before: 0.4},
		{ID: "g1", ProjectID: memory.GlobalProjectID, Stage: stageBudget, Reason: "slice_item_cap", Before: 0.9},
		{ID: "k1", ProjectID: "p1", Stage: stageValidity, Reason: "validity_unparseable", Kept: true, Before: 0.7},
	}}

	tally := CountsFor(trace, "p1", 13)
	if tally.Shown != 13 {
		t.Errorf("Shown = %d, want 13 (the caller's admitted slice, not a count of decisions)", tally.Shown)
	}
	if tally.RankedOut != 2 {
		t.Errorf("RankedOut = %d, want 2 (one budget cut and one response-fit cut — both are the cap's doing)", tally.RankedOut)
	}
	if tally.Withheld != 2 {
		t.Errorf("Withheld = %d, want 2 (both validity drops)", tally.Withheld)
	}
	if tally.Reason != "expired" {
		t.Errorf("Reason = %q, want expired — the dominant withheld cause", tally.Reason)
	}
	if w := tally.Window(); w != 17 {
		t.Errorf("Window = %d, want 17 (13 + 2 + 2)", w)
	}
}

// TestWithheldNoteIsGatedOnAnEmptyBlockNoWhollyWithheldRows: the note answers the
// ONE question the block would otherwise leave silent — every row was withheld and
// nothing says why.
func TestWithheldNoteIsGatedOnAnEmptyBlockNoWhollyWithheldRows(t *testing.T) {
	// Shown > 0: the header already accounts for the withheld half.
	if got := (BucketTally{Shown: 1, Withheld: 2, Reason: "expired"}).WithheldNote(); got != "" {
		t.Errorf("a block that shows rows rendered a withholding note %q", got)
	}
	// Withheld == 0: there is nothing withheld to explain.
	if got := (BucketTally{Shown: 0, Withheld: 0}).WithheldNote(); got != "" {
		t.Errorf("a block with nothing withheld rendered a note %q", got)
	}
	// The one case the note exists for.
	if got := (BucketTally{Shown: 0, Withheld: 2, Reason: "expired"}).WithheldNote(); got == "" {
		t.Errorf("a wholly-withheld block rendered no note; that is the silence this issue is about")
	}
}

// TestWithheldNoteIsTheAssemblersOwnEmptyNote pins issue #897's second review
// finding: a wholly-withheld project bucket at session start and the assembler's
// own verdict for the same state must print the SAME sentence.
func TestWithheldNoteIsTheAssemblersOwnEmptyNote(t *testing.T) {
	expired := projectCandidate("p_exp", 0.9)
	expired.ValidUntil = stampPtr("2026-01-01 00:00:00")

	future := projectCandidate("p_fut", 0.9)
	future.ValidFrom = stampPtr("2999-01-01 00:00:00")

	scoped := projectCandidate("p_scope", 0.9)
	scoped.Scope = map[string]string{"environment": "development"}

	for _, tc := range []struct {
		name  string
		row   memory.Candidate
		scope map[string]string
	}{
		{name: "expired", row: expired},
		{name: "not yet valid", row: future},
		{name: "out of scope", row: scoped, scope: map[string]string{"environment": "production"}},
	} {
		req := passiveRequest()
		req.Scope = tc.scope
		res := run(t, &fakeRetriever{set: passiveSet(tc.row)}, req)
		if res.Outcome != OutcomeEmpty {
			t.Fatalf("%s: outcome = %q, want empty (precondition: the whole block is withheld)", tc.name, res.Outcome)
		}
		want := EmptyNote(res)
		if want == "" {
			t.Fatalf("%s: EmptyNote is empty for a withheld result (reason %q)", tc.name, res.Reason)
		}
		got := CountsFor(res.Trace, "proj", 0).WithheldNote()
		if got != want {
			t.Errorf("%s: the session-start note and the assembler's note differ:\n got %q\nwant %q", tc.name, got, want)
		}
	}
}

// TestCountedAgainstCountsValidityExcludedRowsAsWithheldOnce: the fetch's SQL
// validity predicate removes closed-window rows before the LIMIT, so they are in
// the eligible count, in no trace decision, and are the withholding's doing.
func TestCountedAgainstCountsValidityExcludedRowsAsWithheldOnce(t *testing.T) {
	// 3 eligible: 1 live row shown, 2 removed by the predicate.
	tally := BucketTally{Shown: 1}.CountedAgainst(3, 2, 45)
	if tally.Total() != 3 || tally.Withheld != 2 || tally.RankedOut != 0 || tally.Beyond != 0 {
		t.Errorf("tally = %+v, want total 3 with 2 withheld and nothing ranked out", tally)
	}
	if tally.Reason != validityExpired {
		t.Errorf("Reason = %q, want %q: a predicate-withheld row has no trace decision to name its cause", tally.Reason, validityExpired)
	}
	if tally.Window() != 1 {
		t.Errorf("Window = %d, want 1 (the withheld rows were never in the window)", tally.Window())
	}
	// 60 eligible, 45 of them excluded, over-fetch 45: the 15 live rows all fit
	// the window, so nothing is beyond it.
	big := BucketTally{Shown: 15}.CountedAgainst(60, 45, 45)
	if big.Beyond != 0 || big.Total() != 60 || big.Withheld != 45 {
		t.Errorf("big = %+v, want 15 shown + 45 withheld and nothing beyond", big)
	}
}

// TestWithheldNoteStillRendersWhenTheWholeWindowIsWithheldAndRowsLieBeyondIt: a
// project holding more rows than the over-fetch whose entire window is withheld is
// the same state ghost_project_context answers with the abstention.
func TestWithheldNoteStillRendersWhenTheWholeWindowIsWithheldAndRowsLieBeyondIt(t *testing.T) {
	beyond := BucketTally{Shown: 0, Withheld: 45, Reason: validityExpired}.CountedAgainst(60, 0, 45)
	if beyond.WithheldNote() == "" {
		t.Errorf("a window withheld whole, with rows behind it, lost the note: %+v", beyond)
	}
	inside := BucketTally{Shown: 0, Withheld: 2, RankedOut: 3, Reason: validityExpired}
	if got := inside.WithheldNote(); got != "" {
		t.Errorf("rows ranked out inside the window mean the bucket is not wholly withheld, got %q", got)
	}
}

func TestCountedAtLeastMarksTheTallyALowerBound(t *testing.T) {
	got := BucketTally{Shown: 15, RankedOut: 30}.CountedAtLeast(10000, 0, 45)
	if got.Total() != 10000 || !got.AtLeast {
		t.Errorf("Total/AtLeast = %d/%v, want 10000/true: %+v", got.Total(), got.AtLeast, got)
	}
	if got.RankedOut != 9985 || got.Beyond != 9955 {
		t.Errorf("RankedOut/Beyond = %d/%d, want 9985/9955", got.RankedOut, got.Beyond)
	}
	if exact := (BucketTally{Shown: 15, RankedOut: 30}).CountedAgainst(60, 0, 45); exact.AtLeast {
		t.Errorf("an exact count was marked a lower bound: %+v", exact)
	}
	if un := (BucketTally{Shown: 15, RankedOut: 30}).CountedAtLeast(-1, 0, 45); un.AtLeast || un.Total() != 45 {
		t.Errorf("an unfoldable count changed or marked the tally: %+v", un)
	}
}

func TestCountsForNilTraceAsksNothingOfAnUnavailableTrace(t *testing.T) {
	tally := CountsFor(nil, "p1", 5)
	if tally.Shown != 5 || tally.RankedOut != 0 || tally.Withheld != 0 || tally.Reason != "" {
		t.Errorf("nil trace produced %+v, want {Shown:5} with zeros", tally)
	}
	if tally.Window() != 5 {
		t.Errorf("Window = %d, want 5", tally.Window())
	}
}

func TestEmptyNoteIsSilentOnAnAnswerAndOnAnAbsentWindow(t *testing.T) {
	answer := run(t, &fakeRetriever{set: passiveSet(projectCandidate("p1", 0.9))}, passiveRequest())
	if got := EmptyNote(answer); got != "" {
		t.Errorf("an answerable block has an empty note %q", got)
	}
	absent := run(t, &fakeRetriever{set: passiveSet()}, passiveRequest())
	if got := EmptyNote(absent); got != "" {
		t.Errorf("an empty window is an absence, not a withholding, got %q", got)
	}
}
