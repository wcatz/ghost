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
	//
	// "withdraws one ... on it" is SCOPED, not "withdraws only what all N passes
	// agree on": the note's population is already the classifier-decided edges
	// (`verdictWithdrawn` drops the vetoed ones, and the veto is settled before
	// the gate), so the unscoped phrasing would be false of every vetoed row the
	// report printed directly above this line. The whole phrase is asserted, not
	// the substring "all 3 passes agree on it", because a substring is matched by
	// the unscoped wording too and would pin nothing.
	note := reassessConsensusNote("proj", true, 1, 2)
	for _, fragment := range []string{"ONE classifier verdict each", "withdraws one only when all 3 passes agree on it", "3x the classify calls", "ghost supersede proj --reassess --consensus 3 --apply"} {
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

// usageFlagEntry returns one flag's block of `supersedeUsage`, collapsed to a
// single line so a phrase is matched as a SENTENCE and not as a wrapped layout:
// the claim is what is under test here, and re-wrapping the same words to a
// different column is not a change to it.
//
// The block runs from the line that opens the flag to the line that opens the
// next one, which is how a reader sees it: an entry's own text is whatever
// follows its name on the same column, and the next entry is the first thing
// at that column again.
func usageFlagEntry(t *testing.T, flag string) string {
	t.Helper()
	lines := strings.Split(supersedeUsage, "\n")
	open := "  " + flag
	var block []string
	collecting := false
	for _, line := range lines {
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
	// The walk above already stops at the first line that OPENS the next entry, so
	// no line past the first can open one — which is why there is no second pass
	// over the block to notice that it ran too long. The Fatalf above is the only
	// way out: a flag with no entry is a help that lost it, and an entry walked
	// past its end would be reported as the next flag's block instead.
	return strings.Join(strings.Fields(strings.Join(block, " ")), " ")
}

// usageLongHelp returns the PROSE of `supersedeUsage` — the paragraphs below the
// flag table, which is the third place the gate is claimed and the one an operator
// reads top to bottom.
//
// The flag table is what lies between the "Flags:" header and the first line that
// is not indented: an entry's name opens at two columns and its own text hangs
// under it at the same column, so indentation is the only thing that separates the
// two. Skipping by a "--" prefix instead would sweep the entries' own text into
// the prose, and a test that reads the flag table when it asked for the prose then
// passes for the wrong reason — so the helper asserts what it returned rather than
// trusting the walk.
func usageLongHelp(t *testing.T) string {
	t.Helper()
	var body []string
	inFlags := false
	for _, line := range strings.Split(supersedeUsage, "\n") {
		switch {
		case line == "Flags:":
			inFlags = true
		case inFlags && line == "":
			// A blank line inside the table separates entries; the one that ENDS it
			// is followed by an unindented line, which the next case collects.
		case inFlags && line[0] == ' ':
			// A flag entry, and its continuation text.
		case inFlags:
			body = append(body, line)
		}
	}
	out := strings.Join(strings.Fields(strings.Join(body, " ")), " ")
	if out == "" {
		t.Fatalf("usageLongHelp found no prose below the flag table:\n%s", supersedeUsage)
	}
	// Text that lives ONLY in the flag table. Every flag NAME is discussed down in
	// the prose — --consensus, --withdraw, --apply, --reassess all are — so a flag
	// name cannot be the evidence. These two sentences appear exactly once each in
	// the whole help, in the table, and their presence here would mean the walk
	// leaked it: every assertion below would then be reading the flag entries it
	// meant to be reading the long help for.
	for _, tableOnly := range []string{
		"N >= 2; default 1, which is no gate",
		"unambiguous prefix of one (8 or more characters)",
	} {
		if strings.Contains(out, tableOnly) {
			t.Fatalf("usageLongHelp returned the flag table, not the prose (%q leaked in):\n%s", tableOnly, out)
		}
	}
	return out
}

// TestSupersedeUsageNamesTheGateAndTheVetoItDoesNotCover: the help must not
// promise more than the pass does.
//
// `ReassessWith` settles every `VetoSupersede` pair into `settled` and never puts
// it in `open`, so a vetoed edge costs ZERO classify calls and is never put to the
// vote. `--reassess --consensus 3 --apply` therefore still deletes a live
// 'supersedes' edge on a veto, and the repo's own rules call that deletion
// permanent: the ordinary pass will not re-create it, because the veto is
// deterministic on the same two note bodies. #862's help read "an edge moves only
// when all N passes agree" and "act ONLY on what all N passes agreed", which is
// FALSE for that population — and the veto is the population whose error argument
// does not carry over at all.
//
// The three claims are pinned SEPARATELY, per surface, because a qualifier in one
// place and an absolute in another reads as the absolute: an operator who reads
// the --consensus flag entry is not holding the long help's --reassess paragraph.
func TestSupersedeUsageNamesTheGateAndTheVetoItDoesNotCover(t *testing.T) {
	// Why the exemption holds, as one phrase, required in all THREE surfaces. The
	// halves are load-bearing: the veto is settled BEFORE the gate and costs no
	// classify call, so there is nothing for N passes to vote on. A bare "the veto
	// is not gated" leaves the reader to assume the pass simply forgot to count it.
	const why = "settled before the gate, costs no classify call, and is never voted on"

	for _, tc := range []struct {
		name string
		text string
		// scope names the population the gate is claimed over, in that surface's
		// own words. A gate that does not say which verdicts it covers has not
		// said which it does not.
		scope string
	}{
		{
			name:  "--reassess entry",
			text:  usageFlagEntry(t, "--reassess"),
			scope: "gates the classifier's verdicts",
		},
		{
			name:  "--consensus entry",
			text:  usageFlagEntry(t, "--consensus"),
			scope: "gates the classifier's verdicts and not the deterministic veto",
		},
		{
			name:  "long help",
			text:  usageLongHelp(t),
			scope: "an edge the classifier decided moves only when all N passes agree",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.text, tc.scope) {
				t.Errorf("the help does not scope the gate to the classifier: %q is missing", tc.scope)
			}
			if !strings.Contains(tc.text, why) {
				t.Errorf("the help does not say why the veto is exempt: %q is missing", why)
			}
		})
	}

	// The absolutes are quoted EXACTLY, so this test fails if one comes back
	// rather than passing while some other wording is wrong. Each shipped in #866,
	// and each contradicts the report's own `[veto, no harness call]` rows — which
	// `verdictWithdrawn` deliberately EXCLUDES from the population the gate note
	// counts, so the run's own output already told the reader the veto is a
	// different population.
	for _, unwanted := range []string{
		"an edge moves only when all N passes agree, and a pair they split on keeps its edge and is reported",
		"act ONLY on what all N passes agreed",
		"an edge moves only when all N passes agree, and a pair they do not all agree on keeps its edge",
	} {
		if strings.Contains(strings.Join(strings.Fields(supersedeUsage), " "), unwanted) {
			t.Errorf("the help still claims the gate covers the veto: %q", unwanted)
		}
	}
}
