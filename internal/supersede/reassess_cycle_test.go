package supersede

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// seedCycle writes the CYCLE a pass before #778 could leave: both directions of
// one pair live, each a real graph row, over two memories whose timestamps DO
// order them. The pair is the #641 shape, and both edges are the state the
// creation pass now refuses outright (Result.Bidirectional) and names
// `ghost supersede --reassess --apply` as the repair — so the repair has to be
// able to do it, or the advertised command is a dry run that changes nothing.
//
// fixFirst chooses which edge is written first, and therefore which one the
// store hands back first. That order is not a promise — LinksByRelationSource
// has no ORDER BY — and the pass must not read it as a direction, so every
// assertion about a cycle is written over BOTH orders. That is what keeps a
// pass which happens to agree with this machine's row order from passing for the
// right reason.
func seedCycle(t *testing.T, store *memory.Store, db *sql.DB, staleText, fixText string, fixFirst bool) (stale, fix string) {
	t.Helper()
	ctx := context.Background()
	// The stale note is the OLDER one, and the edge onto the fix is the
	// wrong-direction link #641 found; the second edge is the one the same-run
	// bidirectional defect added on top of it.
	stale = add(t, store, db, staleText, []float32{1, 0, 0, 0}, "2026-01-01 00:00:00")
	fix = add(t, store, db, fixText, []float32{0.99, 0, 0}, "2026-09-01 00:00:00")
	dirs := [][2]string{{stale, fix}, {fix, stale}}
	if fixFirst {
		dirs = [][2]string{{fix, stale}, {stale, fix}}
	}
	for _, dir := range dirs {
		if err := store.CreateLink(ctx, dir[0], dir[1], string(RelationSupersedes), 0.95, "llm"); err != nil {
			t.Fatal(err)
		}
	}
	return stale, fix
}

// linkOrderStore hands Reassess the live edges in a chosen order. It exists
// because "the store's row order is not a promise" is only testable if a test can
// CHOOSE the order: the shipped query has no ORDER BY and in practice returns the
// edge whose source id sorts first, which on random hex ids is a coin flip — so a
// test that relied on it would pin one side of that flip or the other, and a pass
// that read the order as a direction would pass half the time for the wrong
// reason. Every cycle assertion therefore runs on both.
type linkOrderStore struct {
	*memory.Store
	reversed bool
}

func (s linkOrderStore) LinksByRelationSource(ctx context.Context, projectID, relation, source string) ([]memory.Link, error) {
	links, err := s.Store.LinksByRelationSource(ctx, projectID, relation, source)
	if err != nil || !s.reversed {
		return links, err
	}
	out := make([]memory.Link, 0, len(links))
	for i := len(links) - 1; i >= 0; i-- {
		out = append(out, links[i])
	}
	return out, nil
}

// cycleOrders is the two row orders a cycle's two edges can come back in. Which
// one a given run gets is a property of the stored ids, never of the pass.
var cycleOrders = []struct {
	name     string
	fixFirst bool
	reversed bool
}{
	{name: "the store's first row is the stale→fix edge", fixFirst: false, reversed: false},
	{name: "the store's first row is the fix→stale edge", fixFirst: true, reversed: true},
}

// outcomeForEdge is the CycleOutcome that names a given live edge as the one
// that stands, read off the pair the pass reported. The constants name a
// POSITION in the pair, and the position is the store's row order, so a test
// pins the constant that matches the edge that survived rather than a position
// it read from the store first.
func outcomeForEdge(t *testing.T, c CyclicPair, edge [2]string) CycleOutcome {
	t.Helper()
	switch edge {
	case [2]string{c.First.SourceID, c.First.TargetID}:
		return CycleKeptFirst
	case [2]string{c.Second.SourceID, c.Second.TargetID}:
		return CycleKeptSecond
	default:
		t.Fatalf("edge %v is neither of the cycle's two (%+v)", edge, c)
		return ""
	}
}

// liveEdges reads the live 'supersedes' edges among ids, as the graph holds them.
func liveEdges(t *testing.T, store *memory.Store, ids ...string) [][2]string {
	t.Helper()
	pairs, err := store.SupersedesWithin(context.Background(), ids)
	if err != nil {
		t.Fatalf("SupersedesWithin: %v", err)
	}
	return pairs
}

// liveCausesEdges reads the live 'causes' edges touching any of ids, as the graph
// holds them. 'causes' is written older→newer, the reverse of 'supersedes', so a
// withdrawal of stale→fix is the one whose sweep takes fix→stale — and getting
// that backwards is what a sweep test has to be able to see, which is why the
// reader returns the direction rather than a count.
func liveCausesEdges(t *testing.T, store *memory.Store, ids ...string) [][2]string {
	t.Helper()
	seen := make(map[[2]string]bool)
	var out [][2]string
	for _, id := range ids {
		links, err := store.GetLinks(context.Background(), id)
		if err != nil {
			t.Fatalf("GetLinks(%s): %v", id, err)
		}
		for _, l := range links {
			if l.Relation != string(RelationCauses) {
				continue
			}
			edge := [2]string{l.SourceID, l.TargetID}
			if !seen[edge] {
				seen[edge] = true
				out = append(out, edge)
			}
		}
	}
	return out
}

// seedCauses writes a live 'causes'/'llm' edge, which is what a withdrawal
// sweeps: a 'causes' link pointing INTO a note the pass just decided is still
// current asserts the opposite of that decision, and 'causes' runs older→newer,
// so the edge is (older of the supersedes edge, newer of it).
func seedCauses(t *testing.T, store *memory.Store, from, to string) {
	t.Helper()
	if err := store.CreateLink(context.Background(), from, to, string(RelationCauses), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}
}

// TestReassessJudgesACycleOnceAndWithdrawsTheEdgeItsVerdictDenies is the reason
// the advertised repair works. Reassess used to load every live edge and judge
// each as an INDEPENDENT candidate, so a pair claimed in both directions was
// asked the same question twice — and, with a classifier that cannot decline a
// direction, both answers were SUPERSEDES and the cycle survived the repair that
// exists to remove it, silently: Confirmed 2, Withdrawn 0.
//
// The pass now judges the UNORDERED pair once, in the orientation the timestamps
// give it, and a verdict that names a direction keeps exactly that edge and
// withdraws the one asserting the opposite.
func TestReassessJudgesACycleOnceAndWithdrawsTheEdgeItsVerdictDenies(t *testing.T) {
	for _, order := range cycleOrders {
		t.Run(order.name, func(t *testing.T) {
			base, db := seed(t)
			// Seeded through the base store, run through the wrapper: the rows the
			// pass sees come back in the order THIS sub-test is about, because the
			// store's own order is a property of the stored ids.
			store := linkOrderStore{Store: base, reversed: order.reversed}
			ctx := context.Background()
			stale, fix := seedCycle(t, base, db,
				"bug: the relay stalls on every consumer rebalance",
				"the relay rebalance stall is fixed: pin the consumer", order.fixFirst)

			cls := &supersedesEverything{}
			res, withdrawn, err := Reassess(ctx, store, cls, "p", true, discardLogger())
			if err != nil {
				t.Fatalf("Reassess: %v", err)
			}
			if len(cls.judged) != 1 {
				t.Errorf("the pass judged %d pair-orientation(s) %v, want 1: a pair with two live edges is ONE question", len(cls.judged), cls.judged)
			}
			if len(res.Cyclic) != 1 {
				t.Fatalf("Cyclic = %d pair(s), want 1: the report has to name the cycle, because a withdrawn count alone cannot show that a cycle was found", len(res.Cyclic))
			}
			// The classifier confirmed whatever direction it was asked about, and
			// the direction asked about is the newer→older one by updated_at: the
			// fix's own claim about the stale bug list. So THAT edge stands — in
			// either row order, since the pass reads the timestamps and not the
			// store's row order — and its reverse, the edge asserting the opposite,
			// is withdrawn.
			edges := liveEdges(t, base, stale, fix)
			if len(edges) != 1 || edges[0] != [2]string{fix, stale} {
				t.Fatalf("live supersedes edges = %v, want exactly [%s %s]: a cycle that survives the repair still demotes BOTH endpoints", edges, fix, stale)
			}
			if want := outcomeForEdge(t, res.Cyclic[0], edges[0]); res.Cyclic[0].Outcome != want {
				t.Errorf("outcome = %q, want %q: the constant has to name the edge that actually stands", res.Cyclic[0].Outcome, want)
			}
			if res.Withdrawn != 1 {
				t.Errorf("Result.Withdrawn = %d, want 1", res.Withdrawn)
			}
			if len(withdrawn) != 1 {
				t.Fatalf("withdrawn rows = %d, want 1", len(withdrawn))
			}
			if withdrawn[0].NewerID != stale || withdrawn[0].OlderID != fix {
				t.Errorf("withdrew %s→%s, want the edge the verdict denied: %s→%s", withdrawn[0].NewerID, withdrawn[0].OlderID, stale, fix)
			}
			if !strings.Contains(withdrawn[0].Reason, "reverse") {
				t.Errorf("reason %q does not say the edge was the reverse of the confirmed direction", withdrawn[0].Reason)
			}
		})
	}
}

// TestReassessReversedVerdictOnACycleKeepsTheOtherEdge: the mirror, and the case
// that shows the pass is reading the verdict rather than counting verdicts. Asked
// about fix→stale, a REVERSED verdict says the OLDER note (the stale bug list) is
// the current one — which is what the stale→fix edge asserts. So the edge the
// verdict names stands and the one it was asked about is withdrawn; a pass that
// withdrew both on REVERSED would delete the correct claim along with the wrong
// one and leave nothing.
func TestReassessReversedVerdictOnACycleKeepsTheOtherEdge(t *testing.T) {
	for _, order := range cycleOrders {
		t.Run(order.name, func(t *testing.T) {
			base, db := seed(t)
			// Seeded through the base store, run through the wrapper: the rows the
			// pass sees come back in the order THIS sub-test is about, because the
			// store's own order is a property of the stored ids.
			store := linkOrderStore{Store: base, reversed: order.reversed}
			ctx := context.Background()
			stale, fix := seedCycle(t, base, db,
				"bug: the relay stalls on every consumer rebalance",
				"the relay rebalance stall is fixed: pin the consumer", order.fixFirst)

			cls := &mockClassifier{verdict: func(string, string) Relation { return RelationReversed }}
			res, _, err := Reassess(ctx, store, cls, "p", true, discardLogger())
			if err != nil {
				t.Fatalf("Reassess: %v", err)
			}
			// Asked about fix→stale, REVERSED says the stale note is the current
			// one — which is what the stale→fix edge asserts, so THAT is the edge
			// that stands, whatever order the store returned the two rows in.
			edges := liveEdges(t, base, stale, fix)
			if len(edges) != 1 || edges[0] != [2]string{stale, fix} {
				t.Fatalf("live supersedes edges = %v, want exactly [%s %s]: a reversed verdict names the OTHER direction as current, so that edge is the one that stands", edges, stale, fix)
			}
			if len(res.Cyclic) != 1 {
				t.Fatalf("Cyclic = %+v, want the pair reported", res.Cyclic)
			}
			if want := outcomeForEdge(t, res.Cyclic[0], edges[0]); res.Cyclic[0].Outcome != want {
				t.Errorf("outcome = %q, want %q: the constant has to name the edge that actually stands", res.Cyclic[0].Outcome, want)
			}
			if res.Withdrawn != 1 {
				t.Errorf("Result.Withdrawn = %d, want 1: exactly the edge the verdict denied", res.Withdrawn)
			}
		})
	}
}

// TestReassessWithdrawsBothEdgesOfACycleTheVerdictDoesNotSupport: NEITHER and
// CAUSES say the two notes are not a replacement of one another — in EITHER
// direction, which is the only thing a verdict that declines to name a direction
// can mean about a pair that has an edge in both. Both go: an edge left live
// asserts what the verdict just denied, and a cycle is the version of that where
// neither endpoint is reachable.
func TestReassessWithdrawsBothEdgesOfACycleTheVerdictDoesNotSupport(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict Relation
		count   func(ReassessResult) int
	}{
		{name: "neither", verdict: RelationNeither, count: func(r ReassessResult) int { return r.Neither }},
		{name: "causes", verdict: RelationCauses, count: func(r ReassessResult) int { return r.Causes }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := seed(t)
			ctx := context.Background()
			stale, fix := seedCycle(t, store, db,
				"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
				"The restore path on one spindle is safe and takes under a minute.", false)

			cls := &mockClassifier{verdict: func(string, string) Relation { return tc.verdict }}
			res, _, err := Reassess(ctx, store, cls, "p", true, discardLogger())
			if err != nil {
				t.Fatalf("Reassess: %v", err)
			}
			if got := tc.count(res); got != 1 {
				t.Errorf("verdict count = %d, want 1: one question, one verdict", got)
			}
			if len(res.Cyclic) != 1 || res.Cyclic[0].Outcome != CycleBothWithdrawn {
				t.Fatalf("Cyclic = %+v, want one pair with both edges withdrawn", res.Cyclic)
			}
			if res.Withdrawn != 2 {
				t.Errorf("Result.Withdrawn = %d, want 2: a cycle needs both edges gone", res.Withdrawn)
			}
			if edges := liveEdges(t, store, stale, fix); len(edges) != 0 {
				t.Errorf("live supersedes edges = %v, want none: the pass cannot keep an edge that asserts what the verdict denied", edges)
			}
		})
	}
}

// TestReassessWithdrawsNeitherEdgeOfAnUndecidedCycle: the state where the pass
// CANNOT decide, and the one that has to be loud. A missing verdict, and a pair
// whose two rows share both timestamps so no direction is knowable at all, both
// leave the graph exactly as they found it: there is no verdict to act on, and
// withdrawing a half of a cycle on a hunch would be a claim this pass cannot
// make. The cycle is still demoting both endpoints, so the report has to say so
// and hand the operator the commands that settle it.
//
// This is the branch the timestamp tie and the unanswerable call share, and it
// is the one the creation pass's own rule points at: #778 made Run refuse to
// propose a direction it cannot know, and the repair refuses to invent one too.
func TestReassessWithdrawsNeitherEdgeOfAnUndecidedCycle(t *testing.T) {
	t.Run("no verdict", func(t *testing.T) {
		store, db := seed(t)
		stale, fix := seedCycle(t, store, db,
			"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
			"The restore path on one spindle is safe and takes under a minute.", false)

		cls := &mockClassifier{verdict: func(string, string) Relation { return Relation("") }}
		res, withdrawn, err := Reassess(context.Background(), store, cls, "p", true, discardLogger())
		if err != nil {
			t.Fatalf("Reassess: %v", err)
		}
		if res.Unclassified != 1 {
			t.Errorf("Result.Unclassified = %d, want 1: a missing verdict is counted, not dropped", res.Unclassified)
		}
		assertUndecidedCycle(t, store, res, withdrawn, stale, fix, CycleNoVerdict)
	})

	t.Run("tied timestamps", func(t *testing.T) {
		store, db := seed(t)
		ctx := context.Background()
		// A bulk-import stamp on both columns, so orient() cannot order the pair
		// (see #778) and the pass has no direction to ask the classifier about.
		const bulkStamp = "2026-09-20 09:26:05"
		a := add(t, store, db, "prod db timeout is 30s", []float32{1, 0, 0, 0}, bulkStamp)
		b := add(t, store, db, "prod db timeout is 5s", []float32{0.99, 0, 0, 0}, bulkStamp)
		for _, dir := range [][2]string{{a, b}, {b, a}} {
			if err := store.CreateLink(ctx, dir[0], dir[1], string(RelationSupersedes), 0.95, "llm"); err != nil {
				t.Fatal(err)
			}
		}

		cls := &supersedesEverything{}
		res, withdrawn, err := Reassess(ctx, store, cls, "p", true, discardLogger())
		if err != nil {
			t.Fatalf("Reassess: %v", err)
		}
		if len(cls.judged) != 0 {
			t.Errorf("the pass asked about a pair whose direction is unknowable: %v", cls.judged)
		}
		if res.Unoriented != 1 {
			t.Errorf("Result.Unoriented = %d, want 1: a tied pair is counted, and it is the one undecided cycle whose next step is the operator's", res.Unoriented)
		}
		assertUndecidedCycle(t, store, res, withdrawn, a, b, CycleUnoriented)
	})
}

// assertUndecidedCycle is the shared shape of the two outcomes that decide
// NOTHING: nothing withdrawn, the cycle reported, both edges still live. The
// outcome differs, and it is the one thing that tells the operator whether a re-run
// answers it or only they do — so the caller names which of the two it is looking
// at, rather than the test reading one sentence that claimed to be both.
func assertUndecidedCycle(t *testing.T, store *memory.Store, res ReassessResult, withdrawn []WithdrawnEdge, a, b string, outcome CycleOutcome) {
	t.Helper()
	if res.Withdrawn != 0 || len(withdrawn) != 0 {
		t.Errorf("withdrawn=%d listed=%d, want 0/0: with no verdict there is no basis for withdrawing half a cycle", res.Withdrawn, len(withdrawn))
	}
	if len(res.Cyclic) != 1 || res.Cyclic[0].Outcome != outcome {
		t.Fatalf("Cyclic = %+v, want exactly one pair reported as %q", res.Cyclic, outcome)
	}
	c := res.Cyclic[0]
	if (c.First.SourceID != a && c.First.SourceID != b) || (c.Second.SourceID != a && c.Second.SourceID != b) {
		t.Errorf("cycle names %s→%s and %s→%s, want the two edges of the pair %s/%s", c.First.SourceID, c.First.TargetID, c.Second.SourceID, c.Second.TargetID, a, b)
	}
	if edges := liveEdges(t, store, a, b); len(edges) != 2 {
		t.Errorf("live supersedes edges = %v, want both still live: the operator decides this one", edges)
	}
}

// TestReassessLeavesAnOrdinaryEdgeAloneWhileRepairingACycle: the cycle rule is
// narrow on purpose. An ordinary edge in the same project is still judged as its
// own candidate and answered by the same verdict function, so a fix that treated
// every pair as a cycle (or grouped the wrong rows together) fails here.
func TestReassessLeavesAnOrdinaryEdgeAloneWhileRepairingACycle(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	_, _ = seedEdge(t, store, db,
		"The ingest service now runs Redis 7.2: the compose pin moved to 7.2 in the upgrade.",
		"The ingest service runs Redis 6.2, pinned in the compose file.")
	stale, fix := seedCycle(t, store, db,
		"bug: the relay stalls on every consumer rebalance",
		"the relay rebalance stall is fixed: pin the consumer", false)

	cls := &supersedesEverything{}
	res, _, err := Reassess(ctx, store, cls, "p", true, discardLogger())
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Loaded != 3 {
		t.Errorf("Result.Loaded = %d, want 3 live edges (one ordinary pair, one cycle)", res.Loaded)
	}
	if len(cls.judged) != 2 {
		t.Errorf("judged %d orientation(s) %v, want 2: the ordinary pair and the cycle", len(cls.judged), cls.judged)
	}
	if res.Confirmed != 2 {
		t.Errorf("Result.Confirmed = %d, want 2: the cycle's confirmed verdict plus the ordinary edge's", res.Confirmed)
	}
	if res.Withdrawn != 1 {
		t.Errorf("Result.Withdrawn = %d, want 1: only the cycle's denied edge", res.Withdrawn)
	}
	if edges := liveEdges(t, store, stale, fix); len(edges) != 1 {
		t.Errorf("cycle edges = %v, want 1 remaining", edges)
	}
}

// TestReassessCycleSurvivesADryRunUnwritten: a dry run must not repair the
// cycle it reports, or the preview is a claim about a change it did not make.
func TestReassessCycleSurvivesADryRunUnwritten(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	stale, fix := seedCycle(t, store, db,
		"bug: the relay stalls on every consumer rebalance",
		"the relay rebalance stall is fixed: pin the consumer", false)

	cls := &supersedesEverything{}
	res, withdrawn, err := Reassess(ctx, store, cls, "p", false, discardLogger())
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Withdrawn != 0 {
		t.Errorf("Result.Withdrawn = %d, want 0 in a dry run", res.Withdrawn)
	}
	if len(withdrawn) != 1 {
		t.Fatalf("withdrawn rows = %d, want 1: the dry run still lists what it would do", len(withdrawn))
	}
	if withdrawn[0].Written {
		t.Error("a dry-run row claims its edge was withdrawn")
	}
	if edges := liveEdges(t, store, stale, fix); len(edges) != 2 {
		t.Errorf("live supersedes edges = %v, want the cycle untouched by a dry run", edges)
	}
}

// TestReassessSweepsTheCausesEdgeOfEveryCycleWithdrawalItDenies is the
// both-relations sweep on a cycle, and it is quieter here than it is for an
// ordinary edge — which is the whole reason it needs its own test.
// InvalidateLink writes a history row for a 'supersedes' edge and NONE for a
// 'causes' one, so a cycle withdrawal that drops its sweep deletes a second graph
// row with nothing in memory_history to show it ever happened, and a review found
// two mutations that turned the sweep off without any test noticing: one on the
// SUPERSEDES branch and one on the second row of the NEITHER branch.
//
// Three shapes, because the sweep is a per-ROW decision and each row's arm
// differs:
//
//   - SUPERSEDES keeps one edge and denies the other: that row sweeps.
//   - NEITHER denies both: BOTH rows sweep, and each sweeps the 'causes' edge
//     running the other way, so a pair holding two of them loses both.
//   - CAUSES denies both but sweeps NEITHER, because a causes verdict AFFIRMS that
//     relation — the same rule the ordinary path follows, and the one a "sweep
//     everything on a cycle" simplification would break.
func TestReassessSweepsTheCausesEdgeOfEveryCycleWithdrawalItDenies(t *testing.T) {
	t.Run("one edge denied, its causes edge swept", func(t *testing.T) {
		base, db := seed(t)
		store := linkOrderStore{Store: base, reversed: false}
		ctx := context.Background()
		stale, fix := seedCycle(t, base, db,
			"bug: the relay stalls on every consumer rebalance",
			"the relay rebalance stall is fixed: pin the consumer", false)
		// The denied edge is stale→fix, so the 'causes' edge its withdrawal
		// sweeps is fix→stale: 'causes' runs older→newer, and the sweep is
		// InvalidateLink(older, newer). Both are seeded, so the assertion can also
		// see that the KEPT edge's own 'causes' counterpart survives — only the
		// denied edge sweeps, and a 'causes' edge pointing INTO the note the
		// verdict affirmed may well be true.
		seedCauses(t, base, fix, stale)
		seedCauses(t, base, stale, fix)

		res, withdrawn, err := Reassess(ctx, store, &supersedesEverything{}, "p", true, discardLogger())
		if err != nil {
			t.Fatalf("Reassess: %v", err)
		}
		if res.Withdrawn != 1 || len(withdrawn) != 1 {
			t.Fatalf("withdrawn=%d rows=%d, want 1 and 1", res.Withdrawn, len(withdrawn))
		}
		if res.CausesWithdrawn != 1 || withdrawn[0].CausesSwept != 1 {
			t.Errorf("CausesWithdrawn=%d, row CausesSwept=%d, want 1 and 1: a withdrawal that denies the pair also drops the 'causes' edge that contradicts it",
				res.CausesWithdrawn, withdrawn[0].CausesSwept)
		}
		edges := liveCausesEdges(t, base, stale, fix)
		if len(edges) != 1 || edges[0] != [2]string{stale, fix} {
			t.Errorf("live causes edges = %v, want only [%s %s]: the denied edge's 'causes' row is swept, and the KEPT edge's is not this pass's to remove", edges, stale, fix)
		}
	})

	t.Run("both edges denied, both causes edges swept", func(t *testing.T) {
		base, db := seed(t)
		store := linkOrderStore{Store: base, reversed: false}
		ctx := context.Background()
		stale, fix := seedCycle(t, base, db,
			"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
			"The restore path on one spindle is safe and takes under a minute.", false)
		// One 'causes' edge per direction, so each of the two rows has its own to
		// sweep and a pass that swept only the first is visible in the graph.
		seedCauses(t, base, fix, stale)
		seedCauses(t, base, stale, fix)

		res, withdrawn, err := Reassess(ctx, store, &mockClassifier{verdict: func(string, string) Relation { return RelationNeither }}, "p", true, discardLogger())
		if err != nil {
			t.Fatalf("Reassess: %v", err)
		}
		if len(withdrawn) != 2 {
			t.Fatalf("rows = %d, want 2: a NEITHER verdict denies both edges of the cycle", len(withdrawn))
		}
		for i, w := range withdrawn {
			if w.CausesSwept != 1 {
				t.Errorf("row %d (%s→%s): CausesSwept = %d, want 1", i, w.NewerID, w.OlderID, w.CausesSwept)
			}
		}
		if res.CausesWithdrawn != 2 {
			t.Errorf("Result.CausesWithdrawn = %d, want 2: each denied edge sweeps its own 'causes' edge", res.CausesWithdrawn)
		}
		if edges := liveCausesEdges(t, base, stale, fix); len(edges) != 0 {
			t.Errorf("live causes edges = %v, want none", edges)
		}
	})

	t.Run("a reversed verdict sweeps the edge it denied, not the one it kept", func(t *testing.T) {
		base, db := seed(t)
		store := linkOrderStore{Store: base, reversed: false}
		ctx := context.Background()
		stale, fix := seedCycle(t, base, db,
			"bug: the relay stalls on every consumer rebalance",
			"the relay rebalance stall is fixed: pin the consumer", false)
		seedCauses(t, base, fix, stale)
		seedCauses(t, base, stale, fix)

		// REVERSED names the note the pass was asked ABOUT as the obsolete one, so
		// the row it withdraws is the edge in the JUDGED direction — the mirror of
		// the SUPERSEDES arm, and a different row, which is why it needs its own
		// assertion rather than a shared one.
		res, withdrawn, err := Reassess(ctx, store, &mockClassifier{verdict: func(string, string) Relation { return RelationReversed }}, "p", true, discardLogger())
		if err != nil {
			t.Fatalf("Reassess: %v", err)
		}
		if res.Withdrawn != 1 || len(withdrawn) != 1 {
			t.Fatalf("withdrawn=%d rows=%d, want 1 and 1", res.Withdrawn, len(withdrawn))
		}
		if res.CausesWithdrawn != 1 || withdrawn[0].CausesSwept != 1 {
			t.Errorf("CausesWithdrawn=%d, row CausesSwept=%d, want 1 and 1: a REVERSED verdict is a denial like any other, so its row sweeps too",
				res.CausesWithdrawn, withdrawn[0].CausesSwept)
		}
		// The withdrawn edge is fix→stale, so the 'causes' row it sweeps is
		// stale→fix, and the KEPT edge's (fix→stale) survives.
		edges := liveCausesEdges(t, base, stale, fix)
		if len(edges) != 1 || edges[0] != [2]string{fix, stale} {
			t.Errorf("live causes edges = %v, want only [%s %s]: the denied edge's 'causes' row is swept, and the KEPT edge's is not this pass's to remove", edges, fix, stale)
		}
	})

	t.Run("a causes verdict sweeps nothing, because it affirms that relation", func(t *testing.T) {
		base, db := seed(t)
		store := linkOrderStore{Store: base, reversed: false}
		ctx := context.Background()
		stale, fix := seedCycle(t, base, db,
			"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
			"The restore path on one spindle is safe and takes under a minute.", false)
		seedCauses(t, base, fix, stale)
		seedCauses(t, base, stale, fix)

		res, withdrawn, err := Reassess(ctx, store, &mockClassifier{verdict: func(string, string) Relation { return RelationCauses }}, "p", true, discardLogger())
		if err != nil {
			t.Fatalf("Reassess: %v", err)
		}
		if len(withdrawn) != 2 {
			t.Fatalf("rows = %d, want 2: both supersedes edges are denied", len(withdrawn))
		}
		for i, w := range withdrawn {
			if w.CausesSwept != 0 {
				t.Errorf("row %d swept %d 'causes' edge(s): a CAUSES verdict affirms that relation, so its rows sweep nothing", i, w.CausesSwept)
			}
		}
		if res.CausesWithdrawn != 0 {
			t.Errorf("Result.CausesWithdrawn = %d, want 0", res.CausesWithdrawn)
		}
		// Left live, and the ordinary pass writes the replacement: a repair pass
		// that also created 'causes' edges would have two jobs and one dry-run
		// answer.
		if edges := liveCausesEdges(t, base, stale, fix); len(edges) != 2 {
			t.Errorf("live causes edges = %v, want both still there", edges)
		}
		if edges := liveEdges(t, base, stale, fix); len(edges) != 0 {
			t.Errorf("live supersedes edges = %v, want none: both were denied", edges)
		}
	})

	// A DRY RUN is the other half of the sweep's contract: an operator deciding
	// about --apply is deciding about that second deletion too, and this reads it
	// through a different code path from the --apply sweep above (a GetLinks
	// prediction rather than the invalidation's own count).
	t.Run("a dry run predicts the sweep it would perform", func(t *testing.T) {
		base, db := seed(t)
		store := linkOrderStore{Store: base, reversed: false}
		ctx := context.Background()
		stale, fix := seedCycle(t, base, db,
			"A restore that spanned two spindles took 41 minutes and the row count matched afterwards.",
			"The restore path on one spindle is safe and takes under a minute.", false)
		seedCauses(t, base, fix, stale)
		seedCauses(t, base, stale, fix)

		res, withdrawn, err := Reassess(ctx, store, &mockClassifier{verdict: func(string, string) Relation { return RelationNeither }}, "p", false, discardLogger())
		if err != nil {
			t.Fatalf("Reassess: %v", err)
		}
		if res.CausesWithdrawn != 2 {
			t.Errorf("Result.CausesWithdrawn = %d, want the PREDICTION 2: a dry run that hides the second deletion is deciding for the operator", res.CausesWithdrawn)
		}
		for i, w := range withdrawn {
			if w.CausesSwept != 1 || w.Written {
				t.Errorf("row %d: CausesSwept = %d, Written = %v, want 1 and false", i, w.CausesSwept, w.Written)
			}
		}
		if edges := liveCausesEdges(t, base, stale, fix); len(edges) != 2 {
			t.Errorf("live causes edges = %v, want both untouched by a dry run", edges)
		}
	})
}
