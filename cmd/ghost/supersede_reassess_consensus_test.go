package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/supersede"
)

// #862 at the CLI surface: --reassess accepts --consensus N, a gated repair
// reports the split instead of acting on one verdict, and an ungated
// `--reassess --apply` says it acted on one verdict per edge. The pass itself is
// tested in internal/supersede.

// TestReassessReportNamesTheGateAndTheSplit: the headline says the gate ran (an
// ungated "0 withdrawn" is the weaker claim, #845), a split is printed with its
// tally in descending vote order and the fact that the edge stands, and an ungated
// run prints no gate clause and no split block.
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
		"  not agreed  11223344 -> 88776655  [2 neither, 1 supersedes, and the edge stands]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("gated reassess report missing %q:\n%s", want, out)
		}
	}
	// No per-edge row claims a withdrawal, and there is no invitation to apply.
	for _, unwanted := range []string{"\n  would withdraw", "\n  withdrew ", "\n  already gone", "Re-run with --apply"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("a split-only report contains the withdrawal marker %q:\n%s", unwanted, out)
		}
	}

	ungated := supersedeReassessReport("proj", supersede.ReassessResult{Loaded: 1, Neither: 1, Consensus: 1, Withdrawn: 1}, true,
		[]supersede.WithdrawnEdge{{NewerID: "1122334455667788", OlderID: "8877665544332211", Reason: "neither", Written: true}}, 1, 0)
	if strings.Contains(ungated, "consensus") || strings.Contains(ungated, "not agreed") {
		t.Errorf("an ungated run prints a gate clause or split block it did not run:\n%s", ungated)
	}
}

// TestReassessReportNamesTheUnjudgedReasonTheModeActuallyHas: ungated, an
// unjudged pair is one no pass answered; under a gate it may have been answered
// by the first of three passes, so "no verdict" would blame the harness wrongly.
func TestReassessReportNamesTheUnjudgedReasonTheModeActuallyHas(t *testing.T) {
	unjudged := []supersede.UnjudgedPair{{NewerID: "1122334455667788", OlderID: "8877665544332211"}}
	for _, tc := range []struct {
		name        string
		consensus   int
		want, avoid string
	}{
		{"ungated", 1, "no verdict: the classify call failed", "no quorum"},
		{"gated", 3, "no quorum: fewer than the 3 passes answered the pair", "no verdict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := supersedeReassessReport("proj", supersede.ReassessResult{Loaded: 1, Consensus: tc.consensus, Unjudged: unjudged}, true, nil, 2, 0)
			if !strings.Contains(out, tc.want) || strings.Contains(out, tc.avoid) || !strings.Contains(out, "  unjudged    11223344 -> 88776655  [") {
				t.Errorf("the report does not say %q (and not %q):\n%s", tc.want, tc.avoid, out)
			}
		})
	}
}

// TestReassessConsensusNote: the recommendation is printed only for an ungated
// --apply that withdrew an edge, and never to a run that already gated.
func TestReassessConsensusNote(t *testing.T) {
	for _, tc := range []struct {
		name      string
		apply     bool
		consensus int
		withdrawn int
		wantNote  bool
	}{
		{"ungated apply that withdrew", true, 1, 1, true},
		{"a dry run withdrew nothing", false, 1, 0, false},
		{"already gated", true, 3, 1, false},
		{"gated higher than the recommendation", true, 5, 1, false},
		{"nothing withdrawn", true, 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note := reassessConsensusNote("proj", tc.apply, tc.consensus, tc.withdrawn)
			if got := strings.Contains(note, "--consensus 3 --apply"); got != tc.wantNote {
				t.Errorf("note %q printed = %v, want %v", note, got, tc.wantNote)
			}
		})
	}
	// The note is a command naming the real project, scoped to the classifier's
	// verdicts, with --apply exactly once (both flags are boolean, so a doubled one
	// still parses and only the rendered text can catch it).
	note := reassessConsensusNote("proj", true, 1, 2)
	for _, fragment := range []string{"ONE classifier verdict each", "withdraws one only when all 3 passes agree on it", "3x the classify calls", "ghost supersede proj --reassess --consensus 3 --apply"} {
		if !strings.Contains(note, fragment) {
			t.Errorf("the note does not say %q:\n%s", fragment, note)
		}
	}
	if strings.Contains(note, "<project>") || strings.Count(note, "--apply") != 1 {
		t.Errorf("the note has a placeholder or a doubled --apply:\n%s", note)
	}
	if got := reassessConsensusNote("my proj", true, 1, 1); !strings.Contains(got, `--project 'my proj'`) {
		t.Errorf("a project name holding a space was not quoted: %q", got)
	}
}

// TestVerdictWithdrawnCountsOnlyTheRowsAModelDecided: a vetoed edge costs no
// classify call, and a row a concurrent pass took first is not this run's
// deletion, so neither counts toward the gate recommendation.
func TestVerdictWithdrawnCountsOnlyTheRowsAModelDecided(t *testing.T) {
	classifier := supersede.WithdrawnEdge{NewerID: "c", OlderID: "d", Reason: "neither", Written: true}
	vetoed := supersede.WithdrawnEdge{NewerID: "a", OlderID: "b", Reason: "vetoed: a rule", Vetoed: true, Written: true}
	for _, tc := range []struct {
		name string
		rows []supersede.WithdrawnEdge
		want int
	}{
		{"a classifier row that landed", []supersede.WithdrawnEdge{classifier}, 1},
		{"a vetoed row only", []supersede.WithdrawnEdge{vetoed}, 0},
		{"a vetoed row beside a classifier row", []supersede.WithdrawnEdge{vetoed, classifier}, 1},
		{"a dry-run row", []supersede.WithdrawnEdge{{NewerID: "a", OlderID: "b", Reason: "neither"}}, 0},
	} {
		if got := verdictWithdrawn(tc.rows); got != tc.want {
			t.Errorf("%s: verdictWithdrawn = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// usageFlagEntry returns one flag's block of supersedeUsage collapsed to one line
// so a phrase is matched as a sentence, not as a wrapped layout.
func usageFlagEntry(t *testing.T, flag string) string {
	t.Helper()
	open := "  " + flag
	var block []string
	collecting := false
	for _, line := range strings.Split(supersedeUsage, "\n") {
		if strings.HasPrefix(line, "  --") {
			if collecting {
				break
			}
			collecting = line == open || strings.HasPrefix(line, open+" ")
		}
		if collecting {
			block = append(block, line)
		}
	}
	if len(block) == 0 {
		t.Fatalf("supersedeUsage has no entry for %s:\n%s", flag, supersedeUsage)
	}
	return strings.Join(strings.Fields(strings.Join(block, " ")), " ")
}

// usageLongHelp returns the prose below the flag table (the unindented lines
// after "Flags:"), collapsed to one line.
func usageLongHelp(t *testing.T) string {
	t.Helper()
	var body []string
	inFlags := false
	for _, line := range strings.Split(supersedeUsage, "\n") {
		switch {
		case line == "Flags:":
			inFlags = true
		case inFlags && (line == "" || line[0] == ' '):
		case inFlags:
			body = append(body, line)
		}
	}
	out := strings.Join(strings.Fields(strings.Join(body, " ")), " ")
	if out == "" || strings.Contains(out, "N >= 2; default 1, which is no gate") {
		t.Fatalf("usageLongHelp did not return the prose below the flag table:\n%s", out)
	}
	return out
}

// TestSupersedeUsageNamesTheGateAndTheVetoItDoesNotCover: a vetoed edge is
// settled before the gate with no classify call, so `--reassess --consensus 3
// --apply` still deletes it on the veto. Each surface (flag entries and long
// help) must scope the gate to the classifier and say why the veto is exempt, and
// the absolutes that shipped in #866 must not come back.
func TestSupersedeUsageNamesTheGateAndTheVetoItDoesNotCover(t *testing.T) {
	const why = "settled before the gate, costs no classify call, and is never voted on"
	for _, tc := range []struct{ name, text, scope string }{
		{"--reassess entry", usageFlagEntry(t, "--reassess"), "gates the classifier's verdicts"},
		{"--consensus entry", usageFlagEntry(t, "--consensus"), "gates the classifier's verdicts and not the deterministic veto"},
		{"long help", usageLongHelp(t), "an edge the classifier decided moves only when all N passes agree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.text, tc.scope) || !strings.Contains(tc.text, why) {
				t.Errorf("the help lacks the scope %q or the exemption reason %q", tc.scope, why)
			}
		})
	}
	flat := strings.Join(strings.Fields(supersedeUsage), " ")
	for _, unwanted := range []string{
		"an edge moves only when all N passes agree, and a pair they split on keeps its edge and is reported",
		"act ONLY on what all N passes agreed",
		"an edge moves only when all N passes agree, and a pair they do not all agree on keeps its edge",
	} {
		if strings.Contains(flat, unwanted) {
			t.Errorf("the help still claims the gate covers the veto: %q", unwanted)
		}
	}
}
