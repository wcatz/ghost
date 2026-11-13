package supersede

import (
	"context"
	"database/sql"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

const (
	npmShippedContent = "note: the publishable npm package shipped on 2026-08-25"
	npmRemovedContent = "note: the standalone npm package was removed on 2026-08-26"
)

// seedNpmPackagePair is #845's measured failure shape as a graph state: a live
// claim of the older one and leaves the rest standing.
//
// (#779) — the two notes are not the same fact, so there is no whole claim
// nothing in the graph clears.
//
// re-judge, not by the first pass that wrote the edge.
//
// Timestamps are pinned to whole seconds and a day apart (#847): `orient` reads
func seedNpmPackagePair(t *testing.T, store *memory.Store, db *sql.DB) (newer, older string) {
	t.Helper()
	ctx := context.Background()
	older = add(t, store, db, npmShippedContent, []float32{1, 0, 0}, "2026-08-25 09:00:00")
	newer = add(t, store, db, npmRemovedContent, []float32{0.99, 0, 0}, "2026-08-26 09:00:00")
	if err := store.CreateLink(ctx, newer, older, string(RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	backdateLink(t, db, newer, older)
	return newer, older
}

// TestTheOrdinaryPassReportsTheSupersedesWithdrawalItRefusesToMake is #845, at
// the layer the pass owns.
//
// apart, are the same three #785 asked for: that the edge WAS this pair's
func TestTheOrdinaryPassReportsTheSupersedesWithdrawalItRefusesToMake(t *testing.T) {
	//
	// edge (a WRITE, which #845 does not touch), so the pair's claim really did
	for _, tc := range []struct {
		verdict          Relation
		wantReclassified int
	}{
		{verdict: RelationNeither, wantReclassified: 0},
		{verdict: RelationCauses, wantReclassified: 1},
		{verdict: RelationReversed, wantReclassified: 0},
	} {
		for _, apply := range []bool{false, true} {
			t.Run(string(tc.verdict)+"/apply="+map[bool]string{true: "on", false: "off"}[apply], func(t *testing.T) {
				store, db := seed(t)
				newer, older := seedNpmPackagePair(t, store, db)
				cls := &mockClassifier{verdict: func(_, _ string) Relation { return tc.verdict }}

				res, classified, err := RunWith(context.Background(), store, cls, "p",
					Options{Threshold: 0.9, Apply: apply, Consensus: 3}, nil)
				if err != nil {
					t.Fatalf("RunWith: %v", err)
				}
				if len(classified) != 1 {
					t.Fatalf("classified = %+v, want exactly the one pair", classified)
				}
				row := classified[0]
				if row.NewerID != newer || row.OlderID != older {
					t.Fatalf("the row names %s→%s, want %s→%s", row.NewerID, row.OlderID, newer, older)
				}
				if got := liveSupersedes(t, store, "p"); got != 1 {
					t.Errorf("live supersedes edge(s) = %d, want 1: the ordinary pass removed an edge on a verdict measured to be wrong more often than right", got)
				}
				if row.Withdrawn {
					t.Error("Withdrawn = true over a run that left the edge live, so the report's marker is a claim about a deletion nobody made")
				}
				if !row.WithdrawSuppressed {
					t.Error("WithdrawSuppressed = false, so a caller cannot tell a withheld withdrawal from a concurrent pass that took the edge first — they need different advice and the same line of text")
				}
				if res.WithdrawSuppressed != 1 {
					t.Errorf("WithdrawSuppressed = %d, want 1: the count is what the summary line reports, and a suppressed withdrawal that no count names is a withdrawal nothing tells the operator about", res.WithdrawSuppressed)
				}
				if res.Reclassified != tc.wantReclassified {
					t.Errorf("Reclassified = %d, want %d", res.Reclassified, tc.wantReclassified)
				}
			})
		}
	}
}

// the endpoints against the row that is still there.
func TestASuppressedWithdrawalIsNotRecordedAndIsReportedAgainNextPass(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	seedNpmPackagePair(t, store, db)
	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationNeither }}

	for pass := 1; pass <= 2; pass++ {
		res, classified, err := RunWith(ctx, store, cls, "p", Options{Threshold: 0.9, Apply: true, Consensus: 3}, nil)
		if err != nil {
			t.Fatalf("RunWith (pass %d): %v", pass, err)
		}
		if len(classified) != 1 || !classified[0].WithdrawSuppressed {
			t.Fatalf("pass %d: classified = %+v, want one row carrying the withheld withdrawal: an edge nothing withdraws is an edge that has to keep being reported", pass, classified)
		}
		if res.WithdrawSuppressed != 1 {
			t.Errorf("pass %d: WithdrawSuppressed = %d, want 1", pass, res.WithdrawSuppressed)
		}
		if res.Skipped != 0 {
			t.Errorf("pass %d: Skipped = %d, want 0: a NEITHER cache row for a pair whose live edge was left in place is a skip that hides the edge for the life of its text", pass, res.Skipped)
		}
		// counter that happens to read zero.
		checked, err := store.SupersedeChecked(ctx, "p")
		if err != nil {
			t.Fatalf("SupersedeChecked: %v", err)
		}
		if len(checked) != 0 {
			t.Errorf("pass %d: supersede_checked = %+v, want none: the pass withheld the withdrawal, so it has no verdict to record against a pair the graph still links", pass, checked)
		}
		if got := liveSupersedes(t, store, "p"); got != 1 {
			t.Fatalf("pass %d: live supersedes edge(s) = %d, want 1", pass, got)
		}
	}
}

// reflect race, and it exists because #845's flag is set BEFORE the pre-write
// existence check rather than after it.
//
// live and still demoting its target". Before #845 the same row fell through to
// `already gone`, which is what actually happened.
//
// withheld cannot be counted as withheld.
func TestAStaleWithheldWithdrawalSaysTheEdgeIsGoneBecauseItIs(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	npmNew, npmOld := seedNpmPackagePair(t, store, db)
	// belongs to exactly one row.
	otherNew := add(t, store, db, "note: the nightly backup moved to object storage", []float32{0, 1, 0}, "2026-08-27 09:00:00")
	otherOld := add(t, store, db, "note: the nightly backup ran on the db host", []float32{0, 0.99, 0}, "2026-08-24 09:00:00")

	// uses, and the same race.
	cls := &mockClassifier{verdict: func(newer, older string) Relation {
		if newer == npmRemovedContent && older == npmShippedContent {
			if err := store.Delete(ctx, npmOld); err != nil {
				t.Errorf("delete stale endpoint: %v", err)
			}
		}
		return RelationNeither
	}}

	res, classified, err := RunWith(ctx, store, cls, "p", Options{Threshold: 0.9, Apply: true}, nil)
	if err != nil {
		t.Fatalf("RunWith: %v", err)
	}
	if res.StaleAtWrite != 1 {
		t.Fatalf("StaleAtWrite = %d, want 1: the endpoint was replaced during the classify call, so the pair was classified and only the write was skipped", res.StaleAtWrite)
	}
	if res.WithdrawSuppressed != 0 {
		t.Errorf("WithdrawSuppressed = %d, want 0: the stale row's edge was CASCADE-deleted with its endpoint, so the summary would be quoting a live edge for a memory that is gone", res.WithdrawSuppressed)
	}

	var stale *Classified
	for i := range classified {
		if classified[i].NewerID == npmNew {
			stale = &classified[i]
		}
	}
	if stale == nil {
		t.Fatalf("no row for the withheld pair %s→%s; classified = %+v", npmNew, npmOld, classified)
	}
	if stale.WithdrawSuppressed {
		t.Error("WithdrawSuppressed = true on a stale row: the row claims an edge is STILL LIVE and the report would print `already gone` never, over a memory the concurrent pass deleted")
	}

	if got := liveSupersedes(t, store, "p"); got != 0 {
		t.Errorf("live supersedes edge(s) = %d, want 0: deleting the endpoint cascades the edge away, so the row's claim of a live edge would be false", got)
	}
	if len(classified) != 2 {
		t.Fatalf("classified = %d row(s), want 2: the batch has to hold a second pair for this to be the concurrent-reflect shape rather than a single-pair one", len(classified))
	}
	var survivor *Classified
	for i := range classified {
		if classified[i].NewerID == otherNew && classified[i].OlderID == otherOld {
			survivor = &classified[i]
		}
	}
	if survivor == nil {
		t.Fatalf("no row for %s→%s; classified = %+v", otherNew, otherOld, classified)
	}
	if survivor.WithdrawSuppressed {
		t.Error("WithdrawSuppressed = true on a row with no live edge: there was nothing to withhold, so the flag is the pass losing track of its own reason")
	}
}

// to the command that exists to be asked for it.
//
// target the operator is told how to clear. If this test fails, #845 has not moved
// a withdrawal anywhere — it has deleted one.
func TestReassessStillWithdrawsWhatTheOrdinaryPassOnlyReports(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	_, older := seedNpmPackagePair(t, store, db)
	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationNeither }}

	res, withdrawn, err := Reassess(ctx, store, cls, "p", true, nil)
	if err != nil {
		t.Fatalf("Reassess: %v", err)
	}
	if res.Withdrawn != 1 {
		t.Errorf("ReassessResult.Withdrawn = %d, want 1", res.Withdrawn)
	}
	if len(withdrawn) != 1 {
		t.Fatalf("withdrawn = %+v, want exactly the one edge, named for the follow-up", withdrawn)
	}
	if !withdrawn[0].Written || withdrawn[0].OlderID != older {
		t.Errorf("withdrawn[0] = %+v, want the invalidation recorded against %s", withdrawn[0], older)
	}
	if got := liveSupersedes(t, store, "p"); got != 0 {
		t.Errorf("live supersedes edge(s) = %d, want 0: the repair pass is where the withdrawal lives now", got)
	}
}

// half of the promise: #845 removes a DELETE, and nothing else. A new 'supersedes'
// unanimity still gates them (#779), and the ordinary pass still sweeps a
// #823 relies on, and the reason a 'causes' cycle can converge at all (see
//
// only that a model was scripted.
func TestTheOrdinaryPassStillWritesEveryLinkItsUnanimousVerdictNames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict Relation
	}{
		{name: "supersedes", verdict: RelationSupersedes},
		{
			// from the orientation the pair was asked in.
			name:    "causes",
			verdict: RelationCauses,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, db := seed(t)
			newer := add(t, store, db, "decision: the deploy pipeline now runs the migration check before staging", []float32{1, 0, 0}, "2026-07-01 00:00:00")
			older := add(t, store, db, "note: the deploy pipeline runs the unit suite before staging", []float32{0.99, 0, 0}, "2026-01-01 00:00:00")
			cls := &mockClassifier{verdict: func(_, _ string) Relation { return tc.verdict }}

			res, _, err := RunWith(context.Background(), store, cls, "p",
				Options{Threshold: 0.9, Apply: true, Consensus: 3}, nil)
			if err != nil {
				t.Fatalf("RunWith: %v", err)
			}
			wantSupersedes := [][2]string(nil)
			wantCauses := [][2]string(nil)
			switch tc.verdict {
			case RelationSupersedes:
				wantSupersedes = [][2]string{{newer, older}}
			case RelationCauses:
				wantCauses = [][2]string{{older, newer}}
			}
			if got := liveSupersedesEdges(t, store, newer, older); !sameEdges(got, wantSupersedes) {
				t.Errorf("live 'supersedes' edges = %v, want %v", got, wantSupersedes)
			}
			if got := liveCausesEdges(t, store, newer, older); !sameEdges(got, wantCauses) {
				t.Errorf("live 'causes' edges = %v, want %v", got, wantCauses)
			}
			if res.WithdrawSuppressed != 0 {
				t.Errorf("WithdrawSuppressed = %d over a pair that carried no live edge, want 0: there is nothing to withhold", res.WithdrawSuppressed)
			}
			if res.Reclassified != 0 {
				t.Errorf("Reclassified = %d over a pair the graph never linked, want 0", res.Reclassified)
			}
			// the gate is for.
			switch tc.verdict {
			case RelationSupersedes:
				if res.Created != 1 || res.Confirmed != 1 {
					t.Errorf("Created=%d Confirmed=%d, want 1/1", res.Created, res.Confirmed)
				}
			case RelationCauses:
				if res.CausesWritten != 1 || res.CausesCreated != 1 {
					t.Errorf("CausesWritten=%d CausesCreated=%d, want 1/1", res.CausesWritten, res.CausesCreated)
				}
			}
		})
	}
}

// scope of #845, stated as a test rather than as a sentence in a pull request: the
//
// harm #845's measurement is about. More decisively there is no repair: Reassess
// cycle.
//
// `CausesDropped` is 2 (two rows really went).
func TestAWithheldSupersedesWithdrawalStillSweepsTheCausesCycleItDenies(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer, older := seedCausesCycle(t, store, db)
	if err := store.CreateLinkJudged(ctx, newer, older, string(RelationSupersedes), 0.9, "llm", "2020-01-01 00:00:00"); err != nil {
		t.Fatal(err)
	}

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationNeither }}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := liveCausesEdges(t, store, newer, older); len(got) != 0 {
		t.Errorf("live 'causes' edges = %v, want none: a denying verdict says the pair is no relation at all, and a cycle left live is a contradiction no command in the product can reach", got)
	}
	if got := liveSupersedesEdges(t, store, newer, older); len(got) != 1 {
		t.Errorf("live 'supersedes' edges = %v, want the one edge left in place: #845 covers this relation only", got)
	}
	if len(classified) != 1 {
		t.Fatalf("classified = %+v, want one row", classified)
	}
	row := classified[0]
	if row.CausesDropped != 2 {
		t.Errorf("CausesDropped = %d, want 2: the row has to name both rows the run really moved, or the report reads as though it moved none", row.CausesDropped)
	}
	if row.Withdrawn {
		t.Error("Withdrawn = true over a pair whose 'supersedes' edge is still live: the marker is a claim about the edge this row is about, and `already gone` is the wrong one")
	}
	if !row.WithdrawSuppressed {
		t.Error("WithdrawSuppressed = false, so the report cannot say the edge is still there")
	}
	if res.Reclassified != 1 || res.WithdrawSuppressed != 1 {
		t.Errorf("Reclassified=%d WithdrawSuppressed=%d, want 1/1", res.Reclassified, res.WithdrawSuppressed)
	}
}

// "no edges" as `nil` and the comparison still holds.
func sameEdges(got, want [][2]string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
