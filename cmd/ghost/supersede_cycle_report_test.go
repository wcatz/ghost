package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/followup"
	"github.com/wcatz/ghost/internal/supersede"
)

// The two supersede-side repair commands are only useful if the string a report
// prints PARSES into the invocation it means, so this walks the whole path for
// the shapes that break it: render, split on shell words the way a POSIX shell
// would, parse. A renderer that emits `--withdraw 'a b' 'c'` and a parser that
// wants two bare operands fail in the middle of this, which is a test of either
// half alone cannot see — and it is the same reason the resolve follow-up has one
// of these.
func TestSupersedeRepairCommandsParse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project string
	}{
		{name: "an ordinary project name", project: "myproj"},
		{name: "a project name holding a space", project: "my project"},
		{name: "a project name holding a semicolon", project: "proj; rm -rf /"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			words := shellSplit(t, followup.ReassessCommand(tc.project, 3))
			if len(words) < 2 || words[1] != "supersede" {
				t.Fatalf("not a supersede command: %v", words)
			}
			// The rendered repair names the gate (#862), so the parser has to carry
			// it through to the invocation it means — and because both flags are
			// boolean, a renderer that doubled --apply would still parse, so the
			// count below is what catches the doubling a substring cannot.
			project, _, apply, reassess, _, consensus, withdraw, _, err := parseSupersedeArgs(words[2:])
			if err != nil {
				t.Fatalf("ReassessCommand does not parse: %v", err)
			}
			if project != tc.project {
				t.Errorf("project = %q, want %q", project, tc.project)
			}
			if !reassess || !apply {
				t.Errorf("reassess=%v apply=%v, want both true: a repair that predicts withdraws nothing", reassess, apply)
			}
			if consensus != 3 {
				t.Errorf("consensus = %d, want 3: the gate a report names is the gate the command carries", consensus)
			}
			if len(withdraw) != 0 {
				t.Errorf("withdraw = %+v, want none: --reassess and --withdraw are refused together, by design", withdraw)
			}
			if n := strings.Count(strings.Join(words, " "), "--apply"); n != 1 {
				t.Errorf("the rendered repair = %q, want --apply exactly once: the renderer already carries it", strings.Join(words, " "))
			}

			// The withdraw command, with the id shapes an imported artifact brings
			// in — a full id is never hex, and the resolver accepts any shape as
			// long as the whole id is given. A DASH-LEADING id is the one shape the
			// CLI cannot be given, and it is the case the renderer's `nameable`
			// return exists for: parseSupersedeArgs refuses a --withdraw operand
			// that looks like a flag, and quoting does not change that.
			for _, tc2 := range []struct {
				name, id string
				nameable bool
			}{
				{name: "a hex id", id: "abcdef0123456789abcdef0123456789", nameable: true},
				{name: "an id holding a space and a semicolon", id: "imported note; rm -rf /", nameable: true},
				{name: "an eight-character prefix", id: "abcdef01", nameable: true},
				{name: "an id beginning with a dash", id: "-imported-id", nameable: false},
			} {
				t.Run(tc2.name, func(t *testing.T) {
					cmd, nameable := followup.WithdrawCommand(tc.project, tc2.id, "9876543210fedcba9876543210fedcba")
					if nameable != tc2.nameable {
						t.Fatalf("nameable = %v, want %v for %q", nameable, tc2.nameable, tc2.id)
					}
					if !nameable {
						if cmd != "" {
							t.Errorf("a command was rendered for an id the parser refuses: %q", cmd)
						}
						return
					}
					words := shellSplit(t, cmd)
					project, _, apply, reassess, _, _, withdraw, _, err := parseSupersedeArgs(words[2:])
					if err != nil {
						t.Fatalf("WithdrawCommand does not parse: %v\n%s", err, cmd)
					}
					if project != tc.project {
						t.Errorf("project = %q, want %q\n%s", project, tc.project, cmd)
					}
					if !apply || reassess {
						t.Errorf("apply=%v reassess=%v, want apply and no reassess: without --apply the command withdraws nothing", apply, reassess)
					}
					if len(withdraw) != 1 {
						t.Fatalf("withdraw = %+v, want exactly one named pair\n%s", withdraw, cmd)
					}
					if withdraw[0].source != tc2.id || withdraw[0].target != "9876543210fedcba9876543210fedcba" {
						t.Errorf("named %s→%s, want %s→98765432…\n%s", withdraw[0].source, withdraw[0].target, tc2.id, cmd)
					}
				})
			}
		})
	}
}

// The note under a cycle's two edges states the DECISION and never a write,
// because `apply` does not determine one: a failed InvalidateLink leaves the
// denied edge live, and a concurrent pass may have taken it first. The per-edge
// markers carry the tense, so the note must be mode-independent — and a test that
// only checked it under --apply would have let the past tense back in.
func TestCycleNoteClaimsNoWrite(t *testing.T) {
	c := supersede.CyclicPair{
		First:   supersede.CyclicEdge{SourceID: "abcdef0123456789", TargetID: "9876543210fedcba"},
		Second:  supersede.CyclicEdge{SourceID: "9876543210fedcba", TargetID: "abcdef0123456789"},
		Outcome: supersede.CycleKeptSecond,
	}
	note := cycleNote(c)
	if note == "" {
		t.Fatal("a kept cycle must say which edge the verdict named")
	}
	for _, claim := range []string{"withdrew", "withdrawn", "was removed", "has been withdrawn"} {
		if strings.Contains(note, claim) {
			t.Errorf("the note claims a write (%q) that the per-row markers own: %q", claim, note)
		}
	}
	// The two denied-both shapes say the same thing without naming a write, and
	// the note is the same sentence in every mode — there is no `apply` to read.
	for _, outcome := range []supersede.CycleOutcome{supersede.CycleKeptFirst, supersede.CycleBothWithdrawn, supersede.CycleNoVerdict, supersede.CycleUnoriented} {
		c.Outcome = outcome
		if note := cycleNote(c); strings.Contains(note, "withdrew") || strings.Contains(note, "withdrawn") {
			t.Errorf("outcome %q: the note claims a write: %q", outcome, note)
		}
	}
	// And the wording the operator acts on is the judgement, in the present tense.
	c.Outcome = supersede.CycleKeptSecond
	if note := cycleNote(c); !strings.Contains(note, "its reverse is denied") {
		t.Errorf("the note does not say the reverse is denied: %q", note)
	}
}

// The cycle block the repair pass prints is the operator's only route out of a
// demotion it did not cause, so the commands in it are rendered by the one
// renderer the rest of the CLI uses rather than spelled at the call site — and
// this pins that the two agree, since a report and a renderer that spelled the
// command differently would each look right alone.
func TestSupersedeCycleBlockUsesTheFollowupCommands(t *testing.T) {
	c := supersede.CyclicPair{
		First:   supersede.CyclicEdge{SourceID: "abcdef0123456789", TargetID: "9876543210fedcba"},
		Second:  supersede.CyclicEdge{SourceID: "9876543210fedcba", TargetID: "abcdef0123456789"},
		Outcome: supersede.CycleUnoriented,
	}
	out := supersedeReassessReport("myproj", supersede.ReassessResult{
		Loaded: 2, Unclassified: 1, Cyclic: []supersede.CyclicPair{c},
	}, true, nil, 1, 0)
	for _, e := range []supersede.CyclicEdge{c.First, c.Second} {
		want, nameable := followup.WithdrawCommand("myproj", e.SourceID, e.TargetID)
		if !nameable {
			t.Fatalf("an ordinary id is nameable, so WithdrawCommand refused %s", e.SourceID)
		}
		if !strings.Contains(out, want) {
			t.Errorf("the cycle block does not print %q:\n%s", want, out)
		}
	}
}

// An id the CLI cannot name must not be printed inside a --withdraw command the
// parser refuses, and the answer is to name the ids and the surface that CAN take
// them — the same split ResolveCommand makes for an id no --only form can carry.
// This is `ghost import`'s verbatim ids reaching memory_links, so it is a shape
// the store really holds rather than a hypothetical.
func TestSupersedeCycleBlockNamesAnEdgeTheCLICannotWithdraw(t *testing.T) {
	dashy := "-imported-id-01"
	c := supersede.CyclicPair{
		First:   supersede.CyclicEdge{SourceID: dashy, TargetID: "9876543210fedcba"},
		Second:  supersede.CyclicEdge{SourceID: "9876543210fedcba", TargetID: dashy},
		Outcome: supersede.CycleUnoriented,
	}
	if _, nameable := followup.WithdrawCommand("myproj", dashy, "9876543210fedcba"); nameable {
		t.Fatal("an id beginning with a dash is not a --withdraw operand, so it must not be reported as nameable")
	}
	out := supersedeReassessReport("myproj", supersede.ReassessResult{
		Loaded: 2, Cyclic: []supersede.CyclicPair{c},
	}, true, nil, 1, 0)
	// The dead command, printed for an operator to run and fail on. The check is on
	// the command PREFIX, not on the flag: the block's own explanation says "no
	// --withdraw command can carry it", and a flag-only match would flag that
	// sentence as the thing it forbids.
	if strings.Contains(out, "ghost supersede myproj --withdraw") {
		t.Errorf("the cycle block printed a --withdraw command for an id the parser refuses:\n%s", out)
	}
	// What the block says instead, and what the operator is pointed at.
	// All THREE parameters, under the tool's own names: ghost_link_withdraw's
	// handler refuses the call when any of project_id, source_id or target_id is
	// empty, and project_id is the ownership check as well as a required field.
	// A fallback naming two of the three is the same dead command on another
	// surface.
	for _, want := range []string{
		"not nameable from the CLI",
		"ghost_link_withdraw",
		"project_id myproj",
		"source_id " + dashy,
		"target_id 9876543210fedcba",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the cycle block is missing %q:\n%s", want, out)
		}
	}
}
