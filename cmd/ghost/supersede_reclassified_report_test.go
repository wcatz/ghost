package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/supersede"
)

// The three ids here are 32-character hex-shaped, so shortID abbreviates each to
// the eight characters every Ghost report prints — which is the point: the rows
// below are read from a report, so the ids in them have to be the form a report
// prints and the follow-up has to be driveable from what it names.
const (
	reclassNewer = "aaaaaaaabbbbccccddddeeeeffff0000"
	reclassOlder = "11111111222233334444555566667777"
	reclassFresh = "88888888777766665555444433332222"
	// reclassCauses is a pair of its own for the 'causes'-edge rows below, and it
	// has to be: a target the follow-up already lists would be deduplicated away,
	// and a row that cannot change the list cannot prove the list excludes it.
	reclassCausesNewer  = "ccccccccddddeeeeffff0000aaaaaaaabbbb"
	reclassCausesTarget = "22222222333344445555666677778888"
)

// reclassRow is a row the pass produced over a pair that CARRIED a live
// 'supersedes' edge, which is the shape every case below but the last one is.
// ReclassifiedFrom names that edge, and a fixture that leaves it empty would be
// describing a reclassify of nothing: a live edge may be 'causes' as well now
// (#823), and both the report and the follow-up branch on which relation it was.
func reclassRow(relation supersede.Relation, reclassified, withdrawn bool) supersede.Classified {
	return supersede.Classified{
		Candidate: supersede.Candidate{
			NewerID: reclassNewer, OlderID: reclassOlder,
		},
		Relation:         relation,
		Reclassified:     reclassified,
		ReclassifiedFrom: supersede.RelationSupersedes,
		Withdrawn:        withdrawn,
		TargetProjectID:  "proj",
	}
}

// TestSupersedePairLinesNamesTheEdgeAPassWouldWithdraw is #785 as the operator
// meets it, after #845. #785 was that the ordinary pass performs the identical
// InvalidateLink that --reassess and --withdraw perform and leaves the identical
// orphaned `resolved_at`, so its withdrawal is as much a repair as theirs — and it
// used to appear only as the `N reclassified` total in the header.
//
// #845 removed the withdrawal and kept the report, which is what this table is
// now mostly about. Measured over a real store, 6 of 11 of these withdrawals were
// wrong (a newer note retiring ONE claim of an older note whose other claims
// still held), so the ordinary pass names the edge and declines to remove it. The
// block is therefore the report of THREE states of a live 'supersedes' edge, not
// one: it would be removed and was not (the suppressed row, in both modes), it
// was removed, and it was already gone when this run got to it.
//
// Every row therefore carries the three things the other two paths carry: which
// edge (both ids), which verdict denied it, and whether THIS run moved it.
func TestSupersedePairLinesNamesTheEdgeAPassWouldWithdraw(t *testing.T) {
	rows := []struct {
		name       string
		apply      bool
		classified []supersede.Classified
		want       []string
		notWant    []string
	}{
		{
			name:  "a dry run says it would withdraw, and claims no write",
			apply: false,
			classified: []supersede.Classified{
				suppressedRow(reclassRow(supersede.RelationNeither, true, false)),
			},
			want: []string{
				"would withdraw",
				shortID(reclassNewer) + " -> " + shortID(reclassOlder),
				"neither",
				"STILL LIVE",
			},
			notWant: []string{"\nwithdrew", "\nalready gone"},
		},
		{
			// The headline of #845: --apply is a WRITE flag and this removal is not
			// a write, so the two modes print the SAME row. A row that gained the
			// past tense under --apply would be the report claiming a deletion that
			// the run did not make, and the deletion is the thing the whole change
			// exists to stop.
			name:  "an applied run reports the same withheld withdrawal as a dry run",
			apply: true,
			classified: []supersede.Classified{
				suppressedRow(reclassRow(supersede.RelationNeither, true, false)),
			},
			want: []string{
				"would withdraw",
				shortID(reclassNewer) + " -> " + shortID(reclassOlder),
				"neither",
				"STILL LIVE",
			},
			notWant: []string{"withdrew", "already gone", "kept"},
		},
		{
			// The clause that carries the finding, and it is the half a count
			// cannot: `would withdraw` is what a dry run says about an edge --apply
			// WOULD take, so under --apply it says the edge should be in a
			// concurrent pass's hands rather than this run's.
			name:  "a withheld row names the repair that does remove the edge",
			apply: true,
			classified: []supersede.Classified{
				suppressedRow(reclassRow(supersede.RelationReversed, true, false)),
			},
			want:    []string{"reversed", "reassess", "--withdraw"},
			notWant: []string{"withdrew", "already gone"},
		},
		{
			// A CAUSES verdict on a live 'supersedes' edge leaves BOTH relations
			// live now: the supersession is withheld and the 'causes' write is not
			// (#823 kept the sweep). So the row is a re-link AND a withheld
			// withdrawal at once, and one clause replacing the other would drop a
			// graph change the run really made.
			name:  "a causes verdict on a withheld supersedes edge names both",
			apply: true,
			classified: []supersede.Classified{
				withCauses(suppressedRow(reclassRow(supersede.RelationCauses, true, false)), 1, 1),
			},
			want: []string{
				"would withdraw", "STILL LIVE", "[+1 causes edge dropped]",
				"re-linked as " + shortID(reclassOlder) + " causes -> " + shortID(reclassNewer),
			},
			notWant: []string{"withdrew", "already gone"},
		},
		{
			// The live 'causes' withdrawal this pass really does make, and the
			// only supersedes-shaped marker left in the block that means a
			// deletion happened. `already gone` is reachable for a 'causes'-edge
			// pair a concurrent pass invalidated first, so the fall-through
			// marker is not dead — it is just no longer the answer for the
			// supersedes row, which is what the withheld case above is for.
			name:  "a causes edge this run really removed says withdrew",
			apply: true,
			classified: []supersede.Classified{
				withCauses(causesRow(supersede.RelationNeither, true), 1, 1),
			},
			want:    []string{"withdrew", "[+1 causes edge dropped]"},
			notWant: []string{"would withdraw", "already gone", "STILL LIVE"},
		},
		{
			name:  "an edge a concurrent pass took first is not claimed",
			apply: true,
			classified: []supersede.Classified{
				withCauses(causesRow(supersede.RelationNeither, false), 0, 0),
			},
			want:    []string{"already gone", shortID(reclassCausesNewer) + " -> " + shortID(reclassCausesTarget)},
			notWant: []string{"would withdraw", "\nwithdrew", "STILL LIVE"},
		},
		{
			name:  "a fresh pair is still reported as the write it is",
			apply: true,
			classified: []supersede.Classified{
				reclassRow(supersede.RelationSupersedes, false, false),
			},
			want:    []string{shortID(reclassNewer) + "  supersedes  " + shortID(reclassOlder)},
			notWant: []string{"withdrew", "would withdraw", "->"},
		},
		{
			name:  "a fresh causes pair keeps the causes line",
			apply: true,
			classified: []supersede.Classified{
				reclassRow(supersede.RelationCauses, false, false),
			},
			want:    []string{shortID(reclassOlder) + "  causes  " + shortID(reclassNewer)},
			notWant: []string{"withdrew", "would withdraw"},
		},
		{
			// A reversed FRESH pair has no edge to withdraw, and the refusal line
			// is the whole report for it. A reversed RECLASSIFY pair has one, so
			// it gets the withdrawal row instead — the two are different findings
			// and one line cannot carry both.
			name:  "a fresh reversal keeps the refusal line",
			apply: true,
			classified: []supersede.Classified{
				reclassRow(supersede.RelationReversed, false, false),
			},
			want:    []string{"reversed, not written"},
			notWant: []string{"withdrew", "would withdraw"},
		},
		{
			// A denying verdict moves a SECOND row: NEITHER and REVERSED both drop
			// the pair's live 'causes' edge, and a row that named only the
			// supersedes edge would say the run moved one row when it moved two.
			// `--reassess` marks the same thing, and these rows are printed in its
			// shape. It is reachable on a supersedes row under #845 because only
			// the SUPERSEDES sweep is withheld — the 'causes' one is not, so this
			// row really did delete a row while its withheld clause says it did not.
			name:  "a withheld supersedes row that swept a causes edge says so",
			apply: true,
			classified: []supersede.Classified{
				withCauses(suppressedRow(reclassRow(supersede.RelationNeither, true, false)), 1, 1),
			},
			want:    []string{"would withdraw", "[+1 causes edge dropped]", "STILL LIVE"},
			notWant: []string{"+0 causes", "withdrew", "already gone"},
		},
		{
			// The count is what the write returned, so a dry run has none — and a
			// marker over a pass that deleted nothing is the one line this report
			// must not print. What it MAY print is the forecast, in its own words,
			// and the two differ by the tense rather than by the number: the
			// forecast is counted off the edges the pass read, the marker is
			// counted off the writes it made.
			name:  "a dry run names the deletion it would make, in the future tense",
			apply: false,
			classified: []supersede.Classified{
				withCauses(suppressedRow(reclassRow(supersede.RelationNeither, true, false)), 0, 1),
			},
			want:    []string{"would withdraw", "[+1 causes edge would be dropped]", "STILL LIVE"},
			notWant: []string{"[+1 causes edge dropped]"},
		},
		{
			// The control for the row above: a dry run over a pair whose live
			// 'causes' edges the pass did not read — a fresh pair, which reports
			// no sweep at all — forecasts nothing rather than forecasting zero.
			name:  "a dry run with no live causes edge to read forecasts nothing",
			apply: false,
			classified: []supersede.Classified{
				withCauses(suppressedRow(reclassRow(supersede.RelationNeither, true, false)), 0, 0),
			},
			want:    []string{"would withdraw", "STILL LIVE"},
			notWant: []string{"causes edge"},
		},
		{
			// A pair the pass JUDGED and did not write, because the other
			// direction was already live when the write was attempted (#806). The
			// row is still here — a verdict was reached — and it has to say
			// so on the row, because a line reading as a link the pass created
			// is the false claim the other three markers above exist to
			// prevent. The "withdrew" vocabulary is held away from it for the
			// same reason: nothing was withdrawn either.
			name:  "a pair the pass judged and did not write is not claimed",
			apply: true,
			classified: []supersede.Classified{
				opposedRow(supersede.RelationSupersedes),
			},
			want:    []string{"not written", "the pair's reverse direction is already live", shortID(reclassNewer) + "  supersedes  " + shortID(reclassOlder)},
			notWant: []string{"withdrew", "would withdraw", "already gone"},
		},
		{
			// The same refusal carried by a CAUSES row. Only a SUPERSEDES row
			// gets the flag in production — the 'causes' write is not guarded,
			// because nothing demotes on it and refusing it there would cost a
			// call per pass forever — and a reclassified row cannot get it
			// either, because the live edge decides the direction it is asked
			// about. The row is here because the RENDERER's rule is "a row
			// that claims a write this run declined says so", whatever the row
			// holds: without the two guards this fails, printing a re-link
			// and a withdrawal for a run that did neither.
			name:  "a refused write is never dressed as a re-link or a withdrawal",
			apply: true,
			classified: []supersede.Classified{
				opposedRow(supersede.RelationCauses),
			},
			want:    []string{"not written", "causes"},
			notWant: []string{"re-linked", "withdrew", "would withdraw", "already gone"},
		},
		{
			// A reversal on a live 'supersedes' edge is the case whose withdrawal
			// used to be the point: a backwards supersession is the harm #641 found,
			// and this pass used to be the only way it left the graph. It is the one
			// place #845's trade is visible — the row reports, and `--reassess` is
			// now the only way a backwards edge leaves. The row must still name the
			// reversal, and must NOT print the FRESH-pair refusal line either, which
			// would claim the pair had no edge at all.
			name:  "a reversed reclassification is reported, not withdrawn",
			apply: true,
			classified: []supersede.Classified{
				suppressedRow(reclassRow(supersede.RelationReversed, true, false)),
			},
			want: []string{
				"would withdraw", "reversed", "STILL LIVE",
				shortID(reclassNewer) + " -> " + shortID(reclassOlder),
			},
			notWant: []string{"withdrew", "reversed, not written", "already gone"},
		},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			got := supersedePairLines(tc.apply, tc.classified)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("supersedePairLines() =\n%s\nwant it to contain %q", got, want)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("supersedePairLines() =\n%s\nwant it NOT to contain %q", got, notWant)
				}
			}
		})
	}
}

// opposedRow is reclassRow for the one outcome where the pass reached a
// verdict and wrote nothing: the pair's opposite direction was already
// live when the write was attempted (#806). Reclassified is true for the
// CAUSES shape because that is the row the refusal can reach — a
// SUPERSEDES reclassification is asked in the live edge's own direction,
// so nothing opposes it.
func opposedRow(relation supersede.Relation) supersede.Classified {
	row := reclassRow(relation, relation == supersede.RelationCauses, false)
	row.OpposedLive = true
	return row
}

// withCauses is reclassRow plus the second graph row a denying verdict moves.
// withCauses sets both counts, because a row carries both and a fixture that set
// only the observed one would be asserting against a dry run that has no way to
// produce the other. droppable is the dry run's forecast of dropped.
func withCauses(row supersede.Classified, dropped, droppable int) supersede.Classified {
	row.CausesDropped = dropped
	row.CausesDroppable = droppable
	return row
}

// suppressedRow marks the row as having a withheld 'supersedes' withdrawal, the
// case #845 added: the denying verdict is reported, but the edge is left live.
// Only a reclassified row that CARRIED the supersedes edge is affected, which is
// why the guard is here rather than at each call site — a 'causes'-edge row is
// swept for real and must not be dressed as withheld.
func suppressedRow(row supersede.Classified) supersede.Classified {
	if row.ReclassifiedFrom == supersede.RelationSupersedes {
		row.WithdrawSuppressed = true
		row.Withdrawn = false
	}
	return row
}

// causesRow is reclassRow for a pair whose live edge was a 'causes' edge, and it
// uses the 'causes' pair's own ids so a row printed from it cannot be confused
// with the supersedes pair's. It is the shape --apply really does mutate now: a
// denying verdict sweeps the 'causes' edge rather than withholding it, because no
// command in the product can reach one (see supersede.Reassess).
func causesRow(relation supersede.Relation, withdrawn bool) supersede.Classified {
	return supersede.Classified{
		Candidate: supersede.Candidate{
			NewerID: reclassCausesNewer, OlderID: reclassCausesTarget,
		},
		Relation:         relation,
		Reclassified:     true,
		ReclassifiedFrom: supersede.RelationCauses,
		Withdrawn:        withdrawn,
		TargetProjectID:  "proj",
	}
}

// TestAStaleWithheldWithdrawalSaysTheEdgeIsGoneBecauseItIs is the CLI half of
// the stale-row rule, and it is a separate test because the fix is in two files:
// `internal/supersede` clears `Classified.WithdrawSuppressed` at the site that
// learns an endpoint is gone (pinned there by
// TestAStaleWithheldWithdrawalSaysTheEdgeIsGoneBecauseItIs), and this file holds
// what the REPORT says once it has — the row falls back to `already gone`, and
// the summary carries no withheld line.
//
// Both matter because the two are what the flag drives: `WithdrawSuppressed`
// selects the `STILL LIVE` clause on the row and gates the `N pair(s) withheld`
// count in the summary, and a stale row carries neither. A report that printed
// the clause would assert an edge is still live and still demoting its target for
// a memory the concurrent reflect pass had already deleted — and
// `memory_links.source_id`/`target_id` are `ON DELETE CASCADE`, so the edge went
// with it. Before #845 that row said `already gone`, which is the truth.
//
// The rows here are built by hand rather than driven from a pass, because the
// pass cannot emit them: the stale shape is exactly the one the pass now refuses
// to emit, so a fixture that reached it through `runSupersede` would be testing
// the fix's absence. What is held is the renderer's rule over both shapes, and
// the summary's count, so a future producer of a stale row cannot make the report
// claim a live edge.
func TestAStaleWithheldWithdrawalSaysTheEdgeIsGoneBecauseItIs(t *testing.T) {
	// The shape the pass emits for a stale pair: classified, relation differs from
	// the edge it carried, `Withdrawn` false because nothing here removed it, and
	// `WithdrawSuppressed` FALSE because the edge is not there to withhold.
	stale := reclassRow(supersede.RelationNeither, true, false)
	rows := supersedePairLines(true, []supersede.Classified{stale})
	if !strings.Contains(rows, "already gone") {
		t.Errorf("a stale pair's row does not say its edge is gone:\n%s", rows)
	}
	for _, notWant := range []string{"STILL LIVE", "would withdraw", "withdrew"} {
		if strings.Contains(rows, notWant) {
			t.Errorf("a stale pair's row claims %q about an edge a concurrent pass cascade-deleted:\n%s", notWant, rows)
		}
	}
	// The row is still REACHED: a verdict was reached and a classify call was
	// spent, so dropping the row too would leave the run reporting less than it
	// did, which is the other half of the same lie.
	if !strings.Contains(rows, shortID(reclassNewer)+" -> "+shortID(reclassOlder)) {
		t.Errorf("a stale pair's row is not reported at all:\n%s", rows)
	}

	// And the summary's withheld line is keyed on the SAME flag, so a pass whose
	// only stale row contributed nothing to the count prints the stale line and not
	// the withheld one — which is the compounding the review named, "still live and
	// still demoting its target" over a memory that no longer exists.
	summary := supersedeReport("projy", supersede.Result{
		Reclassified: 1, StaleAtWrite: 1,
	}, "linked", true, 1, 0)
	if !strings.Contains(summary, "1 pair(s) not written") {
		t.Errorf("the summary does not report the stale pair:\n%s", summary)
	}
	if strings.Contains(summary, "pair(s) withheld") || strings.Contains(summary, "STILL LIVE") {
		t.Errorf("the summary counts a stale pair as a withheld withdrawal:\n%s", summary)
	}
}

// TestSupersedePairLinesPrintsNothingForAnEmptyPass: the header line above says
// how many pairs were considered, and a block that printed a header of its own
// would be a second count of the same thing.
func TestSupersedePairLinesPrintsNothingForAnEmptyPass(t *testing.T) {
	if got := supersedePairLines(true, nil); got != "" {
		t.Errorf("supersedePairLines(true, nil) = %q, want the empty string", got)
	}
	if got := supersedePairLines(true, []supersede.Classified{
		{ // A pair the classifier could not parse: nothing was decided, so there
			// is no relation to print and no edge to have withdrawn.
			Candidate: supersede.Candidate{NewerID: reclassNewer, OlderID: reclassOlder},
			Relation:  supersede.Relation(""),
		},
	}); got != "" {
		t.Errorf("an unparseable verdict printed %q, want nothing: the pass decided nothing about this pair", got)
	}
}

// TestReclassifiedWithdrawalsNamesEveryOrphanedTarget: the withdrawal is half the
// repair, and the half the graph cannot do on its own is the `resolved_at` the
// edge's piggyback stamped on the target. So the follow-up is built over these
// rows and only these rows — a fresh pair contributes none, because no edge ever
// justified a resolution for it.
//
// Every REAL withdrawal counts, INCLUDING one this run did not write: a
// concurrent pass that took the edge first left the same state behind (no live
// edge, and a resolved_at nothing defends), which is exactly the state the repair
// clears. The reason is the same one withdrawnTargets gives.
//
// A WITHHELD withdrawal is the case that must NOT count (#845), and it is here as
// the sharpest row in the list: the pass denied the edge, the edge is still in the
// graph, and the piggyback that stamped the `resolved_at` is still holding the
// target down. Naming its target would send an operator to un-hide a memory a live
// supersession is correctly hiding, and — in a dry run — would print "Re-run with
// --apply to withdraw these edges." for an edge --apply will not touch.
func TestReclassifiedWithdrawalsNamesEveryOrphanedTarget(t *testing.T) {
	got := withdrawnTargets(reclassifiedWithdrawals([]supersede.Classified{
		suppressedRow(reclassRow(supersede.RelationNeither, true, false)),
		suppressedRow(reclassRow(supersede.RelationReversed, true, false)),
		suppressedRow(reclassRow(supersede.RelationCauses, true, false)),
		reclassRow(supersede.RelationSupersedes, true, false),
		reclassRow(supersede.RelationCauses, false, false),
		{
			Candidate:    supersede.Candidate{NewerID: reclassFresh, OlderID: reclassOlder},
			Relation:     supersede.RelationNeither,
			Reclassified: false,
		},
		// A pair that carried a live 'causes' edge and came back NEITHER: a
		// real graph change, and NO repair. The follow-up is a `ghost resolve
		// --reassess`, and resolve's supersedes piggyback — the thing that
		// stamped the resolved_at this repair clears — acts on 'supersedes'
		// edges alone, so a withdrawn 'causes' edge orphaned nothing and naming
		// its target would send the operator after a memory nothing is holding
		// down.
		{
			Candidate: supersede.Candidate{
				NewerID: reclassCausesNewer, OlderID: reclassCausesTarget,
			},
			Relation:         supersede.RelationNeither,
			Reclassified:     true,
			ReclassifiedFrom: supersede.RelationCauses,
			Withdrawn:        true,
			TargetProjectID:  "proj",
		},
	}))
	if len(got) != 0 {
		t.Errorf("withdrawnTargets(reclassifiedWithdrawals(...)) = %+v, want nothing: every supersedes withdrawal in that list was WITHHELD, so every edge is still live and nothing is orphaned", got)
	}
	for _, g := range got {
		for _, target := range g.Targets {
			if target == reclassCausesTarget {
				t.Errorf("the follow-up names %s, the target of a withdrawn 'causes' edge: no 'causes' edge ever stamped a resolved_at, so there is nothing there to clear", reclassCausesTarget)
			}
		}
	}

	// The other half of the same function: a row that DID leave an orphaned
	// `resolved_at` still reaches the SAME follow-up the two repair modes print —
	// a scoped resolve repair over the withdrawn targets, never the project-wide
	// re-judge. Without it, the exclusion above would read as "the follow-up is
	// gone", and it is not: the projection is what carries it, and it is the
	// WITHELD filter above that empties the list for an ordinary pass, not a
	// removal of the path. The command goes through the shared renderer, so the id
	// is quoted the way a shell reads it as one argument.
	//
	// The row is built as the pass itself would build it if
	// ordinaryPassWithdrawsSupersedes were true — suppressed false, `Withdrawn`
	// set — which is the point: the two are told apart by the suppression flag and
	// not by the withdrawal flag, so a row can carry `Withdrawn` and still be
	// withheld, and one without it can still have been withdrawn by a concurrent
	// pass. Holding the invariant that `reclassifiedWithdrawals` excludes a
	// withheld row WHATEVER its Withdrawn holds is what stops the flag being
	// "cleaned up" into one call to Withdrawn later.
	withdrawnRow := reclassRow(supersede.RelationNeither, true, true)
	one := withdrawnTargets(reclassifiedWithdrawals([]supersede.Classified{withdrawnRow}))
	if len(one) != 1 {
		t.Fatalf("withdrawnTargets = %+v, want one project group", one)
	}
	block := supersedeReassessFollowup(one[0].ProjectID, one[0].Targets, "")
	if !strings.Contains(block, "ghost resolve proj --reassess --only '"+reclassOlder+"' --apply") {
		t.Errorf("the follow-up an ordinary pass prints is not the scoped resolve repair:\n%s", block)
	}
}

// TestRunSupersedeReportsButDoesNotWithdrawTheEdgeItWouldRemove drives the real
// command, because the formatters above are the report's WORDING and what #845
// changed is the report's CLAIM — a formatter test would pass against a command
// that removed the edge and printed a hopeful sentence about it.
//
// The fixture is a live 'supersedes'/'llm' edge over two notes, with the link row
// backdated so skip-if-unchanged re-judges the pair — which is how a pair whose
// endpoints moved reaches the classifier with no vector involved. The fake harness
// answers NEITHER on every consensus pass, so the verdict is unanimous and the
// edge is denied.
//
// Both modes must print the SAME thing and BOTH must leave the edge live:
//
//   - The row names the edge by both ids and the verdict, in the tense the pass
//     really used — `would withdraw`, plus the clause saying the edge is STILL
//     LIVE. Under --apply the old tense was `withdrew`, and that is the sentence
//     this change withdraws its permission for.
//   - The summary carries its own count and names the repair, through the one
//     renderer that spells a project as a shell argument.
//   - NO resolve follow-up, in either mode. The follow-up exists because a
//     withdrawal orphans the `resolved_at` the edge's piggyback stamped on its
//     target; the edge is live, so the piggyback is still holding that target
//     down and there is nothing orphaned. An --apply run printing one would be
//     running a repair against a supersession it just declined to remove.
//   - And no "Re-run with --apply to withdraw these edges." — that hint is keyed
//     off the same follow-up list, so it would tell the operator the flag is the
//     repair when --apply is what was just run and changed nothing.
func TestRunSupersedeReportsButDoesNotWithdrawTheEdgeItWouldRemove(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply bool
	}{
		{name: "dry run", apply: false},
		{name: "applied", apply: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataHome := isolatedLifecycleEnv(t)
			// A harness the pass can resolve, answering the one-word verdict
			// contract inside the JSON-lines envelope `opencode run --format json`
			// emits. The stub ignores its arguments, so the same script answers the
			// availability probe and the classify call.
			binDir := t.TempDir()
			harness := filepath.Join(binDir, "opencode")
			writeExecutable(t, harness, "#!/bin/sh\n"+`printf '{"type":"text","part":{"type":"text","text":"NEITHER"}}\n'`+"\n")
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GHOST_CLI_OPENCODE_BINARY", harness)

			dbPath := filepath.Join(dataHome, "ghost", "ghost.db")
			if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
				t.Fatalf("mkdir data dir: %v", err)
			}
			newer, older := seedLiveSupersedesEdge(t, dbPath)

			origArgs := os.Args
			args := []string{origArgs[0], "supersede", "projy", "--source", "opencode", "--consensus", "3"}
			if tc.apply {
				args = append(args, "--apply")
			}
			os.Args = args
			t.Cleanup(func() { os.Args = origArgs })

			out := captureStdout(t, runSupersede)

			// The edge, by both ids, in the report's own eight-character form; the
			// verdict that denied it; and the clause that says the edge is still
			// there — the half a count cannot carry.
			for _, want := range []string{
				"would withdraw",
				shortID(newer) + " -> " + shortID(older),
				"neither",
				"STILL LIVE",
			} {
				if !strings.Contains(out, want) {
					t.Errorf("`ghost supersede` did not report %q:\n%s", want, out)
				}
			}
			// The summary's own line, and the repair through the shared renderer —
			// so the project is spelled the way a shell reads it as one argument.
			if !strings.Contains(out, "1 pair(s) withheld") {
				t.Errorf("the summary does not count the withheld withdrawal:\n%s", out)
			}
			if !strings.Contains(out, "ghost supersede projy --reassess --consensus 3 --apply") {
				t.Errorf("the summary does not name the repair that removes the edge:\n%s", out)
			}
			// And the repair that is NOT owed.
			if strings.Contains(out, "Follow-up:") || strings.Contains(out, "ghost resolve projy --reassess") {
				t.Errorf("a run that left the edge live printed a resolve repair for it:\n%s", out)
			}
			if strings.Contains(out, "Re-run with --apply to withdraw these edges.") {
				t.Errorf("the report tells the operator --apply withdraws the edge, and it does not:\n%s", out)
			}

			// The tense is a claim about the graph, so the graph is checked.
			store := openStore(t, dbPath)
			links, err := store.LinksByRelationSource(context.Background(), "projy", string(supersede.RelationSupersedes), "llm")
			if err != nil {
				t.Fatalf("LinksByRelationSource: %v", err)
			}
			if len(links) != 1 {
				t.Errorf("live edge(s) = %d after a pass that reports the withdrawal instead of making it, want 1", len(links))
			}
		})
	}
}

// seedLiveSupersedesEdge writes a project with two notes and a live
// 'supersedes'/'llm' edge between them, and backdates the link row so
// skip-if-unchanged re-judges the pair on the next pass. It returns the two ids.
//
// The backdating is what makes this a reclassification rather than a fresh
// proposal: neither memory carries a vector, so the candidate scan proposes
// nothing, and the reclassify half — which reads link rows and never a vector —
// is the only thing that can put this pair to the classifier.
func seedLiveSupersedesEdge(t *testing.T, dbPath string) (newer, older string) {
	t.Helper()
	ctx := context.Background()
	store := openStore(t, dbPath)
	if err := store.EnsureProject(ctx, "projy", "/tmp/projy", "projy"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ids := make([]string, 0, 2)
	for _, content := range []string{
		"the deploy pipeline now runs the migration check before it stages a build",
		"the deploy pipeline runs the unit suite before it stages a build",
	} {
		id, err := store.Create(ctx, "projy", memory.Memory{
			Category: "architecture", Content: content, Source: "mcp", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("create memory: %v", err)
		}
		ids = append(ids, id)
	}
	newer, older = ids[0], ids[1]
	if err := store.CreateLink(ctx, newer, older, string(supersede.RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	// Closed before the raw handle opens, so the backdate cannot queue behind a
	// connection the seeding store still holds.
	if err := store.Close(); err != nil {
		t.Fatalf("close the seeding store: %v", err)
	}
	raw, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer raw.Close() //nolint:errcheck
	if _, err := raw.ExecContext(ctx,
		`UPDATE memory_links SET created_at = '2020-01-01 00:00:00' WHERE source_id = ? AND target_id = ?`,
		newer, older,
	); err != nil {
		t.Fatalf("backdate the link row: %v", err)
	}
	return newer, older
}

// TestSupersedeWithdrawFollowsTheTargetIntoItsOwnProject is the second half of
// #786, and it is a promise the first half would otherwise break. A `ghost
// supersede _global --withdraw` whose target stayed in a project is the whole
// point of reaching such a pair from `_global` — and the follow-up it printed was
// `ghost resolve _global --reassess --only <target> --apply`, which resolves the
// selector (a `_global` ref scope reaches a project memory) and then finds
// nothing: `ResolvedCandidates` filters `project_id = ?`, so a memory in `p` is
// not in the `_global` pool. Every selector comes back a miss and the orphaned
// `resolved_at` is never cleared, under a block that says it can now be cleared.
//
// So the repair is scoped to the project that owns the memory being un-hidden,
// which is the project its `resolved_at` lives in. The common case — a target in
// the project the command was run against — is unchanged, because that is the same
// project.
func TestSupersedeWithdrawFollowsTheTargetIntoItsOwnProject(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	dbPath := filepath.Join(dataHome, "ghost", "ghost.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	source, target := seedPromotedSupersedesEdge(t, dbPath)

	origArgs := os.Args
	os.Args = []string{origArgs[0], "supersede", "_global",
		"--withdraw", source[:8], target[:8], "--apply"}
	t.Cleanup(func() { os.Args = origArgs })

	out := captureStdout(t, runSupersede)

	// The edge is gone, and the report says so.
	store := openStore(t, dbPath)
	links, err := store.LinksByRelationSource(context.Background(), "projy", string(supersede.RelationSupersedes), "llm")
	if err != nil {
		t.Fatalf("LinksByRelationSource: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("live edge(s) = %d after the withdrawal, want 0: the run did not reach the edge at all", len(links))
	}
	// And the follow-up names the project whose pool holds the target, not the one
	// the command was run against.
	if !strings.Contains(out, "ghost resolve projy --reassess --only") {
		t.Errorf("the follow-up is not scoped to the TARGET's project, so its repair cannot reach the memory it un-hides:\n%s", out)
	}
	if strings.Contains(out, "ghost resolve _global --reassess --only") {
		t.Errorf("the follow-up is scoped to _global, whose repair pool holds no project memory:\n%s", out)
	}
	if !strings.Contains(out, target) {
		t.Errorf("the follow-up names no target id:\n%s", out)
	}
}

// seedPromotedSupersedesEdge writes a project with a live 'supersedes'/'llm' edge
// and then promotes the SOURCE into `_global`, which is the shape
// `ghost_memory_promote` leaves behind. It returns the two ids.
func seedPromotedSupersedesEdge(t *testing.T, dbPath string) (source, target string) {
	t.Helper()
	ctx := context.Background()
	store := openStore(t, dbPath)
	if err := store.EnsureProject(ctx, "projy", "/tmp/projy", "projy"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ids := make([]string, 0, 2)
	for _, content := range []string{
		"the ingest service now writes both replicas before acknowledging",
		"the ingest service acknowledges after the primary replica only",
	} {
		id, err := store.Create(ctx, "projy", memory.Memory{
			Category: "architecture", Content: content, Source: "mcp", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("create memory: %v", err)
		}
		ids = append(ids, id)
	}
	source, target = ids[0], ids[1]
	if err := store.CreateLink(ctx, source, target, string(supersede.RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if err := store.PromoteToGlobal(ctx, "projy", source); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close the seeding store: %v", err)
	}
	return source, target
}

// seedLiveCausesEdge is seedLiveSupersedesEdge for the other relation, and the
// pair it writes is a live 'causes' edge — older→newer, so the SOURCE is the
// older note. Stamped 2020 for the same reason the supersedes fixture is: the
// pair has to be re-judged for a verdict to exist, and the way to reach the
// classifier with no vector involved is a pair whose edge predates both
// endpoints.
//
// The first memory is backdated where the supersedes fixture does not have to be,
// and the asymmetry is the rule rather than an inconvenience. A 'supersedes' edge
// carries its own direction, so a pair whose rows share a timestamp is still
// re-judged in the direction the edge names. A 'causes' edge settles nothing about
// the question, so the pair is asked in the direction `orient` gives — and
// `orient` refuses two rows that share both timestamps, exactly as it refuses them
// in a scan. Two memories created in the same instant therefore tie, and the pair
// would be reported as unproposed; giving the two notes real ages is what makes
// the fixture reachable at all.
func seedLiveCausesEdge(t *testing.T, dbPath string) (older, newer string) {
	t.Helper()
	ctx := context.Background()
	store := openStore(t, dbPath)
	if err := store.EnsureProject(ctx, "projy", "/tmp/projy", "projy"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	ids := make([]string, 0, 2)
	for _, content := range []string{
		"the deploy pipeline runs the unit suite before it stages a build",
		"the deploy pipeline now runs the migration check before it stages a build",
	} {
		id, err := store.Create(ctx, "projy", memory.Memory{
			Category: "architecture", Content: content, Source: "mcp", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("create memory: %v", err)
		}
		ids = append(ids, id)
	}
	older, newer = ids[0], ids[1]
	if err := store.CreateLink(ctx, older, newer, string(supersede.RelationCauses), 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close the seeding store: %v", err)
	}
	raw, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer raw.Close() //nolint:errcheck
	if _, err := raw.ExecContext(ctx,
		`UPDATE memory_links SET created_at = '2020-01-01 00:00:00' WHERE source_id = ? AND target_id = ?`,
		older, newer,
	); err != nil {
		t.Fatalf("backdate the link row: %v", err)
	}
	// The first note gets a real age. BOTH columns, because `orient` reads
	// updated_at first and falls back to created_at only on a tie, so a backdate
	// that left updated_at alone would be decided by the column the backdate did
	// not touch.
	if _, err := raw.ExecContext(ctx,
		`UPDATE memories SET created_at = '2026-01-01 00:00:00', updated_at = '2026-01-01 00:00:00' WHERE id = ?`,
		older,
	); err != nil {
		t.Fatalf("backdate the older memory row: %v", err)
	}
	return older, newer
}

// TestRunSupersedeReportsACausesEdgeItReplacedWithASupersession is #823 on the
// report, and it drives the real command because the report is the whole of what
// an operator is told about a graph change.
//
// The pass read a live 'causes' edge, judged the pair SUPERSEDES, and replaced
// one graph row with another. Three things have to be true of what it printed:
//
//   - the reclassified row says the pair is now a SUPERSEDES, in words. The
//     bracketed finding is what distinguishes a real replacement from a wrong
//     edge, and an empty one leaves a line that says the run did something and
//     not what.
//
//   - the row names the edge that replaced it. "1 reclassified" in the summary is
//     a total; a total over a relation change is a number nobody can act on, and
//     the second graph row the run moved is the half the line used to leave out.
//
//   - NO resolve follow-up. The follow-up is a `ghost resolve --reassess`, and
//     resolve's supersedes piggyback — the thing that stamped the resolved_at it
//     clears — acts on 'supersedes'/'llm' edges alone. A withdrawn 'causes' edge
//     orphaned no resolution, so printing the repair would send an operator to
//     clear a memory nothing is holding down, scoped to a target that is not the
//     one whose state changed.
func TestRunSupersedeReportsACausesEdgeItReplacedWithASupersession(t *testing.T) {
	for _, tc := range []struct {
		name  string
		apply bool
		row   string
	}{
		{name: "dry run", apply: false, row: "would withdraw"},
		{name: "applied", apply: true, row: "withdrew"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataHome := isolatedLifecycleEnv(t)
			binDir := t.TempDir()
			harness := filepath.Join(binDir, "opencode")
			// A supersession with a `replaced:` claim, because the rubric requires
			// one and a verdict without it reads NEITHER — which would make this
			// test pass for a reason that has nothing to do with the relation.
			writeExecutable(t, harness, "#!/bin/sh\n"+`printf '{"type":"text","part":{"type":"text","text":"SUPERSEDES\\nreplaced: the deploy pipeline runs the unit suite before it stages a build"}}\n'`+"\n")
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("GHOST_CLI_OPENCODE_BINARY", harness)

			dbPath := filepath.Join(dataHome, "ghost", "ghost.db")
			if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
				t.Fatalf("mkdir data dir: %v", err)
			}
			older, newer := seedLiveCausesEdge(t, dbPath)

			origArgs := os.Args
			args := []string{origArgs[0], "supersede", "projy", "--source", "opencode"}
			if tc.apply {
				args = append(args, "--apply")
			}
			os.Args = args
			t.Cleanup(func() { os.Args = origArgs })

			out := captureStdout(t, runSupersede)

			for _, want := range []string{
				tc.row,
				"supersedes: the newer note replaces the older one",
				"re-linked",
				"supersedes -> " + shortID(older),
				shortID(newer),
			} {
				if !strings.Contains(out, want) {
					t.Errorf("`ghost supersede` did not report %q:\n%s", want, out)
				}
			}
			// The summary has to count it too: the row is the detail and this is
			// the line an operator reads when they only read one.
			if !strings.Contains(out, "1 reclassified") {
				t.Errorf("the summary does not count the relation change:\n%s", out)
			}
			// And the dry run's hint must be the one that fits what it would do.
			// The "withdraw these edges" hint is tied to the resolve follow-up,
			// which this pair does not owe, so a dry run here promises to write a
			// link — which is exactly what --apply would do.
			if !tc.apply && !strings.Contains(out, "Re-run with --apply to write these links.") {
				t.Errorf("a dry run that would replace the edge does not tell the operator to apply:\n%s", out)
			}
			if !tc.apply && strings.Contains(out, "Re-run with --apply to withdraw these edges.") {
				t.Errorf("a dry run promises to withdraw an edge, and this pair's withdrawal orphans no resolution to repair:\n%s", out)
			}
			// And the repair that is NOT owed.
			if strings.Contains(out, "Follow-up:") || strings.Contains(out, "ghost resolve projy --reassess") {
				t.Errorf("a withdrawn 'causes' edge printed a resolve repair, and no 'causes' edge ever stamped the resolved_at it clears:\n%s", out)
			}

			// The graph, so the tense above is a claim about something.
			store := openStore(t, dbPath)
			ctx := context.Background()
			causes, err := store.LinksByRelationSource(ctx, "projy", string(supersede.RelationCauses), "llm")
			if err != nil {
				t.Fatalf("LinksByRelationSource(causes): %v", err)
			}
			supers, err := store.LinksByRelationSource(ctx, "projy", string(supersede.RelationSupersedes), "llm")
			if err != nil {
				t.Fatalf("LinksByRelationSource(supersedes): %v", err)
			}
			if tc.apply {
				if len(causes) != 0 {
					t.Errorf("the report says the 'causes' edge went and %d live edge(s) remain", len(causes))
				}
				if len(supers) != 1 {
					t.Errorf("the report says the pair is now a supersession and %d live edge(s) are", len(supers))
				}
			} else {
				if len(causes) != 1 || len(supers) != 0 {
					t.Errorf("a dry run left %d 'causes' and %d 'supersedes' edge(s), want 1 and 0: it wrote something", len(causes), len(supers))
				}
			}
		})
	}
}

// TestSupersedePairLinesNamesTheEdgeACausesCycleRemoved is the reporting half of
// the 'causes'-cycle rule, and it is a formatter test on purpose: what the pass
// writes is held by internal/supersede, and what the operator is TOLD is held
// here.
//
// A 'causes' CYCLE answered CAUSES keeps the direction the pass was asked about
// and drops the edge asserting the other, so the pair's live claim goes from two
// edges to one while the relation is unchanged. The row is a reclassified pair
// whose verdict equals the edge it carried, which is the one combination the
// report's skip test used to treat as "nothing happened" — so the run deleted a
// graph row, the summary counted zero reclassifications, and the row printed as an
// ordinary `causes` line.
func TestSupersedePairLinesNamesTheEdgeACausesCycleRemoved(t *testing.T) {
	row := supersede.Classified{
		Candidate: supersede.Candidate{
			NewerID: reclassCausesNewer, OlderID: reclassCausesTarget,
		},
		Relation:         supersede.RelationCauses,
		Reclassified:     true,
		ReclassifiedFrom: supersede.RelationCauses,
		Withdrawn:        true,
		CausesDropped:    1,
		TargetProjectID:  "proj",
	}
	// The two halves of the row: the marker, which is a claim about the graph and
	// has to be the one this run earns, and the clause naming the second row.
	out := supersedePairLines(true, []supersede.Classified{row})
	if !strings.Contains(out, "withdrew") {
		t.Errorf("a row whose live edge this run removed does not carry the `withdrew` marker:\n%s", out)
	}
	if strings.Contains(out, "already gone") {
		t.Errorf("a row this run withdrew claims the edge was already gone:\n%s", out)
	}
	if !strings.Contains(out, "[+1 causes edge dropped]") {
		t.Errorf("the row says nothing about the edge the run removed:\n%s", out)
	}
	// And the same pair in a dry run promises rather than claims. Two fields have
	// to change together, and both changes are what a real dry run produces:
	// `Withdrawn` is false because the apply block never ran, and
	// `CausesDropped` is 0 because nothing was invalidated. Setting only the
	// first would leave a row no dry run can emit — one claiming a deletion —
	// and this block's own predicate would then admit it on the observed count,
	// so the forecast below would never be the reason the row was reached and
	// the assertion would prove nothing about the dry run's own path.
	promised := row
	promised.Withdrawn = false
	promised.CausesDropped = 0
	promised.CausesDroppable = row.CausesDropped
	dry := supersedePairLines(false, []supersede.Classified{promised})
	if !strings.Contains(dry, "would withdraw") {
		t.Errorf("a dry run's row is not in the would-withdraw tense:\n%s", dry)
	}
	if !strings.Contains(dry, "[+1 causes edge would be dropped]") {
		t.Errorf("a dry run says nothing about the edge it would remove:\n%s", dry)
	}
	if strings.Contains(dry, "[+1 causes edge dropped]") {
		t.Errorf("a dry run's row claims a deletion it did not make:\n%s", dry)
	}
	// The re-link clause follows the forecast rather than the relation alone: the
	// relation did not change, but the edge the verdict wrote is a different edge
	// from the one the row is about, so `re-affirmed` would be the false one.
	if !strings.Contains(dry, "re-linked") {
		t.Errorf("a dry run's row calls a corrected 'causes' edge a re-affirmation:\n%s", dry)
	}

	// The control: a re-affirmation that moved NOTHING stays off this block. A
	// predicate that admits every re-classified row would make every quiet
	// `causes` re-confirmation print as a withdrawal, which is the false claim in
	// the other direction.
	quiet := row
	quiet.Withdrawn = false
	quiet.CausesDropped = 0
	if out := supersedePairLines(true, []supersede.Classified{quiet}); strings.Contains(out, "withdrew") {
		t.Errorf("a re-affirmation that moved nothing is dressed as a withdrawal:\n%s", out)
	}
}

// TestSupersedePairLinesSaysKeptWhenTheEdgeItIsAboutSurvived is the shape the
// second review round found, and it is a line that made THREE claims where two
// were false.
//
// A live 'supersedes' edge re-AFFIRMED beside a 'causes' CYCLE: the pass kept the
// edge (and re-stamped it), and the verdict removed the cycle's two rows. The row
// was routed into the withdrawal block because the run did move graph rows, and
// the block's markers then said `already gone` — a claim about the 'supersedes'
// edge, which is still live and which this run never removed — over a `re-linked`
// clause announcing a change of relation that had not happened.
func TestSupersedePairLinesSaysKeptWhenTheEdgeItIsAboutSurvived(t *testing.T) {
	row := supersede.Classified{
		Candidate: supersede.Candidate{
			NewerID: reclassNewer, OlderID: reclassOlder,
		},
		Relation:         supersede.RelationSupersedes,
		Reclassified:     true,
		ReclassifiedFrom: supersede.RelationSupersedes,
		CausesDropped:    2,
		TargetProjectID:  "proj",
	}
	out := supersedePairLines(true, []supersede.Classified{row})
	for _, want := range []string{
		"kept",
		"supersedes: the newer note replaces the older one",
		"re-affirmed",
		"[+2 causes edge dropped]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the row does not say %q:\n%s", want, out)
		}
	}
	// The two markers that would be false, named.
	for _, notWant := range []string{"already gone", "would withdraw", "re-linked"} {
		if strings.Contains(out, notWant) {
			t.Errorf("the row claims %q, and the edge it is about never left the graph:\n%s", notWant, out)
		}
	}

	// The control in the other direction: a row whose edge really WAS taken by a
	// concurrent pass still says `already gone`, because that is a fact about the
	// graph and the marker above is only a fact about THIS run.
	taken := row
	taken.Relation = supersede.RelationNeither
	taken.ReclassifiedFrom = supersede.RelationSupersedes
	taken.CausesDropped = 0
	if out := supersedePairLines(true, []supersede.Classified{taken}); !strings.Contains(out, "already gone") {
		t.Errorf("an edge this run did not remove is not reported as already gone:\n%s", out)
	}
}
