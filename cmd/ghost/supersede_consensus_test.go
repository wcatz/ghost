package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/supersede"
)

// The --consensus flag and the reflection.supersede_consensus key that feeds it
// to the automatic phase. The pass's behaviour is internal/supersede's own test.

// TestParseSupersedeArgsConsensusDefaultsOff: the gate is OFF unless typed.
func TestParseSupersedeArgsConsensusDefaultsOff(t *testing.T) {
	_, _, _, _, _, consensus, _, _, err := parseSupersedeArgs([]string{"myproj", "--apply"})
	if err != nil || consensus != 1 {
		t.Errorf("consensus = %d, %v, want 1: the flag is off until it is typed", consensus, err)
	}
}

func TestParseSupersedeArgsConsensus(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"separate value", []string{"myproj", "--apply", "--consensus", "3"}, 3},
		{"equals value", []string{"myproj", "--apply", "--consensus=5"}, 5},
		{"last wins", []string{"myproj", "--consensus", "2", "--consensus", "7"}, 7},
		{"project before flag", []string{"--project", "myproj", "--consensus", "3", "--apply"}, 3},
		// #862: the gate reaches the repair.
		{"with reassess and apply", []string{"myproj", "--reassess", "--consensus", "3", "--apply"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, apply, _, _, consensus, _, _, err := parseSupersedeArgs(tc.args)
			if err != nil {
				t.Fatalf("parseSupersedeArgs(%v): %v", tc.args, err)
			}
			if wantApply := strings.Contains(strings.Join(tc.args, " "), "--apply"); consensus != tc.want || apply != wantApply {
				t.Errorf("consensus/apply = %d/%v, want %d/%v", consensus, apply, tc.want, wantApply)
			}
		})
	}
}

// TestParseSupersedeArgsConsensusErrors: a gate below 2 is the ungated pass
// wearing a safety flag; --withdraw judges nothing, so it refuses the flag.
func TestParseSupersedeArgsConsensusErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"one is not a gate", []string{"myproj", "--consensus", "1"}, "at least 2 passes"},
		{"negative equals form", []string{"myproj", "--consensus=-1"}, "at least 2 passes"},
		{"not a number", []string{"myproj", "--consensus", "three"}, "whole number"},
		{"missing value", []string{"myproj", "--consensus"}, `unknown flag "--consensus"`},
		{"with withdraw", []string{"myproj", "--withdraw", "a1b2c3d4", "e5f6a7b8", "--consensus", "3"}, "--withdraw judges nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, _, _, _, _, err := parseSupersedeArgs(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parseSupersedeArgs(%v) error = %v, want one containing %q", tc.args, err, tc.want)
			}
		})
	}
}

// TestSupersedePhaseConsensusIsReadOnlyUnderAutoSupersede: the configured value
// reaches the phase, is omitted (not emitted as a refused 1) when below the
// minimum, is inert with auto_supersede off, and never scales the phase deadline
// (a hang detector).
func TestSupersedePhaseConsensusIsReadOnlyUnderAutoSupersede(t *testing.T) {
	phase := func(auto bool, n int) *lifecyclePhase {
		cfg := &config.Config{}
		cfg.Reflection.AutoSupersede = auto
		cfg.Reflection.SupersedeConsensus = n
		cfg.Reflection.LifecycleTimeoutMinutes = 90
		for _, p := range lifecyclePhases(cfg, "proj", true) {
			if p.name == "supersede" {
				p := p
				return &p
			}
		}
		return nil
	}
	for _, n := range []int{2, 3, 12, -1, 0, 1} {
		p := phase(true, n)
		if p == nil {
			t.Fatalf("auto_supersede = true emitted no supersede phase")
		}
		project, _, apply, _, _, consensus, _, _, err := parseSupersedeArgs(p.args[1:])
		if err != nil {
			t.Fatalf("the emitted argv does not parse: %v (argv %v)", err, p.args)
		}
		want := n
		if n < supersede.MinConsensus {
			want = 1
			for _, a := range p.args {
				if a == "--consensus" {
					t.Errorf("supersede_consensus = %d emitted --consensus (argv %v)", n, p.args)
				}
			}
		}
		if project != "proj" || consensus != want || !apply {
			t.Errorf("n=%d: project/consensus/apply = %q/%d/%v, want proj/%d/true", n, project, consensus, apply, want)
		}
		if p.timeout != 90*time.Minute {
			t.Errorf("supersede_consensus = %d gave a %s deadline, want the configured 90m", n, p.timeout)
		}
	}
	if p := phase(false, 3); p != nil {
		t.Errorf("a supersede phase was emitted with auto_supersede off (argv %v)", p.args)
	}
}

func disputedPair(newer, older string, tally map[supersede.Relation]int, unreadable int) supersede.Disputed {
	return supersede.Disputed{NewerID: newer, OlderID: older, Tally: tally, Unreadable: unreadable}
}

// TestSupersedeReportNamesTheGateAndTheSplit: a gated run that wrote nothing and
// said nothing reads as a run that found nothing to do.
func TestSupersedeReportNamesTheGateAndTheSplit(t *testing.T) {
	agreeing := supersede.Result{Candidates: 3, Consensus: 3, ConsensusPairsAsked: 9, Confirmed: 1, Created: 1}
	out := supersedeReport("proj", agreeing, "linked", true, 3, 0)
	for _, want := range []string{"consensus 3", "9 pair(s) asked"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary does not say %q:\n%s", want, out)
		}
	}

	disputed := agreeing
	disputed.Created, disputed.Confirmed, disputed.NotAgreed = 0, 0, 2
	disputed.Disputed = []supersede.Disputed{
		disputedPair("0123456789ABCDEF0123456789ABCDEF", "FEDCBA9876543210FEDCBA9876543210",
			map[supersede.Relation]int{supersede.RelationSupersedes: 2, supersede.RelationNeither: 1}, 0),
		disputedPair("AAAABBBBCCCCDDDDEEEEFFFF00001111", "11110000FFFFEEEEDDDDCCCCBBBBAAAA",
			map[supersede.Relation]int{supersede.RelationReversed: 1, supersede.RelationCauses: 1, supersede.Relation(""): 1}, 1),
	}
	out = supersedeReport("proj", disputed, "linked", true, 3, 0)
	for _, want := range []string{"2 pair(s) not agreed", "01234567", "FEDCBA98", "AAAABBBB", "11110000", "2 supersedes, 1 neither", "1 unreadable"} {
		if !strings.Contains(out, want) {
			t.Errorf("the not-agreed rows do not say %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "withdrew") || strings.Contains(out, "did not agree on") {
		t.Errorf("a not-agreed row implies a write happened:\n%s", out)
	}
}

// TestTheNotAgreedTallyLeadsWithTheMajority: the tally sorts by count (the
// majority leads) and a tie keeps the fixed verdict order, so the line is
// deterministic. Each case is asserted as a whole line.
func TestTheNotAgreedTallyLeadsWithTheMajority(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tally map[supersede.Relation]int
		want  string
	}{
		{"a majority of NEITHER is not led by a minority of supersedes",
			map[supersede.Relation]int{supersede.RelationSupersedes: 1, supersede.RelationNeither: 2},
			"[not agreed: 2 neither, 1 supersedes]"},
		{"a three-way tie keeps the fixed verdict order",
			map[supersede.Relation]int{supersede.RelationSupersedes: 1, supersede.RelationCauses: 1, supersede.RelationNeither: 1},
			"[not agreed: 1 supersedes, 1 causes, 1 neither]"},
		{"a tie including unreadable is ordered by the fixed list",
			map[supersede.Relation]int{supersede.RelationReversed: 1, supersede.RelationCauses: 1, "": 1},
			"[not agreed: 1 causes, 1 reversed, 1 unreadable]"},
		{"a majority of unreadable leads",
			map[supersede.Relation]int{supersede.RelationSupersedes: 1, "": 2},
			"[not agreed: 2 unreadable, 1 supersedes]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := supersedeReport("proj", supersede.Result{
				Candidates: 1, Consensus: 3, ConsensusPairsAsked: 3, NotAgreed: 1,
				Disputed: []supersede.Disputed{disputedPair("0123456789ABCDEF0123456789ABCDEF", "FEDCBA9876543210FEDCBA9876543210", tc.tally, 0)},
			}, "would link", false, 3, 0)
			if !strings.Contains(out, tc.want) {
				t.Errorf("tally %v did not render %q\n\nrendered:\n%s", tc.tally, tc.want, out)
			}
		})
	}
}

// TestSupersedeNotAgreedAdviceNamesRemediesThatWork: raising --consensus makes
// unanimity harder, so the report must not advise it; a re-run and dropping the
// flag are the real remedies.
func TestSupersedeNotAgreedAdviceNamesRemediesThatWork(t *testing.T) {
	out := supersedeReport("proj", supersede.Result{
		Candidates: 3, Consensus: 3, ConsensusPairsAsked: 9, NotAgreed: 1,
		Disputed: []supersede.Disputed{disputedPair("0123456789ABCDEF0123456789ABCDEF", "FEDCBA9876543210FEDCBA9876543210",
			map[supersede.Relation]int{supersede.RelationSupersedes: 2, supersede.RelationNeither: 1}, 0)},
	}, "would link", false, 3, 0)
	if strings.Contains(out, "raise --consensus") {
		t.Errorf("the not-agreed line advises raising --consensus:\n%s", out)
	}
	for _, want := range []string{"raising it makes unanimity harder", "re-run to ask again", "drop --consensus", "1 pair(s) not agreed"} {
		if !strings.Contains(out, want) {
			t.Errorf("the not-agreed line does not say %q:\n%s", want, out)
		}
	}
}

// TestSupersedeGateLinePrintsEvenWithNothingToAsk: a gated run with nothing to
// ask still says so, and an ungated summary carries no gate line at all.
func TestSupersedeGateLinePrintsEvenWithNothingToAsk(t *testing.T) {
	for name, res := range map[string]supersede.Result{
		"no candidates": {Consensus: 3},
		"all vetoed":    {Consensus: 3, Candidates: 5, Vetoed: 5},
		"ungated":       {Candidates: 2, Confirmed: 1},
	} {
		t.Run(name, func(t *testing.T) {
			out := supersedeReport("proj", res, "would link", false, 0, 0)
			wantGate := res.Consensus > 1
			if gotGate := strings.Contains(out, "consensus"); gotGate != wantGate {
				t.Errorf("the gate line is present=%v, want %v:\n%s", gotGate, wantGate, out)
			}
			if wantGate && !strings.Contains(out, "0 pair(s) asked") {
				t.Errorf("a gated run with nothing to ask must say so:\n%s", out)
			}
		})
	}
}

// TestSupersedeConsensusDryRunHintIsSilentUnderApply: the hint describes a run
// that has not happened, so an --apply run never prints it.
func TestSupersedeConsensusDryRunHintIsSilentUnderApply(t *testing.T) {
	applied := supersede.Result{Consensus: 3, Candidates: 4, NotAgreed: 2, Unclassified: 2, Reclassified: 2, ReclassifiedNoWrite: 2}
	if applied.WouldWriteLinks() {
		t.Fatalf("the fixture must be a run that wrote nothing")
	}
	if out := supersedeConsensusDryRunHint(true, applied); out != "" {
		t.Errorf("an --apply run printed the dry-run hint:\n%s", out)
	}
	if out := supersedeConsensusDryRunHint(false, applied); out == "" {
		t.Error("a dry run over the same result printed nothing")
	}
}

// TestTheClampIsAnnouncedRatherThanClaimedVisible: a supersede_consensus below
// the minimum runs the phase ungated, so lifecycle announces the clamp itself.
func TestTheClampIsAnnouncedRatherThanClaimedVisible(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want bool
	}{{1, true}, {-3, true}, {supersede.MinConsensus, false}, {5, false}} {
		out := supersedeConsensusClampNotice("proj", tc.n)
		if got := out != ""; got != tc.want {
			t.Fatalf("n=%d: notice printed=%v, want %v:\n%s", tc.n, got, tc.want, out)
		}
		for _, want := range []string{"supersede_consensus", "UNGATED", "proj", fmt.Sprintf("minimum of %d", supersede.MinConsensus)} {
			if tc.want && !strings.Contains(out, want) {
				t.Errorf("n=%d: the notice does not name %q:\n%s", tc.n, want, out)
			}
		}
	}
}

// TestSupersedeConsensusDryRunHintNamesOnlyTheCauseItDescribes: the hint is keyed
// on NotAgreed > 0 alone, since a gated run writes nothing for three unrelated
// reasons and the other two have their own lines.
func TestSupersedeConsensusDryRunHintNamesOnlyTheCauseItDescribes(t *testing.T) {
	for name, tc := range map[string]struct {
		res       supersede.Result
		wantPrint bool
	}{
		"the gate split the pairs":      {supersede.Result{Consensus: 3, Candidates: 4, NotAgreed: 2}, true},
		"every candidate was vetoed":    {supersede.Result{Consensus: 3, Candidates: 5, Vetoed: 5}, false},
		"every pass was unreadable":     {supersede.Result{Consensus: 3, Candidates: 3, Unclassified: 3}, false},
		"the gate agreed on some pairs": {supersede.Result{Consensus: 3, Candidates: 4, Confirmed: 1, NotAgreed: 1}, false},
		"an ungated run":                {supersede.Result{Candidates: 4, NotAgreed: 1}, false},
	} {
		t.Run(name, func(t *testing.T) {
			out := supersedeConsensusDryRunHint(false, tc.res)
			if got := out != ""; got != tc.wantPrint {
				t.Fatalf("hint printed=%v, want %v:\n%s", got, tc.wantPrint, out)
			}
			for _, want := range []string{"the model is not deterministic", "drop --consensus", "Raising it makes unanimity harder", "3 consensus pass(es)"} {
				if tc.wantPrint && !strings.Contains(out, want) {
					t.Errorf("the hint does not say %q:\n%s", want, out)
				}
			}
		})
	}
}
