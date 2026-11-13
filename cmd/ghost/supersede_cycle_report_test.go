package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/followup"
	"github.com/wcatz/ghost/internal/supersede"
)

// The repair commands a report prints are only useful if the string PARSES into
// the invocation it means, so this walks render -> POSIX shell split -> parse for
// the shapes that break it (a renderer and a parser can each look right alone).
func TestSupersedeRepairCommandsParse(t *testing.T) {
	for _, project := range []string{"myproj", "my project", "proj; rm -rf /"} {
		t.Run(project, func(t *testing.T) {
			words := shellSplit(t, followup.ReassessCommand(project, 3))
			if len(words) < 2 || words[1] != "supersede" {
				t.Fatalf("not a supersede command: %v", words)
			}
			// The rendered repair names the gate (#862); both flags are boolean, so
			// the count is what catches a doubled --apply.
			got, _, apply, reassess, _, consensus, withdraw, _, err := parseSupersedeArgs(words[2:])
			if err != nil {
				t.Fatalf("ReassessCommand does not parse: %v", err)
			}
			if got != project || !reassess || !apply || consensus != 3 || len(withdraw) != 0 {
				t.Errorf("parsed project/reassess/apply/consensus/withdraw = %q/%v/%v/%d/%+v", got, reassess, apply, consensus, withdraw)
			}
			if n := strings.Count(strings.Join(words, " "), "--apply"); n != 1 {
				t.Errorf("the rendered repair = %q, want --apply exactly once", strings.Join(words, " "))
			}

			// A dash-leading id is the one shape the CLI cannot be given, which is
			// what WithdrawCommand's nameable return is for.
			for _, tc := range []struct {
				id       string
				nameable bool
			}{
				{"abcdef0123456789abcdef0123456789", true},
				{"imported note; rm -rf /", true},
				{"-imported-id", false},
			} {
				cmd, nameable := followup.WithdrawCommand(project, tc.id, "9876543210fedcba9876543210fedcba")
				if nameable != tc.nameable {
					t.Fatalf("nameable = %v, want %v for %q", nameable, tc.nameable, tc.id)
				}
				if !nameable {
					if cmd != "" {
						t.Errorf("a command was rendered for an id the parser refuses: %q", cmd)
					}
					continue
				}
				got, _, apply, reassess, _, _, withdraw, _, err := parseSupersedeArgs(shellSplit(t, cmd)[2:])
				if err != nil {
					t.Fatalf("WithdrawCommand does not parse: %v\n%s", err, cmd)
				}
				if got != project || !apply || reassess || len(withdraw) != 1 ||
					withdraw[0].source != tc.id || withdraw[0].target != "9876543210fedcba9876543210fedcba" {
					t.Errorf("parsed %q apply=%v reassess=%v withdraw=%+v from\n%s", got, apply, reassess, withdraw, cmd)
				}
			}
		})
	}
}

// The note under a cycle's two edges states the DECISION and never a write: a
// failed InvalidateLink leaves the denied edge live, and the per-edge markers
// carry the tense.
func TestCycleNoteClaimsNoWrite(t *testing.T) {
	c := supersede.CyclicPair{
		First:   supersede.CyclicEdge{SourceID: "abcdef0123456789", TargetID: "9876543210fedcba"},
		Second:  supersede.CyclicEdge{SourceID: "9876543210fedcba", TargetID: "abcdef0123456789"},
		Outcome: supersede.CycleKeptSecond,
	}
	if note := cycleNote(c); !strings.Contains(note, "its reverse is denied") {
		t.Errorf("the note does not say the reverse is denied: %q", note)
	}
	for _, outcome := range []supersede.CycleOutcome{supersede.CycleKeptFirst, supersede.CycleKeptSecond, supersede.CycleBothWithdrawn, supersede.CycleNoVerdict, supersede.CycleUnoriented} {
		c.Outcome = outcome
		note := cycleNote(c)
		if note == "" && outcome == supersede.CycleKeptSecond {
			t.Fatal("a kept cycle must say which edge the verdict named")
		}
		for _, claim := range []string{"withdrew", "withdrawn", "was removed"} {
			if strings.Contains(note, claim) {
				t.Errorf("outcome %q: the note claims a write (%q): %q", outcome, claim, note)
			}
		}
	}
}

// The cycle block's commands come from the one renderer the rest of the CLI uses;
// an id the CLI cannot name (a dash-leading imported id) is never printed in a
// --withdraw command the parser refuses, and the block points at the MCP tool
// with all three parameters instead.
func TestSupersedeCycleBlockUsesTheFollowupCommands(t *testing.T) {
	cyc := func(a, b string) supersede.CyclicPair {
		return supersede.CyclicPair{
			First:   supersede.CyclicEdge{SourceID: a, TargetID: b},
			Second:  supersede.CyclicEdge{SourceID: b, TargetID: a},
			Outcome: supersede.CycleUnoriented,
		}
	}
	c := cyc("abcdef0123456789", "9876543210fedcba")
	out := supersedeReassessReport("myproj", supersede.ReassessResult{Loaded: 2, Unclassified: 1, Cyclic: []supersede.CyclicPair{c}}, true, nil, 1, 0)
	for _, e := range []supersede.CyclicEdge{c.First, c.Second} {
		want, nameable := followup.WithdrawCommand("myproj", e.SourceID, e.TargetID)
		if !nameable || !strings.Contains(out, want) {
			t.Errorf("the cycle block does not print %q (nameable=%v):\n%s", want, nameable, out)
		}
	}

	dashy := "-imported-id-01"
	out = supersedeReassessReport("myproj", supersede.ReassessResult{Loaded: 2, Cyclic: []supersede.CyclicPair{cyc(dashy, "9876543210fedcba")}}, true, nil, 1, 0)
	if strings.Contains(out, "ghost supersede myproj --withdraw") {
		t.Errorf("the cycle block printed a --withdraw command for an id the parser refuses:\n%s", out)
	}
	for _, want := range []string{"not nameable from the CLI", "ghost_link_withdraw", "project_id myproj", "source_id " + dashy, "target_id 9876543210fedcba"} {
		if !strings.Contains(out, want) {
			t.Errorf("the cycle block is missing %q:\n%s", want, out)
		}
	}
}
