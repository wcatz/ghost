package assemble

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The abstention contract of #580: retrieval stops being a list that is either
// long or literally empty. A result carries a verdict (answerable, weak, empty),
// a reason from a closed vocabulary, a machine line a harness can parse and a
// human sentence that tells it whether to rely on the rows. The tests below pin
// each rule separately, because the whole point of the change is that the
// interesting cases are distinguishable from one another.

// ranked is a candidate with an explicit keyword rank, which is what Arm A
// evaluates. -1 is the retriever's "the keyword leg did not retrieve this row".
func ranked(id string, ftsRank int, content string) memory.Candidate {
	c := candidate(id, "proj", "fact", content, 0.5)
	c.FTSRank = ftsRank
	return c
}

// vectorScored is a candidate carrying the vector leg's cosine, for the arm-B
// half of the floor.
func vectorScored(id string, ftsRank int, score float64, content string) memory.Candidate {
	c := ranked(id, ftsRank, content)
	c.VectorScore = score
	return c
}

// hybridSet is a candidate set whose two applicable legs both ran and answered.
// It is the only state in which a floor verdict is made from both arms.
//
// The coverage flags mirror what memory.Candidates reports rather than what a
// test would like them to be: the keyword leg vouches for itself when it was not
// cut off at the window, and the vector leg vouches for nothing until its
// expected/indexed/unembedded counts are reconciled. A helper that set both true
// would let an absence claim pass on a corpus the real leg cannot vouch for.
func hybridSet(rows ...memory.Candidate) *memory.CandidateSet {
	set := setOf(rows...)
	set.Legs = map[string]memory.LegStatus{
		"fts":    {Applicable: true, Attempted: true, Available: true, CoverageComplete: true},
		"vector": {Applicable: true, Attempted: true, Available: true},
	}
	return set
}

// weakSet is a candidate set whose two applicable legs both ran and answered,
// holding rows that cleared neither arm — the one state in which a result can
// be weak. A real store cannot produce it from a query with a keyword hit (the
// best keyword row is always rank 0), which is why the shape is built here.
func weakSet(rows ...memory.Candidate) *memory.CandidateSet {
	return hybridSet(rows...)
}

// ftsOnlySet is a candidate set whose vector leg was not applicable, so the
// request had no vector to compare against.
func ftsOnlySet(rows ...memory.Candidate) *memory.CandidateSet {
	set := setOf(rows...)
	set.Legs = map[string]memory.LegStatus{
		"fts":    {Applicable: true, Attempted: true, Available: true, CoverageComplete: true},
		"vector": {Applicable: false},
	}
	return set
}

// hybridRequest is a request that reaches both legs.
func hybridRequest() Request {
	req := baseRequest()
	req.Condition = CondHybrid
	req.QueryVec = []float32{0.1, 0.2, 0.3}
	return req
}

// TestAStrongKeywordMatchStaysAnswerable is #580's regression test. The floor
// must be a no-op for a clear keyword hit: withholding a row the search found
// on the exact words the caller asked for is worse than returning a weak one.
func TestAStrongKeywordMatchStaysAnswerable(t *testing.T) {
	set := hybridSet(ranked("A1", 0, "helm deploy runs the release job"))

	res := run(t, &fakeRetriever{set: set}, hybridRequest())

	if res.Outcome != OutcomeAnswerable {
		t.Errorf("outcome = %q, want answerable: rank 0 is a clear keyword hit", res.Outcome)
	}
	if res.Reason != "floor_met" {
		t.Errorf("reason = %q, want floor_met", res.Reason)
	}
	if res.Abstention != "" {
		t.Errorf("abstention = %q, want none: an answerable result withholds nothing", res.Abstention)
	}
	if !strings.Contains(res.Response, "outcome=answerable") {
		t.Errorf("response carries no machine verdict:\n%s", res.Response)
	}
	if strings.Contains(res.Response, "do not rely") || strings.Contains(res.Response, "trustworthy") {
		t.Errorf("an answerable result must not carry abstention copy:\n%s", res.Response)
	}
}

// TestAnEmptyStoreStillSaysNothingMatched: over complete coverage the absence
// sentence is exactly right, and the new wording must not swallow it. A caller
// that cannot tell the two cases apart has learned nothing.
func TestAnEmptyStoreStillSaysNothingMatched(t *testing.T) {
	// A keyword-only request whose leg answered and was not truncated searched
	// everything it could see, which is the only state in which absence is a
	// statement about the store.
	res := run(t, &fakeRetriever{set: ftsOnlySet()}, baseRequest())

	if res.Reason != "no_candidates" {
		t.Fatalf("reason = %q, want no_candidates", res.Reason)
	}
	if !strings.Contains(res.Response, "No matching memories found") {
		t.Errorf("response = %q, want the absence sentence for an empty store", res.Response)
	}
}

// TestAConfiguredCosineNeverRendersAsTheTokenOffAssigns: the machine line gives
// `abstain_cosine` three states, and a number means the arm is in force. Three
// decimals is the field's width, but a threshold below 0.0005 rounds to
// `0.000` — the same text a reader parses as "a threshold of zero", which every
// row clears and which the glossary hands to `off`. An arm in force that renders
// as the number `off` means is a line that says the opposite of what happened.
func TestAConfiguredCosineNeverRendersAsTheTokenOffAssigns(t *testing.T) {
	set := hybridSet(
		vectorScored("A1", 9, 0.0001, "database configuration pooling"),
		vectorScored("A2", 8, 0.0002, "database configuration retries"),
	)
	req := hybridRequest()
	req.AbstainCosine = 0.0004

	res := run(t, &fakeRetriever{set: set}, req)

	if !res.Trace.Floors.VectorArmOn {
		t.Fatalf("precondition: a cosine of 0.0004 did not arm the vector leg: %+v", res.Trace.Floors)
	}
	if res.Outcome != OutcomeWeak {
		t.Fatalf("precondition: outcome = %q, want weak: only a weak result proves the arm ran", res.Outcome)
	}
	if strings.Contains(res.Machine, "abstain_cosine=0.000 ") || strings.HasSuffix(res.Machine, "abstain_cosine=0.000") {
		t.Errorf("machine line = %q, which renders a threshold in force as the number `off` is given", res.Machine)
	}
	if !strings.Contains(res.Machine, "abstain_cosine=0.0004") {
		t.Errorf("machine line = %q, want the configured threshold at a precision that states it", res.Machine)
	}
}

// TestTheFloorsRecordTheThresholdTheyUsed: a reader that has to re-derive a
// verdict cannot, so the trace carries the exact numbers that produced it.
func TestTheFloorsRecordTheThresholdTheyUsed(t *testing.T) {
	req := hybridRequest()
	req.AbstainCosine = 0.62

	res := run(t, &fakeRetriever{set: hybridSet(ranked("A1", 0, "pooling"))}, req)

	f := res.Trace.Floors
	if f.FTSRankMax != 3 {
		t.Errorf("FTSRankMax = %d, want 3", f.FTSRankMax)
	}
	if f.VectorCosine != 0.62 {
		t.Errorf("VectorCosine = %v, want the caller's 0.62", f.VectorCosine)
	}
}

// TestAFractionOfATokenIsStillAToken: rounding down would report a short memory
// as free, and a caller budgeting from this would overspend.
func TestAFractionOfATokenIsStillAToken(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 5

	res := run(t, &fakeRetriever{set: ftsOnlySet(candidate("A1", "proj", "fact", "hi", 0.9))}, req)

	if res.Items[0].Tokens != 1 {
		t.Errorf("tokens = %d for a 2-byte memory, want 1", res.Items[0].Tokens)
	}
}

// TestTheResponseFitPassReportsAllOverBudget: a response that fits no row at
// all is not an absence claim, it is a budget that could not hold the answer.
// It runs a query request under a Budget.Measure, a combination no production
// caller makes: only a Measure caller's fit can still empty a block (#1483).
func TestTheResponseFitPassReportsAllOverBudget(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 5
	set := ftsOnlySet(
		ranked("A1", 0, strings.Repeat("first row content ", 12)),
		ranked("A2", 1, strings.Repeat("second row content ", 12)),
	)

	full := run(t, &fakeRetriever{set: set}, req)
	if full.Outcome != OutcomeAnswerable || len(full.Items) != 2 {
		t.Fatalf("precondition: the uncapped result was %q with %d items, want answerable with 2",
			full.Outcome, len(full.Items))
	}

	_, boundary := roomiestEmptyCap(t, set, req, full)
	if len(boundary.Items) != 0 {
		t.Fatalf("no cap in (0, %d) emptied the block: the pass cannot report all_over_budget, "+
			"so a response that fits no row would be an error instead", full.Bytes)
	}
	if boundary.Outcome != OutcomeEmpty || boundary.Reason != reasonAllOverBudget {
		t.Errorf("outcome/reason = %q/%q, want empty/all_over_budget", boundary.Outcome, boundary.Reason)
	}
	if !strings.Contains(boundary.Response, "response budget") {
		t.Errorf("response = %q, want the sentence that says the budget is what removed the rows", boundary.Response)
	}
}

// TestTheBudgetCopyNamesTheCapRatherThanAnUnreachableLimit: the byte cap is
// server-side — no tool argument reaches it — so advice to "raise the limit"
// sends a caller after a knob it does not have. The number, and the action the
// caller does have, are what the sentence has to carry.
// It runs a query request under a Budget.Measure, a combination no production
// caller makes: only a Measure caller's fit can still empty a block (#1483).
func TestTheBudgetCopyNamesTheCapRatherThanAnUnreachableLimit(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 5
	set := ftsOnlySet(
		ranked("A1", 0, strings.Repeat("first row content ", 12)),
		ranked("A2", 1, strings.Repeat("second row content ", 12)),
	)
	full := run(t, &fakeRetriever{set: set}, req)

	capAt, res := roomiestEmptyCap(t, set, req, full)

	if res.Reason != reasonAllOverBudget {
		t.Fatalf("precondition: reason = %q, want all_over_budget", res.Reason)
	}
	if strings.Contains(res.Abstention, "Raise the limit") {
		t.Errorf("the sentence tells the caller to raise a limit no tool argument reaches: %q", res.Abstention)
	}
	if !strings.Contains(res.Abstention, fmt.Sprintf("%d", capAt)) {
		t.Errorf("the sentence does not name the byte cap the caller cannot change: %q", res.Abstention)
	}
}

// TestTheResponseFitSentenceNamesTheCapAndTheOtherStage: the fit pass has its
// own `all_over_budget` branch, and it was the one empty sentence attributing
// every removed row to the byte cap. A set that validity emptied and that the
// fit pass then emptied renders it above a breakdown naming both stages, which
// is the sentence/note contradiction the other four branches do not have — and
// unlike them it needs no large cap to reach, only a caller that sets
// `Budget.MaxBytes` itself.
// It runs a query request under a Budget.Measure, a combination no production
// caller makes: only a Measure caller's fit can still empty a block (#1483).
func TestTheResponseFitSentenceNamesTheCapAndTheOtherStage(t *testing.T) {
	expired := "2020-01-01 00:00:00"
	rows := []memory.Candidate{
		ranked("A1", 0, strings.Repeat("first row content ", 120)),
		ranked("A2", 1, strings.Repeat("second row content ", 120)),
	}
	gone := ranked("A3", 0, "an expired row")
	gone.ValidUntil = &expired
	set := ftsOnlySet(append(rows, gone)...)

	req := baseRequest()
	req.Budget.MaxItems = 5
	full := run(t, &fakeRetriever{set: set}, req)
	_, res := roomiestEmptyCap(t, set, req, full)

	if res.Reason != reasonAllOverBudget {
		t.Fatalf("precondition: reason = %q, want all_over_budget", res.Reason)
	}
	if !strings.Contains(res.Abstention, "response budget") {
		t.Fatalf("precondition: the sentence is not the response-fit one, so this fixture cannot test "+
			"its claim: %q", res.Abstention)
	}
	if !hasNote(res.Notes, "validity 1") {
		t.Fatalf("precondition: the breakdown does not name the other stage, so the sentence has only "+
			"one cause to account for and the test would pass for the wrong reason: %v", res.Notes)
	}
	if !strings.Contains(res.Abstention, perStageNote) {
		t.Errorf("the sentence names one stage while the note names two, and does not say so: %q",
			res.Abstention)
	}
}

// TestTheStagePointerDisappearsWithTheNoteItPointsAt: perStageNote is a pointer
// to a note the response-fit pass is allowed to cut, and it is cut from the end
// so the breakdown — which leads the list — is the last one to go. Under a cap
// tight enough to reach it, an answer would read "The note below breaks the
// removals down per stage." with nothing below, which is the checkability the
// sentence exists to promise, gone. The pointer has to leave with its note.
//
// The block is emptied by VALIDITY, not by the fit: a query answer the fit
// started with rows in keeps its first row (#1483), so the notes it cuts are
// only ever an answer that was empty before the fit ran.
func TestTheStagePointerDisappearsWithTheNoteItPointsAt(t *testing.T) {
	expired := "2020-01-01 00:00:00"
	var rows []memory.Candidate
	for i, id := range []string{"A1", "A2", "A3"} {
		r := ranked(id, i, "an expired row")
		r.ValidUntil = &expired
		rows = append(rows, r)
	}
	set := ftsOnlySet(rows...)
	// A SECOND note, and that is the point of the fixture. The tail cut takes from
	// the end, so with only the breakdown in the list one cut removes the note the
	// pointer names and the two states are indistinguishable. The retrieval-failure
	// note sorts after it, so a one-note cut here leaves the breakdown in place —
	// the case a pointer tested by cut COUNT gets wrong, dropping the promise off
	// an answer that still carries it. It does not change the reason: rows were
	// found and removed, so emptyReason names the stage that removed them.
	set.Legs["vector"] = memory.LegStatus{Applicable: true, Attempted: true, Available: false, Err: "connection refused"}

	req := baseRequest()
	req.Budget.MaxItems = 5
	full := run(t, &fakeRetriever{set: set}, req)
	if full.Reason != reasonAllInvalid {
		t.Fatalf("precondition: reason = %q, want all_invalid", full.Reason)
	}
	if !hasNote(full.Notes, "validity 3") || len(full.Notes) < 2 {
		t.Fatalf("precondition: the uncut answer does not carry the breakdown and a second note, so no "+
			"cap in this fixture can test losing one without the other: %v", full.Notes)
	}

	// The cap is swept rather than guessed at, and the sweep has to reach all
	// three states (both notes, only the breakdown, no notes) or it proves
	// nothing. Across the band the pointer is present exactly when the breakdown is.
	both, breakdownOnly, none := 0, 0, 0
	for cap := full.Bytes; cap > full.Bytes/2; cap-- {
		req.Budget.MaxBytes = cap
		got, err := Run(context.Background(), &fakeRetriever{set: set}, req)
		if err != nil || len(got.Items) != 0 {
			continue
		}
		switch breakdown := hasNote(got.Notes, "validity 3"); {
		case breakdown && len(got.Notes) > 1:
			both++
		case breakdown:
			breakdownOnly++
		default:
			none++
		}
		// The exact pairing, stated on the note the pointer names rather than on
		// the note COUNT: a second note surviving is not a reason to drop it.
		if strings.Contains(got.Abstention, perStageNote) != hasNote(got.Notes, "validity 3") {
			t.Errorf("cap %d: the sentence's pointer (%t) and the breakdown it names (%t) disagree: %q",
				cap, strings.Contains(got.Abstention, perStageNote), hasNote(got.Notes, "validity 3"),
				got.Abstention)
		}
		if !strings.Contains(got.Abstention, "out of date") {
			t.Errorf("cap %d: the sentence lost its cause along with the pointer: %q", cap, got.Abstention)
		}
	}
	if both == 0 || breakdownOnly == 0 || none == 0 {
		t.Fatalf("the sweep never reached all three states (both=%d breakdownOnly=%d none=%d), so it "+
			"did not test the transition the pointer has to follow", both, breakdownOnly, none)
	}
}

// roomiestEmptyCap finds the largest byte cap whose answer still holds no row,
// with the result it produced.
//
// It scans downward rather than bisecting, because the property is not monotone
// in the cap and a bisection over a non-monotone predicate converges to the
// wrong answer without ever failing. There are three bands: above the full render
// everything is admitted, in a band between the empty envelope's size and a
// one-row render's size no row is admitted, and below the empty envelope Run
// refuses the block outright. Monotonicity holds only within the first band, and
// the band a caller cares about is the third.
//
// The ROOMIEST end is returned, and only that end, for two reasons. The cap is
// measured rather than written down because it moves with every sentence the
// envelope carries — including the cap number the budget copy quotes — so a
// hard-coded figure would go stale the first time a word changed. And a test
// asserting that the diagnostic notes SURVIVED needs room for them: the
// breakdown note is itself part of the envelope it explains, so at the tight end
// the pass drops the very note such a test is checking for. A caller that needs
// the tightest cap does not need this helper — it gets whichever end fits, and
// the verdict is the same either way.
//
// The sweep runs under rowsOnlyMeasure. A query answer measured against the
// search envelope never empties any more: its first row is kept whole however
// far over the cap it is (#1483). A caller's own Budget.Measure still cuts down
// to none (FramingCeiling), so that is the path an emptied block is reached on.
func roomiestEmptyCap(t *testing.T, set *memory.CandidateSet, req Request, full Result) (int, Result) {
	t.Helper()
	if req.Budget.Measure == nil {
		req.Budget.Measure = rowsOnlyMeasure
	}
	for cap := full.Bytes; cap > 0; cap-- {
		req.Budget.MaxBytes = cap
		got, err := Run(context.Background(), &fakeRetriever{set: set}, req)
		if err == nil && len(got.Items) == 0 {
			return cap, got
		}
	}
	t.Fatalf("no cap in (0, %d) emptied the block, so the pass cannot report all_over_budget", full.Bytes)
	return 0, Result{}
}

// rowsOnlyMeasure is a Budget.Measure that counts the rendered rows alone, the
// shape a session-start block with no frame measures. It is what the tests of an
// emptied block run under, because only a Measure caller's fit can still cut the
// last row.
func rowsOnlyMeasure(items []Item, _ *Trace) int {
	n := 0
	for _, it := range items {
		n += len(it.Line()) + 1
	}
	return n
}

// TestAResponseThatCannotFitIsAnError: the empty envelope still carries the
// verdict, the reason and the machine line. A cap below that is not an empty
// answer — it is a caller asking for less than an answer costs, and reporting
// `empty` would claim the store had nothing to say.
func TestAResponseThatCannotFitIsAnError(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxBytes = 20

	_, err := Run(context.Background(), &fakeRetriever{set: ftsOnlySet()}, req)

	if !errors.Is(err, ErrResponseBudgetExceeded) {
		t.Fatalf("Run error = %v, want ErrResponseBudgetExceeded", err)
	}
}

// TestAFitPassEmptyStillExplainsWhatRemovedTheRows: the per-stage breakdown is
// what makes an empty answer's leading sentence checkable, and the response-fit
// pass is the one thing that empties a set without going through stage 9. If it
// shrinks the admitted items but not the surviving rows, the breakdown's own gate
// stays false and the note silently disappears — on exactly the case this
// post-pass introduces.
// It runs a query request under a Budget.Measure, a combination no production
// caller makes: only a Measure caller's fit can still empty a block (#1483).
func TestAFitPassEmptyStillExplainsWhatRemovedTheRows(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 5
	// Rows big enough that a one-row render is far larger than an empty one, so
	// the zero-item band is wide and the breakdown note fits inside it. With small
	// rows the band is a byte wide and the pass has no room for the note at any
	// cap in it, which would make this test pass or fail on the fixture's size
	// rather than on the derivation.
	set := ftsOnlySet(
		ranked("A1", 0, strings.Repeat("first row content ", 40)),
		ranked("A2", 1, strings.Repeat("second row content ", 40)),
	)
	full := run(t, &fakeRetriever{set: set}, req)

	// The roomiest cap that still admits nothing, which is what roomiestEmptyCap
	// returns: the breakdown note is part of the envelope it explains, so at the
	// tight end the pass drops the very note this asserts on. That is the pass
	// working as designed; the defect pinned here is the note never being
	// derived at all.
	capAt, res := roomiestEmptyCap(t, set, req, full)

	if len(res.Items) != 0 {
		t.Fatalf("precondition: %d items, want none", len(res.Items))
	}
	if !hasNote(res.Notes, "none reached the answer") {
		t.Errorf("notes %v do not explain that rows were removed, so the sentence above them is "+
			"not checkable:\n%s", res.Notes, res.Response)
	}
	if !hasNote(res.Notes, "response_fit 2") {
		t.Errorf("the breakdown does not count the pass that removed them (cap %d): %v", capAt, res.Notes)
	}
}

// TestTheResponseFitEmptyNamesTheByteCap: the response-fit producer of
// `all_over_budget`. This one really was the response cap, so the sentence quotes
// it and says the one thing the caller can actually do.
// It runs a query request under a Budget.Measure, a combination no production
// caller makes: only a Measure caller's fit can still empty a block (#1483).
func TestTheResponseFitEmptyNamesTheByteCap(t *testing.T) {
	req := baseRequest()
	req.Budget.MaxItems = 5
	set := ftsOnlySet(
		ranked("A1", 0, strings.Repeat("first row content ", 40)),
		ranked("A2", 1, strings.Repeat("second row content ", 40)),
	)
	full := run(t, &fakeRetriever{set: set}, req)

	capAt, res := roomiestEmptyCap(t, set, req, full)

	if res.Reason != reasonAllOverBudget {
		t.Fatalf("precondition: reason = %q, want all_over_budget", res.Reason)
	}
	if strings.Contains(res.Abstention, "Raise the limit") {
		t.Errorf("the sentence tells the caller to raise a limit no tool argument reaches: %q", res.Abstention)
	}
	if !strings.Contains(res.Abstention, fmt.Sprintf("%d", capAt)) {
		t.Errorf("the sentence does not name the byte cap the caller cannot change: %q", res.Abstention)
	}
}

// TestAWeakFilteredAnswerKeepsTheWindowCaveat: the caveat says further matches
// may exist past the window, which is the one hint a caller needs when the
// result is short AND untrustworthy. It reached every non-empty filtered answer
// before the verdict landed, so a weak one losing it is a regression on exactly
// the outcome where the hint matters most.
func TestAWeakFilteredAnswerKeepsTheWindowCaveat(t *testing.T) {
	req := hybridRequest()
	req.Scope = map[string]string{"environment": "production"}

	res := run(t, &fakeRetriever{set: weakSet(weakScoredRow("A1"))}, req)

	if res.Outcome != OutcomeWeak {
		t.Fatalf("precondition: outcome = %q, want weak", res.Outcome)
	}
	if !strings.Contains(res.Response, "scope filter") {
		t.Errorf("a weak filtered answer lost the window caveat:\n%s", res.Response)
	}
}

// weakScoredRow is a row the keyword leg retrieved well outside Arm A's window,
// in a set whose vector leg answered.
func weakScoredRow(id string) memory.Candidate {
	c := ranked(id, 6, "the reconciliation ledger closes nightly")
	c.VectorRank = 0
	c.VectorScore = 0.44
	return c
}

// TestTheSeamItselfRefusesAnUnusableCosine: config refuses NaN, Inf, a negative
// and anything above 1 on both of ITS paths, and then the seam accepted all four
// — which is the wrong place for the only guard. Run is the package's exported
// entry point, and the two silent failures are the ones config's refusal exists
// to prevent: a negative reads as OFF and the line tells a user who set a floor
// that they have none, and anything above 1 arms a threshold no row's cosine can
// reach, so every result outside the keyword arm comes back weak with nothing on
// the line to tell it from a measured verdict.
func TestTheSeamItselfRefusesAnUnusableCosine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value float32
	}{
		{"negative", -0.5},
		{"above one", 1.5},
		{"infinite", float32(math.Inf(1))},
		{"negative infinite", float32(math.Inf(-1))},
		// NaN is the one a YAML file can produce and no comparison catches by
		// accident: it compares false against 0, so it arms nothing and reads
		// as OFF — a floor the user set that silently does not exist.
		{"NaN", float32(math.NaN())},
	} {
		set := hybridSet(vectorScored("A1", 0, 0.9, "database configuration pooling"))
		_, err := Run(context.Background(), &fakeRetriever{set: set}, func() Request {
			r := hybridRequest()
			r.AbstainCosine = tc.value
			return r
		}())
		if err == nil {
			t.Errorf("%s cosine: Run accepted a value config.cosineValue refuses", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), "AbstainCosine") {
			t.Errorf("%s cosine: the error does not name the field to fix: %v", tc.name, err)
		}
	}
	// Zero and one are not refusals: 0 is the shipped OFF and 1 is the largest
	// cosine there is, which a confident row clears.
	for _, ok := range []float32{0, 1} {
		set := hybridSet(vectorScored("A1", 0, 0.9, "database configuration pooling"))
		_, err := Run(context.Background(), &fakeRetriever{set: set}, func() Request {
			r := hybridRequest()
			r.AbstainCosine = ok
			return r
		}())
		if err != nil {
			t.Errorf("cosine %v: Run refused a value in range: %v", ok, err)
		}
	}
}

// TestTheTraceCarriesTheBoundedNotes: the explain projection reads the notes
// from the trace, so the fit pass's re-derivation has to land there too or an
// explanation would show notes the answer dropped.
func TestTheTraceCarriesTheBoundedNotes(t *testing.T) {
	set := ftsOnlySet()
	set.Legs["fts"] = memory.LegStatus{Applicable: true, Attempted: true, Available: false, Err: "no such table"}

	res := run(t, &fakeRetriever{set: set}, baseRequest())

	if !hasNote(res.Trace.Notes, "no such table") {
		t.Errorf("trace notes %v do not carry what Result.Notes carries: %+v", res.Trace.Notes, res.Notes)
	}
}

// TestTheWindowCaveatStillNamesAWideShortFilteredAnswer: the other half. A
// windowed search that came back short on its own has no other explanation, and
// the caveat is the only thing that says so.
func TestTheWindowCaveatStillNamesAWideShortFilteredAnswer(t *testing.T) {
	set := ftsOnlySet(ranked("A1", 0, "a row of text"))
	req := baseRequest()
	req.Budget.MaxItems = 5
	req.Category = "fact"

	res := run(t, &fakeRetriever{set: set}, req)

	if !strings.Contains(res.Response, "finite search window") {
		t.Errorf("a short filtered answer with no trim lost the window caveat:\n%s", res.Response)
	}
	if !strings.Contains(res.Response, "raise the limit") {
		t.Errorf("the caveat does not name the action that can help here:\n%s", res.Response)
	}
}

// TestTheMachineLineDoesNotPrintAThresholdItNeverApplied: `abstain_cosine` on the
// verdict line is a statement about the thresholds that PRODUCED the verdict. A
// user who sets context.abstain_cosine on a machine with no working embedder gets
// a line reading `abstain_cosine=0.620` beside a verdict that never compared it and
// `legs=vector:not_run`, which reads as though the cosine cleared a row it was
// never compared against.
func TestTheMachineLineDoesNotPrintAThresholdItNeverApplied(t *testing.T) {
	notRun := ftsOnlySet(ranked("A1", 0, "database configuration pooling"))
	notRun.Legs["vector"] = memory.LegStatus{Applicable: true, Attempted: false}
	req := baseRequest()
	req.AbstainCosine = 0.62

	res := run(t, &fakeRetriever{set: notRun}, req)

	// A rank-0 keyword hit clears arm A whatever the vector leg did, so this is
	// floor_met now — the point of the assertions below is the LINE, not the
	// reason.
	if res.Outcome != OutcomeAnswerable || res.Reason != reasonFloorMet {
		t.Fatalf("precondition: outcome/reason = %q/%q, want answerable/floor_met", res.Outcome, res.Reason)
	}
	if strings.Contains(res.Machine, "0.620") {
		t.Errorf("machine line = %q, prints a cosine the run never compared a row against", res.Machine)
	}
	if !strings.Contains(res.Machine, "abstain_cosine=not_applied") {
		t.Errorf("machine line = %q, want a configured-but-unused arm distinguishable from an "+
			"unconfigured one", res.Machine)
	}
	if !res.Trace.Floors.VectorArmOn {
		t.Error("VectorArmOn is false, so the trace would report the request as unconfigured when it was not")
	}
	if res.Trace.Floors.VectorApplied {
		t.Error("VectorApplied is true for a run whose vector leg never executed")
	}

	// The three states stay distinct: unconfigured, configured-and-unused, and
	// applied. Collapsing the first two tells a reader who set the key that it did
	// nothing; collapsing the last into the first tells a reader who did not that
	// a threshold exists.
	if line := run(t, &fakeRetriever{set: ftsOnlySet(ranked("A1", 0, "x"))}, baseRequest()); !strings.Contains(line.Machine, "abstain_cosine=off") {
		t.Errorf("an unconfigured arm renders %q, want abstain_cosine=off", line.Machine)
	}
	appliedReq := hybridRequest()
	appliedReq.AbstainCosine = 0.62
	applied := run(t, &fakeRetriever{set: hybridSet(ranked("A1", 0, "x"))}, appliedReq)
	if !strings.Contains(applied.Machine, "abstain_cosine=0.620") {
		t.Errorf("an applied arm renders %q, want the threshold", applied.Machine)
	}
	if !applied.Trace.Floors.VectorApplied {
		t.Error("VectorApplied is false for a run whose vector leg answered")
	}
}

// TestAFitPassThatEmptiesTheBlockClearsTheArmsState: the pass re-renders after
// every drop, and `verdict()` returns for the empty case before it recomputes the
// arms. So `FTSApplied` kept whatever the last non-empty iteration left in it, and
// a block that ended empty printed `floor_fts_rank=3` beside a verdict that
// compared no row — the exact hole the field was added to close, reopened by the
// pass that makes the empty case reachable.
// It runs a query request under a Budget.Measure, a combination no production
// caller makes: only a Measure caller's fit can still empty a block (#1483).
func TestAFitPassThatEmptiesTheBlockClearsTheArmsState(t *testing.T) {
	expired := "2020-01-01 00:00:00"
	gone := ranked("A2", 0, "an expired row")
	gone.ValidUntil = &expired
	set := hybridSet(ranked("A1", 0, strings.Repeat("first row content ", 120)), gone)

	req := baseRequest()
	req.Budget.MaxItems = 5
	full := run(t, &fakeRetriever{set: set}, req)
	_, res := roomiestEmptyCap(t, set, req, full)

	if len(res.Items) != 0 {
		t.Fatalf("precondition: the pass admitted %d rows, so this is not the empty case", len(res.Items))
	}
	if strings.Contains(res.Machine, "floor_fts_rank=3") {
		t.Errorf("machine line = %q, which carries the keyword threshold of an earlier iteration next "+
			"to a verdict that compared no row", res.Machine)
	}
	if !strings.Contains(res.Machine, "floor_fts_rank=not_applied") {
		t.Errorf("machine line = %q, want the unapplied state on an answer with no rows", res.Machine)
	}
}

// stageTrace returns the named stage's record, failing the test when the stage
// was never recorded.
func stageTrace(t *testing.T, res Result, name string) StageTrace {
	t.Helper()
	for _, st := range res.Trace.Stages {
		if st.Stage == name {
			return st
		}
	}
	t.Fatalf("the trace records no %q stage: %+v", name, res.Trace.Stages)
	return StageTrace{}
}
