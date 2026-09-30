package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

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
	_, _, _, _, _, consensus, _, _, err := parseSupersedeArgs([]string{"myproj", "--apply"})
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
			_, _, apply, _, _, consensus, _, _, err := parseSupersedeArgs(tc.args)
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
			_, _, _, _, _, _, _, _, err := parseSupersedeArgs(tc.args)
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
			project, _, _, _, _, consensus, _, _, err := parseSupersedeArgs(args[1:])
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
			if _, _, apply, _, _, consensus, _, _, err := parseSupersedeArgs(args[1:]); err != nil {
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

	t.Run("the phase keeps the SAME deadline whatever the gate costs", func(t *testing.T) {
		// `lifecycle_timeout_minutes` is a HANG DETECTOR: it exists so a phase
		// that will not finish stops holding the per-project PID claim instead of
		// wedging maintenance until someone kills it by hand. Multiplying it by
		// the work factor makes it N times longer to notice a model that never
		// answers, which is how a hang detector stops being one — and there is no
		// principled factor to multiply by anyway, since per-pass cost depends on
		// the corpus and the model. So the bound stays, the cost lands on the
		// operator, and it is documented in config.example.yaml,
		// docs/configuration.md and docs/cli.md.
		//
		// This case exists so that scaling the bound later is a DELIBERATE change
		// someone makes against this, rather than a quiet "improvement" that turns
		// the guard off.
		want := 90 * time.Minute
		for _, n := range []int{1, 3, 7} {
			cfg := &config.Config{}
			cfg.Reflection.AutoSupersede = true
			cfg.Reflection.SupersedeConsensus = n
			cfg.Reflection.LifecycleTimeoutMinutes = 90
			for _, p := range lifecyclePhases(cfg, "proj", true) {
				if p.name != "supersede" {
					continue
				}
				if p.timeout != want {
					t.Errorf("supersede_consensus = %d gave the phase a %s deadline, want the configured %s: the bound is a hang detector and must not scale with the work factor", n, p.timeout, want)
				}
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
	out := supersedeReport("proj", agreeing, "linked", true, 3, 0)
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
	out = supersedeReport("proj", disputed, "linked", true, 3, 0)
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

// TestTheNotAgreedTallyLeadsWithTheMajority pins the ORDER of a not-agreed
// tally line, which was a claim in the code's own comment with nothing behind it.
//
// The comment above the loop said "descending vote order" while the loop walked a
// FIXED verdict order, so the two contradicted each other — and the consequence
// was not cosmetic. A fixed order prints `1 supersedes, 2 neither` for a split
// whose majority is NEITHER, leading with the minority, and this gate's entire
// finding is that the verdict a reader takes away is the one most passes gave.
// The doc examples had drifted with it: both docs/cli.md and
// docs/architecture.md showed `1 reversed, 1 causes, 1 unreadable`, an order the
// renderer could not produce at all.
//
// So the renderer now sorts by count, and this test holds the whole contract:
// the majority leads, the minority follows, and a tie keeps the FIXED verdict
// order so the line is still deterministic. The tie case matters on its own — an
// unstable classifier is exactly what a 1-1-1 split reports, and a sort that
// ordered equal counts arbitrarily would make that line unreproducible between
// runs, which is the opposite of what a report about instability should do.
//
// Every case is asserted as a whole line, not as a substring, so a renderer that
// emitted the right words in the wrong order fails.
func TestTheNotAgreedTallyLeadsWithTheMajority(t *testing.T) {
	// A pair for the line, and a run of one dispute, so the only thing varying
	// between cases is the tally.
	const (
		newer = "0123456789ABCDEF0123456789ABCDEF"
		older = "FEDCBA9876543210FEDCBA9876543210"
	)
	for _, tc := range []struct {
		name  string
		tally map[supersede.Relation]int
		want  string
		why   string
	}{
		{
			name:  "a majority of NEITHER must not be led by a minority of supersedes",
			tally: map[supersede.Relation]int{supersede.RelationSupersedes: 1, supersede.RelationNeither: 2},
			want:  "[not agreed: 2 neither, 1 supersedes]",
			why:   "the fixed verdict order printed this as `1 supersedes, 2 neither`, leading with the minority — and the leading verdict is the one a reader keeps",
		},
		{
			name:  "a majority of causes leads over a minority of reversed",
			tally: map[supersede.Relation]int{supersede.RelationCauses: 2, supersede.RelationReversed: 1},
			want:  "[not agreed: 2 causes, 1 reversed]",
			why:   "reversed comes last in the fixed order, so this is the case where the bug was most visible",
		},
		{
			name:  "a wide split reads 3 then 1",
			tally: map[supersede.Relation]int{supersede.RelationSupersedes: 3, supersede.RelationReversed: 1},
			want:  "[not agreed: 3 supersedes, 1 reversed]",
		},
		{
			name:  "a three-way tie keeps the FIXED verdict order",
			tally: map[supersede.Relation]int{supersede.RelationSupersedes: 1, supersede.RelationCauses: 1, supersede.RelationNeither: 1},
			want:  "[not agreed: 1 supersedes, 1 causes, 1 neither]",
			why:   "equal counts, so the line must still be reproducible run to run; an unstable classifier is exactly what this line reports, and a nondeterministic order would make it unreproducible",
		},
		{
			name:  "a tie INCLUDING unreadable is ordered by the fixed list, not by the map",
			tally: map[supersede.Relation]int{supersede.RelationReversed: 1, supersede.RelationCauses: 1, "": 1},
			want:  "[not agreed: 1 causes, 1 reversed, 1 unreadable]",
			why:   "this is the exact example both docs showed as `1 reversed, 1 causes, 1 unreadable`, an order the renderer could never produce; unreadable is last in the fixed list",
		},
		{
			name:  "a majority of unreadable leads, because unreadable is a verdict here",
			tally: map[supersede.Relation]int{supersede.RelationSupersedes: 1, "": 2},
			want:  "[not agreed: 2 unreadable, 1 supersedes]",
			why:   "unreadable is counted, not hidden, so it leads on a count like any other — a harness fault is the more actionable finding",
		},
		{
			name:  "a single verdict is unaffected by the sort",
			tally: map[supersede.Relation]int{supersede.RelationSupersedes: 3},
			want:  "[not agreed: 3 supersedes]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := supersedeReport("proj", supersede.Result{
				Candidates: 1, Consensus: 3, ConsensusPairsAsked: 3, NotAgreed: 1,
				Disputed: []supersede.Disputed{{NewerID: newer, OlderID: older, Tally: tc.tally}},
			}, "would link", false, 3, 0)
			if !strings.Contains(out, tc.want) {
				t.Errorf("tally %v did not render %q%s\n\nrendered:\n%s", tc.tally, tc.want, reasonSuffixSupersede(tc.why), out)
			}
		})
	}
}

// TestTheDocumentedNotAgreedExamplesAreOrderableByTheRenderer closes the gap a
// mutation found: the RENDERER's order was pinned but the two DOC EXAMPLES were
// not, and both had drifted to an order no tally can produce — docs/cli.md and
// docs/architecture.md showed `1 reversed, 1 causes, 1 unreadable`, while the
// loop walked supersedes, causes, neither, reversed, unreadable. A reader copying
// either example into a bug report was quoting output Ghost does not produce.
//
// So the check is not "the docs mention the right words" but "the docs contain a
// line the renderer emits for the tally they quote". The tally is declared once
// here, rendered through the real supersedeReport, and both pages are then
// required to contain the rendered line. A doc that reorders the example fails; a
// renderer whose order changes fails the other test in this file; and the two
// cannot drift apart without one of them going red.
func TestTheDocumentedNotAgreedExamplesAreOrderableByTheRenderer(t *testing.T) {
	// The tally both pages quote, and it is deliberately a three-way tie so the
	// fixed order is the thing under test: a tie is the only case where the
	// example could be wrong without any majority being misreported.
	const (
		// The TALLY FRAGMENT, not the bracketed line. docs/cli.md quotes the
		// whole line inside a sample block and docs/architecture.md quotes the
		// fragment inline in prose, so the fragment is the one string both can
		// carry, and its ORDER is the whole finding.
		fragment = "1 causes, 1 reversed, 1 unreadable"
		// The order no tally can produce, which is what both pages had drifted
		// to: the fixed list emits causes before reversed.
		impossible = "1 reversed, 1 causes, 1 unreadable"
	)
	out := supersedeReport("proj", supersede.Result{
		Candidates: 1, Consensus: 3, ConsensusPairsAsked: 3, NotAgreed: 1,
		Disputed: []supersede.Disputed{{
			NewerID: "0123456789ABCDEF0123456789ABCDEF", OlderID: "FEDCBA9876543210FEDCBA9876543210",
			Tally: map[supersede.Relation]int{supersede.RelationCauses: 1, supersede.RelationReversed: 1, "": 1},
		}},
	}, "would link", false, 3, 0)
	if !strings.Contains(out, fragment) {
		t.Fatalf("the renderer does not emit the tally the docs quote (%s):\n%s", fragment, out)
	}
	if strings.Contains(out, impossible) {
		t.Fatalf("the renderer emits an order no tally can produce, which is what the docs had drifted to:\n%s", out)
	}
	for _, page := range []string{"../../docs/cli.md", "../../docs/architecture.md"} {
		text := readDocPage(t, page)
		if !strings.Contains(text, fragment) {
			t.Errorf("%s does not carry the not-agreed tally the renderer emits (%s); a doc example in an order Ghost cannot print sends a reader looking for a bug that is not there", page, fragment)
		}
		if strings.Contains(text, impossible) {
			t.Errorf("%s still shows the not-agreed tally as %q, an order the renderer cannot produce, so the example describes output Ghost does not emit", page, impossible)
		}
	}
}

// readDocPage reads one documentation page, and names the page it could not read
// rather than only that a read failed.
func readDocPage(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// reasonSuffixSupersede puts a case's reasoning on the failure line, because
// these are the cases whose expected output looks arbitrary until you know which
// reading of the split it is defending.
func reasonSuffixSupersede(why string) string {
	if why == "" {
		return ""
	}
	return " (" + why + ")"
}

// TestSupersedeNotAgreedAdviceNamesRemediesThatWork: the report must not tell an
// operator to raise --consensus, because raising N makes unanimity STRICTLY
// HARDER — a pair that split 2-1 at N=3 has to satisfy one more pass at N=4, so
// the advice sends them into a run that costs more and suppresses more.
//
// The two remedies that are real are a re-run (fresh passes may land on the same
// answer) and dropping the flag (write what the first pass said). The dry-run
// hint further down the same command says the same thing, and two lines on one
// page must not disagree — which is what makes this a test rather than a
// wording preference.
func TestSupersedeNotAgreedAdviceNamesRemediesThatWork(t *testing.T) {
	out := supersedeReport("proj", supersede.Result{
		Candidates: 3, Consensus: 3, ConsensusPairsAsked: 9, NotAgreed: 1,
		Disputed: []supersede.Disputed{
			{NewerID: "0123456789ABCDEF0123456789ABCDEF", OlderID: "FEDCBA9876543210FEDCBA9876543210",
				Tally: map[supersede.Relation]int{supersede.RelationSupersedes: 2, supersede.RelationNeither: 1}},
		},
	}, "would link", false, 3, 0)
	if strings.Contains(out, "raise --consensus") {
		t.Errorf("the not-agreed line advises raising --consensus, which makes unanimity harder:\n%s", out)
	}
	// The correction is stated, not just omitted, because an operator who has
	// read the issue's "run it three times" advice will reach for the number.
	if !strings.Contains(out, "raising it makes unanimity harder") {
		t.Errorf("the not-agreed line does not say why raising the count is not the remedy:\n%s", out)
	}
	for _, want := range []string{"re-run to ask again", "drop --consensus"} {
		if !strings.Contains(out, want) {
			t.Errorf("the not-agreed line does not offer %q, which is one of the two remedies that work:\n%s", want, out)
		}
	}
	// The count and the rows are the same number by construction, and an example
	// that disagrees with itself teaches a report shape the code cannot produce.
	if !strings.Contains(out, "1 pair(s) not agreed") {
		t.Errorf("the not-agreed count is wrong for one record:\n%s", out)
	}
}

// TestSupersedeGateLinePrintsEvenWithNothingToAsk: a gated run that found no
// candidate pairs, or had every one vetoed, returns before the classify loop.
// Consensus is set where the result is created for exactly that reason, and the
// gate line is gated on it — so a gated run on a quiet project is never
// indistinguishable from an ungated one.
func TestSupersedeGateLinePrintsEvenWithNothingToAsk(t *testing.T) {
	for name, res := range map[string]supersede.Result{
		"no candidates":  {Consensus: 3, ConsensusPairsAsked: 0},
		"all vetoed":     {Consensus: 3, ConsensusPairsAsked: 0, Candidates: 5, Vetoed: 5},
		"all cached":     {Consensus: 3, ConsensusPairsAsked: 0, Skipped: 4},
		"nothing at all": {},
	} {
		t.Run(name, func(t *testing.T) {
			out := supersedeReport("proj", res, "would link", false, 0, 0)
			wantGate := res.Consensus > 1
			gotGate := strings.Contains(out, "consensus ")
			if gotGate != wantGate {
				t.Errorf("the gate line is present=%v, want %v for Consensus=%d:\n%s", gotGate, wantGate, res.Consensus, out)
			}
			if wantGate && !strings.Contains(out, "0 pair(s) asked") {
				t.Errorf("a gated run with nothing to ask must say so:\n%s", out)
			}
		})
	}
}

// TestSupersedeConsensusDryRunHintIsSilentUnderApply: the block is about a run
// that has NOT happened, so an --apply run must never print it — and an --apply
// run whose unanimous verdicts were all `neither` or `reversed` satisfies every
// other condition (it wrote no links, it had splits, it is gated). Without the
// `apply` guard the summary says `linked` and is then followed by "so --apply
// would write nothing for them", which is a prediction about a run that has
// already run and advice to re-apply a flag for an apply that already happened.
func TestSupersedeConsensusDryRunHintIsSilentUnderApply(t *testing.T) {
	// The shape that satisfies every condition except the one under test: a
	// gated run whose only unanimous verdicts denied the pairs.
	applied := supersede.Result{
		Consensus: 3, Candidates: 4, NotAgreed: 2, Unclassified: 2,
		Reclassified: 2, ReclassifiedNoWrite: 2,
	}
	if applied.WouldWriteLinks() {
		t.Fatalf("the fixture must be a run that wrote nothing, or it is not testing the guard")
	}
	if out := supersedeConsensusDryRunHint(true, applied); out != "" {
		t.Errorf("an --apply run printed the dry-run hint:\n%s", out)
	}
	// And the same result in a dry run DOES print it, so the guard is on the mode
	// and not on the outcome.
	if out := supersedeConsensusDryRunHint(false, applied); out == "" {
		t.Error("a dry run over the same result printed nothing: the guard must be the mode, not the outcome")
	}
}

// TestTheClampIsAnnouncedRatherThanClaimedVisible: a
// reflection.supersede_consensus below the minimum runs the phase UNGATED, and
// the phase's own report cannot say so — an ungated pass prints no gate line, so
// a clamped phase's stdout is byte-identical to one with no key at all.
//
// So the clamp has to be announced somewhere, and the honest place is
// `ghost lifecycle`'s own stderr beside the reflect-skip notice: on the
// unattended path that is the phase tail and the lifecycle.log beneath it, and
// it is the same surface the function already uses to report a precondition it
// is about to work around. The text is held through a formatter rather than
// through the live run, because runLifecycle spawns processes; what this must not
// become is a claim in a comment that nothing prints.
func TestTheClampIsAnnouncedRatherThanClaimedVisible(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    int
		want bool
	}{
		{"below the minimum", 1, true},
		{"zero", 0, true},
		{"negative", -3, true},
		{"at the minimum", supersede.MinConsensus, false},
		{"above the minimum", 5, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := supersedeConsensusClampNotice("proj", tc.n)
			if got := out != ""; got != tc.want {
				t.Fatalf("notice printed=%v, want %v for supersede_consensus=%d:\n%s", got, tc.want, tc.n, out)
			}
			if !tc.want {
				return
			}
			for _, want := range []string{"supersede_consensus", "UNGATED", "proj"} {
				if !strings.Contains(out, want) {
					t.Errorf("the notice does not name %q, and a clamp without it is not actionable:\n%s", want, out)
				}
			}
			// The boundary is named, so a reader learns the minimum rather than
			// inferring that the number was arbitrary.
			if !strings.Contains(out, fmt.Sprintf("minimum of %d", supersede.MinConsensus)) {
				t.Errorf("the notice does not state the minimum:\n%s", out)
			}
		})
	}
}

// TestSupersedeConsensusDryRunHintNamesOnlyTheCauseItDescribes: the hint is
// gated on NotAgreed > 0 and not on "the run wrote nothing", because a gated run
// writes nothing for three unrelated reasons and only one of them is a re-run's
// problem. Each of the other two already has its own line on the page, so the
// hint claiming them would tell an operator to re-run a model that was never
// asked, or to blame instability for a reply nothing could parse.
func TestSupersedeConsensusDryRunHintNamesOnlyTheCauseItDescribes(t *testing.T) {
	const split = "the model is not deterministic"
	for name, tc := range map[string]struct {
		res       supersede.Result
		wantPrint bool
		why       string
	}{
		"the gate split the pairs": {
			res:       supersede.Result{Consensus: 3, Candidates: 4, NotAgreed: 2},
			wantPrint: true,
			why:       "a split is this block's reason to exist",
		},
		"every candidate was vetoed": {
			res:       supersede.Result{Consensus: 3, Candidates: 5, Vetoed: 5},
			wantPrint: false,
			why:       "nothing was asked, so nothing could disagree; the veto line already says so",
		},
		"every pass was unreadable": {
			res:       supersede.Result{Consensus: 3, Candidates: 3, Unclassified: 3},
			wantPrint: false,
			why:       "a parse failure is a prompt or harness problem, not instability",
		},
		"nothing was asked at all": {
			res:       supersede.Result{Consensus: 3},
			wantPrint: false,
			why:       "a quiet project is not a gated run that found nothing",
		},
		"the gate agreed on some pairs": {
			res:       supersede.Result{Consensus: 3, Candidates: 4, Confirmed: 1, NotAgreed: 1},
			wantPrint: false,
			why:       "--apply IS worth running here, and the ordinary 'Re-run with --apply to write these links' hint already says so; a second block about the split would bury it",
		},
		"an ungated run": {
			res:       supersede.Result{Candidates: 4, NotAgreed: 1},
			wantPrint: false,
			why:       "there is no gate to explain",
		},
	} {
		t.Run(name, func(t *testing.T) {
			out := supersedeConsensusDryRunHint(false, tc.res)
			if got := out != ""; got != tc.wantPrint {
				t.Fatalf("hint printed=%v, want %v (%s):\n%s", got, tc.wantPrint, tc.why, out)
			}
			if !tc.wantPrint {
				return
			}
			for _, want := range []string{split, "drop --consensus", "Raising it makes unanimity harder"} {
				if !strings.Contains(out, want) {
					t.Errorf("the hint does not say %q:\n%s", want, out)
				}
			}
			// The number is the one that RAN, so a report cannot say "the 0
			// consensus pass(es)" — which is what the first version printed on a
			// fully-vetoed corpus.
			if !strings.Contains(out, "3 consensus pass(es)") {
				t.Errorf("the hint does not report the multiplier that ran:\n%s", out)
			}
		})
	}
}

// TestSupersedeReportOmitsTheGateLineWhenOff: an ungated pass's summary is
// unchanged, because a line about a gate nobody asked for is noise on the
// ordinary path — and the ordinary path is what most runs are.
func TestSupersedeReportOmitsTheGateLineWhenOff(t *testing.T) {
	out := supersedeReport("proj", supersede.Result{Candidates: 2, Confirmed: 1}, "would link", false, 1, 0)
	if strings.Contains(out, "consensus") {
		t.Errorf("an ungated pass's summary mentions the gate:\n%s", out)
	}
	if !strings.Contains(out, "would link") {
		t.Errorf("an ungated pass's summary lost its ordinary line:\n%s", out)
	}
}
