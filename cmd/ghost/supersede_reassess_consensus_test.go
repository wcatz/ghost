package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/supersede"
)

// #862 at the CLI surface: --reassess accepts --consensus N, a gated repair
// reports the split instead of acting on one of its verdicts, and an ungated
// `--reassess --apply` says out loud that it acted on one verdict per edge.
//
// The pass's own behaviour is internal/supersede/reassess_consensus_test.go.
// These are the report and the argv, which are the two surfaces that carry a
// deletion claim to an operator.

// TestReassessReportNamesTheGateAndTheSplit: what the reader is shown.
//
// The headline has to say the gate ran, because "0 withdrawn" over three
// unanimous passes and the same total over one pass are different claims and the
// ungated one is the weaker (#845). And a split has to be PRINTED with its
// tally and the fact that the edge stands: a report that only counted the
// refusals reads as a pass that had nothing to withdraw, which is the invisibility
// this whole repair path was built to remove.
func TestReassessReportNamesTheGateAndTheSplit(t *testing.T) {
	res := supersede.ReassessResult{
		Loaded: 3, Confirmed: 1, Consensus: 3, NotAgreed: 1,
		Disputed: []supersede.Disputed{{
			NewerID: "1122334455667788", OlderID: "8877665544332211",
			Tally: map[supersede.Relation]int{supersede.RelationNeither: 2, supersede.RelationSupersedes: 1},
		}},
	}
	out := supersedeReassessReport("proj", res, true, nil, 3, 0)
	for _, want := range []string{
		"consensus 3",
		"1 pair(s) not agreed: the 3 classification passes did not all agree, so every edge of these pairs stands exactly as it was",
		// Descending vote order: the verdict a reader takes from a split is the
		// one MOST passes gave, so NEITHER leads and the supersedes minority does
		// not. A fixed verdict order would print this line the other way round.
		"  not agreed  11223344 -> 88776655  [2 neither, 1 supersedes, and the edge stands]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("gated reassess report missing %q:\n%s", want, out)
		}
	}
	// And the things it must NOT say: nothing was withdrawn here, so a row
	// claiming a withdrawal, or an invitation to apply, would be a lie about the
	// graph.
	// No per-edge row claims a withdrawal, and there is no invitation to apply.
	// The headline's own "withdrew 0" is not what this checks — it is the count of
	// writes that landed and 0 is true — so the markers tested are the ROW
	// markers, which are the only lines that claim an edge moved.
	for _, unwanted := range []string{"\n  would withdraw", "\n  withdrew ", "\n  already gone", "Re-run with --apply"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("a split-only report contains the withdrawal marker %q:\n%s", unwanted, out)
		}
	}

	// An ungated run's line is unchanged — no ", consensus N" clause — because
	// every existing reader and every existing script sees exactly the report it
	// saw before #862.
	ungated := supersedeReassessReport("proj", supersede.ReassessResult{
		Loaded: 3, Confirmed: 1, Consensus: 1, Withdrawn: 1,
	}, true, []supersede.WithdrawnEdge{
		{NewerID: "1122334455667788", OlderID: "8877665544332211", Reason: "neither: both notes are still true", Written: true},
	}, 1, 0)
	if strings.Contains(ungated, "consensus") {
		t.Errorf("an ungated run prints a gate clause it did not run:\n%s", ungated)
	}
}

// TestReassessReportPrintsNoSplitLineForAnUngatedRun guards the other
// direction: the not-agreed block is the GATE's report, and an ungated pass has
// no tally because it never ran twice. A block printed on one verdict would tell
// the operator N passes disagreed when one pass was asked.
func TestReassessReportPrintsNoSplitLineForAnUngatedRun(t *testing.T) {
	out := supersedeReassessReport("proj", supersede.ReassessResult{
		Loaded: 1, Neither: 1, Consensus: 1, Withdrawn: 1,
	}, true, []supersede.WithdrawnEdge{
		{NewerID: "1122334455667788", OlderID: "8877665544332211", Reason: "neither", Written: true},
	}, 1, 0)
	if strings.Contains(out, "not agreed") {
		t.Errorf("an ungated run reported a split between passes that never happened:\n%s", out)
	}
}

// TestReassessReportNamesTheUnjudgedReasonTheModeActuallyHas: the wording of a
// state the gate made narrower.
//
// Ungated, every unjudged pair is one NO pass answered. Under a gate a pair can
// have been answered by the first of three passes and missed by the one that
// died — and printing "no verdict" for it would blame the harness for a pair it
// did answer. So the clause changes with the mode, and the row changes with it.
func TestReassessReportNamesTheUnjudgedReasonTheModeActuallyHas(t *testing.T) {
	res := supersede.ReassessResult{
		Loaded: 1, Consensus: 3, Unjudged: []supersede.UnjudgedPair{
			{NewerID: "1122334455667788", OlderID: "8877665544332211"},
		},
	}
	for _, tc := range []struct {
		name        string
		res         supersede.ReassessResult
		want, avoid string
	}{
		{
			name:  "ungated",
			res:   supersede.ReassessResult{Loaded: 1, Consensus: 1, Unjudged: res.Unjudged},
			want:  "no verdict: the classify call failed",
			avoid: "no quorum",
		},
		{
			name:  "gated",
			res:   res,
			want:  "no quorum: fewer than the 3 passes answered the pair",
			avoid: "no verdict",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := supersedeReassessReport("proj", tc.res, true, nil, 2, 0)
			if !strings.Contains(out, tc.want) {
				t.Errorf("the report does not say %q:\n%s", tc.want, out)
			}
			if strings.Contains(out, tc.avoid) {
				t.Errorf("the report says %q over this run:\n%s", tc.avoid, out)
			}
			if !strings.Contains(out, "  unjudged    11223344 -> 88776655  [") {
				t.Errorf("the unjudged row lost its shape:\n%s", out)
			}
		})
	}
}

// TestReassessConsensusNote: the recommendation, and the three conditions that
// gate it.
//
// It is printed only where there is a deletion to gate — an ungated --apply that
// actually withdrew an edge — because the alternative is a note on every repair
// run, and a line an operator learns to skip stops being read when it matters.
// And it is suppressed for a run that already gated, which is the case where
// printing it would be actively wrong: someone who typed --consensus 5 must not
// be told to type --consensus 3.
func TestReassessConsensusNote(t *testing.T) {
	const want = "--consensus 3 --apply"
	for _, tc := range []struct {
		name      string
		apply     bool
		consensus int
		withdrawn int
		wantNote  bool
	}{
		{name: "ungated apply that withdrew", apply: true, consensus: 1, withdrawn: 1, wantNote: true},
		{name: "a dry run withdrew nothing", apply: false, consensus: 1, withdrawn: 0, wantNote: false},
		{name: "already gated", apply: true, consensus: 3, withdrawn: 1, wantNote: false},
		{name: "gated higher than the recommendation", apply: true, consensus: 5, withdrawn: 1, wantNote: false},
		{name: "nothing withdrawn", apply: true, consensus: 1, withdrawn: 0, wantNote: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note := reassessConsensusNote("proj", tc.apply, tc.consensus, tc.withdrawn)
			if got := strings.Contains(note, want); got != tc.wantNote {
				t.Errorf("note %q contains %q = %v, want %v", note, want, got, tc.wantNote)
			}
		})
	}
	// The note names the gate as a COMMAND, not as a slogan: the reader's next
	// decision is whether to re-run the harder version, and it also has to say
	// that the harder version costs more calls, because that is the trade. And it
	// names the REAL project — a command printed with a literal `<project>` is a
	// command the operator has to edit before it runs, and an edit is where a
	// repair goes to the wrong project.
	note := reassessConsensusNote("proj", true, 1, 2)
	for _, fragment := range []string{"ONE classifier verdict each", "3x the classify calls", "ghost supersede proj --reassess --consensus 3 --apply"} {
		if !strings.Contains(note, fragment) {
			t.Errorf("the note does not say %q:\n%s", fragment, note)
		}
	}
	if strings.Contains(note, "<project>") {
		t.Errorf("the note printed a project placeholder the operator has to edit:\n%s", note)
	}
	if got := reassessConsensusNote("my proj", true, 1, 1); !strings.Contains(got, `--project 'my proj'`) {
		t.Errorf("a project name holding a space was not quoted: %q", got)
	}
	// And it does not repeat a flag the renderer already carries: ReassessCommand
	// renders `--reassess --apply`, so appending --apply again printed
	// `--reassess --apply --consensus 3 --apply` — which PARSES, because both flags
	// are boolean, and which is why only the rendered text can catch it.
	if strings.Count(note, "--apply") != 1 {
		t.Errorf("the note quotes --apply %d times:\n%s", strings.Count(note, "--apply"), note)
	}
}

// TestVerdictWithdrawnCountsOnlyTheRowsAModelDecided is the note's population.
//
// A VETOED edge is settled from the two note bodies with no classify call at all,
// so counting it would recommend gating a pass that never asked a model — and
// the row directly above the note reads `[veto, no harness call]`, which would
// make the sentence under it a lie about the run that just happened. `Written` is
// the other filter: under --apply a row a concurrent pass took first is not a
// deletion this run made.
func TestVerdictWithdrawnCountsOnlyTheRowsAModelDecided(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rows     []supersede.WithdrawnEdge
		wantNote bool
	}{
		{name: "a classifier row that landed", rows: []supersede.WithdrawnEdge{
			{NewerID: "a", OlderID: "b", Reason: "neither", Written: true},
		}, wantNote: true},
		{name: "a vetoed row only", rows: []supersede.WithdrawnEdge{
			{NewerID: "a", OlderID: "b", Reason: "vetoed: a rule", Vetoed: true, Written: true},
		}, wantNote: false},
		{name: "a vetoed row beside a classifier row", rows: []supersede.WithdrawnEdge{
			{NewerID: "a", OlderID: "b", Reason: "vetoed: a rule", Vetoed: true, Written: true},
			{NewerID: "c", OlderID: "d", Reason: "neither", Written: true},
		}, wantNote: true},
		{name: "a dry-run row", rows: []supersede.WithdrawnEdge{
			{NewerID: "a", OlderID: "b", Reason: "neither"},
		}, wantNote: false},
		{name: "a row a concurrent pass took first", rows: []supersede.WithdrawnEdge{
			{NewerID: "a", OlderID: "b", Reason: "neither", Written: false},
		}, wantNote: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note := reassessConsensusNote("proj", true, 1, verdictWithdrawn(tc.rows))
			if got := strings.Contains(note, "--consensus 3 --apply"); got != tc.wantNote {
				t.Errorf("note %q printed = %v, want %v", note, got, tc.wantNote)
			}
		})
	}
}
