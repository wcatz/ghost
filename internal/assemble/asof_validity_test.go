package assemble

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// windowedCandidate is a candidate whose memory states a validity window — the
// one field an as_of row borrows from the live row, because memory_history
// records no validity.
func windowedCandidate(id string, from, until *string) memory.Candidate {
	c := candidate(id, "proj", "fact", "database configuration "+id, 1)
	c.ValidFrom, c.ValidUntil = from, until
	return c
}

// asOfValidityRows are the three shapes #910 is about. The bounds are written
// against two different instants on purpose, because that is the shape the
// borrow produces: a window that OPENED before T (so it was open at T) and whose
// end is the live row's, measured against NOW.
//
//   - closed since: opened three days before T and ends an hour before now, so
//     the row was inside its window at T and is retired today. Stage 2 judged it
//     valid at T even before this change; what it now also gets is the note
//     saying the bounds are today's.
//   - opens after now: the window has not opened yet, so the row is not-yet-valid
//     today and was future at T as well — but only because the bound it is being
//     judged by is a bound the row did not hold then. This is the row the old
//     stage 2 dropped and the one this change brings back.
//   - closed before T: the window ends an hour before T, so the row is expired at
//     T on the strength of the same borrowed bound. Dropped before, kept now.
func asOfValidityRows(t0, now time.Time) []memory.Candidate {
	f := memory.StoredStampLayout
	at := func(base time.Time, d time.Duration) *string { s := base.Add(d).Format(f); return &s }
	return []memory.Candidate{
		windowedCandidate("m-closed-since", at(t0, -72*time.Hour), at(now, -time.Hour)),
		windowedCandidate("m-opens-after-now", at(now, time.Hour), at(now, 72*time.Hour)),
		windowedCandidate("m-closed-before-t", at(t0, -96*time.Hour), at(t0, -time.Hour)),
		windowedCandidate("m-open-ended", nil, nil),
	}
}

// stageDropped counts the rows one trace stage dropped, or -1 when it never ran.
func stageDropped(res Result, stage string) int {
	if res.Trace == nil {
		return -1
	}
	for _, s := range res.Trace.Stages {
		if s.Stage == stage {
			return len(s.DroppedIDs)
		}
	}
	return -1
}

// TestAsOfSearchDrawsNoValidityVerdictFromTheBorrowedWindow is #910's rule: the
// window an as_of row carries is the live row's, because memory_history records
// no validity, so it is not evidence about the instant and stage 2 draws no
// verdict from it. Every row survives — including the two whose CURRENT window
// has closed or has not yet opened — the bounds still render, and the disclosure
// is memory.AsOfValidityNote rather than a verdict word on the line.
func TestAsOfSearchDrawsNoValidityVerdictFromTheBorrowedWindow(t *testing.T) {
	t0 := at(0)
	now := t0.Add(30 * 24 * time.Hour)
	cands := asOfValidityRows(t0, now)

	req := historicalRequest(t0)
	req.Budget = Budget{MaxItems: len(cands) + 1}
	// The wall clock is deliberately somewhere else: the binding must make it
	// irrelevant to the verdict, and on this path it is irrelevant to whether
	// there is one.
	req.Now = now
	res := run(t, &fakeRetriever{set: setOf(cands...)}, req)

	if len(res.Items) != len(cands) {
		t.Fatalf("an as_of search admitted %d of %d rows, want all of them: stage 2 drops nothing on a "+
			"historical request\n%s", len(res.Items), len(cands), res.Response)
	}
	for _, it := range res.Items {
		line := it.Line()
		// No verdict word: the bounds are today's, and a row kept on that basis
		// must not be labelled with a clock's answer about them.
		for _, word := range []string{"expired", "not yet valid", "unverified"} {
			if strings.Contains(line, word) {
				t.Errorf("%s carries the verdict %q on a row the stage drew no verdict for:\n%s", it.ID, word, line)
			}
		}
	}
	// The window is still shown — a bound nobody can see is not disclosed either.
	shown := 0
	for _, it := range res.Items {
		if strings.Contains(it.Line(), "until") || strings.Contains(it.Line(), "valid from") {
			shown++
		}
	}
	if shown != 3 {
		t.Errorf("%d of 3 windowed rows render their bounds, want 3: the borrow has to be visible to be disclosed", shown)
	}

	// The disclosure. It is the SEARCH's sentence, not the listings': both lead
	// with the shared borrow, and the verdict clause is what only a surface that
	// drew a verdict may state. A note that opened by claiming a judgement this
	// read made and then retracted it is the misreading the change exists to close.
	want := memory.AsOfBorrowedWindowNote(t0)
	if len(res.Qualifiers) == 0 || !strings.Contains(res.Qualifiers[len(res.Qualifiers)-1], want) {
		t.Errorf("the qualifiers do not carry the as_of window note, want:\n%s", want)
	}
	if !strings.Contains(res.Response, want) {
		t.Errorf("the answer does not state that the window shown is today's:\n%s", res.Response)
	}
	// And specifically not the listing's sentence, which asserts a verdict at T.
	if strings.Contains(res.Response, "Validity judged at") {
		t.Errorf("the search answer claims validity was judged at T, a judgement this read did not make:\n%s", res.Response)
	}
}

// TestAsOfSearchStatesTheNoteOnAnEmptyAnswerToo: an absence is the answer that
// most needs the disclosure. A historical read that matched nothing reads as a
// present one that matched nothing, and the reader then acts on "Ghost has
// nothing about this" rather than "Ghost had nothing about this at T".
func TestAsOfSearchStatesTheNoteOnAnEmptyAnswerToo(t *testing.T) {
	t0 := at(0)
	res := run(t, &fakeRetriever{set: setOf()}, historicalRequest(t0))
	if len(res.Items) != 0 {
		t.Fatalf("the block is not empty, so the assertion below would prove nothing about an empty answer")
	}
	if want := memory.AsOfBorrowedWindowNote(t0); !strings.Contains(res.Response, want) {
		t.Errorf("an empty historical answer does not state the as_of window note, want:\n%s\n%s", want, res.Response)
	}
}

// TestACurrentSearchStillDropsARowOutsideItsWindow is the other half: the change
// is scoped to a historical request. A current search has a real clock and a real
// window, so a row whose window has closed or has not opened is still dropped as
// expired / not-yet-valid, and no historical note appears.
func TestACurrentSearchStillDropsARowOutsideItsWindow(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	f := memory.StoredStampLayout
	stamp := func(d time.Duration) *string { s := now.Add(d).Format(f); return &s }
	// The same two rows the as_of read keeps, read at the clock the request names.
	cands := []memory.Candidate{
		windowedCandidate("m-closed-since", stamp(-72*time.Hour), stamp(-time.Hour)),
		windowedCandidate("m-opens-later", stamp(time.Hour), nil),
	}
	req := baseRequest()
	req.Budget = Budget{MaxItems: len(cands) + 1}
	res := run(t, &fakeRetriever{set: setOf(cands...)}, req)
	if len(res.Items) != 0 {
		t.Errorf("a current search admitted %d rows, want 0: a closed window and an unopened one are both "+
			"dropped as of today\n%s", len(res.Items), res.Response)
	}
	if got := stageDropped(res, stageValidity); got != len(cands) {
		t.Errorf("stage 2 dropped %d rows, want %d: the historical change must not reach a current read",
			got, len(cands))
	}
	for _, bad := range []string{"Validity judged at", "as_of "} {
		if strings.Contains(res.Response, bad) {
			t.Errorf("a current answer carries the historical disclosure %q:\n%s", bad, res.Response)
		}
	}
}

// TestAsOfSearchRefusesExplain pins the explain half of the same rule: explain is
// refused with as_of, so there is no payload that could report a validity drop
// for a historical read, and the refusal is what keeps the two surfaces from
// disagreeing about a row that was kept. A projection that drew its own verdict
// would be the #571 class of bug one layer up, and the refusal is the reason this
// change needs nothing in the explain path.
func TestAsOfSearchRefusesExplain(t *testing.T) {
	t0 := at(0)
	cands := asOfValidityRows(t0, t0.Add(30*24*time.Hour))
	req := withExplain(historicalRequest(t0))
	req.Budget = Budget{MaxItems: len(cands) + 1}
	if _, err := Run(context.Background(), &fakeRetriever{set: setOf(cands...)}, req); err == nil {
		t.Fatal("explain with as_of was served, want a refusal: a projection of the current ranking would " +
			"report a verdict for a read that drew none")
	} else if !strings.Contains(err.Error(), "as_of") {
		t.Errorf("explain with as_of was refused with %q, want it to name as_of", err)
	}
}
