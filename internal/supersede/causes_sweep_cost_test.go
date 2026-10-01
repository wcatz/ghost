package supersede

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// countingCausesStore records every 'causes' invalidation the pass ISSUES, not
// every one that moves a row. `causesTakenStore` in reassess_sweep_test.go counts
// the other half — what moved — and the two halves are the same distinction the
// row itself draws between CausesDroppable and CausesDropped. This one exists for
// the third question: how many writes the pass was willing to make at all.
type countingCausesStore struct {
	*memory.Store
	calls []string
}

func (c *countingCausesStore) InvalidateLink(ctx context.Context, sourceID, targetID, relation string) (int64, error) {
	if relation == string(RelationCauses) {
		c.calls = append(c.calls, sourceID+"->"+targetID)
	}
	return c.Store.InvalidateLink(ctx, sourceID, targetID, relation)
}

// TestTheCausesSweepIsSkippedWhereThereIsNothingToSweep is the cost half of the
// gated reverse sweep in the CAUSES branch, and the two halves are separate tests
// because they are separate claims.
//
// The correctness half is TestACausesCycleIsSettledRatherThanFrozen: a gated
// sweep has to DROP the cycle's other edge, or the pair stays a contradiction. This
// one is about the other side — an UNGATED sweep reaches InvalidateLink for a pair
// whose graph holds nothing in that direction, and every call is a write attempt
// inside the pass's transaction: a second round trip per CAUSES verdict on the
// ordinary agreeing pair, which is the pair shape the pass produces most of.
//
// Nothing observable in the RESULT distinguishes the two, which is exactly why the
// claim needs its own probe rather than a bigger assertion on a shared fixture: a
// mutation that ungates the sweep changes the write count and nothing else, and an
// assertion made only from counters would not notice.
func TestTheCausesSweepIsSkippedWhereThereIsNothingToSweep(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// The ORDINARY shape: one live 'causes' edge, already running the way a
	// CAUSES verdict writes it, and stamped old so the pair is re-judged. This is
	// not seedCausesCycle — a cycle is the shape where the sweep is LOAD-BEARING,
	// and this test is about the pair where there is nothing to sweep.
	newer, older := add(t, store, db, "the restore is being rewritten to run on one spindle", []float32{1, 0, 0}, "2026-09-01 00:00:00"),
		add(t, store, db, "the restore path on one spindle is safe and fast", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	if err := store.CreateLinkJudged(ctx, older, newer, string(RelationCauses), 0.9, "llm", "2020-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}
	settled, err := store.GetLinks(ctx, newer)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	live := 0
	for _, l := range settled {
		if l.Relation == string(RelationCauses) && l.InvalidatedAt == nil {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("live 'causes' edges = %d, want 1: the fixture is a settled pair, and a settled pair holds one edge", live)
	}
	// And both endpoints move, so nothing but the sweep's own gate can keep the
	// reverse invalidation out of the write path.
	retagBoth(t, store, db, newer, older)

	spy := &countingCausesStore{Store: store}
	cls := &recordingCauses{}
	res, classified, err := Run(ctx, spy, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.judged) != 1 {
		t.Fatalf("the pass asked about %v, want exactly 1 pair", cls.judged)
	}
	if len(classified) != 1 || classified[0].Relation != RelationCauses {
		t.Fatalf("classified = %+v, want one CAUSES row", classified)
	}
	if classified[0].CausesDropped != 0 {
		t.Fatalf("CausesDropped = %d, want 0: nothing was dropped, so this row is the one that must have cost no extra write either",
			classified[0].CausesDropped)
	}
	// The reverse direction is the only 'causes' invalidation this verdict could
	// have issued, and it names an edge the graph does not hold.
	if len(spy.calls) != 0 {
		t.Errorf("the pass issued %d 'causes' invalidation(s) %v against a pair holding no edge that way: a sweep that can only move a row the pass has already seen is the one worth making",
			len(spy.calls), spy.calls)
	}
	if res.Reclassified != 0 {
		t.Errorf("Reclassified = %d, want 0: a verdict agreeing with the live edge moved nothing", res.Reclassified)
	}
}
