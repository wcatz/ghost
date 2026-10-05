package supersede

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The gate on the REPAIR pass (#862). Everything here uses a fake classifier and
// an in-memory store: no harness, no network.
//
// After #845 the ordinary pass reports a supersedes withdrawal instead of
// making one, so `ghost supersede --reassess --apply` is the one path that
// removes a live 'supersedes' edge — and it was acting on ONE classifier verdict
// per edge while `--consensus` was refused beside it. That made the only
// deletion path in the product the least-gated of the two, and #845's own
// measurement (6 of 11 withdrawals wrong even on a UNANIMOUS NEITHER) is the
// reason it has to be gated at all.
//
// The gate is the ordinary pass's (#779): N independent ClassifyBatch calls over
// the same pairs, and an edge moves only when all N name the same outcome. A
// split keeps the edge exactly as the pass found it and is REPORTED.
//
// The one population the gate does NOT cover is the DETERMINISTIC VETO, and
// TestReassessWithdrawsAVetoedEdgeUnderTheGate is the pass's own half of that:
// the veto is settled before the gate exists, so a vetoed edge is withdrawn with
// no classify call at all. The help and docs state the exemption, and a stated
// exemption nothing tests is a promise about code that may change under it.
//
// What the gate covers is the classifier's three REFUSING verdicts, and each is
// pinned here in both states it can be in — unanimous (the edge goes, with the
// reason and the sweep that verdict implies) and split (nothing moves, and the
// split is reported). CAUSES and REVERSED also differ from NEITHER in what they
// sweep, which is a second reason they need their own assertions rather than a
// shared one: a denial takes the 'causes' row that contradicts it with it, and a
// CAUSES verdict affirms that very relation, so its row sweeps nothing.

// TestReassessWithdrawsAVetoedEdgeUnderTheGate: #862's help says the gate covers
// the classifier's verdicts and not the deterministic veto. This is the other
// half of that sentence, in the graph.
//
// A vetoed edge is settled into `settled` before any candidate reaches `open`,
// so under `--consensus 3` it is withdrawn after ZERO classify calls — not three
// of them, and not a vote the three passes would have to agree on. The fake is
// scripted to return SUPERSEDES on every pass, so if the veto were ever moved
// behind the gate this run would end with a unanimous confirmation and a LIVE
// edge: the test would fail on both counts, which is the point. Asserting only
// "withdrawn" would not catch that, because the ungated pass withdraws it too —
// the call count is the half that distinguishes the two behaviours.
func TestReassessWithdrawsAVetoedEdgeUnderTheGate(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	_, older := seedEdge(t, store, db,
		"The restore path was rewritten last month; the timings below are from the new implementation.",
		"NEVER run the restore with source and target on the same spindle.")

	cls := &perPassClassifier{script: []func(string, string) Relation{
		supersedesEverywhere, supersedesEverywhere, supersedesEverywhere,
	}}

	res, withdrawn, err := ReassessWith(ctx, store, cls, "p", ReassessOptions{Apply: true, Consensus: 3}, nil)
	if err != nil {
		t.Fatalf("ReassessWith: %v", err)
	}

	// The load-bearing assertion. N x 0 = 0: there is nothing to vote on, because
	// no pass was asked, so the gate has no say and the row is settled.
	if len(cls.passSizes) != 0 {
		t.Errorf("classify passes = %v, want none: the veto is settled BEFORE the gate, so a gated run must not bill a call to withdraw it", cls.passSizes)
	}
	if cls.pairsAsked != 0 {
		t.Errorf("pairs asked = %d, want 0", cls.pairsAsked)
	}

	if res.Vetoed != 1 {
		t.Errorf("Vetoed = %d, want 1: a vetoed row is counted even under a gate, so a report can say the run declined this work for free", res.Vetoed)
	}
	if res.Withdrawn != 1 || len(withdrawn) != 1 {
		t.Fatalf("withdrew %d edge(s) (%+v), want exactly the one the veto settled: the gate narrows the classifier's verdicts and leaves this one alone", res.Withdrawn, withdrawn)
	}
	if !withdrawn[0].Vetoed || !withdrawn[0].Written {
		t.Errorf("withdrawn[0] = %+v, want Vetoed and Written set: the row is the only place an operator can see this edge was settled by a rule rather than by a model", withdrawn[0])
	}
	if !strings.Contains(withdrawn[0].Reason, "vetoed") {
		t.Errorf("withdrawn[0].Reason = %q, want it to name the veto", withdrawn[0].Reason)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{older}); len(pairs) != 0 {
		t.Errorf("a vetoed edge must not survive a gated repair: %d pair(s) remain", len(pairs))
	}
	assertUnsupersedeHistory(t, store, older)

	// The gate is not merely bypassed here — it RAN, and it ran at three. A
	// result reading Consensus 1 would mean the flag was dropped rather than the
	// veto exempted, which is a different bug with the same graph outcome.
	if res.Consensus != 3 {
		t.Errorf("Consensus = %d, want 3: the gate must have run, with the veto settled ahead of it", res.Consensus)
	}
}

// TestReassessKeepsAnEdgeTheConsensusPassesSplitOn is the issue's own case, in
// the pass's own graph: NEITHER, NEITHER, SUPERSEDES over three passes.
//
// Two of three passes said both notes are still true, which is the single
// ungated verdict that would have deleted a correct edge. Under the gate it is
// not a majority to act on — unanimity is the rule, and #779's measurement is
// why a 2-of-3 rule is not offered: it writes exactly the middle row.
//
// The assertions are the graph first and the report second. A withdrawn edge
// cannot be argued back by any wording, and the report is only meaningful once
// the edge is known to be there.
func TestReassessKeepsAnEdgeTheConsensusPassesSplitOn(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedNpmPackagePair(t, store, db)

	cls := &perPassClassifier{script: []func(string, string) Relation{
		neitherEverywhere, neitherEverywhere, supersedesEverywhere,
	}}

	res, withdrawn, err := ReassessWith(ctx, store, cls, "p", ReassessOptions{Apply: true, Consensus: 3}, nil)
	if err != nil {
		t.Fatalf("ReassessWith: %v", err)
	}
	if got := liveSupersedes(t, store, "p"); got != 1 {
		t.Fatalf("live supersedes edge(s) = %d, want 1: two of three passes said NEITHER and the third said SUPERSEDES, so nothing was decided and the edge stands", got)
	}
	if res.Withdrawn != 0 || len(withdrawn) != 0 {
		t.Errorf("withdrew %d edge(s) (%+v) on a split: a disagreement is a statement about the model's stability, not about which note is current", res.Withdrawn, withdrawn)
	}
	if res.Neither != 0 || res.Confirmed != 0 {
		t.Errorf("Neither=%d Confirmed=%d, want 0/0: no single verdict settled this pair, so no outcome counter may claim one did", res.Neither, res.Confirmed)
	}
	// The split is REPORTED, because a run that quietly changed nothing over a
	// pair it asked about three times reads as a pass that found nothing.
	if res.NotAgreed != 1 {
		t.Errorf("NotAgreed = %d, want 1", res.NotAgreed)
	}
	if len(res.Disputed) != 1 {
		t.Fatalf("Disputed = %+v, want exactly the split pair, named so an operator can find it", res.Disputed)
	}
	d := res.Disputed[0]
	if d.NewerID != newer || d.OlderID != older {
		t.Errorf("Disputed names %s→%s, want %s→%s", d.NewerID, d.OlderID, newer, older)
	}
	if d.Tally[RelationNeither] != 2 || d.Tally[RelationSupersedes] != 1 || len(d.Tally) != 2 {
		t.Errorf("Tally = %v, want {neither:2 supersedes:1}: the tally is the evidence for the refusal, and a map that lost a verdict would make a split look unanimous", d.Tally)
	}
	if d.Unreadable != 0 {
		t.Errorf("Unreadable = %d, want 0", d.Unreadable)
	}
	// And the multiplier is on the result, so a report can state what it ran:
	// a gated repair costs N times the classify calls and the operator checks
	// the bill against this number.
	if res.Consensus != 3 {
		t.Errorf("Consensus = %d, want 3", res.Consensus)
	}
	if len(cls.passSizes) != 3 {
		t.Errorf("classify passes = %d, want 3", len(cls.passSizes))
	}
}

// TestReassessWithdrawsOnAUnanimousDenial is the other half: the gate must not
// become a block on the repair. Three NEITHER passes agree the two notes are
// both still true, and that is the one answer every pass gave — so the edge
// goes, with the unsupersede history row, exactly as the ungated pass does.
func TestReassessWithdrawsOnAUnanimousDenial(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedNpmPackagePair(t, store, db)

	cls := &perPassClassifier{script: []func(string, string) Relation{
		neitherEverywhere, neitherEverywhere, neitherEverywhere,
	}}

	res, withdrawn, err := ReassessWith(ctx, store, cls, "p", ReassessOptions{Apply: true, Consensus: 3}, nil)
	if err != nil {
		t.Fatalf("ReassessWith: %v", err)
	}
	if got := liveSupersedes(t, store, "p"); got != 0 {
		t.Errorf("live supersedes edge(s) = %d, want 0: all %d passes agreed the edge does not hold, which is the repair this pass exists for", got, 3)
	}
	if res.Withdrawn != 1 || len(withdrawn) != 1 {
		t.Fatalf("withdrew %d edge(s) (%+v), want exactly the one every pass denied", res.Withdrawn, withdrawn)
	}
	if !withdrawn[0].Written || withdrawn[0].NewerID != newer || withdrawn[0].OlderID != older {
		t.Errorf("withdrawn[0] = %+v, want the invalidation recorded against %s→%s", withdrawn[0], newer, older)
	}
	if res.Neither != 1 {
		t.Errorf("Neither = %d, want 1: the unanimous verdict is counted like any other, or the summary line describes a pass that judged nothing", res.Neither)
	}
	if res.NotAgreed != 0 || len(res.Disputed) != 0 {
		t.Errorf("NotAgreed=%d Disputed=%+v over a unanimous pair, want 0 and none", res.NotAgreed, res.Disputed)
	}
}

// TestReassessWithoutConsensusReadsOneVerdictPerEdge is the DEFAULT, and the
// reason the gate is opt-in: an existing script's `--reassess --apply` must do
// what it did yesterday, on one verdict, with no extra classify calls.
//
// The classifier answers NEITHER on the first pass and SUPERSEDES on the two
// after it, so a run that asked more than once would show it — and would, under
// a gate, have kept the edge. It is asked once, and the first verdict is acted
// on. That asymmetry is the price of not changing the default, and it is the
// reason the CLI prints a note recommending --consensus 3 rather than turning
// it on.
func TestReassessWithoutConsensusReadsOneVerdictPerEdge(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	_, older := seedNpmPackagePair(t, store, db)

	cls := &perPassClassifier{script: []func(string, string) Relation{
		neitherEverywhere, supersedesEverywhere, supersedesEverywhere,
	}}

	res, withdrawn, err := Reassess(ctx, store, cls, "p", true, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if len(cls.passSizes) != 1 {
		t.Errorf("classify passes = %d, want 1: with no --consensus the repair asks the classifier once, as it always has", len(cls.passSizes))
	}
	if res.Consensus != 1 {
		t.Errorf("Consensus = %d, want 1", res.Consensus)
	}
	if res.Withdrawn != 1 || len(withdrawn) != 1 || withdrawn[0].OlderID != older {
		t.Errorf("withdrew %d edge(s) (%+v), want the single NEITHER verdict applied as it is today", res.Withdrawn, withdrawn)
	}
	if got := liveSupersedes(t, store, "p"); got != 0 {
		t.Errorf("live supersedes edge(s) = %d, want 0", got)
	}
	if res.NotAgreed != 0 {
		t.Errorf("NotAgreed = %d, want 0: nothing was asked twice, so nothing can have split", res.NotAgreed)
	}
}

// TestReassessKeepsACycleWhosePassesSplit is the cycle shape under the gate.
//
// A pair live in BOTH directions is judged ONCE and its verdict read as a
// direction, so a split is the one thing it must never be: a half-withdrawn
// cycle leaves a live edge this pass never judged, which is indistinguishable
// from the cycle the repair was run to clear. Both edges therefore stand and the
// pair is reported as not agreed — which is why the outcome is its own value
// rather than CycleNoVerdict, since the passes DID answer and disagreed.
func TestReassessKeepsACycleWhosePassesSplit(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	stale, fix := seedCycle(t, store, db,
		"the nightly relay rebuild runs on the db host",
		"the nightly relay rebuild moved to object storage", false)

	cls := &perPassClassifier{script: []func(string, string) Relation{
		neitherEverywhere, neitherEverywhere, supersedesEverywhere,
	}}

	res, withdrawn, err := ReassessWith(ctx, store, cls, "p", ReassessOptions{Apply: true, Consensus: 3}, nil)
	if err != nil {
		t.Fatalf("ReassessWith: %v", err)
	}
	if len(res.Cyclic) != 1 {
		t.Fatalf("Cyclic = %+v, want the one cycle the pass reported", res.Cyclic)
	}
	if res.Cyclic[0].Outcome != CycleNotAgreed {
		t.Errorf("Outcome = %q, want %q: the passes answered and split, which is a different finding from a call that never came back", res.Cyclic[0].Outcome, CycleNotAgreed)
	}
	if res.Withdrawn != 0 || len(withdrawn) != 0 {
		t.Errorf("withdrew %d edge(s) (%+v) over a split cycle: withdrawing half of it would leave a live edge nothing judged", res.Withdrawn, withdrawn)
	}
	if got := liveEdges(t, store, stale, fix); len(got) != 2 {
		t.Errorf("live edges = %v, want both directions still live", got)
	}
	if res.NotAgreed != 1 || len(res.Disputed) != 1 {
		t.Errorf("NotAgreed=%d Disputed=%+v, want 1 and one record: a split the report does not name is a pass that changed nothing for no stated reason", res.NotAgreed, res.Disputed)
	}
}

// TestAGatedReassessLeavesEveryPairLiveWhenAPassFails: the failure contract
// under a gate.
//
// The ungated pass keeps the partial repair #699 needed — the chunks that
// answered are applied and only the failed call's pairs wait for a rerun. Under
// a gate that is no longer available and pretending otherwise would be the
// worst kind of quiet: a pair answered on one of three passes has NOT been
// agreed by three passes, so it cannot be acted on, and withdrawing it on the
// strength of the one answer is exactly the ungated behaviour the gate exists to
// remove. So a failed pass leaves every edge it carried LIVE, reports them
// unjudged, and still returns the error so the exit says "rerun me".
func TestAGatedReassessLeavesEveryPairLiveWhenAPassFails(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	_, older := seedNpmPackagePair(t, store, db)

	cls := &perPassClassifier{
		script: []func(string, string) Relation{neitherEverywhere, neitherEverywhere, neitherEverywhere},
		err:    context.DeadlineExceeded,
	}

	res, withdrawn, err := ReassessWith(ctx, store, cls, "p", ReassessOptions{Apply: true, Consensus: 3}, nil)
	if err == nil {
		t.Fatal("a dead harness must not read as a pass that found nothing")
	}
	if res.Withdrawn != 0 || len(withdrawn) != 0 {
		t.Errorf("withdrew %d edge(s) (%+v) after a failed pass: one answer out of three is not agreement", res.Withdrawn, withdrawn)
	}
	if got := liveSupersedes(t, store, "p"); got != 1 {
		t.Errorf("live supersedes edge(s) = %d, want 1: an unjudged edge is left alone so the rerun can find it again", got)
	}
	if len(res.Unjudged) != 1 || res.Unjudged[0].OlderID != older {
		t.Errorf("Unjudged = %+v, want the pair the failed call carried", res.Unjudged)
	}
}

// TestAGatedReassessSettlesThePairsThatReachedAQuorum is the claim that keeps
// the gate from being all-or-nothing, and it is per PAIR rather than per pass.
//
// Two live edges, one batch, one verdict list: the first pair is answered NEITHER
// by all three passes and the second splits. The gate must move exactly the
// first and leave the second alone — a gate that refused everything because one
// pair disagreed would be a repair that cannot repair anything on a corpus where
// the model is unstable about one pair, which is the corpus #845 was measured
// on. The graph is the witness: exactly one edge gone.
func TestAGatedReassessSettlesThePairsThatReachedAQuorum(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// Two real edges over four notes. The pairs are distinct by content AND by
	// embedding axis, so the batch really carries two pairs and the classifier's
	// per-pass answer can differ between them.
	const (
		olderA = "note: the nightly export wrote to the shared volume"
		newerA = "note: the nightly export writes to the vault bucket"
		olderB = "bug: the relay stalls on every consumer rebalance"
		newerB = "the relay rebalance stall is fixed: pin the consumer"
	)
	newerAID := add(t, store, db, newerA, []float32{1, 0, 0}, "2026-02-02 09:00:00")
	olderAID := add(t, store, db, olderA, []float32{0.99, 0.01, 0}, "2026-01-01 09:00:00")
	newerBID := add(t, store, db, newerB, []float32{0, 1, 0}, "2026-02-02 09:00:00")
	olderBID := add(t, store, db, olderB, []float32{0, 0.99, 0.01}, "2026-01-01 09:00:00")
	for _, dir := range [][2]string{{newerAID, olderAID}, {newerBID, olderBID}} {
		if err := store.CreateLink(ctx, dir[0], dir[1], string(RelationSupersedes), 0.95, "llm"); err != nil {
			t.Fatal(err)
		}
	}

	// The export pair is denied on all three passes; the relay pair answers
	// SUPERSEDES once and NEITHER twice, so it splits 2-1. The fake is handed
	// CONTENT rather than ids, so the script matches on the newer note's text —
	// which is also why the two pairs must carry distinct text.
	says := func(relay Relation) func(string, string) Relation {
		return func(newer, _ string) Relation {
			if newer == newerB {
				return relay
			}
			return RelationNeither
		}
	}
	cls := &perPassClassifier{script: []func(string, string) Relation{
		says(RelationSupersedes), says(RelationNeither), says(RelationNeither),
	}}

	res, withdrawn, err := ReassessWith(ctx, store, cls, "p", ReassessOptions{Apply: true, Consensus: 3}, nil)
	if err != nil {
		t.Fatalf("ReassessWith: %v", err)
	}
	if res.Withdrawn != 1 || len(withdrawn) != 1 {
		t.Fatalf("withdrew %d edge(s) (%+v), want only the pair every pass denied", res.Withdrawn, withdrawn)
	}
	if withdrawn[0].NewerID != newerAID || withdrawn[0].OlderID != olderAID {
		t.Errorf("withdrew %s→%s, want the unanimous pair %s→%s", withdrawn[0].NewerID, withdrawn[0].OlderID, newerAID, olderAID)
	}
	if got := liveEdges(t, store, newerAID, olderAID); len(got) != 0 {
		t.Errorf("the unanimously denied edge is still live: %v", got)
	}
	if got := liveEdges(t, store, newerBID, olderBID); len(got) != 1 {
		t.Errorf("the split pair's live edge(s) = %v, want the one it started with", got)
	}
	if res.NotAgreed != 1 || len(res.Disputed) != 1 || res.Disputed[0].NewerID != newerBID {
		t.Errorf("NotAgreed=%d Disputed=%+v, want exactly the split pair named", res.NotAgreed, res.Disputed)
	}
}

// TestAReassessOptionsBelowTheMinimumIsTheUngatedRepair: the clamp, stated so a
// programmatic caller is not surprised by it.
//
// ReassessOptions is exported and MinConsensus is a refusal the CLI makes at its
// own boundary, so the pass reads a smaller number the way RunWith reads its own:
// as the ungated repair, with Result.Consensus carrying back what actually ran.
// A caller that expected an error would have to handle one this function never
// returns, and a caller that expected a gate must ask for MinConsensus.
func TestAReassessOptionsBelowTheMinimumIsTheUngatedRepair(t *testing.T) {
	for _, n := range []int{0, 1, -3} {
		// A fresh store and a fresh classifier per case: the pass above withdraws
		// the edge, and perPassClassifier indexes its script by how many passes it
		// has already answered, so a reused fake would hand the second case the
		// second script entry.
		store, db := seed(t)
		ctx := context.Background()
		_, _ = seedNpmPackagePair(t, store, db)
		cls := &perPassClassifier{script: []func(string, string) Relation{neitherEverywhere, supersedesEverywhere, supersedesEverywhere}}

		res, withdrawn, err := ReassessWith(ctx, store, cls, "p", ReassessOptions{Apply: true, Consensus: n}, nil)
		if err != nil {
			t.Fatalf("ReassessWith(Consensus: %d): %v", n, err)
		}
		if res.Consensus != 1 {
			t.Errorf("Consensus: %d gave Result.Consensus = %d, want 1: the result carries back what ran, so a report can state it", n, res.Consensus)
		}
		if res.Withdrawn != 1 || len(withdrawn) != 1 {
			t.Errorf("Consensus: %d withdrew %d edge(s), want the single first-pass verdict acted on", n, res.Withdrawn)
		}
		// One pass, not three: the clamp must not spend the money either.
		if len(cls.passSizes) != 1 {
			t.Errorf("Consensus: %d made %d classify pass(es), want 1", n, len(cls.passSizes))
		}
	}
}

// TestAGatedReassessLeavesAnUnreadablePairAlone is the third state, and the one
// a majority rule would have got wrong in the other direction: all N passes
// answered UNREADABLY.
//
// That is `undecided`, not `disputed` — the ordinary pass's own distinction, read
// through the same quorum function — and it must not become a withdrawal or a
// split report. Nothing was voted for, so the edge stands, the pair is counted
// as UNKNOWN (which is the bucket a later parser improvement must be able to
// fall out of without the meaning of the count moving), and no NotAgreed row is
// printed for a harness fault.
func TestAGatedReassessLeavesAnUnreadablePairAlone(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	_, _ = seedNpmPackagePair(t, store, db)

	cls := &perPassClassifier{script: []func(string, string) Relation{
		func(string, string) Relation { return Relation("") },
		func(string, string) Relation { return Relation("") },
		func(string, string) Relation { return Relation("") },
	}}

	res, withdrawn, err := ReassessWith(ctx, store, cls, "p", ReassessOptions{Apply: true, Consensus: 3}, nil)
	if err != nil {
		t.Fatalf("ReassessWith: %v", err)
	}
	if res.Unclassified != 1 {
		t.Errorf("Unclassified = %d, want 1: no pass produced a readable verdict, which is the bucket a later parser improvement falls out of", res.Unclassified)
	}
	if res.Withdrawn != 0 || len(withdrawn) != 0 {
		t.Errorf("withdrew %d edge(s) (%+v) on votes for nothing", res.Withdrawn, withdrawn)
	}
	if res.NotAgreed != 0 {
		t.Errorf("NotAgreed = %d, want 0: a broken reply is a harness fault, and reporting it as model instability sends the operator after the wrong thing", res.NotAgreed)
	}
	if got := liveSupersedes(t, store, "p"); got != 1 {
		t.Errorf("live supersedes edge(s) = %d, want 1", got)
	}
	if len(res.Unjudged) != 0 {
		t.Errorf("Unjudged = %+v, want none: every pass DID answer this pair, so it is unknown rather than unanswered", res.Unjudged)
	}
}

// seedPairWithAContradictingCausesEdge is the graph the two REFUSING verdicts
// need, and it is a pair plus one row: a live 'supersedes' edge newer→older, and
// a live 'causes' edge older→newer saying the pair stands. Those two assert
// opposite things about one pair, so whichever withdrawal settles the pair has to
// say what became of the other relation's row — and that is reported two ways, a
// count on the withdrawal and the edges left in the store, which is why the tests
// below assert both rather than trusting either.
//
// The 'causes' edge runs older→newer, the reverse of 'supersedes', because it is
// the direction the ordinary pass writes it in. Seeding it the other way round
// would put the contradicting row where the sweep does not look, so the count
// would read 0 and the test would be failing about its fixture.
//
// seedNpmPackagePair's timestamps are pinned a day apart and its link is
// backdated, so `orient` knows the direction and the pair is a candidate.
func seedPairWithAContradictingCausesEdge(t *testing.T) (store *memory.Store, newer, older string) {
	t.Helper()
	store, db := seed(t)
	newer, older = seedNpmPackagePair(t, store, db)
	seedCauses(t, store, older, newer)
	return store, newer, older
}

// TestReassessWithdrawsOnAUnanimousReversedVerdict: REVERSED under the gate, in
// the state where every pass gave it.
//
// REVERSED names the note the pass was asked ABOUT as the obsolete one, so it
// denies that this edge asserts — the same denial NEITHER makes, arrived at from
// the other direction, and it takes the 'causes' row with it. Leaving that row
// live would replace one contradiction with another while the report claimed a
// repair, so the sweep is asserted as well as the withdrawal: a change that kept
// the first and dropped the second still "withdrew the edge", and the operator
// reading the count is the one who has to be able to tell.
func TestReassessWithdrawsOnAUnanimousReversedVerdict(t *testing.T) {
	store, newer, older := seedPairWithAContradictingCausesEdge(t)
	ctx := context.Background()

	cls := &perPassClassifier{script: []func(string, string) Relation{
		reversedEverywhere, reversedEverywhere, reversedEverywhere,
	}}
	res, withdrawn, err := ReassessWith(ctx, store, cls, "p", ReassessOptions{Apply: true, Consensus: 3}, nil)
	if err != nil {
		t.Fatalf("ReassessWith: %v", err)
	}

	// The gate ran, at three, and asked about this pair on every one: the edge is
	// gone because three passes agreed, not because a rule settled it first.
	if res.Consensus != 3 || len(cls.passSizes) != 3 || cls.pairsAsked != 3 {
		t.Errorf("Consensus=%d classify passes=%v pairs asked=%d, want 3/3 passes/3 asks: the withdrawal must be the gate's, so the bill is N and the pair was judged N times",
			res.Consensus, cls.passSizes, cls.pairsAsked)
	}
	if res.Reversed != 1 || res.Neither != 0 || res.Unclassified != 0 {
		t.Errorf("Reversed=%d Neither=%d Unclassified=%d, want 1/0/0: the unanimous verdict is counted under its own name, or the summary describes a pass that judged something else", res.Reversed, res.Neither, res.Unclassified)
	}
	if res.NotAgreed != 0 || len(res.Disputed) != 0 {
		t.Errorf("NotAgreed=%d Disputed=%+v over a unanimous pair, want 0 and none", res.NotAgreed, res.Disputed)
	}
	if res.Vetoed != 0 {
		t.Errorf("Vetoed = %d, want 0: this pair was the classifier's to decide, and a row counted as vetoed would tell an operator no model looked at it", res.Vetoed)
	}

	if res.Withdrawn != 1 || len(withdrawn) != 1 {
		t.Fatalf("withdrew %d edge(s) (%+v), want exactly the one all %d passes reversed", res.Withdrawn, withdrawn, 3)
	}
	row := withdrawn[0]
	if !row.Written || row.NewerID != newer || row.OlderID != older {
		t.Errorf("withdrawn[0] = %+v, want the invalidation recorded against %s→%s", row, newer, older)
	}
	if row.Vetoed {
		t.Error("withdrawn[0].Vetoed = true, want false: the row must say a model decided this, because the veto is the exemption the help states")
	}
	if !strings.Contains(row.Reason, "reversed") {
		t.Errorf("withdrawn[0].Reason = %q, want it to name the reversed verdict: the reason is the rule that withdrew the edge, and 'neither' would be a false account of it", row.Reason)
	}

	// The sweep, counted the way the store confirms it.
	if row.CausesSwept != 1 || res.CausesWithdrawn != 1 {
		t.Errorf("row CausesSwept=%d res.CausesWithdrawn=%d, want 1 and 1: a REVERSED verdict is a denial like any other, so the 'causes' row contradicting it goes with it",
			row.CausesSwept, res.CausesWithdrawn)
	}
	if row.SweepFailed || row.PredictionUnknown {
		t.Errorf("withdrawn[0] = %+v, want no sweep markers: the sweep ran under --apply and reported what it moved", row)
	}
	if got := liveSupersedes(t, store, "p"); got != 0 {
		t.Errorf("live supersedes edge(s) = %d, want 0", got)
	}
	if got := liveCausesEdges(t, store, newer, older); len(got) != 0 {
		t.Errorf("live causes edges = %v, want none: the edge withdrawn here asserted the pair stands, which is what the verdict just denied", got)
	}
	assertUnsupersedeHistory(t, store, older)
}

// TestReassessKeepsAnEdgeWhenAReversedVerdictSplits: the same verdict, split
// 2-1, and the edge stays exactly as it was.
//
// Two of three passes said the note this edge supersedes is the obsolete one.
// Under the gate that is not a majority to act on — unanimity is the rule — and
// the cost of holding it is a wrong edge left in place for one run, against a
// wrong withdrawal that no rerun would notice. The second row is asserted too:
// a split must move no relation's rows, so the 'causes' row that still stands is
// not swept by a verdict that was never agreed on.
func TestReassessKeepsAnEdgeWhenAReversedVerdictSplits(t *testing.T) {
	store, newer, older := seedPairWithAContradictingCausesEdge(t)
	ctx := context.Background()

	cls := &perPassClassifier{script: []func(string, string) Relation{
		reversedEverywhere, reversedEverywhere, neitherEverywhere,
	}}
	res, withdrawn, err := ReassessWith(ctx, store, cls, "p", ReassessOptions{Apply: true, Consensus: 3}, nil)
	if err != nil {
		t.Fatalf("ReassessWith: %v", err)
	}
	if got := liveSupersedes(t, store, "p"); got != 1 {
		t.Fatalf("live supersedes edge(s) = %d, want 1: two passes reversed it and one did not, so nothing was decided and the edge stands", got)
	}
	if res.Withdrawn != 0 || len(withdrawn) != 0 {
		t.Errorf("withdrew %d edge(s) (%+v) on a split: a disagreement is a statement about the model's stability, not about which note is current", res.Withdrawn, withdrawn)
	}
	if res.Reversed != 0 || res.Neither != 0 {
		t.Errorf("Reversed=%d Neither=%d, want 0/0: no single verdict settled this pair, so no outcome counter may claim one did", res.Reversed, res.Neither)
	}
	if res.CausesWithdrawn != 0 {
		t.Errorf("CausesWithdrawn = %d, want 0: a split withdraws nothing, so the 'causes' row standing beside an undecided pair is the state the split leaves for the rerun", res.CausesWithdrawn)
	}
	if got := liveCausesEdges(t, store, newer, older); len(got) != 1 {
		t.Errorf("live causes edges = %v, want the one it started with: a verdict nobody agreed on may not delete a second row", got)
	}
	if res.NotAgreed != 1 || len(res.Disputed) != 1 {
		t.Fatalf("NotAgreed=%d Disputed=%+v, want 1 and one record: a split the report does not name is a pass that changed nothing for no stated reason", res.NotAgreed, res.Disputed)
	}
	d := res.Disputed[0]
	if d.NewerID != newer || d.OlderID != older {
		t.Errorf("Disputed names %s→%s, want %s→%s", d.NewerID, d.OlderID, newer, older)
	}
	if d.Tally[RelationReversed] != 2 || d.Tally[RelationNeither] != 1 || len(d.Tally) != 2 {
		t.Errorf("Tally = %v, want {reversed:2 neither:1}: the tally is the evidence for the refusal, and a map that lost a verdict would make a split look unanimous", d.Tally)
	}
}

// TestReassessWithdrawsOnAUnanimousCausesVerdict: CAUSES under the gate, and the
// one refusal whose withdrawal sweeps NOTHING.
//
// CAUSES says the older note is still independently true, so it denies that the
// newer one replaced it — the 'supersedes' edge asserts exactly what was denied,
// and the edge goes with the reason named after the verdict. But it affirms the
// 'causes' relation rather than denying it, so the live 'causes' row on this pair
// is a row this verdict ENDORSES. Sweeping it would delete the one edge that
// records what the classifier found, in the name of applying what it found, so
// the row's sweep count is asserted to be zero and the edge is asserted still
// live. That asymmetry is why CAUSES cannot share NEITHER's assertions: the two
// verdicts both withdraw the supersedes edge, and only one of them touches the
// second relation.
func TestReassessWithdrawsOnAUnanimousCausesVerdict(t *testing.T) {
	store, newer, older := seedPairWithAContradictingCausesEdge(t)
	ctx := context.Background()

	cls := &perPassClassifier{script: []func(string, string) Relation{
		causesEverywhere, causesEverywhere, causesEverywhere,
	}}
	res, withdrawn, err := ReassessWith(ctx, store, cls, "p", ReassessOptions{Apply: true, Consensus: 3}, nil)
	if err != nil {
		t.Fatalf("ReassessWith: %v", err)
	}
	if res.Consensus != 3 || len(cls.passSizes) != 3 || cls.pairsAsked != 3 {
		t.Errorf("Consensus=%d classify passes=%v pairs asked=%d, want 3/3 passes/3 asks", res.Consensus, cls.passSizes, cls.pairsAsked)
	}
	if res.Causes != 1 || res.Reversed != 0 || res.Neither != 0 {
		t.Errorf("Causes=%d Reversed=%d Neither=%d, want 1/0/0", res.Causes, res.Reversed, res.Neither)
	}
	if res.NotAgreed != 0 || len(res.Disputed) != 0 {
		t.Errorf("NotAgreed=%d Disputed=%+v over a unanimous pair, want 0 and none", res.NotAgreed, res.Disputed)
	}
	if res.Withdrawn != 1 || len(withdrawn) != 1 {
		t.Fatalf("withdrew %d edge(s) (%+v), want exactly the one all %d passes answered CAUSES about", res.Withdrawn, withdrawn, 3)
	}
	row := withdrawn[0]
	if !row.Written || row.NewerID != newer || row.OlderID != older || row.Vetoed {
		t.Errorf("withdrawn[0] = %+v, want the invalidation recorded against %s→%s and not marked vetoed", row, newer, older)
	}
	if !strings.Contains(row.Reason, "causes") {
		t.Errorf("withdrawn[0].Reason = %q, want it to name the causes verdict", row.Reason)
	}
	if row.CausesSwept != 0 || res.CausesWithdrawn != 0 {
		t.Errorf("row CausesSwept=%d res.CausesWithdrawn=%d, want 0 and 0: a CAUSES verdict affirms that relation, so the 'causes' row it leaves live is the finding, not a contradiction of it",
			row.CausesSwept, res.CausesWithdrawn)
	}
	if got := liveSupersedes(t, store, "p"); got != 0 {
		t.Errorf("live supersedes edge(s) = %d, want 0", got)
	}
	if got := liveCausesEdges(t, store, newer, older); len(got) != 1 || got[0] != [2]string{older, newer} {
		t.Errorf("live causes edges = %v, want exactly [%s %s]: the verdict affirmed this relation", got, older, newer)
	}
	assertUnsupersedeHistory(t, store, older)
}

// TestReassessKeepsAnEdgeWhenTheCausesVerdictSplits: CAUSES split 2-1, and the
// edge stays.
//
// The same rule as the reversed split, asserted over the other refusal because a
// test that only ever splits one verdict tests the split once: two passes saying
// the older note still stands is not agreement about what replaced it, and the
// 'supersedes' row asserting that is the row this run leaves for the next one.
func TestReassessKeepsAnEdgeWhenTheCausesVerdictSplits(t *testing.T) {
	store, newer, older := seedPairWithAContradictingCausesEdge(t)
	ctx := context.Background()

	cls := &perPassClassifier{script: []func(string, string) Relation{
		causesEverywhere, causesEverywhere, neitherEverywhere,
	}}
	res, withdrawn, err := ReassessWith(ctx, store, cls, "p", ReassessOptions{Apply: true, Consensus: 3}, nil)
	if err != nil {
		t.Fatalf("ReassessWith: %v", err)
	}
	if got := liveSupersedes(t, store, "p"); got != 1 {
		t.Fatalf("live supersedes edge(s) = %d, want 1: two passes answered CAUSES and one did not, so nothing was decided and the edge stands", got)
	}
	if res.Withdrawn != 0 || len(withdrawn) != 0 {
		t.Errorf("withdrew %d edge(s) (%+v) on a split", res.Withdrawn, withdrawn)
	}
	if res.Causes != 0 || res.Neither != 0 || res.CausesWithdrawn != 0 {
		t.Errorf("Causes=%d Neither=%d CausesWithdrawn=%d, want 0/0/0", res.Causes, res.Neither, res.CausesWithdrawn)
	}
	if got := liveCausesEdges(t, store, newer, older); len(got) != 1 {
		t.Errorf("live causes edges = %v, want the one it started with", got)
	}
	if res.NotAgreed != 1 || len(res.Disputed) != 1 {
		t.Fatalf("NotAgreed=%d Disputed=%+v, want 1 and one record", res.NotAgreed, res.Disputed)
	}
	d := res.Disputed[0]
	if d.NewerID != newer || d.OlderID != older {
		t.Errorf("Disputed names %s→%s, want %s→%s", d.NewerID, d.OlderID, newer, older)
	}
	if d.Tally[RelationCauses] != 2 || d.Tally[RelationNeither] != 1 || len(d.Tally) != 2 {
		t.Errorf("Tally = %v, want {causes:2 neither:1}", d.Tally)
	}
}

// partialOnPassNClassifier answers NEITHER for every pair on every pass, except
// the pass named by partialAt: that call answers only the first `answered` pairs
// and reports the rest as a chunk that died, which is the shape
// RelationClassifier returns when one batched chunk came back and the next did
// not.
//
// The pair order the pass handed this call is recorded rather than assumed —
// LinksByRelationSource has no ORDER BY, so a test that pinned "the first pair is
// the export pair" would be reading the store's row order, and would quietly be
// testing the wrong pair on a different store or after a VACUUM.
type partialOnPassNClassifier struct {
	partialAt  int
	answered   int
	passSizes  []int
	pairsAsked int
	order      []string // the pairs' NewerIDs, in the order the failing pass saw them
}

func (m *partialOnPassNClassifier) ClassifyBatch(_ context.Context, pairs []Candidate) ([]Relation, error) {
	pass := len(m.passSizes)
	m.passSizes = append(m.passSizes, len(pairs))
	if pass != m.partialAt {
		for range pairs {
			m.pairsAsked++
		}
		return fill(RelationNeither, len(pairs)), nil
	}
	for _, p := range pairs {
		m.order = append(m.order, p.NewerID)
	}
	n := min(m.answered, len(pairs))
	m.pairsAsked += n
	return fill(RelationNeither, len(pairs)), &PartialVerdictsError{
		Answered: n, Err: errors.New("opencode run: exit status 1"),
	}
}

func fill(rel Relation, n int) []Relation {
	out := make([]Relation, n)
	for i := range out {
		out[i] = rel
	}
	return out
}

// TestAGatedReassessStillAppliesAPairAnsweredByEveryPassWhenALaterChunkDies:
// docs/cli.md promises a gated repair is not all-or-nothing under a FAILURE too,
// not only under a split, and this is that promise in the graph.
//
// classifyVotes stops asking once a call fails, so a pair the failed call did not
// answer is short of a quorum and stays live — but a pair the failing pass DID
// answer, in the prefix its partial error declares complete, holds all N answers
// and is settled like any other. Without this the doc's claim rests on a reading
// of the loop, and the opposite reading (the loop stops, so nothing settles once
// anything fails) is one edit away.
func TestAGatedReassessStillAppliesAPairAnsweredByEveryPassWhenALaterChunkDies(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	const (
		exportOlder = "note: the nightly export wrote to the shared volume"
		exportNewer = "note: the nightly export writes to the vault bucket"
		relayOlder  = "bug: the relay stalls on every consumer rebalance"
		relayNewer  = "the relay rebalance stall is fixed: pin the consumer"
	)
	exportOlderID := add(t, store, db, exportOlder, []float32{1, 0, 0}, "2026-01-01 09:00:00")
	exportNewerID := add(t, store, db, exportNewer, []float32{0.99, 0.01, 0}, "2026-02-02 09:00:00")
	relayOlderID := add(t, store, db, relayOlder, []float32{0, 1, 0}, "2026-01-01 09:00:00")
	relayNewerID := add(t, store, db, relayNewer, []float32{0, 0.99, 0.01}, "2026-02-02 09:00:00")
	for _, dir := range [][2]string{{exportNewerID, exportOlderID}, {relayNewerID, relayOlderID}} {
		if err := store.CreateLink(ctx, dir[0], dir[1], string(RelationSupersedes), 0.95, "llm"); err != nil {
			t.Fatal(err)
		}
	}

	cls := &partialOnPassNClassifier{partialAt: 2, answered: 1}
	res, withdrawn, err := ReassessWith(ctx, store, cls, "p", ReassessOptions{Apply: true, Consensus: 3}, nil)
	if err == nil {
		t.Fatal("a dead chunk must still fail the run, so the exit says rerun me")
	}
	if len(cls.order) != 2 {
		t.Fatalf("the failing pass saw %v, want both pairs: this test is about the pair it did not answer", cls.order)
	}
	settled, owed := cls.order[0], cls.order[1]

	if res.Withdrawn != 1 || len(withdrawn) != 1 {
		t.Fatalf("withdrew %d edge(s) (%+v), want exactly the pair that reached N answers before the chunk died", res.Withdrawn, withdrawn)
	}
	if withdrawn[0].NewerID != settled {
		t.Errorf("withdrew %s, want %s: the pair the failing pass still answered holds N answers and settles; the other holds N-1", withdrawn[0].NewerID, settled)
	}
	if res.Neither != 1 {
		t.Errorf("Neither = %d, want 1: a settled pair is counted under its own verdict even in a run that failed", res.Neither)
	}
	if got := liveSupersedes(t, store, "p"); got != 1 {
		t.Errorf("live supersedes edge(s) = %d, want 1", got)
	}
	if got := liveEdges(t, store, owed, relayOlderID, exportOlderID); len(got) != 1 {
		t.Errorf("live edge(s) touching the pair that was owed an answer = %v, want exactly its one edge", got)
	}
	if len(res.Unjudged) != 1 {
		t.Fatalf("Unjudged = %+v, want exactly the pair the failed call did not answer, so the rerun finds it again", res.Unjudged)
	}
	if res.Unjudged[0].NewerID != owed {
		t.Errorf("Unjudged names %s, want %s", res.Unjudged[0].NewerID, owed)
	}
	if res.NotAgreed != 0 {
		t.Errorf("NotAgreed = %d, want 0: the owed pair was never asked N times, so nothing disagreed", res.NotAgreed)
	}
}
