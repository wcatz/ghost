package supersede

import (
	"context"
	"database/sql"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// seedReclassifyPair writes a live 'supersedes'/'llm' edge over two near-identical
// notes and returns their ids. The vectors are close enough that the candidate
// scan proposes the pair AND the live edge decides its direction, which is the
// state a reclassification is: the graph already asserts the pair, so a verdict
// other than SUPERSEDES WITHDRAWS an edge rather than declining to write one.
//
// The two notes are deliberately about two different steps of one pipeline, so
// neither states a rule the imperative veto would retire (#686) — the fixture has
// to reach the classifier or it proves nothing about the verdict.
func seedReclassifyPair(t *testing.T, store *memory.Store, db *sql.DB) (newer, older string) {
	t.Helper()
	ctx := context.Background()
	newer = add(t, store, db, "decision: the deploy pipeline now runs the migration check before staging", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older = add(t, store, db, "note: the deploy pipeline runs the unit suite before staging", []float32{0.99, 0, 0}, "2026-01-01 00:00:00")
	if err := store.CreateLink(ctx, newer, older, string(RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	return newer, older
}

// liveSupersedes counts the live 'supersedes'/'llm' edges a project holds, which
// is the graph state every assertion here is about: whether the pass moved an
// edge, and whether it claimed to have moved one.
func liveSupersedes(t *testing.T, store *memory.Store, projectID string) int {
	t.Helper()
	links, err := store.LinksByRelationSource(context.Background(), projectID, string(RelationSupersedes), "llm")
	if err != nil {
		t.Fatalf("LinksByRelationSource: %v", err)
	}
	return len(links)
}

// TestRunNamesTheEdgeAReclassificationWithdrew is #785 at the layer the pass
// owns. A live 'supersedes' edge the pass re-judges and that comes back denied
// is invalidated through the ordinary path — a real graph mutation, and a
// `resolved_at` on its target that nothing else will clear. Every other
// withdrawal path in the product names the edge it moved, with the ids and the
// command that finishes the repair; this one reported a verdict and nothing
// else, so a caller could not tell that pair from a fresh proposal the pass
// merely declined.
//
// The row therefore has to say two separate things, and the tests below keep them
// separate on purpose: that the edge WAS this pair's (Reclassified), and that
// THIS call moved it (Withdrawn). A report that conflated them would either
// claim a withdrawal for a dry run or hide one a concurrent pass had already
// made.
func TestRunNamesTheEdgeAReclassificationWithdrew(t *testing.T) {
	// All three verdicts that deny the replacement, and all three reach the
	// same place: the live 'supersedes' edge goes. NEITHER and REVERSED also
	// drop the 'causes' edge, and CAUSES writes one, but the withdrawal this
	// report is about is the supersedes edge in every case.
	for _, verdict := range []Relation{RelationNeither, RelationCauses, RelationReversed} {
		t.Run(string(verdict), func(t *testing.T) {
			store, db := seed(t)
			newer, older := seedReclassifyPair(t, store, db)
			cls := &mockClassifier{verdict: func(_, _ string) Relation { return verdict }}

			res, classified, err := Run(context.Background(), store, cls, "p", 0.9, true, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Reclassified != 1 {
				t.Fatalf("Reclassified = %d, want 1: the fixture writes one live edge", res.Reclassified)
			}
			if len(classified) != 1 {
				t.Fatalf("classified = %+v, want exactly the one pair", classified)
			}
			row := classified[0]
			if row.NewerID != newer || row.OlderID != older {
				t.Fatalf("the row names %s→%s, want %s→%s", row.NewerID, row.OlderID, newer, older)
			}
			if !row.Reclassified {
				t.Error("the row does not say the pair carried a live edge, so a caller cannot tell this withdrawal from a fresh pair's silence")
			}
			if !row.Withdrawn {
				t.Error("the row does not say the invalidation landed, although the edge is gone from the graph")
			}
			if got := liveSupersedes(t, store, "p"); got != 0 {
				t.Errorf("live supersedes edge(s) = %d, want 0: the pass reported a %s verdict and left the edge live", got, verdict)
			}
		})
	}
}

// TestRunReportsNoWithdrawalItHasNotMade: the marker is the report's tense, and
// it can only be true if the pass wrote nothing. A dry run's job is to predict
// --apply, and a row that claimed a withdrawal nobody could read in the graph
// would send the operator to re-run a repair that had already happened.
func TestRunReportsNoWithdrawalItHasNotMade(t *testing.T) {
	store, db := seed(t)
	seedReclassifyPair(t, store, db)
	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationNeither }}

	_, classified, err := Run(context.Background(), store, cls, "p", 0.9, false, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(classified) != 1 {
		t.Fatalf("classified = %+v, want exactly the one pair", classified)
	}
	if classified[0].Withdrawn {
		t.Error("a dry run reported a withdrawal it did not make")
	}
	if got := liveSupersedes(t, store, "p"); got != 1 {
		t.Errorf("live supersedes edge(s) = %d, want 1: the dry run wrote something", got)
	}
}

// TestAFreshNeitherIsNotAWithdrawal: NEITHER on a pair the graph never linked
// withdraws nothing, because there is no edge — the two notes simply are not a
// replacement. A report that printed a withdrawal row for it would be naming a
// memory whose resolution no edge ever justified, and the follow-up it hands
// over would send the operator to re-judge a row for no reason.
func TestAFreshNeitherIsNotAWithdrawal(t *testing.T) {
	store, db := seed(t)
	// The same two notes as the reclassify fixture, with no edge written: the
	// scan proposes the pair on cosine and the verdict is NEITHER.
	add(t, store, db, "decision: the deploy pipeline now runs the migration check before staging", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	add(t, store, db, "note: the deploy pipeline runs the unit suite before staging", []float32{0.99, 0, 0}, "2026-01-01 00:00:00")
	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationNeither }}

	res, classified, err := Run(context.Background(), store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reclassified != 0 {
		t.Fatalf("Reclassified = %d, want 0: the fixture writes no live edge", res.Reclassified)
	}
	for _, c := range classified {
		if c.Reclassified || c.Withdrawn {
			t.Errorf("a fresh pair reported a withdrawal: %+v", c)
		}
	}
	if got := liveSupersedes(t, store, "p"); got != 0 {
		t.Errorf("live supersedes edge(s) = %d, want 0", got)
	}
}
