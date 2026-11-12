package supersede

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// seedCycle writes the CYCLE a pass before #778 could leave: both directions of
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
func seedCauses(t *testing.T, store *memory.Store, from, to string) {
	t.Helper()
	if err := store.CreateLink(context.Background(), from, to, string(RelationCauses), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}
}

// TestReassessJudgesACycleOnceAndWithdrawsTheEdgeItsVerdictDenies is the reason
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
func TestReassessSweepsTheCausesEdgeOfEveryCycleWithdrawalItDenies(t *testing.T) {
	t.Run("one edge denied, its causes edge swept", func(t *testing.T) {
		base, db := seed(t)
		store := linkOrderStore{Store: base, reversed: false}
		ctx := context.Background()
		stale, fix := seedCycle(t, base, db,
			"bug: the relay stalls on every consumer rebalance",
			"the relay rebalance stall is fixed: pin the consumer", false)
		// The denied edge is stale→fix, so the 'causes' edge its withdrawal
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
