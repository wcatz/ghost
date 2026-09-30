package supersede

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The consensus gate (#779). Everything here uses a fake classifier and an
// in-memory store: no harness, no network, no real corpus.
//
// The gate's claim is a claim about what a pass WRITES, so almost every test
// here is stated as "this link exists in the graph" rather than "this counter is
// N" — the counters are checked too, but a counter can be right while the graph
// is wrong, and the graph is the harm.

// perPassClassifier answers differently on each pass, which is the only property
// a consensus test actually needs from a fake: the gate's entire purpose is to
// act on agreement and refuse on disagreement, and a classifier that answers the
// same thing every time can only exercise the first half.
//
// `script` is indexed by pass number and returns a verdict per pair, in the
// order the pass received them. `pairsAsked` counts every pair across every
// pass, and `passSizes` records how many pairs each pass was handed — so a test
// can assert the multiplier on pairs separately from the batching, which are
// different numbers and used to be conflated.
type perPassClassifier struct {
	script     []func(newer, older string) Relation
	pairsAsked int
	passSizes  []int
	err        error
}

func (m *perPassClassifier) ClassifyBatch(_ context.Context, pairs []Candidate) ([]Relation, error) {
	pass := len(m.passSizes)
	m.passSizes = append(m.passSizes, len(pairs))
	if m.err != nil {
		return nil, m.err
	}
	if pass >= len(m.script) {
		return nil, fmt.Errorf("fake asked for pass %d but the script has %d", pass, len(m.script))
	}
	out := make([]Relation, len(pairs))
	for i, p := range pairs {
		m.pairsAsked++
		out[i] = m.script[pass](p.NewerContent, p.OlderContent)
	}
	return out, nil
}

// supersedesEverywhere and neitherEverywhere are the two constant scripts, so a
// test that wants unanimity does not have to write a closure to get it.
func supersedesEverywhere(_, _ string) Relation { return RelationSupersedes }
func neitherEverywhere(_, _ string) Relation    { return RelationNeither }

// twoNotePair seeds one similar pair and returns its ids, oriented newer-last.
func twoNotePair(t *testing.T, olderContent, newerContent string) (store *memory.Store, older, newer string) {
	t.Helper()
	store, db := seed(t)
	older = add(t, store, db, olderContent, []float32{1, 0, 0}, "2026-01-01 00:00:00")
	newer = add(t, store, db, newerContent, []float32{0.99, 0.01, 0}, "2026-06-01 00:00:00")
	return store, older, newer
}

func linkCount(t *testing.T, store *memory.Store, ids ...string) int {
	t.Helper()
	pairs, err := store.SupersedesWithin(context.Background(), ids)
	if err != nil {
		t.Fatalf("SupersedesWithin: %v", err)
	}
	return len(pairs)
}

// TestConsensusWritesOnlyTheUnanimousEdges is the gate's whole claim: three
// pairs, three passes, two unanimous and one split. Only the two may reach the
// graph.
//
// The split is 2-of-1 rather than 1-of-1-of-1 because that is the case a
// majority rule gets wrong, and a test that only refused a 1-of-3 split would
// pass under a plurality implementation too. Nothing is written for it, and
// nothing is written in the OTHER direction either: a pair the model cannot
// settle must not acquire an edge pointing the other way, because the harm of a
// backwards edge is the same as the harm of a wrong one.
func TestConsensusWritesOnlyTheUnanimousEdges(t *testing.T) {
	const (
		unanimousA      = "the ingest service runs redis 6.2"
		unanimousB      = "the ingest service runs redis 7.2"
		unanimousANewer = "the ingest service now runs redis 7.2, pin moved in the upgrade"
		unanimousBNewer = "the ingest service now runs redis 7.2, pin moved in the upgrade that also changed eviction"
		splitOlder      = "the ledger stream is missing a consumer"
		splitNewer      = "the ledger stream now has a consumer on the archive topic"
	)
	// Each pair's two notes are near-identical vectors and every OTHER pair's are
	// orthogonal, so the scan proposes exactly the three pairs under test and no
	// cross-pair candidates. Without that, the counter assertions would be about
	// a dozen incidental pairs the fixtures never mention.
	store, db := seed(t)
	ctx := context.Background()
	aOlder := add(t, store, db, unanimousA, []float32{1, 0, 0, 0}, "2026-01-01 00:00:00")
	aNewer := add(t, store, db, unanimousANewer, []float32{1, 0.01, 0, 0}, "2026-06-01 00:00:00")
	bOlder := add(t, store, db, unanimousB, []float32{0, 1, 0, 0}, "2026-02-01 00:00:00")
	bNewer := add(t, store, db, unanimousBNewer, []float32{0, 1, 0.01, 0}, "2026-07-01 00:00:00")
	sOlder := add(t, store, db, splitOlder, []float32{0, 0, 1, 0}, "2026-03-01 00:00:00")
	sNewer := add(t, store, db, splitNewer, []float32{0, 0, 1, 0.01}, "2026-08-01 00:00:00")

	// Every pass: the two unanimous pairs are supersessions, the split pair is a
	// supersession on passes 1 and 2 and a NEITHER on pass 3.
	cls := &perPassClassifier{script: []func(string, string) Relation{
		supersedesEverywhere,
		supersedesEverywhere,
		func(newer, older string) Relation {
			if older == splitOlder {
				return RelationNeither
			}
			return RelationSupersedes
		},
	}}

	res, classified, err := RunWith(ctx, store, cls, "p", Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default())
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}

	if res.Confirmed != 2 || res.Created != 2 {
		t.Errorf("confirmed/created = %d/%d, want 2/2: only the unanimous pairs may be written", res.Confirmed, res.Created)
	}
	if len(classified) != 2 {
		t.Errorf("classified %d row(s), want 2: the split pair must not appear as a decided row", len(classified))
	}
	if res.NotAgreed != 1 {
		t.Errorf("NotAgreed = %d, want 1", res.NotAgreed)
	}
	if len(res.Disputed) != 1 {
		t.Fatalf("Disputed holds %d record(s), want 1", len(res.Disputed))
	}
	d := res.Disputed[0]
	if d.NewerID != sNewer || d.OlderID != sOlder {
		t.Errorf("the disputed record names %s->%s, want the split pair %s->%s", d.NewerID, d.OlderID, sNewer, sOlder)
	}
	// The tally is the evidence for the refusal, and it has to be exact: a
	// 2-supersedes-1-neither split is a different finding from a three-way one.
	if d.Tally[RelationSupersedes] != 2 || d.Tally[RelationNeither] != 1 {
		t.Errorf("tally = %v, want 2 supersedes and 1 neither", d.Tally)
	}
	if d.Unreadable != 0 {
		t.Errorf("Unreadable = %d, want 0: both verdict kinds here were readable", d.Unreadable)
	}

	// The graph, which is the harm. The two unanimous edges exist in the
	// newer->older direction and the split pair has NO edge in either direction.
	if got := linkCount(t, store, aNewer, aOlder); got != 1 {
		t.Errorf("the first unanimous pair has %d link(s), want 1", got)
	}
	if got := linkCount(t, store, bNewer, bOlder); got != 1 {
		t.Errorf("the second unanimous pair has %d link(s), want 1", got)
	}
	if got := linkCount(t, store, sNewer, sOlder, sOlder, sNewer); got != 0 {
		t.Errorf("the split pair has %d link(s), want 0 in EITHER direction", got)
	}
}

// TestConsensusAsksEveryPass is the re-ask requirement, and it is the test that
// keeps the NEITHER cache from quietly answering pass 2 out of pass 1's verdict.
//
// The shape is chosen so that a cache read between passes would be visible: the
// fake says NEITHER on pass 1 and SUPERSEDES on passes 2 and 3, and a pass 2 that
// were served from a pass-1 cache row would answer NEITHER. Unanimity then fails
// and the edge is not written — which is the exact silent failure this asserts
// against, because the resulting report would look like a model that changed its
// mind twice rather than a gate that asked once.
func TestConsensusAsksEveryPass(t *testing.T) {
	store, older, newer := twoNotePair(t,
		"the ingest service runs redis 6.2",
		"the ingest service now runs redis 7.2, pin moved in the upgrade")
	cls := &perPassClassifier{script: []func(string, string) Relation{
		neitherEverywhere,
		supersedesEverywhere,
		supersedesEverywhere,
	}}

	res, _, err := RunWith(context.Background(), store, cls, "p",
		Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default())
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	if cls.pairsAsked != 3 {
		t.Errorf("the passes asked about %d pair(s) in total, want 3: one real re-ask per pass", cls.pairsAsked)
	}
	if len(cls.passSizes) != 3 || cls.passSizes[0] != 1 || cls.passSizes[2] != 1 {
		t.Errorf("pass sizes = %v, want three passes of one pair each: a pass that got an empty set was not re-asked", cls.passSizes)
	}
	// Passes 2 and 3 agreed, so the pair is NOT agreed across all three — and
	// that is the finding, not a bug: the gate is unanimity.
	if res.NotAgreed != 1 || res.Created != 0 {
		t.Errorf("NotAgreed/Created = %d/%d, want 1/0: pass 1 disagreed, so unanimity failed", res.NotAgreed, res.Created)
	}
	if got := linkCount(t, store, newer, older); got != 0 {
		t.Errorf("the pair has %d link(s), want 0", got)
	}
	// And nothing was cached, which is the second half: a disputed pair is not a
	// decision, so a cache row would skip it forever on a verdict nobody reached.
	checks, err := store.SupersedeChecked(context.Background(), "p")
	if err != nil {
		t.Fatalf("SupersedeChecked: %v", err)
	}
	if len(checks) != 0 {
		t.Errorf("a disputed pair wrote %d NEITHER cache row(s); a split verdict is not a decision and caching it skips the pair for the life of its text", len(checks))
	}
}

// TestConsensusDoesNotMultiplyThePairsSkipIfUnchangedWouldSkip is the
// composition requirement with #792, and the cost argument with it.
//
// A live edge whose endpoints have not moved is not re-asked even once, and a
// NEITHER-cached fresh pair is not re-asked at all. Neither should become N
// questions. So the test seeds a converged project — one live edge, one cached
// NEITHER, one fresh pair — and asserts the passes were asked about the fresh
// pair N times and about NOTHING else. The counter is the point: without it, a
// gate that tripled a converged project's calls would look identical in the
// report and be invisible in the graph.
func TestConsensusDoesNotMultiplyThePairsSkipIfUnchangedWouldSkip(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	// A converged pair: an edge written long ago with neither endpoint edited
	// since. unchangedLiveEdge is the existing helper for exactly this shape and
	// is used rather than rebuilt so the test cannot drift from what #792 means
	// by "unchanged".
	// quietNewer is the LATER note of the pair, which is the direction
	// unchangedLiveEdge requires: it asserts the scan re-proposes the pair in the
	// edge's own direction, and a link written against it points newer->older.
	quietOlder := add(t, store, db, "the metrics listener binds to port 9091", []float32{1, 0, 0, 0}, "2026-01-01 00:00:00")
	quietNewer := add(t, store, db, "the metrics listener binds to port 9091 and nothing else", []float32{1, 0.01, 0, 0}, "2026-01-02 00:00:00")
	unchangedLiveEdge(t, store, quietNewer, quietOlder)

	// A fresh pair whose NEITHER was cached under the current content hashes.
	// Orthogonal to the pair above, so the scan proposes it as its own candidate
	// and the cached skip is about the cache rather than about similarity.
	cachedOlder := add(t, store, db, "outbound webhooks get 9 retry attempts", []float32{0, 0, 1, 0}, "2026-01-15 00:00:00")
	cachedNewer := add(t, store, db, "outbound webhooks get 3 retry attempts", []float32{0, 0, 1, 0.01}, "2026-02-01 00:00:00")
	if err := store.MarkSupersedeNeither(ctx, "p", map[[2]string]memory.SupersedeCheck{
		{cachedNewer, cachedOlder}: {NewerHash: contentHash("outbound webhooks get 3 retry attempts"), OlderHash: contentHash("outbound webhooks get 9 retry attempts")},
	}); err != nil {
		t.Fatalf("seed NEITHER cache: %v", err)
	}

	// One genuinely fresh pair, orthogonal to the other two.
	freshOlder := add(t, store, db, "the ingest service runs redis 6.2", []float32{0, 0, 0, 1}, "2026-03-01 00:00:00")
	freshNewer := add(t, store, db, "the ingest service now runs redis 7.2", []float32{0, 0, 0, 1}, "2026-09-01 00:00:00")

	cls := &perPassClassifier{script: []func(string, string) Relation{
		supersedesEverywhere, supersedesEverywhere, supersedesEverywhere,
	}}
	res, _, err := RunWith(ctx, store, cls, "p", Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default())
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}

	// The multiplier, as pairs: N passes over the ONE pair that survived every
	// free filter. The skipped pair contributed zero to it, not three.
	if res.ConsensusPairsAsked != 3 {
		t.Errorf("ConsensusPairsAsked = %d, want 3: the passes must be spent on the fresh pair alone", res.ConsensusPairsAsked)
	}
	if cls.pairsAsked != 3 {
		t.Errorf("the passes asked about %d pair(s) in total, want 3 — an unchanged live edge or a cached NEITHER must not be asked N times", cls.pairsAsked)
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (the cached NEITHER pair)", res.Skipped)
	}
	if res.Created != 1 {
		t.Errorf("Created = %d, want 1 (the fresh pair, agreed by all three passes)", res.Created)
	}
	if got := linkCount(t, store, freshNewer, freshOlder); got != 1 {
		t.Errorf("the fresh pair has %d link(s), want 1", got)
	}
	// The quiet edge was not re-asked, so it was not re-rolled: a single
	// re-confirm would have moved its stamp and a split verdict would have
	// withdrawn it.
	if got := linkCount(t, store, quietNewer, quietOlder); got != 1 {
		t.Errorf("the unchanged live edge now has %d link(s), want 1: a pass that did not ask must not have moved it", got)
	}
}

// TestConsensusDryRunWritesNothing is the dry-run half, and it is a separate test
// rather than a table row because a dry run that reports a gate and writes
// anyway is a different defect from one that reports nothing.
func TestConsensusDryRunWritesNothing(t *testing.T) {
	const (
		olderT = "the ingest service runs redis 6.2"
		newerT = "the ingest service now runs redis 7.2, pin moved in the upgrade"
	)
	store, older, newer := twoNotePair(t, olderT, newerT)
	cls := &perPassClassifier{script: []func(string, string) Relation{
		supersedesEverywhere, supersedesEverywhere, supersedesEverywhere,
	}}
	res, classified, err := RunWith(context.Background(), store, cls, "p",
		Options{Threshold: 0.9, Apply: false, Consensus: 3}, slog.Default())
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	// A dry run reports exactly what --apply would write, including the gate's
	// effect on the report, and touches nothing.
	if res.Confirmed != 1 || res.Created != 0 {
		t.Errorf("confirmed/created = %d/%d, want 1/0", res.Confirmed, res.Created)
	}
	if len(classified) != 1 || classified[0].Relation != RelationSupersedes {
		t.Errorf("classified = %+v, want one supersedes row the operator can read before applying", classified)
	}
	if res.Consensus != 3 || res.ConsensusPairsAsked != 3 {
		t.Errorf("Consensus/ConsensusPairsAsked = %d/%d, want 3/3: a dry run reports the multiplier it would pay", res.Consensus, res.ConsensusPairsAsked)
	}
	if got := linkCount(t, store, newer, older); got != 0 {
		t.Errorf("a dry run wrote %d link(s), want 0", got)
	}
	checks, err := store.SupersedeChecked(context.Background(), "p")
	if err != nil {
		t.Fatalf("SupersedeChecked: %v", err)
	}
	if len(checks) != 0 {
		t.Errorf("a dry run wrote %d cache row(s), want 0", len(checks))
	}
}

// TestConsensusUnanimousNeitherCachesAndUnanimousReversedRefuses: the gate is on
// the WRITE, so the other two verdicts have to keep their own behaviour when every
// pass agrees on them. A unanimous NEITHER is a decision (and is cached, so a
// later pass costs nothing); a unanimous REVERSED is still refused rather than
// written backwards, and is still never cached — the two properties #641 bought
// are not properties of "every pass said the same thing".
func TestConsensusUnanimousNeitherCachesAndUnanimousReversedRefuses(t *testing.T) {
	t.Run("neither is cached", func(t *testing.T) {
		store, _, _ := twoNotePair(t,
			"prod database is postgres 16",
			"staging database is postgres 16")
		cls := &perPassClassifier{script: []func(string, string) Relation{
			neitherEverywhere, neitherEverywhere, neitherEverywhere,
		}}
		res, _, err := RunWith(context.Background(), store, cls, "p",
			Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default())
		if err != nil {
			t.Fatalf("RunWith: %v", err)
		}
		if res.Created != 0 || res.CausesCreated != 0 {
			t.Errorf("a unanimous NEITHER wrote an edge: created/causes = %d/%d", res.Created, res.CausesCreated)
		}
		checks, err := store.SupersedeChecked(context.Background(), "p")
		if err != nil {
			t.Fatalf("SupersedeChecked: %v", err)
		}
		if len(checks) != 1 {
			t.Errorf("a unanimous NEITHER wrote %d cache row(s), want 1: every pass agreeing IS a decision, and caching it is what makes the next pass free", len(checks))
		}
	})
	t.Run("reversed is refused and not cached", func(t *testing.T) {
		store, older, newer := twoNotePair(t,
			"the sync job failure is fixed and the job has been green for a week",
			"the sync job is still failing on the nightly run")
		cls := &perPassClassifier{script: []func(string, string) Relation{
			func(_, _ string) Relation { return RelationReversed },
			func(_, _ string) Relation { return RelationReversed },
			func(_, _ string) Relation { return RelationReversed },
		}}
		res, _, err := RunWith(context.Background(), store, cls, "p",
			Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default())
		if err != nil {
			t.Fatalf("RunWith: %v", err)
		}
		if res.Reversed != 1 || res.Created != 0 {
			t.Errorf("Reversed/Created = %d/%d, want 1/0: unanimity does not license the backwards link", res.Reversed, res.Created)
		}
		if got := linkCount(t, store, newer, older, older, newer); got != 0 {
			t.Errorf("a unanimous REVERSED wrote %d link(s), want 0 in either direction", got)
		}
		checks, err := store.SupersedeChecked(context.Background(), "p")
		if err != nil {
			t.Fatalf("SupersedeChecked: %v", err)
		}
		if len(checks) != 0 {
			t.Errorf("a unanimous REVERSED wrote %d cache row(s), want 0: a reversal is never cached or the pair is frozen for the life of its text", len(checks))
		}
	})
}

// TestConsensusUnreadableIsNotDisagreement holds the three outcomes apart. A
// pass whose reply the parser could not read has expressed no opinion, so a pair
// where every pass was unreadable is UNCLASSIFIED — nothing decided, to be
// re-asked — and not "not agreed", which would report a harness or prompt fault
// as model instability and send an operator to raise the quorum for it.
func TestConsensusUnreadableIsNotDisagreement(t *testing.T) {
	t.Run("all passes unreadable", func(t *testing.T) {
		store, _, _ := twoNotePair(t,
			"the ingest service runs redis 6.2",
			"the ingest service now runs redis 7.2")
		blank := func(_, _ string) Relation { return "" }
		cls := &perPassClassifier{script: []func(string, string) Relation{blank, blank, blank}}
		res, classified, err := RunWith(context.Background(), store, cls, "p",
			Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default())
		if err != nil {
			t.Fatalf("RunWith: %v", err)
		}
		if res.Unclassified != 1 {
			t.Errorf("Unclassified = %d, want 1", res.Unclassified)
		}
		if res.NotAgreed != 0 || len(res.Disputed) != 0 {
			t.Errorf("NotAgreed/Disputed = %d/%d, want 0/0: no pass read the pair, so nothing disagreed about it", res.NotAgreed, len(res.Disputed))
		}
		if len(classified) != 0 {
			t.Errorf("classified %d row(s), want 0", len(classified))
		}
	})
	t.Run("one pass unreadable, two agreeing", func(t *testing.T) {
		store, _, _ := twoNotePair(t,
			"the ingest service runs redis 6.2",
			"the ingest service now runs redis 7.2")
		blank := func(_, _ string) Relation { return "" }
		cls := &perPassClassifier{script: []func(string, string) Relation{
			supersedesEverywhere, blank, supersedesEverywhere,
		}}
		res, _, err := RunWith(context.Background(), store, cls, "p",
			Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default())
		if err != nil {
			t.Fatalf("RunWith: %v", err)
		}
		// Disputed, not agreed: a pass that could not read the pair has not
		// endorsed the two that did, and the gate writes only settled edges.
		if res.NotAgreed != 1 || res.Created != 0 {
			t.Errorf("NotAgreed/Created = %d/%d, want 1/0", res.NotAgreed, res.Created)
		}
		if len(res.Disputed) != 1 || res.Disputed[0].Unreadable != 1 {
			t.Errorf("Disputed = %+v, want one record with Unreadable = 1: the unreadable pass is the diagnosis", res.Disputed)
		}
		if got := res.Disputed[0].Tally[RelationSupersedes]; got != 2 {
			t.Errorf("the readable tally holds %d supersedes, want 2", got)
		}
	})
}

// TestConsensusReportsItsOwnMultiplierOnAConvergedProject: a gated pass with
// nothing to ask still has to say it ran, and say it ran N times over zero pairs.
//
// This is the case where the gate is working as designed — every fresh pair
// cached, every live edge unchanged — and a report that read "consensus 0" or
// omitted the line would say the opposite: that no gate was configured, or that
// the run found nothing to do for some other reason. The difference matters to
// the operator deciding whether a converged project is quiet because the gate
// found nothing to disagree about or because the gate was never run.
func TestConsensusReportsItsOwnMultiplierOnAConvergedProject(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	// One cached NEITHER and nothing else, so there is no pending pair at all.
	older := add(t, store, db, "prod database is postgres 16", []float32{1, 0, 0}, "2026-01-01 00:00:00")
	newer := add(t, store, db, "staging database is postgres 16", []float32{0.99, 0.01, 0}, "2026-02-01 00:00:00")
	if err := store.MarkSupersedeNeither(ctx, "p", map[[2]string]memory.SupersedeCheck{
		{newer, older}: {NewerHash: contentHash("staging database is postgres 16"), OlderHash: contentHash("prod database is postgres 16")},
	}); err != nil {
		t.Fatalf("seed NEITHER cache: %v", err)
	}

	cls := &perPassClassifier{script: []func(string, string) Relation{supersedesEverywhere, supersedesEverywhere, supersedesEverywhere}}
	res, _, err := RunWith(ctx, store, cls, "p", Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default())
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	if cls.pairsAsked != 0 {
		t.Errorf("the passes asked about %d pair(s), want 0: a cached NEITHER is skipped once, not asked", cls.pairsAsked)
	}
	// The report's own numbers, and they are what the CLI's gate line is built
	// from — a value left at zero there prints a line claiming no gate ran.
	if res.Consensus != 3 {
		t.Errorf("Consensus = %d, want 3: a gated run must report its multiplier even when it had nothing to ask", res.Consensus)
	}
	if res.ConsensusPairsAsked != 0 {
		t.Errorf("ConsensusPairsAsked = %d, want 0", res.ConsensusPairsAsked)
	}
	if res.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", res.Skipped)
	}
	if got := linkCount(t, store, newer, older); got != 0 {
		t.Errorf("the cached pair has %d link(s), want 0", got)
	}
}

// TestConsensusIsReportedOnEveryReturnPath: the multiplier has to survive the
// early returns, and they are the paths a converged or fully-refused project
// takes — which is exactly where a gated run is most likely to be misread.
//
// There are two shapes here and the second is the one that shipped broken. The
// cached-NEITHER project reaches the classify loop with an empty `pending`, and
// the multiplier was set just before it. A project whose candidates are ALL
// vetoed never gets that far: it returns at the veto, and a Consensus left at
// zero there makes a gated run print no gate line at all, on a corpus that
// produced five candidate pairs. The report would read exactly like an ungated
// run, which is the failure the gate line exists to prevent.
func TestConsensusIsReportedOnEveryReturnPath(t *testing.T) {
	t.Run("every candidate vetoed", func(t *testing.T) {
		store, db := seed(t)
		ctx := context.Background()
		// An imperative older note with a newer note that never retires it: the
		// deterministic veto settles this for free, before any call, so the pass
		// returns at the veto block.
		add(t, store, db, "NEVER run the reindex job against the live cluster; it double-counts the postings table.", []float32{1, 0, 0}, "2026-01-01 00:00:00")
		add(t, store, db, "the reindex job was rebalanced last quarter and the postings table moved.", []float32{0.99, 0.01, 0}, "2026-06-01 00:00:00")

		cls := &perPassClassifier{script: []func(string, string) Relation{supersedesEverywhere, supersedesEverywhere, supersedesEverywhere}}
		res, _, err := RunWith(ctx, store, cls, "p", Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default())
		if err != nil {
			t.Fatalf("RunWith: %v", err)
		}
		if res.Vetoed != 1 {
			t.Fatalf("Vetoed = %d, want 1: the fixture is not exercising the early return this test is about", res.Vetoed)
		}
		if cls.pairsAsked != 0 {
			t.Errorf("asked %d pair(s), want 0: the veto settles a pair for free", cls.pairsAsked)
		}
		if res.Consensus != 3 {
			t.Errorf("Consensus = %d, want 3: a run that returns before the classify loop still ran under the gate, and a report reading 0 is indistinguishable from an ungated run", res.Consensus)
		}
		if res.ConsensusPairsAsked != 0 {
			t.Errorf("ConsensusPairsAsked = %d, want 0", res.ConsensusPairsAsked)
		}
	})
	t.Run("no candidate pairs at all", func(t *testing.T) {
		store, db := seed(t)
		add(t, store, db, "an unrelated note about nothing in particular", []float32{1, 0, 0}, "2026-01-01 00:00:00")
		cls := &perPassClassifier{script: []func(string, string) Relation{supersedesEverywhere, supersedesEverywhere, supersedesEverywhere}}
		res, _, err := RunWith(context.Background(), store, cls, "p",
			Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default())
		if err != nil {
			t.Fatalf("RunWith: %v", err)
		}
		if res.Candidates != 0 {
			t.Fatalf("Candidates = %d, want 0: the fixture is not exercising the empty path", res.Candidates)
		}
		if res.Consensus != 3 {
			t.Errorf("Consensus = %d, want 3: the gate ran even with nothing to ask", res.Consensus)
		}
	})
}

// TestConsensusSurvivesAnErrorReturn: an error path returns a Result alongside
// the error, and a caller that reads it — the bench harness does, and so does
// anything deciding whether to retry — must not be told the multiplier was 1 on a
// run configured for 3.
//
// The scan error is deliberately NOT in this test: `res` does not exist before
// SelectCandidates returns, and the CLI prints no report on an error at all (it
// writes the error to stderr and exits 1), so there is no page and no caller on
// which a wrong multiplier could be read. The two paths covered here are the
// link reads that happen after the result is built, where returning a bare
// Result{} discarded a Result that was already in hand.
func TestConsensusSurvivesAnErrorReturn(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	// A LIVE edge, and it is load-bearing: the reclassify content read is only
	// issued when there is a live edge whose endpoints need refreshing, so a
	// corpus with no edges never reaches the return under test and the fixture
	// would silently be scoring a different path.
	older := add(t, store, db, "the ingest service runs redis 6.2", []float32{1, 0, 0}, "2026-01-01 00:00:00")
	newer := add(t, store, db, "the ingest service now runs redis 7.2", []float32{0.99, 0.01, 0}, "2026-06-01 00:00:00")
	if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("seed live edge: %v", err)
	}

	// A store that fails the reclassify content read, which is the second of the
	// two post-construction error returns.
	failing := &linkReadFailStore{Store: store}
	cls := &perPassClassifier{script: []func(string, string) Relation{supersedesEverywhere, supersedesEverywhere, supersedesEverywhere}}
	res, _, err := RunWith(ctx, failing, cls, "p", Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default())
	if err == nil {
		t.Fatal("the injected read failure did not surface; the fixture is not exercising the error path")
	}
	if !strings.Contains(err.Error(), "load reclassify memory content") {
		t.Fatalf("error = %q, want the reclassify content read: a different return was reached and this test would be scoring it", err)
	}
	if res.Consensus != 3 {
		t.Errorf("Consensus = %d on an error return, want 3: a caller reading the Result alongside the error is told the multiplier that was configured", res.Consensus)
	}
}

// linkReadFailStore fails exactly one store method — the reclassify content read
// — so the error path under test is reached without touching anything else. It
// embeds the real store rather than reimplementing the interface, because the
// point is the RETURN on the error path, not the read.
type linkReadFailStore struct {
	*memory.Store
}

func (f *linkReadFailStore) GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error) {
	return nil, fmt.Errorf("injected: the store is unavailable")
}

// TestTheDisputedTypesCommentNamesTheRemediesThatWork holds the godoc on
// `Disputed` to the same rule the report and both doc pages are held to, and it
// exists because the first version of that comment said the opposite.
//
// A split between two readable verdicts reads as a model that cannot decide the
// pair, and the natural first suggestion is a bigger sample. It is the wrong
// suggestion: unanimity is STRICTLY HARDER to satisfy as N grows, so a pair that
// split 2-1 at 3 has to satisfy one more pass at 4. The remedies are a re-run
// (fresh passes may land on the same answer) and, for an operator, dropping the
// flag to write what the first pass said.
//
// What the four statements share, and what this comment is careful to claim only
// that, is the REFUSAL of a higher N: the report, docs/cli.md and
// docs/architecture.md all say it is not a remedy, and the report and both pages
// all offer a re-run. They do NOT all offer a different harness — the report and
// both pages offer dropping the flag instead, and only the shipped comment names
// a different harness, which is a maintainer's move rather than an operator's
// and is supported by the note at supersedeNotAgreedLines ("only a lower N or a
// different harness changes what is being asked"). Nothing here measures how a
// split behaves as N grows beyond getting harder to satisfy, so nothing here
// claims a trend.
//
// The comment is what a maintainer reads BEFORE touching the split path, and it
// is the only one of the four that is not shown to an operator, so a wrong
// remedy there is acted on silently.
//
// A comment is worth a test here for the same reason it is worth a test in the
// report: a doc is a contract, and this one had already broken.
func TestTheDisputedTypesCommentNamesTheRemediesThatWork(t *testing.T) {
	// The comment is the doc comment on the type, so it is read off the source
	// rather than duplicated here: a test that asserted its own copy of the text
	// would pass while the comment said something else.
	src := readSource(t, "supersede.go")
	start := strings.Index(src, "// Disputed is one pair the consensus gate refused")
	if start < 0 {
		t.Fatal("supersede.go has no doc comment on Disputed")
	}
	end := strings.Index(src[start:], "\ntype Disputed struct")
	if end < 0 {
		t.Fatal("could not find the end of the Disputed doc comment")
	}
	comment := src[start : start+end]

	if !strings.Contains(comment, "raising N makes unanimity") {
		t.Error("the Disputed doc comment does not say that raising N makes unanimity harder, so a reader can take the omission as neutral rather than as the wrong advice it is")
	}
	if !strings.Contains(comment, "STRICTLY HARDER") {
		t.Error("the Disputed doc comment does not say unanimity is STRICTLY HARDER to satisfy as N grows, which is the fact that makes a higher N a non-remedy rather than merely a weak one")
	}
	for _, want := range []string{"a re-run (fresh passes may agree)", "a different harness"} {
		if !strings.Contains(comment, want) {
			t.Errorf("the Disputed doc comment does not name %q as a remedy; the two the report and docs/cli.md offer are a re-run and a different harness", want)
		}
	}
	// And the wrong remedy, if it appears, has to appear as the thing NOT to do.
	if i := strings.Index(comment, "higher N"); i >= 0 {
		window := comment[max(0, i-90):min(len(comment), i+40)]
		if !strings.Contains(window, "NOT a higher N") && !strings.Contains(window, "NOT a") {
			t.Errorf("the Disputed doc comment offers %q without ruling it out: raising N is the opposite of a remedy, so naming it bare sends a maintainer the wrong way", "higher N")
		}
	}
}

// readSource reads one non-test source file in this package, for a check whose
// subject is the file's own prose rather than its behaviour.
func readSource(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// TestConsensusRefusesEveryTwoToOneSplit is (g): a split is a split whatever it
// splits BETWEEN, and the four ways it can look are each separately wrong if the
// tally folds one of them into another.
//
// The dangerous one is 2-SUPERSEDES / 1-REVERSED, because the two verdicts look
// adjacent — they are the same question asked in two directions — and merging
// them (`case v.supersedes + v.reversed == passes`) is a one-line change that
// turns "two passes agree, one refuses the direction" into an EDGE. That is the
// worst outcome the gate has: Run writes a live `supersedes` edge and then, on
// the row itself, reports the pair as carrying a REVERSED verdict it never
// returned. #641's harm was a backwards edge; this would be a forwards edge
// built out of a refusal.
//
// So the tally is exercised over every two-to-one split, and a real pass is run
// for the dangerous one rather than only calling quorum() — the graph is where
// the harm is, and a counter that reads right while the edge exists is not a
// fix.
func TestConsensusRefusesEveryTwoToOneSplit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		a, b     Relation
		wantFrom string
	}{
		{"supersedes and reversed", RelationSupersedes, RelationReversed, "two passes said SUPERSEDES and one said REVERSED"},
		{"supersedes and neither", RelationSupersedes, RelationNeither, "two said SUPERSEDES and one said NEITHER"},
		{"supersedes and causes", RelationSupersedes, RelationCauses, "two said SUPERSEDES and one said CAUSES"},
		{"reversed and neither", RelationReversed, RelationNeither, "two said REVERSED and one said NEITHER"},
		{"reversed and causes", RelationReversed, RelationCauses, "two said REVERSED and one said CAUSES"},
		{"neither and causes", RelationNeither, RelationCauses, "two said NEITHER and one said CAUSES"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := voteCount{}
			v.add(tc.a)
			v.add(tc.a)
			v.add(tc.b)
			verdict, state := v.quorum(3)
			if state != disputed {
				t.Fatalf("a 2/1 split (%s) reached %q, want disputed: %s is not unanimity, and a merged tally would turn a refusal into an edge", tc.wantFrom, verdict, tc.wantFrom)
			}
			if verdict != "" {
				t.Errorf("a disputed pair carried verdict %q; a disputed pair acts on nothing", verdict)
			}
			// The tally still records both, because the report shows it.
			if v.tally()[tc.a] != 2 || v.tally()[tc.b] != 1 {
				t.Errorf("tally = %v, want 2x%q and 1x%q", v.tally(), tc.a, tc.b)
			}
		})
	}
}

// TestARejectedDirectionNeverBecomesAnEdge is the same split through the real
// pass, and it is the half a tally unit test cannot reach. Two passes answer
// SUPERSEDES for a fresh pair and the third answers REVERSED — the pair the gate
// must refuse — and the assertion is on the GRAPH in both directions plus the
// counts, because a Result that reads right while an edge exists is not a fix.
func TestARejectedDirectionNeverBecomesAnEdge(t *testing.T) {
	store, older, newer := twoNotePair(t,
		"the ingest service runs redis 6.2",
		"the ingest service now runs redis 7.2, pin moved in the upgrade")
	// The script is indexed by PASS, so the third entry is the third pass — and
	// that one refuses the direction.
	cls := &perPassClassifier{script: []func(string, string) Relation{
		supersedesEverywhere,
		supersedesEverywhere,
		func(_, _ string) Relation { return RelationReversed },
	}}
	res, classified, err := RunWith(context.Background(), store, cls, "p",
		Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default())
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	if res.NotAgreed != 1 {
		t.Errorf("NotAgreed = %d, want 1: two passes agreeing and one refusing the direction is still a split", res.NotAgreed)
	}
	if res.Confirmed != 0 || res.Created != 0 || res.Reversed != 0 {
		t.Errorf("confirmed/created/reversed = %d/%d/%d, want 0/0/0: the refused pass's REVERSED is not this run's verdict to report either", res.Confirmed, res.Created, res.Reversed)
	}
	if len(classified) != 0 {
		t.Errorf("classified %d row(s), want 0: a split reaches neither the counts nor the decided list", len(classified))
	}
	if got := linkCount(t, store, newer, older, older, newer); got != 0 {
		t.Errorf("the pair has %d link(s), want 0 in EITHER direction", got)
	}
	checks, err := store.SupersedeChecked(context.Background(), "p")
	if err != nil {
		t.Fatalf("SupersedeChecked: %v", err)
	}
	if len(checks) != 0 {
		t.Errorf("a split wrote %d NEITHER cache row(s), want 0: a pair nobody settled must be re-asked, not frozen", len(checks))
	}
}

// TestConsensusLeavesALiveEdgeAloneWhenThePassesSplit is the second half of (g):
// a disputed pair on a pair the graph ALREADY links is not a withdrawal, and the
// code that could get this wrong does not exist — the disputed branch `continue`s
// before the apply block, so nothing in `Run` invalidates. A mutation that adds
// an InvalidateLink there would withdraw a correct edge, which is the one
// direction the gate's whole argument forbids: "the passes split" is a statement
// about the MODEL's stability and says nothing about which endpoint is current.
//
// The fixture is a live edge whose endpoints are edited (so skip-if-unchanged
// does not hold it quiet), the passes split on it, and the edge has to survive —
// with its stamp, because a write would have moved it.
func TestConsensusLeavesALiveEdgeAloneWhenThePassesSplit(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	older := add(t, store, db, "the ingest service runs redis 6.2", []float32{1, 0, 0}, "2026-01-01 00:00:00")
	newer := add(t, store, db, "the ingest service now runs redis 7.2", []float32{0.99, 0.01, 0}, "2026-06-01 00:00:00")
	// A live edge whose stamp predates the endpoints' current updated_at, so
	// skip-if-unchanged does NOT hold the pair quiet and the passes really see
	// it. The stamp is the CONTENT freshness, which is what the test reads back.
	seeded := "2026-01-03 00:00:00"
	if err := store.CreateLinkJudged(ctx, newer, older, "supersedes", 0.95, "llm", seeded); err != nil {
		t.Fatalf("seed live edge: %v", err)
	}

	cls := &perPassClassifier{script: []func(string, string) Relation{
		supersedesEverywhere,
		supersedesEverywhere,
		func(_, _ string) Relation { return RelationReversed },
	}}
	res, _, err := RunWith(ctx, store, cls, "p", Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default())
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	if res.NotAgreed != 1 {
		t.Errorf("NotAgreed = %d, want 1", res.NotAgreed)
	}
	if res.Reclassified != 0 {
		t.Errorf("Reclassified = %d, want 0: a split did not re-judge anything, so nothing was re-linked and nothing was withdrawn", res.Reclassified)
	}
	if got := linkCount(t, store, newer, older); got != 1 {
		t.Fatalf("the live edge has %d link(s), want 1: a disputed pair must not withdraw an edge the graph already holds", got)
	}
	// And the stamp is unmoved, which is the finer half: a write would have
	// refreshed it even if the link count happened to survive, and a refreshed
	// stamp is a re-confirmation of a verdict nobody reached.
	links, err := store.LinksByRelationSource(ctx, "p", "supersedes", "llm")
	if err != nil {
		t.Fatalf("LinksByRelationSource: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("want the one seeded edge, got %d", len(links))
	}
	if links[0].CreatedAt != seeded {
		t.Errorf("the edge's stamp moved to %q, want the seeded %q: a disputed pass must not re-confirm an edge", links[0].CreatedAt, seeded)
	}
	// The pair is re-asked next pass rather than frozen: no cache row either way,
	// because a live pair is never cache-eligible.
	checks, err := store.SupersedeChecked(ctx, "p")
	if err != nil {
		t.Fatalf("SupersedeChecked: %v", err)
	}
	if len(checks) != 0 {
		t.Errorf("a disputed LIVE pair wrote %d cache row(s), want 0", len(checks))
	}
}

// TestConsensusAGateOfOneIsTheUngatedPass: the default is a single pass, and it
// must be byte-for-byte the historical behaviour. A default that quietly gated
// on one pass' agreement with itself would be a no-op costing nothing — which is
// fine — but a default that gated on anything else would be a behaviour change
// nobody asked for, so the ungated case is asserted directly: one pass, one call,
// and the split pair written.
func TestConsensusAGateOfOneIsTheUngatedPass(t *testing.T) {
	store, older, newer := twoNotePair(t,
		"the ingest service runs redis 6.2",
		"the ingest service now runs redis 7.2")
	cls := &perPassClassifier{script: []func(string, string) Relation{supersedesEverywhere}}
	res, _, err := RunWith(context.Background(), store, cls, "p",
		Options{Threshold: 0.9, Apply: true, Consensus: 1}, slog.Default())
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	if cls.pairsAsked != 1 || len(cls.passSizes) != 1 {
		t.Errorf("the pass asked %d pair(s) over %d call(s), want 1/1", cls.pairsAsked, len(cls.passSizes))
	}
	if res.Created != 1 || res.Consensus != 1 {
		t.Errorf("Created/Consensus = %d/%d, want 1/1", res.Created, res.Consensus)
	}
	if got := linkCount(t, store, newer, older); got != 1 {
		t.Errorf("the pair has %d link(s), want 1: a single pass is the ordinary pass", got)
	}
}

// TestRunIsTheUngatedConsensusPass: Run and RunWith(Consensus: 0) are the same
// decision, and they are two entry points rather than one so that a caller which
// has not heard of the gate keeps compiling. The test holds them to each other
// through the graph, because a Run that quietly gained a multiplier would write
// the same links and differ only in cost.
func TestRunIsTheUngatedConsensusPass(t *testing.T) {
	runOne := func(useRun bool) int {
		store, older, newer := twoNotePair(t,
			"the ingest service runs redis 6.2",
			"the ingest service now runs redis 7.2")
		cls := &perPassClassifier{script: []func(string, string) Relation{supersedesEverywhere}}
		var err error
		if useRun {
			_, _, err = Run(context.Background(), store, cls, "p", 0.9, true, slog.Default())
		} else {
			_, _, err = RunWith(context.Background(), store, cls, "p", Options{Threshold: 0.9, Apply: true}, slog.Default())
		}
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if cls.pairsAsked != 1 {
			t.Errorf("asked %d pair(s), want 1: an unset consensus must not gate", cls.pairsAsked)
		}
		return linkCount(t, store, newer, older)
	}
	if got := runOne(true); got != 1 {
		t.Errorf("Run wrote %d link(s), want 1", got)
	}
	if got := runOne(false); got != 1 {
		t.Errorf("RunWith with no consensus wrote %d link(s), want 1", got)
	}
}

// TestConsensusClassifyFailureIsFatalAndNamesThePass: a transport failure must
// still abort the pass — every gate is worse than no gate if a dead harness reads
// as an empty result — and the message has to name WHICH pass failed, because at
// N=3 a reader who is not told which one died cannot tell a first-call failure
// from a third-call one, and the remedy (rerun) is the same but the diagnosis is
// not.
func TestConsensusClassifyFailureIsFatalAndNamesThePass(t *testing.T) {
	store, _, _ := twoNotePair(t,
		"the ingest service runs redis 6.2",
		"the ingest service now runs redis 7.2")
	cls := &perPassClassifier{
		script: []func(string, string) Relation{supersedesEverywhere, supersedesEverywhere, supersedesEverywhere},
		err:    fmt.Errorf("harness exited 1"),
	}
	_, _, err := RunWith(context.Background(), store, cls, "p",
		Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default())
	if err == nil {
		t.Fatal("a dead harness must abort a gated pass, not read as an empty result")
	}
	if got := err.Error(); !contains(got, "pass 1 of 3") {
		t.Errorf("error %q must name the failing pass", got)
	}
}

// TestConsensusVerdictCountMismatchIsFatal: a classifier that answers a different
// number of verdicts than it was asked about is a broken call, and voting over a
// misaligned slice would pair pass 1's answer to pass 2's pair.
func TestConsensusVerdictCountMismatchIsFatal(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	add(t, store, db, "kubernetes cluster runs version 1.27", []float32{1, 0, 0}, "2026-01-01 00:00:00")
	add(t, store, db, "kubernetes upgraded to 1.29", []float32{0.99, 0.01, 0}, "2026-04-01 00:00:00")
	add(t, store, db, "kubernetes now on 1.31", []float32{0.98, 0.02, 0}, "2026-07-01 00:00:00")
	cls := &mockShortClassifier{}
	if _, _, err := RunWith(ctx, store, cls, "p", Options{Threshold: 0.9, Apply: true, Consensus: 3}, slog.Default()); err == nil {
		t.Fatal("a verdict count that does not match the pair count must abort the pass")
	}
}

// contains is a substring test kept local so the failure messages above can
// name a substring without pulling in strings for one call.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
