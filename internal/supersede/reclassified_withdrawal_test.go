package supersede

import (
	"context"
	"database/sql"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// seedReclassifyPair writes a live 'supersedes'/'llm' edge over two near-identical
// notes and returns their ids. The link row is then BACKDATED, which is what puts
// the pair on the schedule: skip-if-unchanged holds a live edge quiet unless an
// endpoint has moved since the edge was written (#787), so a fixture that leaves
// the stamp at now produces a pair the pass never judges — and a pass that judged
// nothing is not a fixture for what a pass does when a verdict denies a live edge.
//
// That is the production shape of a reclassification: an endpoint was edited after
// the edge was written, so the pass re-judges the pair, and a verdict other than
// SUPERSEDES reports an edge rather than declining to write one (#845).
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
	if _, err := db.ExecContext(ctx,
		`UPDATE memory_links SET created_at = '2020-01-01 00:00:00' WHERE source_id = ? AND target_id = ?`,
		newer, older,
	); err != nil {
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

// TestRunNamesTheEdgeAReclassificationWouldWithdraw is #785 at the layer the
// pass owns, under #845. A live 'supersedes' edge the pass re-judges and that
// comes back denied is now REPORTED, not invalidated — and every other
// withdrawal path in the product names the edge it moved, with the ids and the
// command that finishes the repair, so this one has to keep naming it even now
// that the graph it names is untouched. A row that reported a verdict and
// nothing else could not tell that pair from a fresh proposal the pass merely
// declined, and a row that claimed a withdrawal nobody made would send the
// operator after a repair that had already happened.
//
// The row therefore has to say three separate things, and the tests below keep
// them apart on purpose: that the edge WAS this pair's (Reclassified), that THIS
// call did NOT move it (Withdrawn), and that this call declined to move it
// (WithdrawSuppressed).
func TestRunNamesTheEdgeAReclassificationWouldWithdraw(t *testing.T) {
	// All three verdicts that deny the replacement, and all three reach the
	// same place: the live 'supersedes' edge is reported. NEITHER and REVERSED
	// also sweep a 'causes' edge, and CAUSES writes one, but the withdrawal this
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
			if row.Withdrawn {
				t.Error("the row claims a withdrawal although the edge is still in the graph")
			}
			if !row.WithdrawSuppressed {
				t.Error("the row does not say the withdrawal was DECLINED, so a caller cannot tell it from a concurrent pass that took the edge first — the same line of text, opposite advice")
			}
			if got := liveSupersedes(t, store, "p"); got != 1 {
				t.Errorf("live supersedes edge(s) = %d, want 1: the pass reported a %s verdict and left the edge live", got, verdict)
			}
			// A CAUSES verdict writes an edge, so the pair's live claim really did
			// change and Reclassified still counts it; the two denials write
			// nothing and move nothing, so counting them would report a graph
			// change over a pass that made none.
			wantReclassified := 0
			if verdict == RelationCauses {
				wantReclassified = 1
			}
			if res.Reclassified != wantReclassified {
				t.Errorf("Reclassified = %d, want %d", res.Reclassified, wantReclassified)
			}
			if res.WithdrawSuppressed != 1 {
				t.Errorf("WithdrawSuppressed = %d, want 1", res.WithdrawSuppressed)
			}
		})
	}
}

// TestRunReportsNoWithdrawalItHasNotMade: the marker is the report's tense, and
// it can only be true if the pass wrote nothing. A dry run's job is to predict
// --apply, and a row that claimed a withdrawal nobody could read in the graph
// would send the operator to re-run a repair that had already happened. Since
// #845 the marker is false in BOTH modes — the withheld withdrawal is not a
// withdrawal — while WithdrawSuppressed is the field that is true in both,
// because the pass declines it either way.
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
	if !classified[0].WithdrawSuppressed {
		t.Error("a dry run did not report the withdrawal it declined to make: the rule is the pass's and not --apply's, so both modes have to say so")
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

// TestRunReportsTheCausesEdgeTheSameWithdrawalRemoved: a denying verdict moves a
// SECOND graph row. NEITHER and REVERSED both drop the pair's live 'causes'
// edge, and `Run` was discarding the count that says whether they did — so an
// applied pass could report `withdrew X -> Y [neither]` while a live causes edge
// Y→X went with it, and the report read as though one row moved when two did.
// `--reassess` reports exactly this as `[+N causes edge]` on its own rows, and
// these rows are printed in its shape.
//
// Only a count the WRITE returned: a dry run says nothing here, because Run makes
// no prediction read for it and forecasting a deletion nobody performed is the
// claim the marker exists to prevent.
func TestRunReportsTheCausesEdgeTheSameWithdrawalRemoved(t *testing.T) {
	for _, tc := range []struct {
		verdict Relation
		// Both denying verdicts sweep the other relation's edge; a CAUSES verdict
		// CREATES one instead, which the report covers on its own branch.
		wantDropped int
	}{
		{verdict: RelationNeither, wantDropped: 1},
		{verdict: RelationReversed, wantDropped: 1},
	} {
		t.Run(string(tc.verdict), func(t *testing.T) {
			store, db := seed(t)
			ctx := context.Background()
			newer, older := seedReclassifyPair(t, store, db)
			// A live 'causes' edge on the pair, which the denial sweeps with the
			// supersedes edge — the second mutation the row has to account for.
			if err := store.CreateLink(ctx, older, newer, string(RelationCauses), 0.9, "llm"); err != nil {
				t.Fatal(err)
			}
			cls := &mockClassifier{verdict: func(_, _ string) Relation { return tc.verdict }}

			_, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(classified) != 1 {
				t.Fatalf("classified = %+v, want exactly the one pair", classified)
			}
			if got := classified[0].CausesDropped; got != tc.wantDropped {
				t.Errorf("CausesDropped = %d, want %d: the row has to account for the causes edge the same verdict removed", got, tc.wantDropped)
			}
			// And the graph agrees with the count, so the two are not independent.
			links, err := store.GetLinks(ctx, older)
			if err != nil {
				t.Fatalf("GetLinks: %v", err)
			}
			live := 0
			for _, l := range links {
				if l.Relation == string(RelationCauses) && l.InvalidatedAt == nil {
					live++
				}
			}
			if live != 1-tc.wantDropped {
				t.Errorf("live causes edge(s) = %d, want %d", live, 1-tc.wantDropped)
			}
		})
	}
}

// TestADryRunCountsNoCausesDeletionItDidNotPerform: the count is what the write
// returned, and a dry run writes nothing. A marker that read `+1 causes edge`
// over a pass that deleted nothing is the one line this report must not print —
// which is not the same as saying nothing, because the pass READ the edge and can
// say what it would do with it. The observed count and the forecast are two
// fields for that reason, and this test holds both to their own tense.
func TestADryRunCountsNoCausesDeletionItDidNotPerform(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedReclassifyPair(t, store, db)
	if err := store.CreateLink(ctx, older, newer, string(RelationCauses), 0.9, "llm"); err != nil {
		t.Fatal(err)
	}
	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationReversed }}

	_, classified, err := Run(ctx, store, cls, "p", 0.9, false, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(classified) != 1 {
		t.Fatalf("classified = %+v, want exactly the one pair", classified)
	}
	if classified[0].CausesDropped != 0 {
		t.Errorf("a dry run reported %d causes edge(s) dropped, and it dropped none", classified[0].CausesDropped)
	}
	// The forecast, which is what a dry run has instead. The pair holds a live
	// 'causes' edge and the verdict is REVERSED, so the apply block would sweep
	// it — and the forecast is counted off the pass's own read, which is the
	// reason it can exist at all.
	if classified[0].CausesDroppable != 1 {
		t.Errorf("CausesDroppable = %d, want 1: a REVERSED verdict drops the pair's live 'causes' edge, and a dry run is the only place that can be said without doing it",
			classified[0].CausesDroppable)
	}
	links, err := store.GetLinks(ctx, older)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	live := 0
	for _, l := range links {
		if l.Relation == string(RelationCauses) && l.InvalidatedAt == nil {
			live++
		}
	}
	if live != 1 {
		t.Errorf("live causes edge(s) = %d after a dry run, want 1: it wrote nothing", live)
	}
}
