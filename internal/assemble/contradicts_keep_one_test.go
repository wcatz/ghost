package assemble

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// #925: stage 5 separates a contradicts pair. One side stays in the answer, the
// other leaves the block and is recorded AGAINST the side that stayed — in the
// trace, in the retrieval record and in explain — and the kept line says a
// conflicting row exists and was withheld, by id, so the reader can look it up.
//
// The order is the contract docs/architecture.md states, in order: pinned beats
// unpinned (a); a later verified_at beats an earlier or an absent one (b); a
// later updated_at — created_at per row when updated_at is unset — beats an
// earlier one (c); and an exact tie falls to the rank the window already holds
// (d). Relevance rank decides nothing until every other key has tied, because a
// row that ranks higher is not therefore the row that is true.
//
// A scope-conflicting pair is not a contradiction (memory.ScopesConflict, the
// rule every other reader of a link applies), and is neither separated, marked
// nor noted.

// separationRequest is the ordinary budget every test here assembles under: wide
// enough that only the pair under test can be cut, so a missing row is the
// separation's doing and nothing else's.
func separationRequest() Request {
	req := baseRequest()
	req.Budget.MaxItems = 10
	return req
}

// stamped is a candidate whose updated_at the test states, so the tie-break
// under test is the only key that differs between the two sides.
func stamped(id string, score float64, updatedAt string) memory.Candidate {
	c := candidate(id, "proj", "fact", "database configuration "+id, score)
	c.UpdatedAt = updatedAt
	return c
}

// renderedLines returns every rendered item line naming id. lineOf demands
// exactly one; the withheld side of a separated pair has none, which is the
// property the assertions below need.
func renderedLines(response, id string) []string {
	var found []string
	for _, l := range strings.Split(response, "\n") {
		if strings.HasPrefix(l, "- [") && strings.Contains(l, "`"+Token(id)+"` (") {
			found = append(found, l)
		}
	}
	return found
}

// assertSeparated is the one statement about one pair: the answer holds the kept
// side alone, the withheld side carries the stage-5 drop naming the kept side,
// and the kept side carries no decision of its own.
func assertSeparated(t *testing.T, res Result, kept, withheld string) {
	t.Helper()
	if got := itemIDs(res.Items); !eq(got, []string{kept}) {
		t.Fatalf("items = %v, want only the kept side %s", got, kept)
	}
	var sawDrop bool
	for _, d := range res.Trace.Decisions {
		if d.ID == kept && !d.Kept {
			t.Errorf("the kept side %s carries a drop decision: %+v", kept, d)
		}
		if d.ID != withheld {
			continue
		}
		sawDrop = true
		if d.Stage != stageConflicts || d.Reason != "contradiction_separated" || d.Kept {
			t.Errorf("withheld %s: decision = {stage:%q reason:%q kept:%v}, want a drop at %q with reason %q",
				withheld, d.Stage, d.Reason, d.Kept, stageConflicts, "contradiction_separated")
		}
		if !eq(d.Against, []string{kept}) {
			t.Errorf("withheld %s: Against = %v, want [%s] — the trace is where the kept id is named",
				withheld, d.Against, kept)
		}
	}
	if !sawDrop {
		t.Errorf("no stage-5 decision records withholding %s: %+v", withheld, res.Trace.Decisions)
	}
}

// TestContradictsPairKeepsOneSideAndWithholdsTheOther is the contract itself.
// Every other key ties here, so the rank the window already holds decides, and
// the answer must hold one row of the pair rather than both.
func TestContradictsPairKeepsOneSideAndWithholdsTheOther(t *testing.T) {
	a := stamped("A1", 0.9, "")
	b := stamped("B1", 0.8, "")

	res := run(t, &fakeRetriever{set: contradictingSet(a, b)}, separationRequest())

	assertSeparated(t, res, "A1", "B1")
}

// TestTheWithheldSideIsRecordedAsWithheld is the retrieval record's half. The
// record's row verdict has no field that can carry the counterpart — it is
// exactly {id, kept, stage, reason} — so it mirrors what a near-duplicate loser
// records: Kept false, the conflicts stage, and the separation's reason. The
// kept id is named by the trace's Against and by explain's reason, asserted in
// the two tests beside this one.
func TestTheWithheldSideIsRecordedAsWithheld(t *testing.T) {
	sink := &recordingSink{}
	req := separationRequest()
	req.Record = sink

	run(t, &fakeRetriever{set: contradictingSet(stamped("A1", 0.9, ""), stamped("B1", 0.8, ""))}, req)

	rec := sink.last(t)
	byID := map[string]memory.RowVerdict{}
	for _, v := range rec.Verdicts {
		byID[v.ID] = v
	}
	withheld, ok := byID["B1"]
	if !ok {
		t.Fatalf("the withheld side is not in the retrieval record at all: %+v", rec.Verdicts)
	}
	if withheld.Kept {
		t.Errorf("the withheld side is recorded as kept: %+v", withheld)
	}
	if withheld.Stage != stageConflicts || withheld.Reason != "contradiction_separated" {
		t.Errorf("withheld verdict = {stage:%q reason:%q}, want stage %q reason %q",
			withheld.Stage, withheld.Reason, stageConflicts, "contradiction_separated")
	}
	if kept := byID["A1"]; !kept.Kept {
		t.Errorf("the kept side is recorded as dropped: %+v", kept)
	}
}

// TestExplainReportsTheWithheldSideAndNamesTheKeptRow: explain reports the row
// as not included and says which row was kept, by id, so a reader who meets the
// withheld memory in a listing can find out why it is not in the answer.
func TestExplainReportsTheWithheldSideAndNamesTheKeptRow(t *testing.T) {
	req := separationRequest()
	req.Explain = true

	res := run(t, &fakeRetriever{set: contradictingSet(stamped("A1", 0.9, ""), stamped("B1", 0.8, ""))}, req)

	if res.Explain == nil {
		t.Fatal("explain: true produced no payload")
	}
	rowFor := func(id string) memory.ExplainRow {
		t.Helper()
		for _, r := range res.Explain.Rows {
			if r.ID == Token(id) {
				return r
			}
		}
		t.Fatalf("no explain row for %s: %+v", id, res.Explain.Rows)
		return memory.ExplainRow{}
	}

	withheld := rowFor("B1")
	if withheld.Included {
		t.Error("the withheld side is reported as included")
	}
	if !strings.Contains(withheld.Reason, "withheld by the conflicts stage") {
		t.Errorf("explain reason = %q, want it to name the conflicts stage", withheld.Reason)
	}
	if !strings.Contains(withheld.Reason, "A1") {
		t.Errorf("explain reason = %q, want it to name the kept row A1", withheld.Reason)
	}
	if kept := rowFor("A1"); !kept.Included || kept.Reason != "" {
		t.Errorf("the kept side: included=%v reason=%q, want included with no exclusion reason",
			kept.Included, kept.Reason)
	}
}

// TestKeptLineSaysTheWithheldSideExists: the marker's job is now to say a
// conflicting row exists and was withheld, by id. The withheld side renders no
// line at all, and the kept line still names it.
func TestKeptLineSaysTheWithheldSideExists(t *testing.T) {
	res := run(t, &fakeRetriever{set: contradictingSet(stamped("A1", 0.9, ""), stamped("B1", 0.8, ""))},
		separationRequest())

	if got := len(renderedLines(res.Response, "B1")); got != 0 {
		t.Errorf("the withheld side rendered %d line(s):\n%s", got, res.Response)
	}
	kept := lineOf(t, res.Response, "A1")
	if !strings.Contains(kept, "conflicts_with=`B1`") {
		t.Errorf("the kept line does not name the withheld side: %q", kept)
	}
}

// TestSeparatedPairIsReportedOnceInTheNote: the answer's note says what
// happened — one side kept, one withheld — once, and never repeats the stage's
// own sentence, which is about a window that has since closed.
func TestSeparatedPairIsReportedOnceInTheNote(t *testing.T) {
	res := run(t, &fakeRetriever{set: contradictingSet(stamped("A1", 0.9, ""), stamped("B1", 0.8, ""))},
		separationRequest())

	joined := strings.Join(res.Notes, "\n")
	if got := strings.Count(joined, "contradicts pair recorded"); got != 1 {
		t.Errorf("the answer reports the pair %d times, want 1:\n%s", got, joined)
	}
	if !strings.Contains(joined, "A1 kept, B1 withheld") {
		t.Errorf("the answer note does not say which side was withheld:\n%s", joined)
	}
	if strings.Contains(joined, "were both candidates at this stage") {
		t.Errorf("the answer carries the stage-scoped sentence:\n%s", joined)
	}
}

// TestStageNoteSaysThePairWasSeparated: the trace keeps the stage's own
// sentence, which says both were candidates HERE — a different audience from the
// answer, and the one sentence the answer must not carry.
func TestStageNoteSaysThePairWasSeparated(t *testing.T) {
	res := run(t, &fakeRetriever{set: contradictingSet(stamped("A1", 0.9, ""), stamped("B1", 0.8, ""))},
		separationRequest())

	var stageNotes []string
	for _, st := range res.Trace.Stages {
		if st.Stage == stageConflicts {
			stageNotes = st.Notes
		}
	}
	if stageNotes == nil {
		t.Fatalf("no %s stage record in the trace: %+v", stageConflicts, res.Trace.Stages)
	}
	want := "contradicts pair recorded and separated: A1 and B1 were both candidates at this stage; " +
		"A1 kept, B1 withheld"
	if !hasNote(stageNotes, want) {
		t.Errorf("stage record = %v, want it to carry %q", stageNotes, want)
	}
	if hasNote(res.Notes, "were both candidates at this stage") {
		t.Errorf("the answer carries the stage-scoped sentence: %v", res.Notes)
	}
}

// TestSeparationCountsAsWithheldInTheBucketTally: the session-start header's
// "N withheld" is read from CountsFor, and a separated row is a stage refusing
// a row it saw — never a row the ranking cut. The cause is the separation, so a
// wholly-withheld block would name it rather than the score.
func TestSeparationCountsAsWithheldInTheBucketTally(t *testing.T) {
	res := run(t, &fakeRetriever{set: contradictingSet(stamped("A1", 0.9, ""), stamped("B1", 0.8, ""))},
		separationRequest())

	tally := CountsFor(res.Trace, "proj", len(res.Items))
	if tally.Withheld != 1 || tally.RankedOut != 0 {
		t.Errorf("tally = %+v, want one withheld row and none ranked out", tally)
	}
	if tally.Reason != "contradiction_separated" {
		t.Errorf("tally reason = %q, want %q", tally.Reason, "contradiction_separated")
	}
}

// Tie-break (a): pinned beats unpinned, even against the higher-ranked row.
func TestSeparationPinnedBeatsUnpinned(t *testing.T) {
	// The pinned side is the OLDER one and ranks second, so it can only win on
	// the pin.
	pinned := stamped("PIN", 0.5, "2026-01-01 00:00:00")
	pinned.Pinned = true
	unpinned := stamped("PLN", 0.9, "2026-06-01 00:00:00")

	res := run(t, &fakeRetriever{set: contradictingSet(unpinned, pinned)}, separationRequest())

	assertSeparated(t, res, "PIN", "PLN")
}

// Tie-break (b): a later verified_at wins, even against the higher-ranked row
// and the newer one — a stamp somebody checked beats a stamp nobody did.
func TestSeparationLaterVerifiedAtWins(t *testing.T) {
	verified := stamped("VER", 0.5, "2026-01-01 00:00:00")
	stamp := "2026-07-01 00:00:00"
	verified.VerifiedAt = &stamp
	unverified := stamped("UNV", 0.9, "2026-06-01 00:00:00")

	res := run(t, &fakeRetriever{set: contradictingSet(unverified, verified)}, separationRequest())

	assertSeparated(t, res, "VER", "UNV")
}

// Tie-break (c): with no pin and no stamp, the later updated_at wins — and it
// beats the rank the window holds, which is the whole point of putting it
// ahead of the tie-break's last step.
func TestSeparationLaterUpdatedAtWins(t *testing.T) {
	older := stamped("OLD", 0.9, "2026-01-01 00:00:00")
	newer := stamped("NEW", 0.5, "2026-06-01 00:00:00")

	res := run(t, &fakeRetriever{set: contradictingSet(older, newer)}, separationRequest())

	assertSeparated(t, res, "NEW", "OLD")
}

// Tie-break (d): an exact tie falls to the rank the window already holds — the
// first row in rank order stays, and nothing else about the two rows decided it.
func TestSeparationRankBreaksAnExactTie(t *testing.T) {
	first := stamped("AAA", 0.9, "2026-01-01 00:00:00")
	second := stamped("BBB", 0.8, "2026-01-01 00:00:00")

	res := run(t, &fakeRetriever{set: contradictingSet(first, second)}, separationRequest())

	assertSeparated(t, res, "AAA", "BBB")
}

// A chain A-B-C is NOT one component to winnow to a single row: the two ends
// do not contradict each other, so both stay and only the middle is dropped.
// The middle is dropped against the end kept first (the keep-priority winner),
// the only kept row it directly contradicts. The component rule this replaces
// winnowed the whole chain to one row, which over-dropped: C's only edge is to
// B, which is itself dropped, so C was withheld with nothing it contradicts
// left in the block, and the winner's conflicts_with named C — an edge that
// does not exist.
func TestChainKeepsBothEndsAndDropsTheMiddle(t *testing.T) {
	a := stamped("A1", 0.9, "2026-01-01 00:00:00")
	a.Pinned = true // A wins the tie-break, so it is kept first
	middle := stamped("B1", 0.8, "2026-03-01 00:00:00")
	c := stamped("C1", 0.7, "2026-02-01 00:00:00")
	set := setOf(a, middle, c)
	set.Edges = []memory.LinkEdge{
		{From: "A1", To: "B1", Relation: "contradicts", Strength: 1},
		{From: "B1", To: "C1", Relation: "contradicts", Strength: 1},
	}
	set.EdgesStatus = memory.EdgeStatus{Status: "ok"}

	res := run(t, &fakeRetriever{set: set}, separationRequest())

	// Both ends stay: C's only edge is to B, which is itself dropped.
	if got := itemIDs(res.Items); !eq(got, []string{"A1", "C1"}) {
		t.Fatalf("items = %v, want both ends A1 and C1", got)
	}
	// The middle is dropped against the one kept row it directly contradicts.
	var sawB bool
	for _, d := range res.Trace.Decisions {
		if d.ID != "B1" {
			continue
		}
		sawB = true
		if d.Stage != stageConflicts || d.Reason != "contradiction_separated" || d.Kept {
			t.Errorf("B1: decision = {stage:%q reason:%q kept:%v}, want a stage-5 drop", d.Stage, d.Reason, d.Kept)
		}
		if !eq(d.Against, []string{"A1"}) {
			t.Errorf("B1: Against = %v, want [A1]", d.Against)
		}
	}
	if !sawB {
		t.Errorf("no decision records withholding B1: %+v", res.Trace.Decisions)
	}
	// Each kept end names the middle it directly contradicts — and nothing else.
	if la := lineOf(t, res.Response, "A1"); !strings.Contains(la, "conflicts_with=`B1`") {
		t.Errorf("A1 must name the withheld middle: %q", la)
	}
	if lc := lineOf(t, res.Response, "C1"); !strings.Contains(lc, "conflicts_with=`B1`") {
		t.Errorf("C1 must name the withheld middle: %q", lc)
	}
	if strings.Contains(lineOf(t, res.Response, "A1"), "C1") || strings.Contains(lineOf(t, res.Response, "C1"), "A1") {
		t.Errorf("an end names the other end, which it does not contradict")
	}
	// The answer reports each real pair once: A1-B1 and B1-C1.
	joined := strings.Join(res.Notes, "\n")
	if got := strings.Count(joined, "contradicts pair recorded"); got != 2 {
		t.Errorf("a chain reported %d pair(s), want 2: %s", got, joined)
	}
	if !strings.Contains(joined, "A1 kept, B1 withheld") || !strings.Contains(joined, "C1 kept, B1 withheld") {
		t.Errorf("the answer note does not name both surviving pairs: %s", joined)
	}
}

// A triangle — three rows that all contradict each other — keeps one winner
// and drops the other two, each against the winner. This is the case the
// component rule was written for, and the greedy rule agrees with it.
func TestTriangleKeepsOneWinnerAndDropsTwo(t *testing.T) {
	a := stamped("A1", 0.9, "2026-01-01 00:00:00")
	a.Pinned = true
	b := stamped("B1", 0.8, "2026-02-01 00:00:00")
	c := stamped("C1", 0.7, "2026-03-01 00:00:00")
	set := setOf(a, b, c)
	set.Edges = []memory.LinkEdge{
		{From: "A1", To: "B1", Relation: "contradicts", Strength: 1},
		{From: "B1", To: "C1", Relation: "contradicts", Strength: 1},
		{From: "A1", To: "C1", Relation: "contradicts", Strength: 1},
	}
	set.EdgesStatus = memory.EdgeStatus{Status: "ok"}

	res := run(t, &fakeRetriever{set: set}, separationRequest())

	if got := itemIDs(res.Items); !eq(got, []string{"A1"}) {
		t.Fatalf("items = %v, want only the winner A1", got)
	}
	for _, loser := range []string{"B1", "C1"} {
		if got := renderedLines(res.Response, loser); len(got) != 0 {
			t.Errorf("the withheld side %s rendered %d line(s):\n%s", loser, len(got), res.Response)
		}
		var saw bool
		for _, d := range res.Trace.Decisions {
			if d.ID != loser {
				continue
			}
			saw = true
			if !eq(d.Against, []string{"A1"}) {
				t.Errorf("%s: Against = %v, want [A1]", loser, d.Against)
			}
		}
		if !saw {
			t.Errorf("no decision records withholding %s", loser)
		}
	}
	kept := lineOf(t, res.Response, "A1")
	if !strings.Contains(kept, "conflicts_with=`B1`,`C1`") {
		t.Errorf("the winner must name both rows it directly contradicts: %q", kept)
	}
}

// A star — B contradicts A and C, and A and C do not contradict each other —
// with B winning keeps B and drops both A and C, each against B.
func TestStarKeepsCenterAndDropsLeaves(t *testing.T) {
	center := stamped("B1", 0.8, "2026-02-01 00:00:00")
	center.Pinned = true
	a := stamped("A1", 0.9, "2026-01-01 00:00:00")
	c := stamped("C1", 0.7, "2026-03-01 00:00:00")
	set := setOf(a, center, c)
	set.Edges = []memory.LinkEdge{
		{From: "B1", To: "A1", Relation: "contradicts", Strength: 1},
		{From: "B1", To: "C1", Relation: "contradicts", Strength: 1},
	}
	set.EdgesStatus = memory.EdgeStatus{Status: "ok"}

	res := run(t, &fakeRetriever{set: set}, separationRequest())

	if got := itemIDs(res.Items); !eq(got, []string{"B1"}) {
		t.Fatalf("items = %v, want only the center B1", got)
	}
	for _, loser := range []string{"A1", "C1"} {
		var saw bool
		for _, d := range res.Trace.Decisions {
			if d.ID != loser {
				continue
			}
			saw = true
			if !eq(d.Against, []string{"B1"}) {
				t.Errorf("%s: Against = %v, want [B1]", loser, d.Against)
			}
		}
		if !saw {
			t.Errorf("no decision records withholding %s", loser)
		}
	}
	kept := lineOf(t, res.Response, "B1")
	if !strings.Contains(kept, "conflicts_with=`A1`,`C1`") {
		t.Errorf("the center must name both leaves it directly contradicts: %q", kept)
	}
}

// A row that contradicts two kept rows that do not contradict each other is
// dropped against BOTH, and its explain reason names both — each a row it
// really has a contradicts edge to.
func TestRowContradictingTwoKeptRowsIsDroppedAgainstBoth(t *testing.T) {
	k1 := stamped("K1", 0.9, "2026-01-01 00:00:00")
	k2 := stamped("K2", 0.8, "2026-01-01 00:00:00")
	d := stamped("D1", 0.7, "2026-01-01 00:00:00")
	set := setOf(k1, k2, d)
	set.Edges = []memory.LinkEdge{
		{From: "D1", To: "K1", Relation: "contradicts", Strength: 1},
		{From: "D1", To: "K2", Relation: "contradicts", Strength: 1},
	}
	set.EdgesStatus = memory.EdgeStatus{Status: "ok"}

	res := run(t, &fakeRetriever{set: set}, separationRequest())

	if got := itemIDs(res.Items); !eq(got, []string{"K1", "K2"}) {
		t.Fatalf("items = %v, want both kept rows K1 and K2", got)
	}
	var saw bool
	for _, dec := range res.Trace.Decisions {
		if dec.ID != "D1" {
			continue
		}
		saw = true
		if !eq(dec.Against, []string{"K1", "K2"}) {
			t.Errorf("D1: Against = %v, want [K1 K2]", dec.Against)
		}
	}
	if !saw {
		t.Fatalf("no decision records withholding D1: %+v", res.Trace.Decisions)
	}
	// explain names both kept rows D1 directly contradicts.
	req := separationRequest()
	req.Explain = true
	res = run(t, &fakeRetriever{set: set}, req)
	if res.Explain == nil {
		t.Fatal("explain: true produced no payload")
	}
	for _, r := range res.Explain.Rows {
		if r.ID == Token("D1") {
			if !strings.Contains(r.Reason, "K1") || !strings.Contains(r.Reason, "K2") {
				t.Errorf("explain reason for D1 = %q, want it to name both K1 and K2", r.Reason)
			}
		}
	}
}

// A scope-conflicting pair is not a contradiction: two true claims about two
// places. Both sides stay, and nothing marks or notes them.
func TestScopeConflictingPairIsKeptAndUnmarked(t *testing.T) {
	a := candidate("A1", "proj", "fact", "the database is postgres", 0.9)
	a.Scope = map[string]string{"environment": "production"}
	b := candidate("B1", "proj", "fact", "the database is sqlite", 0.8)
	b.Scope = map[string]string{"environment": "development"}

	res := run(t, &fakeRetriever{set: contradictingSet(a, b)}, separationRequest())

	if got := itemIDs(res.Items); !eq(got, []string{"A1", "B1"}) {
		t.Fatalf("items = %v, want both sides of a scope-conflicting pair", got)
	}
	if strings.Contains(res.Response, "conflicts_with") {
		t.Errorf("a scope-conflicting pair is marked:\n%s", res.Response)
	}
	if hasNote(res.Notes, "contradicts") {
		t.Errorf("a scope-conflicting pair is noted: %v", res.Notes)
	}
	for _, d := range res.Trace.Decisions {
		if d.Stage == stageConflicts {
			t.Errorf("a scope-conflicting pair produced a conflicts-stage decision: %+v", d)
		}
	}
}

// A side an earlier stage already dropped never reached stage 5, so there is no
// pair to separate: the survivor stays whole, is not marked, and no separation
// is invented for a row this stage never saw.
func TestPairWithOneSideDroppedEarlierIsNotSeparable(t *testing.T) {
	expired := "2020-01-01 00:00:00"
	live := stamped("KEEP", 0.9, "2026-06-01 00:00:00")
	gone := stamped("GONE", 0.8, "2026-01-01 00:00:00")
	gone.ValidUntil = &expired

	res := run(t, &fakeRetriever{set: contradictingSet(live, gone)}, separationRequest())

	if got := itemIDs(res.Items); !eq(got, []string{"KEEP"}) {
		t.Fatalf("items = %v, want only the live side", got)
	}
	for _, d := range res.Trace.Decisions {
		if d.Stage == stageConflicts {
			t.Errorf("a pair stage 5 never saw produced a conflicts-stage decision: %+v", d)
		}
	}
	if strings.Contains(res.Response, "conflicts_with") {
		t.Errorf("the survivor names a partner that was dropped before the stage ran:\n%s", res.Response)
	}
	if hasNote(res.Notes, "contradicts pair recorded") {
		t.Errorf("a pair that was never recorded is reported: %v", res.Notes)
	}
}

// An answer with no contradicts edges is what it was before separation existed:
// no marker, no note, no decision.
func TestAnswerWithoutContradictsEdgesIsUnchanged(t *testing.T) {
	set := setOf(stamped("A1", 0.9, "2026-01-01 00:00:00"), stamped("B1", 0.8, "2026-02-01 00:00:00"))
	set.EdgesStatus = memory.EdgeStatus{Status: "unavailable"}

	res := run(t, &fakeRetriever{set: set}, separationRequest())

	if got := itemIDs(res.Items); !eq(got, []string{"A1", "B1"}) {
		t.Fatalf("items = %v, want both rows untouched", got)
	}
	for _, id := range []string{"A1", "B1"} {
		if l := lineOf(t, res.Response, id); strings.Contains(l, "conflicts_with") {
			t.Errorf("%s grew a conflict marker: %q", id, l)
		}
	}
	if hasNote(res.Notes, "contradicts") {
		t.Errorf("a no-edge answer grew a contradicts note: %v", res.Notes)
	}
	if len(res.Trace.Decisions) != 0 {
		t.Errorf("a no-edge answer grew decisions: %+v", res.Trace.Decisions)
	}
}

// TestStarWhoseLeafWinsKeepsEveryLeaf pins the other half of the star: what a
// shape keeps follows keep priority, not the shape. A is pinned and C carries a
// later updated_at than the centre B, so the walk is A, C, B: A and C each have
// no kept neighbour when they are reached and stay, and B is withheld against
// both of them. The block holds both leaves and no centre.
func TestStarWhoseLeafWinsKeepsEveryLeaf(t *testing.T) {
	center := stamped("B1", 0.8, "2026-02-01 00:00:00")
	a := stamped("A1", 0.9, "2026-01-01 00:00:00")
	a.Pinned = true
	c := stamped("C1", 0.7, "2026-03-01 00:00:00")
	set := setOf(a, center, c)
	set.Edges = []memory.LinkEdge{
		{From: "B1", To: "A1", Relation: "contradicts", Strength: 1},
		{From: "B1", To: "C1", Relation: "contradicts", Strength: 1},
	}
	set.EdgesStatus = memory.EdgeStatus{Status: "ok"}

	res := run(t, &fakeRetriever{set: set}, separationRequest())

	if got := itemIDs(res.Items); !eq(got, []string{"A1", "C1"}) {
		t.Fatalf("items = %v, want both leaves A1 and C1", got)
	}
	var saw bool
	for _, d := range res.Trace.Decisions {
		if d.ID == "C1" && len(d.Against) > 0 {
			t.Errorf("C1 contradicts nothing kept, yet a decision names it against %v", d.Against)
		}
		if d.ID != "B1" {
			continue
		}
		saw = true
		if !eq(d.Against, []string{"A1", "C1"}) {
			t.Errorf("B1: Against = %v, want [A1 C1] (both kept leaves, rank order)", d.Against)
		}
	}
	if !saw {
		t.Errorf("no decision records withholding the centre B1")
	}
	if kept := lineOf(t, res.Response, "A1"); !strings.Contains(kept, "conflicts_with=`B1`") {
		t.Errorf("the winning leaf must name the centre it directly contradicts: %q", kept)
	}
	if kept := lineOf(t, res.Response, "C1"); !strings.Contains(kept, "conflicts_with=`B1`") {
		t.Errorf("the other leaf directly contradicts the withheld centre and must name it: %q", kept)
	}
}

// #945: the shape whose two rows SCOPE-CONFLICT, joined through a row that does
// not. A names environment=production, C names environment=development, and B
// names no scope at all, so both edges are live conflicts and A-C is not an
// edge. The greedy walk resolves it by which row wins keep priority, and in
// every case the scope-conflicting pair is never separated against EACH OTHER:
// a row is withheld only against a kept row it has a direct contradicts edge to,
// and A has no edge to C. Where an unscoped B wins, both scoped rows are withheld
// against B — through their own edges, which is a real contradiction on each.
//
//   A {environment: production} --contradicts-- B {} --contradicts-- C {environment: development}

// mixedScopeShape is the #945 shape, with winner pinned so exactly that row
// wins keep priority. With no pin, no verified_at and identical stamps, the rank
// the window holds would decide and the winner would always be A1, so the three
// winners cannot be told apart without the pin.
func mixedScopeShape(winner string) *memory.CandidateSet {
	rows := []memory.Candidate{
		scopedCandidate("A1", map[string]string{"environment": "production"}, 0.9),
		candidate("B1", "proj", "fact", "database configuration B1", 0.8),
		scopedCandidate("C1", map[string]string{"environment": "development"}, 0.7),
	}
	for i := range rows {
		if rows[i].ID == winner {
			rows[i].Pinned = true
		}
	}
	set := setOf(rows...)
	set.Edges = []memory.LinkEdge{
		{From: "A1", To: "B1", Relation: "contradicts", Strength: 1},
		{From: "B1", To: "C1", Relation: "contradicts", Strength: 1},
	}
	set.EdgesStatus = memory.EdgeStatus{Status: "ok"}
	return set
}

// conflictsWithTokens reads the ids a rendered line names in conflicts_with=. It
// parses the label rather than matching a substring, so a withheld id that
// happens to appear in a row's own content is not read as a marker.
func conflictsWithTokens(t *testing.T, line string) []string {
	t.Helper()
	at := strings.Index(line, "conflicts_with=")
	if at < 0 {
		return nil
	}
	label := line[at+len("conflicts_with="):]
	// Stop at the next marker or closing paren
	if end := strings.Index(label, " "); end >= 0 {
		label = label[:end]
	}
	if end := strings.Index(label, ")"); end >= 0 {
		label = label[:end]
	}
	var tokens []string
	for _, part := range strings.Split(label, ",") {
		part = strings.Trim(strings.TrimSpace(part), "`")
		if part != "" {
			tokens = append(tokens, part)
		}
	}
	return tokens
}

// contradictedByTokens reads the ids a rendered line names in contradicted_by=.
func contradictedByTokens(t *testing.T, line string) []string {
	t.Helper()
	at := strings.Index(line, "contradicted_by=")
	if at < 0 {
		return nil
	}
	label := line[at+len("contradicted_by="):]
	if end := strings.Index(label, " "); end >= 0 {
		label = label[:end]
	}
	if end := strings.Index(label, ")"); end >= 0 {
		label = label[:end]
	}
	var tokens []string
	for _, part := range strings.Split(label, ",") {
		part = strings.Trim(strings.TrimSpace(part), "`")
		if part != "" {
			tokens = append(tokens, part)
		}
	}
	return tokens
}

// assertMixedScopeWalk is the whole contract over one walk of the #945 shape:
// the stated kept rows stand; every withheld row is dropped against exactly the
// KEPT rows it has a direct contradicts edge to; no Against names a row the
// withheld row scope-conflicts with; and each kept line names only the rows it
// directly contradicts that were withheld — which never includes its
// scope-conflicting partner, because that is not an edge. Pinned rows that are
// the target of a contradicts edge from a withheld row also carry a
// contradicted_by marker naming the contradicting row.
func assertMixedScopeWalk(t *testing.T, res Result, set *memory.CandidateSet, wantKept []string) {
	t.Helper()
	if got := itemIDs(res.Items); !eq(got, wantKept) {
		t.Fatalf("items = %v, want %v", got, wantKept)
	}
	kept := make(map[string]bool, len(wantKept))
	for _, id := range wantKept {
		kept[id] = true
	}
	scopes := make(map[string]map[string]string, len(set.Rows))
	direct := make(map[string][]string, len(set.Rows))
	directed := make(map[string][]string, len(set.Rows)) // source -> targets
	for _, row := range set.Rows {
		scopes[row.ID] = row.Scope
	}
	for _, e := range set.Edges {
		direct[e.From] = append(direct[e.From], e.To)
		direct[e.To] = append(direct[e.To], e.From)
		directed[e.From] = append(directed[e.From], e.To)
	}
	// wantNames, in the rank order the window holds, is every row in ids that is
	// a direct contradicts neighbour of owner and stands on the stated side.
	wantNames := func(owner string, ids map[string]bool) []string {
		var want []string
		for _, row := range set.Rows {
			if !ids[row.ID] || !contains(direct[owner], row.ID) {
				continue
			}
			want = append(want, row.ID)
		}
		return want
	}
	// wantContradictedBy returns the rows that contradict the owner (owner is target)
	// and are withheld.
	wantContradictedBy := func(owner string, withheld map[string]bool) []string {
		var want []string
		for src, targets := range directed {
			if withheld[src] {
				for _, tgt := range targets {
					if tgt == owner {
						want = append(want, src)
					}
				}
			}
		}
		return want
	}
	withheld := map[string]bool{}
	for _, row := range set.Rows {
		if len(direct[row.ID]) > 0 && !kept[row.ID] {
			withheld[row.ID] = true
		}
	}

	var sawDrop bool
	for _, d := range res.Trace.Decisions {
		if d.Stage != stageConflicts {
			continue
		}
		if kept[d.ID] {
			t.Errorf("a kept row carries a conflicts-stage drop decision: %+v", d)
			continue
		}
		sawDrop = true
		if len(d.Against) == 0 {
			t.Errorf("%s is withheld with no decision naming what it was withheld against: %+v", d.ID, d)
		}
		for _, against := range d.Against {
			if !kept[against] {
				t.Errorf("%s: Against names %s, which was not kept", d.ID, against)
			}
			if !contains(direct[d.ID], against) {
				t.Errorf("%s: Against names %s, which it has no contradicts edge to", d.ID, against)
			}
			if memory.ScopesConflict(scopes[d.ID], scopes[against]) {
				t.Errorf("%s is withheld against %s, which it scope-conflicts with (scopes %v vs %v)", d.ID, against, scopes[d.ID], scopes[against])
			}
		}
	}
	if got, want := sawDrop, len(withheld) > 0; got != want {
		t.Errorf("a conflicts-stage drop was recorded = %v, want %v (withheld rows: %v)", got, want, withheld)
	}
	for _, it := range res.Items {
		want := wantNames(it.ID, withheld)
		if !eq(it.ConflictsWith, want) {
			t.Errorf("%s: ConflictsWith = %v, want %v — only the withheld rows it directly contradicts", it.ID, it.ConflictsWith, want)
		}
		line := lineOf(t, res.Response, it.ID)
		var wantTokens []string
		for _, id := range want {
			wantTokens = append(wantTokens, Token(id))
		}
		if got := conflictsWithTokens(t, line); !eq(got, wantTokens) {
			t.Errorf("%s: the rendered line names %v, want %v: %q", it.ID, got, wantTokens, line)
		}
		// Check contradicted_by marker for pinned rows
		if it.Pinned {
			wantContradicted := wantContradictedBy(it.ID, withheld)
			if got := contradictedByTokens(t, line); !eq(got, wantContradicted) {
				t.Errorf("%s: contradicted_by = %v, want %v: %q", it.ID, got, wantContradicted, line)
			}
		}
		for _, partner := range want {
			if memory.ScopesConflict(scopes[it.ID], scopes[partner]) {
				t.Errorf("the kept %s names %s, which it scope-conflicts with", it.ID, partner)
			}
		}
	}
	// The pair #945 is about, stated directly: two rows that scope-conflict and
	// share no edge may never be named against each other on any surface — not
	// as a drop and not as a partner — so neither rendered line may name the
	// other.
	for _, a := range res.Items {
		for _, b := range res.Items {
			if a.ID == b.ID || !memory.ScopesConflict(scopes[a.ID], scopes[b.ID]) {
				continue
			}
			if strings.Contains(lineOf(t, res.Response, a.ID), Token(b.ID)) {
				t.Errorf("%s names %s, a scope-conflicting row it does not contradict:\n%s", a.ID, b.ID, res.Response)
			}
		}
	}
}

// contains reports whether ids holds id.
func contains(ids []string, id string) bool {
	for _, have := range ids {
		if have == id {
			return true
		}
	}
	return false
}

// A wins: A is kept, B is withheld against A — the one kept row it directly
// contradicts — and C is kept, because its only edge is to B, which is itself
// withheld. Both scope-conflicting rows stand, and neither names the other.
func TestMixedScopeWalkWhenAPinnedWins(t *testing.T) {
	set := mixedScopeShape("A1")

	res := run(t, &fakeRetriever{set: set}, separationRequest())

	assertMixedScopeWalk(t, res, set, []string{"A1", "C1"})
	if got := renderedLines(res.Response, "B1"); len(got) != 0 {
		t.Errorf("the withheld B1 rendered %d line(s):\n%s", len(got), res.Response)
	}
}

// B wins: the unscoped row is kept and BOTH scoped rows are withheld, each
// against B through its own direct edge. A and C are never separated against
// each other — the pair #945 says must be untouched is untouched — and each
// drop is a real contradiction on a real edge.
func TestMixedScopeWalkWhenTheUnscopedRowWins(t *testing.T) {
	set := mixedScopeShape("B1")

	res := run(t, &fakeRetriever{set: set}, separationRequest())

	assertMixedScopeWalk(t, res, set, []string{"B1"})
	for _, loser := range []string{"A1", "C1"} {
		if got := renderedLines(res.Response, loser); len(got) != 0 {
			t.Errorf("the withheld %s rendered %d line(s):\n%s", loser, len(got), res.Response)
		}
	}
}

// C wins: symmetric to A. C is kept, B is withheld, and A is kept — its only
// edge is to B, which is itself withheld. A and C both stand.
func TestMixedScopeWalkWhenCPinnedWins(t *testing.T) {
	set := mixedScopeShape("C1")

	res := run(t, &fakeRetriever{set: set}, separationRequest())

	assertMixedScopeWalk(t, res, set, []string{"A1", "C1"})
	if got := renderedLines(res.Response, "B1"); len(got) != 0 {
		t.Errorf("the withheld B1 rendered %d line(s):\n%s", len(got), res.Response)
	}
}
