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
