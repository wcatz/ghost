package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/supersede"
)

// The --consensus flag and the reflection.supersede_consensus key that feeds it
// to the automatic phase. Parsing and rendering are unit-tested here without
// os.Exit; the pass's behaviour is internal/supersede/consensus_test.go.

// TestParseSupersedeArgsConsensusDefaultsOff: the gate is OFF unless typed. A
// default of 3 on the flag would silently triple every hand-run pass, and an
// operator reading "1 classify call(s)" would have no way to know three happened.
func TestParseSupersedeArgsConsensusDefaultsOff(t *testing.T) {
	_, _, _, _, _, consensus, _, err := parseSupersedeArgs([]string{"myproj", "--apply"})
	if err != nil {
		t.Fatalf("parseSupersedeArgs: %v", err)
	}
	if consensus != 1 {
		t.Errorf("consensus = %d, want 1: the flag is off until it is typed", consensus)
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
		{"two is allowed", []string{"myproj", "--consensus", "2"}, 2},
		{"large value is honoured", []string{"myproj", "--consensus", "9"}, 9},
		{"before apply", []string{"myproj", "--consensus", "4", "--apply"}, 4},
		{"last wins", []string{"myproj", "--consensus", "2", "--consensus", "7"}, 7},
		{"project before flag", []string{"--project", "myproj", "--consensus", "3", "--apply"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, apply, _, _, consensus, _, err := parseSupersedeArgs(tc.args)
			if err != nil {
				t.Fatalf("parseSupersedeArgs(%v): %v", tc.args, err)
			}
			if consensus != tc.want {
				t.Errorf("consensus = %d, want %d", consensus, tc.want)
			}
			// The other half of the contract, in the same case: --consensus is a
			// gate on the WRITES, so it has to be usable with --apply and to leave
			// --apply alone.
			if wantApply := len(tc.args) > 0 && strings.Contains(strings.Join(tc.args, " "), "--apply"); apply != wantApply {
				t.Errorf("apply = %v, want %v: --consensus must not swallow --apply", apply, wantApply)
			}
		})
	}
}

// TestParseSupersedeArgsConsensusErrors: the refusals. A value below 2 is the
// load-bearing one — a gate of 1 runs the ordinary pass and writes whatever it
// proposed, which is the ungated pass wearing the flag of a safety control, and
// an operator who typed the flag meant to gate something. The two repair modes
// are refused because they judge edges already in the graph, where a split
// verdict would refuse the withdrawal the repair exists to perform.
func TestParseSupersedeArgsConsensusErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"one is not a gate", []string{"myproj", "--consensus", "1"}, "at least 2 passes"},
		{"zero", []string{"myproj", "--consensus", "0"}, "at least 2 passes"},
		{"negative", []string{"myproj", "--consensus", "-2"}, "at least 2 passes"},
		{"negative equals form", []string{"myproj", "--consensus=-1"}, "at least 2 passes"},
		{"not a number", []string{"myproj", "--consensus", "three"}, "whole number"},
		{"not a number equals form", []string{"myproj", "--consensus=many"}, "whole number"},
		{"missing value", []string{"myproj", "--consensus"}, `unknown flag "--consensus"`},
		{"with reassess", []string{"myproj", "--reassess", "--consensus", "3"}, "--consensus applies to the creation pass"},
		{"with withdraw", []string{"myproj", "--withdraw", "a1b2c3d4", "e5f6a7b8", "--consensus", "3"}, "--consensus applies to the creation pass"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, _, _, _, _, err := parseSupersedeArgs(tc.args)
			if err == nil {
				t.Fatalf("parseSupersedeArgs(%v) must fail", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must contain %q", err, tc.want)
			}
		})
	}
}

// TestSupersedePhaseConsensusIsReadOnlyUnderAutoSupersede is the config side,
// and the auto_supersede half is structural: the key is read inside the branch
// that emits the phase, so a project that never reaches the branch is never
// affected by the number in the file. The test asserts both halves — the phase
// carries the configured value when the phase exists, and the value is clamped
// into the range the flag accepts.
func TestSupersedePhaseConsensusIsReadOnlyUnderAutoSupersede(t *testing.T) {
	phase := func(n int) ([]string, bool) {
		cfg := &config.Config{}
		cfg.Reflection.AutoSupersede = true
		cfg.Reflection.SupersedeConsensus = n
		for _, p := range lifecyclePhases(cfg, "proj", true) {
			if p.name == "supersede" {
				return p.args, true
			}
		}
		return nil, false
	}

	t.Run("configured value reaches the phase", func(t *testing.T) {
		for _, n := range []int{2, 3, 5, 12} {
			args, ok := phase(n)
			if !ok {
				t.Fatalf("auto_supersede = true emitted no supersede phase")
			}
			project, _, _, _, _, consensus, _, err := parseSupersedeArgs(args[1:])
			if err != nil {
				t.Fatalf("the emitted argv does not parse: %v (argv %v)", err, args)
			}
			if project != "proj" {
				t.Errorf("project = %q, want %q", project, "proj")
			}
			if consensus != n {
				t.Errorf("consensus = %d, want %d from the config key", consensus, n)
			}
		}
	})

	t.Run("values below the minimum omit the flag entirely", func(t *testing.T) {
		// Not a refusal: a typo in a config file must not fail a lifecycle phase
		// hours later with a message about quorum arithmetic, and the safe reading
		// of a number the operator could not have meant as a smaller gate is the
		// pass they already had. The flag is OMITTED rather than emitted as 1,
		// because `--consensus 1` is a refused value — a phase printing it would
		// be asserting a gate it is not running, and the parser would exit 1 on
		// its own argv.
		for _, n := range []int{-1, 0, 1} {
			args, ok := phase(n)
			if !ok {
				t.Fatalf("auto_supersede = true emitted no supersede phase")
			}
			for _, a := range args {
				if a == "--consensus" {
					t.Errorf("supersede_consensus = %d emitted --consensus (argv %v); an ungated phase must not name a gate", n, args)
				}
			}
			if _, _, apply, _, _, consensus, _, err := parseSupersedeArgs(args[1:]); err != nil {
				t.Errorf("supersede_consensus = %d emitted argv that does not parse: %v", n, err)
			} else if !apply || consensus != 1 {
				t.Errorf("supersede_consensus = %d gave apply/consensus = %v/%d, want true/1: the phase still applies, ungated", n, apply, consensus)
			}
		}
	})

	t.Run("the key is inert when auto_supersede is off", func(t *testing.T) {
		// Same config, auto_supersede false: no phase, so nothing reads the key.
		// Asserted through the phase list rather than through the clamp function,
		// because the claim is about the whole path.
		cfg := &config.Config{}
		cfg.Reflection.AutoSupersede = false
		cfg.Reflection.SupersedeConsensus = 3
		for _, p := range lifecyclePhases(cfg, "proj", true) {
			if p.name == "supersede" {
				t.Fatalf("a supersede phase was emitted with auto_supersede off (argv %v)", p.args)
			}
		}
	})
}

// TestSupersedeReportNamesTheGateAndTheSplit: the two lines the gate adds. A
// gated run that wrote nothing and said nothing reads as a run that found nothing
// to do, which is the same defect every other refusal in this report is
// documented against.
func TestSupersedeReportNamesTheGateAndTheSplit(t *testing.T) {
	agreeing := supersede.Result{
		Candidates: 3, Consensus: 3, ConsensusPairsAsked: 9,
		Confirmed: 1, Created: 1,
	}
	out := supersedeReport("proj", agreeing, "linked", 3, 0)
	if !strings.Contains(out, "consensus 3") {
		t.Errorf("the summary does not name the gate:\n%s", out)
	}
	// The pairs number is what an operator checks the bill against, and it is the
	// multiplier APPLIED rather than a recount of the call log.
	if !strings.Contains(out, "9 pair(s) asked") {
		t.Errorf("the summary does not report the pairs asked:\n%s", out)
	}

	disputed := agreeing
	disputed.Created = 0
	disputed.Confirmed = 0
	disputed.NotAgreed = 2
	disputed.Disputed = []supersede.Disputed{
		{NewerID: "0123456789ABCDEF0123456789ABCDEF", OlderID: "FEDCBA9876543210FEDCBA9876543210",
			Tally: map[supersede.Relation]int{supersede.RelationSupersedes: 2, supersede.RelationNeither: 1}},
		{NewerID: "AAAABBBBCCCCDDDDEEEEFFFF00001111", OlderID: "11110000FFFFEEEEDDDDCCCCBBBBAAAA",
			Tally: map[supersede.Relation]int{supersede.RelationReversed: 1, supersede.RelationCauses: 1, supersede.Relation(""): 1}, Unreadable: 1},
	}
	out = supersedeReport("proj", disputed, "linked", 3, 0)
	if !strings.Contains(out, "2 pair(s) not agreed") {
		t.Errorf("the summary does not count the split pairs:\n%s", out)
	}
	// The ids, because a count an operator cannot act on is a number, not a report.
	// shortID keeps the first 8 RUNES, and a stored id is hex(randomblob(16))
	// which SQLite renders UPPER-case, so the abbreviations are upper-case here
	// for the same reason every other report on this command prints them that way.
	for _, want := range []string{"01234567", "FEDCBA98", "AAAABBBB", "11110000"} {
		if !strings.Contains(out, want) {
			t.Errorf("the not-agreed rows do not name %s:\n%s", want, out)
		}
	}
	// The tally, in descending order, and the unreadable pass under its own word
	// — a two-verdict split and a split with a garbled pass are different findings.
	if !strings.Contains(out, "2 supersedes, 1 neither") {
		t.Errorf("the first split row does not render its tally:\n%s", out)
	}
	if !strings.Contains(out, "1 unreadable") {
		t.Errorf("an unreadable pass is not named as such:\n%s", out)
	}
	// Nothing is claimed in a past tense, because nothing was written either way.
	if strings.Contains(out, "withdrew") || strings.Contains(out, "did not agree on") {
		t.Errorf("a not-agreed row implies a write happened:\n%s", out)
	}
}

// TestSupersedeReportOmitsTheGateLineWhenOff: an ungated pass's summary is
// unchanged, because a line about a gate nobody asked for is noise on the
// ordinary path — and the ordinary path is what most runs are.
func TestSupersedeReportOmitsTheGateLineWhenOff(t *testing.T) {
	out := supersedeReport("proj", supersede.Result{Candidates: 2, Confirmed: 1}, "would link", 1, 0)
	if strings.Contains(out, "consensus") {
		t.Errorf("an ungated pass's summary mentions the gate:\n%s", out)
	}
	if !strings.Contains(out, "would link") {
		t.Errorf("an ungated pass's summary lost its ordinary line:\n%s", out)
	}
}
