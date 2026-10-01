package supersede

import (
	"context"
	"database/sql"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// seedNpmPackagePair is #845's measured failure shape as a graph state: a live
// 'supersedes'/'llm' edge over two notes where the NEWER one retires a single
// claim of the older one and leaves the rest standing.
//
//   - older (2026-08-25): the publishable npm package shipped.
//   - newer (2026-08-26): the standalone npm package was removed.
//
// A note that retires ONE claim of another is NEITHER under the every-claim rule
// (#779) — the two notes are not the same fact, so there is no whole claim
// standing behind the edge — and the measured shape of a WRONG withdrawal was
// exactly this one: of eleven withdrawals the pass made over a real store, six
// were wrong and the failure was a newer note retiring one claim of an older one
// whose other claims were still true. The edge was correct, the verdict was a
// fair reading of the two bodies, and the pass deleted a correct edge — a demotion
// the ranking had been relying on, plus a `resolved_at` on the older note that
// nothing in the graph clears.
//
// The link row is BACKDATED, so skip-if-unchanged releases the pair and the
// reclassify half asks about it: the withdrawal in the issue was reached by a
// re-judge, not by the first pass that wrote the edge.
//
// Timestamps are pinned to whole seconds and a day apart (#847): `orient` reads
// `updated_at`, which is second-resolution, so a pair written inside one second
// has no knowable direction and this fixture would test nothing.
func seedNpmPackagePair(t *testing.T, store *memory.Store, db *sql.DB) (newer, older string) {
	t.Helper()
	ctx := context.Background()
	older = add(t, store, db, "note: the publishable npm package shipped on 2026-08-25", []float32{1, 0, 0}, "2026-08-25 09:00:00")
	newer = add(t, store, db, "note: the standalone npm package was removed on 2026-08-26", []float32{0.99, 0, 0}, "2026-08-26 09:00:00")
	if err := store.CreateLink(ctx, newer, older, string(RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	backdateLink(t, db, newer, older)
	return newer, older
}

// TestTheOrdinaryPassReportsTheSupersedesWithdrawalItRefusesToMake is #845, at
// the layer the pass owns.
//
// The ordinary pass used to invalidate a live 'supersedes' edge on any denying
// verdict, and the measurement says that verdict is wrong more often than it is
// right: 4 of 11 withdrawals correct, 1 unsure, 6 wrong. So the pass now says
// which edges it would withdraw and does not withdraw them, in BOTH modes —
// `--apply` is a WRITE flag, not a licence to delete graph history, and the shape
// that finally removes these edges is `ghost supersede --reassess` (or
// `--withdraw`), both of which are a decision a human asked for.
//
// The three things the row has to carry, and the three the tests below hold
// apart, are the same three #785 asked for: that the edge WAS this pair's
// (`Reclassified`), that this call DID NOT move it (`Withdrawn`), and — new —
// that this call declined to move it (`WithdrawSuppressed`). Without the third a
// report could not tell a withheld withdrawal from a concurrent pass that took
// the edge first, and the two are the same line of text with opposite advice.
func TestTheOrdinaryPassReportsTheSupersedesWithdrawalItRefusesToMake(t *testing.T) {
	// Every verdict that denies the replacement, in both modes. NEITHER is the
	// shape the measurement is about; CAUSES and REVERSED reach the same
	// withdrawal for the same reason and have to be held to the same rule.
	//
	// wantReclassified is per verdict rather than uniform, and the difference is
	// the whole of what the rule withholds. NEITHER and REVERSED write nothing
	// and, with the withdrawal withheld, move nothing — so the pair's live claim
	// is exactly as the pass found it and "1 reclassified" would be the false
	// claim `Withdrawn` used to make one field away. CAUSES writes a 'causes'
	// edge (a WRITE, which #845 does not touch), so the pair's claim really did
	// change and saying otherwise would be the mirror-image falsehood.
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
				// The graph, which is the only independent witness. A pass that
				// withdrew a correct edge cannot be argued back by any wording.
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

// TestASuppressedWithdrawalIsNotRecordedAndIsReportedAgainNextPass is the CACHE
// half of the rule, and it is what keeps the report from decaying into silence.
//
// A NEITHER verdict is normally recorded in the content-keyed cache, and a cache
// hit is a permanent skip for the life of that text. Recording it for a pair
// whose withdrawal was SUPPRESSED would be worse than caching the wrong thing:
// the graph still holds the edge, and a row saying the pair is not a relation
// would be a claim the graph no longer agrees with — and the next pass would skip
// the pair, so the edge would sit there with nothing re-judging it and nothing
// reporting it. The pair is re-reported on every pass instead, because the edge's
// stamp never moved: no invalidation, no re-stamp, and skip-if-unchanged reads
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
		// And the cache itself, so the assertion above cannot be satisfied by a
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

// TestReassessStillWithdrawsWhatTheOrdinaryPassOnlyReports is the other half of
// the rule and the reason the rule is safe: the withdrawal is not lost, it moves
// to the command that exists to be asked for it.
//
// `--reassess` re-judges every live 'supersedes'/'llm' edge under the current
// rules and withdraws the ones that come back denied, so the same fixture and the
// same NEITHER verdict that the ordinary pass now only reports still produce a
// real withdrawal, a real `unsupersede` history row, and a `resolved_at` on the
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

// TestTheOrdinaryPassStillWritesEveryLinkItsUnanimousVerdictNames is the other
// half of the promise: #845 removes a DELETE, and nothing else. A new 'supersedes'
// edge and a new 'causes' edge are still written under `--apply`, `--consensus`
// unanimity still gates them (#779), and the ordinary pass still sweeps a
// 'causes' edge that denies the supersession it just wrote — which is the shape
// #823 relies on, and the reason a 'causes' cycle can converge at all (see
// TestAWithheldSupersedesWithdrawalStillSweepsTheCausesCycleItDenies).
//
// The gate is checked on the write and not on the fixture: a fixture that asked
// for one verdict three times proves the pass wrote a unanimous edge, which is
// the claim, and a fixture that asked three times for three verdicts would prove
// only that a model was scripted.
func TestTheOrdinaryPassStillWritesEveryLinkItsUnanimousVerdictNames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict Relation
	}{
		{name: "supersedes", verdict: RelationSupersedes},
		{
			// 'causes' is written cause->effect, so it runs the other way round
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
			// Read back from the graph, in the direction each relation is
			// written, so the assertion cannot be satisfied by a counter.
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
			// The write counts, in the tense the report counts them in: a
			// unanimous three-pass verdict is acted on, which is the whole of what
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

// TestAWithheldSupersedesWithdrawalStillSweepsTheCausesCycleItDenies is the
// scope of #845, stated as a test rather than as a sentence in a pull request: the
// rule covers the 'supersedes' edge and NOT the 'causes' edge, and the reason is
// the one pairDirection and Result.Bidirectional already give.
//
// A 'causes' edge demotes nothing, so a wrong withdrawal of one costs a claim
// about which note caused which rather than a memory's place in every later
// session's context — which is the harm this pass is KEEP-biased against, and the
// harm #845's measurement is about. More decisively there is no repair: Reassess
// and `--withdraw` both load live 'supersedes'/'llm' edges and cannot see a
// 'causes' one at all, so a withheld 'causes' withdrawal would be a contradiction
// in the graph with a report pointing at a command that cannot remove it — the
// exact shape Result.Bidirectional refuses to create when it declines to judge a
// cycle.
//
// So a NEITHER verdict on a pair holding a supersession and a 'causes' cycle
// leaves the SUPERSEDES edge in place and drops both 'causes' rows, and the row
// says so: `Withdrawn` is false (the edge this row is about survived) and
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
	// The pair's live claim DID change — two 'causes' rows went — so it is still
	// a reclassification. What is withheld is the one row, not the finding.
	if res.Reclassified != 1 || res.WithdrawSuppressed != 1 {
		t.Errorf("Reclassified=%d WithdrawSuppressed=%d, want 1/1", res.Reclassified, res.WithdrawSuppressed)
	}
}

// sameEdges compares a read-back edge set with a want list, so a fixture can name
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
