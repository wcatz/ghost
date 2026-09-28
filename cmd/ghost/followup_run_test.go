package main

import (
	"testing"

	"github.com/wcatz/ghost/internal/followup"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/resolve"
)

// The follow-up is only useful if the command it prints RUNS and judges the rows
// it names. Those are two failure modes a test of either half alone cannot see:
// a renderer can emit a command the parser refuses, and a parser can accept a
// selector the scope throws away. So this walks the whole path — render, split on
// shell words the way a POSIX shell would, parse, select — for the ids this
// feature exists to name, including the ones an imported artifact brought in.
func TestResolveFollowupCommandRunsAndJudgesWhatItNames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		project string
		ids     []string
	}{
		{name: "two ordinary ids", project: "myproj", ids: []string{"aaaaaaaa1111111111111111111111", "bbbbbbbb2222222222222222222222"}},
		{name: "a project name holding a space", project: "my project", ids: []string{"aaaaaaaa1111111111111111111111"}},
		{name: "an id holding a space and a semicolon", project: "myproj", ids: []string{"imported note; rm -rf /"}},
		{name: "an id holding a dollar", project: "myproj", ids: []string{"proj$HOME"}},
		{name: "a three-character id", project: "myproj", ids: []string{"abc"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, viaFile, unnameable := followup.ResolveCommand(tc.project, tc.ids)
			if len(viaFile) != 0 || len(unnameable) != 0 {
				t.Fatalf("these ids need no other surface: viaFile %v, unnameable %v", viaFile, unnameable)
			}

			// A POSIX shell, which concatenates adjacent quoted words: this is the
			// step that decides how many arguments the parser actually sees.
			words := shellSplit(t, cmd)
			if len(words) < 2 || words[1] != "resolve" {
				t.Fatalf("the command does not start with the command word: %v", words)
			}
			// argv[2:], exactly as main hands a command's own words to its parser.
			parsed, err := parseResolveArgs(words[2:])
			if err != nil {
				t.Fatalf("the command the follow-up prints does not parse: %v\n%s", err, cmd)
			}
			if parsed.project != tc.project {
				t.Errorf("the parsed project is %q, want %q\n%s", parsed.project, tc.project, cmd)
			}

			// And the selectors name the rows, in the pool resolve would be given.
			pool := make([]memory.Memory, 0, len(tc.ids)+1)
			for _, id := range tc.ids {
				pool = append(pool, memory.Memory{ID: id})
			}
			pool = append(pool, memory.Memory{ID: "ffffffff0000000000000000000000"})
			scoped, misses, err := resolve.Scope{Only: parsed.only}.Select(pool)
			if err != nil {
				t.Fatalf("the selectors the command carries are refused: %v\n%s", err, cmd)
			}
			if len(misses) != 0 {
				t.Errorf("the command names rows that are not in the pool: %v\n%s", misses, cmd)
			}
			if len(scoped) != len(tc.ids) {
				t.Errorf("the command judged %d of %d named rows\n%s", len(scoped), len(tc.ids), cmd)
			}
		})
	}
}

// One wrong id in a pasted list must not cost the repair the rest of the list
// names: it is a reported miss, and the row that does exist is still judged. That
// is #702's rule, and it is what makes the follow-up survivable when one target
// was cleared by an earlier repair — the case a withdrawal's own follow-up can
// produce, since an earlier repair may already have cleared one of the targets
// this run names.
//
// The mistyped id is a well-formed one, because that is the only kind that gets
// this far: the shape rules run before a selector can be a prefix, so a selector
// that is neither a stored id nor hex is an error by design and takes the whole
// pass down rather than being skipped.
func TestResolveFollowupCommandToleratesOneWrongID(t *testing.T) {
	real := "aaaaaaaa1111111111111111111111"
	cleared := "cccccccc3333333333333333333333"
	cmd, _, _ := followup.ResolveCommand("myproj", []string{real, cleared})
	parsed, err := parseResolveArgs(shellSplit(t, cmd)[2:])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pool := []memory.Memory{{ID: real}, {ID: "bbbbbbbb2222222222222222222222"}}
	scoped, misses, err := resolve.Scope{Only: parsed.only}.Select(pool)
	if err != nil {
		t.Fatalf("a selector that names nothing is a miss, not a failure: %v", err)
	}
	if len(scoped) != 1 || scoped[0].ID != real {
		t.Errorf("judged %v, want just the row that exists", scopedIDsForTest(scoped))
	}
	if len(misses) != 1 || misses[0].Spec != cleared {
		t.Errorf("misses = %+v, want the absent id reported verbatim", misses)
	}
}

func scopedIDsForTest(ms []memory.Memory) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.ID
	}
	return out
}
