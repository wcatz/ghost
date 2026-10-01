package supersede

import (
	"context"
	"testing"
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
