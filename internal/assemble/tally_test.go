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

// TestWithheldNoteIsTheAssemblersOwnEmptyNote pins issue #897's second review
// finding: a wholly-withheld project bucket at session start and the
// assembler's own verdict for the same state must print the SAME sentence. The
// states are driven through assemble.Run (not through a hand-built pipeline) so
// the comparison is against what a caller of Run receives as Result.Abstention,
// the bytes ghost_project_context prints through EmptyNote.
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

// TestEmptyNoteIsSilentOnAnAnswerAndOnAnAbsentWindow: the note exists for rows
// that were found and withheld; an answerable block and a window that came back
// empty (`no_memories`, an absence) have none.
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

// TestCountedAgainstAttributesRowsBeyondTheWindowToTheRanking: the window is an
// over-fetch, so a store with more eligible rows than it fetched has rows the
// ranking cut before any stage saw them. They are ranked out, and the total is
// the eligible count, not the window's size.
func TestCountedAgainstAttributesRowsBeyondTheWindowToTheRanking(t *testing.T) {
	tally := BucketTally{Shown: 15, RankedOut: 30}.CountedAgainst(60, 0, 45)
	if tally.Total() != 60 {
		t.Errorf("Total = %d, want 60 (the eligible rows, not the 45 the window held)", tally.Total())
	}
	if tally.RankedOut != 45 || tally.Beyond != 15 {
		t.Errorf("RankedOut/Beyond = %d/%d, want 45/15 (30 cut by the budget + 15 beyond the window)", tally.RankedOut, tally.Beyond)
	}
	if tally.Window() != 45 {
		t.Errorf("Window = %d, want 45 (what the retrieval actually fetched)", tally.Window())
	}
	// A count that came back smaller than the window (a write raced it) never
	// shrinks the tally below what the window held.
	if got := (BucketTally{Shown: 15, RankedOut: 30}).CountedAgainst(10, 0, 45); got.Total() != 45 || got.RankedOut != 30 {
		t.Errorf("a stale count changed the tally: %+v", got)
	}
	// A negative count means "unknown" and changes nothing.
	if got := (BucketTally{Shown: 15, RankedOut: 30}).CountedAgainst(-1, 0, 45); got.Total() != 45 {
		t.Errorf("an unknown count changed the tally: %+v", got)
	}
}

// TestCountedAgainstCountsValidityExcludedRowsAsWithheldOnce: the fetch's SQL
// validity predicate removes closed-window rows before the LIMIT, so they are in
// the eligible count, in no trace decision, and are the withholding's doing. They
// are withheld, counted once, and are not beyond the window.
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

// TestCountedAgainstNamesRowsThePolicyRemovedAsNearDuplicates: a row the
// retriever fetched and then removed as a near-duplicate loser never reaches the
// trace, so it is in the eligible count and in none of the trace's fates. It is
// neither beyond the over-fetch nor ranked out; it has its own count, so the
// header cannot hand it to the ranking.
func TestCountedAgainstNamesRowsThePolicyRemovedAsNearDuplicates(t *testing.T) {
	// 11 eligible globals, a window of 16 that held them all, the trace saw 10.
	tally := BucketTally{Shown: 8, RankedOut: 2}.CountedAgainst(11, 0, 16)
	if tally.Total() != 11 {
		t.Errorf("Total = %d, want 11", tally.Total())
	}
	if tally.Deduped != 1 {
		t.Errorf("Deduped = %d, want 1 (fetched, then removed by the bucket policy)", tally.Deduped)
	}
	if tally.RankedOut != 2 || tally.Beyond != 0 {
		t.Errorf("RankedOut/Beyond = %d/%d, want 2/0: nothing was beyond the window and the loser was not ranked", tally.RankedOut, tally.Beyond)
	}
}

// TestWithheldNoteStillRendersWhenTheWholeWindowIsWithheldAndRowsLieBeyondIt: a
// project holding more rows than the over-fetch whose entire window is withheld
// is the same state ghost_project_context answers with the abstention, so the
// note must render; only rows the ranking cut INSIDE the window mean the bucket
// is not wholly withheld.
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
