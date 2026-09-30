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
			words := shellSplit(t, followup.ReassessCommand(tc.project))
			if len(words) < 2 || words[1] != "supersede" {
				t.Fatalf("not a supersede command: %v", words)
			}
			project, _, apply, reassess, _, withdraw, err := parseSupersedeArgs(words[2:])
			if err != nil {
				t.Fatalf("ReassessCommand does not parse: %v", err)
			}
			if project != tc.project {
				t.Errorf("project = %q, want %q", project, tc.project)
			}
			if !reassess || !apply {
				t.Errorf("reassess=%v apply=%v, want both true: the flagless form is a dry run that withdraws nothing", reassess, apply)
			}
			if len(withdraw) != 0 {
				t.Errorf("withdraw = %+v, want none: --reassess and --withdraw are refused together, by design", withdraw)
			}

			// The withdraw command, with the id shapes an imported artifact brings
			// in — a full id is never hex, and the resolver accepts any shape as
			// long as the whole id is given.
			for _, tc2 := range []struct{ name, id string }{
				{name: "a hex id", id: "abcdef0123456789abcdef0123456789"},
				{name: "an id holding a space and a semicolon", id: "imported note; rm -rf /"},
				{name: "an eight-character prefix", id: "abcdef01"},
			} {
				t.Run(tc2.name, func(t *testing.T) {
					cmd := followup.WithdrawCommand(tc.project, tc2.id, "9876543210fedcba9876543210fedcba")
					words := shellSplit(t, cmd)
					project, _, apply, reassess, _, withdraw, err := parseSupersedeArgs(words[2:])
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

// The cycle block the repair pass prints is the operator's only route out of a
// demotion it did not cause, so the commands in it are rendered by the one
// renderer the rest of the CLI uses rather than spelled at the call site — and
// this pins that the two agree, since a report and a renderer that spelled the
// command differently would each look right alone.
func TestSupersedeCycleBlockUsesTheFollowupCommands(t *testing.T) {
	c := supersede.CyclicPair{
		First:   supersede.CyclicEdge{SourceID: "abcdef0123456789", TargetID: "9876543210fedcba"},
		Second:  supersede.CyclicEdge{SourceID: "9876543210fedcba", TargetID: "abcdef0123456789"},
		Outcome: supersede.CycleUndecided,
	}
	out := supersedeReassessReport("myproj", supersede.ReassessResult{
		Loaded: 2, Unclassified: 1, Cyclic: []supersede.CyclicPair{c},
	}, true, nil, 1, 0)
	for _, e := range []supersede.CyclicEdge{c.First, c.Second} {
		want := followup.WithdrawCommand("myproj", e.SourceID, e.TargetID)
		if !strings.Contains(out, want) {
			t.Errorf("the cycle block does not print %q:\n%s", want, out)
		}
	}
}
