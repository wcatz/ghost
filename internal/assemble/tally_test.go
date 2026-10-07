package assemble

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestCountsForSplitsBucketFates pins the one table session-start reads: a
// bucket's rows, as the trace records them, split into what was shown, what the
// budget ranked out, and what a stage withheld. The three fates are not the same
// thing and the block says them differently — "ranked out" is the cap's doing
// and carries the ranking's authority, "withheld" is a stage refusing a row it
// saw — so the count must know which stage dropped a row, not only that it did.
//
// The fixture also mixes in decisions for ANOTHER bucket: CountsFor keys on the
// row's own project, and a `_global` row dropped beside this bucket's rows is
// that bucket's fate being counted into this one.
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

// TestCountsForFirstSeenReasonWinsATie pins the tie-break: when two withheld
// causes account for the same number of rows, the tally names the one an
// upstream reader of the trace would have met FIRST — a later reason must not
// displace an earlier one merely by matching it.
func TestCountsForFirstSeenReasonWinsATie(t *testing.T) {
	trace := &Trace{Decisions: []Decision{
		{ID: "a1", ProjectID: "p1", Stage: stagePredicates, Reason: "scope_contradiction", Before: 0.9},
		{ID: "a2", ProjectID: "p1", Stage: stageValidity, Reason: "future", Before: 0.8},
		{ID: "a3", ProjectID: "p1", Stage: stagePredicates, Reason: "scope_contradiction", Before: 0.7},
	}}
	tally := CountsFor(trace, "p1", 0)
	if tally.Withheld != 3 {
		t.Errorf("Withheld = %d, want 3", tally.Withheld)
	}
	if tally.Reason != "scope_contradiction" {
		t.Errorf("Reason = %q, want scope_contradiction (first-seen on the tie, not the last-seen future)", tally.Reason)
	}
}

// TestCountsForNilTraceAsksNothingOfAnUnavailableTrace: a caller that may not
// hold a trace (a historical renderer that never assembled one) must still get
// a usable tally — the shown half is the caller's own, and the withheld halves
// are zero rather than a panic.
func TestCountsForNilTraceAsksNothingOfAnUnavailableTrace(t *testing.T) {
	tally := CountsFor(nil, "p1", 5)
	if tally.Shown != 5 || tally.RankedOut != 0 || tally.Withheld != 0 || tally.Reason != "" {
		t.Errorf("nil trace produced %+v, want {Shown:5} with zeros", tally)
	}
	if tally.Window() != 5 {
		t.Errorf("Window = %d, want 5", tally.Window())
	}
}

// TestWithheldNoteIsGatedOnAnEmptyBlockNoWhollyWithheldRows: the note answers
// the ONE question the block would otherwise leave silent — every row was
// withheld and nothing says why — so it renders only on a block whose project
// half shows nothing. A block that shows rows, or one with nothing withheld, is
// not that question and gets nothing.
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

// TestWithheldNoteExpiredIsThePassiveAbstentionSentence pins the byte identity
// the surface depends on: the note a wholly-withheld session start carries is
// the SAME sentence `abstention` renders for the same cause on the passive path,
// plus the pointer. Two renderings of one fact must not drift — a reader who
// sees one sentence today and a different one in a tool answer tomorrow would
// conclude the block was lying about one of them.
func TestWithheldNoteExpiredIsThePassiveAbstentionSentence(t *testing.T) {
	got := (BucketTally{Shown: 0, Withheld: 2, Reason: validityExpired}).WithheldNote()
	want := (&pipeline{passive: true}).abstention(OutcomeEmpty, reasonAllInvalid) + WithheldPointer
	if got != want {
		t.Errorf("the wholly-withheld note is not the passive abstention sentence:\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(got, "No sufficiently trustworthy memory found") {
		t.Errorf("the note lost the abstention lead: %q", got)
	}
	if !strings.Contains(got, "withheld as out of date") {
		t.Errorf("the note lost the out-of-date cause: %q", got)
	}
	if !strings.HasSuffix(got, WithheldPointer) {
		t.Errorf("the note does not end with the pointer to the still-marked rows: %q", got)
	}
}

// TestWithheldNoteNamesTheCause: the note's sentence is the withheld cause's
// own, so a scope exclusion does not tell the reader an expiry did it.
func TestWithheldNoteNamesTheCause(t *testing.T) {
	scope := (BucketTally{Shown: 0, Withheld: 1, Reason: "scope_contradiction"}).WithheldNote()
	if !strings.Contains(scope, "nothing found matched the requested scope") {
		t.Errorf("a scope withholding must say so: %q", scope)
	}
	future := (BucketTally{Shown: 0, Withheld: 1, Reason: validityFuture}).WithheldNote()
	if future != (&pipeline{passive: true}).abstention(OutcomeEmpty, reasonAllInvalid)+WithheldPointer {
		t.Errorf("a future-window withholding is not the same note as an expired one: %q", future)
	}
	other := (BucketTally{Shown: 0, Withheld: 1, Reason: "category_mismatch"}).WithheldNote()
	if !strings.Contains(other, "withheld before the answer was assembled") {
		t.Errorf("an uncatalogued cause must still say the rows were withheld: %q", other)
	}
}
