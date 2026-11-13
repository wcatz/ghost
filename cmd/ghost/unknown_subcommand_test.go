package main

// The #691 contract: a command line the CLI cannot act on is a usage error (exit
// 2), not the silent exit 0 that `printUsage(); return 0` gave a mistyped word.
// These tests drive the real dispatch and never a run* function, since the
// commands reachable from a mistyped word would open the database or spawn a
// harness.

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// resetDetectRemote undoes the one global dispatchCommand wires on its way to the
// command switch (memory.SetDetectRemote), so a test that drives the real
// dispatch leaves the package as it found it.
func resetDetectRemote(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { memory.SetDetectRemote(nil) })
}

// pathWords splits a usage-table path ("project delete") into its command words.
func pathWords(path string) []string {
	return strings.Fields(path)
}

// listsWord reports whether subs contains word.
func listsWord(subs []string, word string) bool {
	for _, s := range subs {
		if s == word {
			return true
		}
	}
	return false
}

// usageErrorCase runs argv through the real dispatch and holds it to a usage
// error: exit 2, nothing on stdout, want on stderr, and no file created (a
// command line that could not be acted on creates no configuration or database).
func usageErrorCase(t *testing.T, argv []string, want string, wantUsage ...string) {
	t.Helper()
	roots := isolatedHelpFS(t)
	resetDetectRemote(t)
	var code int
	stdout, stderr := captureStreams(t, func() { code = runCLI(argv, dispatchCommand) })
	if code != 2 || stdout != "" || !strings.Contains(stderr, want) {
		t.Errorf("`ghost %s` = exit %d, stdout %q, stderr %q, want exit 2, no stdout, stderr containing %q", strings.Join(argv, " "), code, stdout, stderr, want)
	}
	for _, usage := range wantUsage {
		if !strings.Contains(stderr, usage) {
			t.Errorf("`ghost %s` stderr = %q, want the usage %q", strings.Join(argv, " "), stderr, usage)
		}
	}
	assertNoFiles(t, roots)
}

// TestRunCLI_UnknownSubcommandIsAUsageError is the table over EVERY command group
// in the usage table, so a group added without unknown-subcommand handling fails
// here. The diagnostic names the group's own level (a fall-through to the
// top-level answer would tell someone who typed `ghost project frobnicate` that
// ghost has no such command) and prints that level's own usage. A group invoked
// with no subcommand is a different mistake with its own diagnostic, except the
// one group whose bare form runs an action (`ghost mcp` starts the server).
func TestRunCLI_UnknownSubcommandIsAUsageError(t *testing.T) {
	if len(commandGroups) == 0 {
		t.Fatal("commandGroups is empty: the table over command groups has nothing to drive")
	}
	for _, g := range commandGroups {
		t.Run(g.path, func(t *testing.T) {
			usage, registered := usageByCommand[g.path]
			if !registered {
				t.Fatalf("%q is a command group but is not in usageByCommand", g.path)
			}
			usageErrorCase(t, append(pathWords(g.path), "frobnicate"), `ghost `+g.path+`: unknown command: "frobnicate"`, usage)
			if !g.defaultAction {
				usageErrorCase(t, pathWords(g.path), `ghost `+g.path+`: no subcommand given`, usage)
			}
		})
	}
}

// TestRunCLI_UnknownTopLevelCommandIsAUsageError covers the top level: a
// mistyped command, no command at all, and a PRESENT but empty word (a wrapper
// running `ghost mcp "$SUB"` with $SUB unset), which every level answers as the
// unrecognised word it is, in particular `ghost mcp ""`, whose bare form starts
// the server. The help paths are not this case (see the next test).
func TestRunCLI_UnknownTopLevelCommandIsAUsageError(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"no command at all", nil, "ghost: no command given"},
		{"a misspelled reflect with an operand", []string{"reflecttt", "myproject"}, `ghost: unknown command: "reflecttt"`},
		{"an unknown flag in the command position", []string{"--frobnicate"}, `ghost: unknown command: "--frobnicate"`},
		{"an empty command word", []string{""}, `ghost: unknown command: ""`},
		{"an empty word under a group with a default action", []string{"mcp", ""}, `ghost mcp: unknown command: ""`},
		{"an empty word under a group", []string{"project", ""}, `ghost project: unknown command: ""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := []string{}
			if !strings.Contains(tc.want, "ghost mcp") && !strings.Contains(tc.want, "ghost project") {
				want = append(want, "ghost <command>") // the top-level command list
			}
			usageErrorCase(t, tc.argv, tc.want, want...)
		})
	}
}

// TestRunCLI_HelpRequestStillExitsZero keeps the #630 gate in front of the usage
// error: a help request is a question, so it exits 0 and never reaches the
// dispatch, even beside an unknown subcommand. The top-level summary goes to
// stderr; a subcommand's usage goes to stdout alone.
func TestRunCLI_HelpRequestStillExitsZero(t *testing.T) {
	for _, tc := range []struct {
		name   string
		argv   []string
		stdout string // a phrase the answered command's own usage prints
		stderr string
	}{
		{name: "top level, short form", argv: []string{"-h"}, stderr: "ghost <command>"},
		{name: "help command", argv: []string{"help"}, stderr: "ghost <command>"},
		{name: "a group with an unknown subcommand", argv: []string{"project", "frobnicate", "-h"}, stdout: "ghost project delete"},
		// The history read takes a ref and its purge a whole id, so the usage line
		// names the two forms (#720).
		{name: "a leaf", argv: []string{"history", "-h"}, stdout: "ghost history <memory-ref>"},
		{name: "a name that matches no command", argv: []string{"help", "frobnicate"}, stderr: `no command "frobnicate"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetDetectRemote(t)
			var code int
			stdout, stderr := captureStreams(t, func() { code = runCLI(tc.argv, dispatchCommand) })
			if code != 0 || strings.Contains(stderr, "unknown command") {
				t.Errorf("`ghost %s` = exit %d, stderr %q, want a question answered with exit 0", strings.Join(tc.argv, " "), code, stderr)
			}
			if tc.stdout != "" && (!strings.Contains(stdout, tc.stdout) || stderr != "") {
				t.Errorf("`ghost %s` stdout = %q, stderr = %q, want the usage %q on stdout alone", strings.Join(tc.argv, " "), stdout, stderr, tc.stdout)
			}
			if tc.stderr != "" && !strings.Contains(stderr, tc.stderr) {
				t.Errorf("`ghost %s` stderr = %q, want it to contain %q", strings.Join(tc.argv, " "), stderr, tc.stderr)
			}
		})
	}
}

// TestCommandGroupsMatchTheUsageTable holds commandGroups to the usage table in
// every direction (the unknown-subcommand table is only a guard while the two
// agree on what a group is): every group is a registered command, every command
// registered UNDER a level is a word that level lists, and every word a group
// lists is a registered path under it or named in its usage text. A group
// registered with only its own usage and no subcommand path under it (obsidian's
// shape) has to be added to commandGroups by hand.
func TestCommandGroupsMatchTheUsageTable(t *testing.T) {
	groups := map[string]commandGroup{}
	for _, g := range commandGroups {
		if _, dup := groups[g.path]; dup {
			t.Errorf("%q is listed twice in commandGroups", g.path)
		}
		if len(g.subs) == 0 {
			t.Errorf("%q is listed with no subcommand words: a command that dispatches on none is not a group", g.path)
		}
		if _, registered := usageByCommand[g.path]; !registered {
			t.Errorf("commandGroups lists %q, which usageByCommand does not register", g.path)
		}
		groups[g.path] = g
	}
	for path := range usageByCommand {
		parts := pathWords(path)
		for n := 1; n < len(parts); n++ {
			level, word := strings.Join(parts[:n], " "), parts[n]
			g, isGroup := groups[level]
			if !isGroup {
				t.Errorf("usageByCommand registers %q under %q, but commandGroups does not list %q", path, level, level)
			} else if !listsWord(g.subs, word) {
				t.Errorf("commandGroups[%q] does not list %q, but the usage table registers %q", level, word, path)
			}
		}
	}
	for level, g := range groups {
		for _, word := range g.subs {
			if _, ok := usageByCommand[level+" "+word]; !ok && !strings.Contains(usageByCommand[level], word) {
				t.Errorf("commandGroups[%q] lists %q, which is neither a registered path nor named in that group's usage text", level, word)
			}
		}
	}
}
