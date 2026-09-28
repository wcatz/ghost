package main

// The #691 contract: a command line the CLI cannot act on is a usage error.
// An unrecognised first word fell out of dispatchCommand's argv switch to
// `printUsage(); return 0`, so `ghost reflecttt myproject` printed the command
// list — a correct diagnostic — and exited 0, which is what every script that
// branches on the exit code reads as success. The commands that dispatch on a
// subcommand answered that same mistake four different ways: project,
// maintenance and opencode exited 1 with their own usage, obsidian exited 1
// with no diagnostic naming the word at all, and mcp ignored the word and
// started the MCP server.
//
// These tests drive the real dispatch and never a run* function: the commands
// reachable from a mistyped word are the ones that would open the database or
// spawn a harness, and nothing here re-execs os.Args[0].

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// resetDetectRemote undoes the one global dispatchCommand wires on its way to
// the command switch (memory.SetDetectRemote), so a test that drives the real
// dispatch leaves the package as it found it: no repository detector, which is
// the default the rest of this package's tests assume.
func resetDetectRemote(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { memory.SetDetectRemote(nil) })
}

// pathWords splits a usage-table path ("project delete") into its command
// words. The empty path is the CLI itself, which is zero words.
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

// TestRunCLI_UnknownSubcommandIsAUsageError is the table over EVERY command
// group in the usage table, so a group added without unknown-subcommand
// handling fails here instead of keeping the silent exit 0 #691 reported. Each
// case drives runCLI with the real dispatch and checks three things separately,
// because one of them alone would let the wrong answer through: the exit code,
// the usage printed (the level's own text, not the top-level command list), and
// the level named in the diagnostic — the last because a fall-through to the
// top-level answer would tell someone who typed `ghost project frobnicate` that
// ghost has no such command, which is the opposite of what happened.
func TestRunCLI_UnknownSubcommandIsAUsageError(t *testing.T) {
	if len(commandGroups) == 0 {
		t.Fatal("commandGroups is empty: the table over command groups has nothing to drive")
	}
	for _, g := range commandGroups {
		t.Run(g.path, func(t *testing.T) {
			usage, registered := usageByCommand[g.path]
			if !registered {
				t.Fatalf("%q is a command group but is not in usageByCommand: it has no usage text to print", g.path)
			}
			roots := isolatedHelpFS(t)
			resetDetectRemote(t)
			argv := append(pathWords(g.path), "frobnicate")

			var code int
			stdout, stderr := captureStreams(t, func() { code = runCLI(argv, dispatchCommand) })

			if code != 2 {
				t.Errorf("`ghost %s` exit code = %d, want 2: a usage error, so a script that branches on the exit code sees the typo", strings.Join(argv, " "), code)
			}
			if stdout != "" {
				t.Errorf("`ghost %s` stdout = %q, want the diagnostic and the usage on stderr", strings.Join(argv, " "), stdout)
			}
			if want := `ghost ` + g.path + `: unknown command: "frobnicate"`; !strings.Contains(stderr, want) {
				t.Errorf("`ghost %s` stderr = %q, want the unrecognised subcommand named at that level: %q", strings.Join(argv, " "), stderr, want)
			}
			if !strings.Contains(stderr, usage) {
				t.Errorf("`ghost %s` stderr = %q, want that level's own usage (usageByCommand[%q])", strings.Join(argv, " "), stderr, g.path)
			}
			// Nothing ran: a command line that could not be acted on created no
			// configuration, no database and no file, so what the reader got is
			// the diagnostic and nothing else.
			assertNoFiles(t, roots)
		})
	}
}

// TestRunCLI_MissingSubcommandIsAUsageError is the other half of the class at
// the nested levels: a group invoked with no subcommand word is a usage error
// with its own diagnostic, because "none was given" and "that one does not
// exist" are different mistakes and the reader is told which they made. The one
// group excluded is the one whose bare invocation runs its own action —
// `ghost mcp` starts the server, which is what MCP clients spawn — and that
// exclusion is the group's own record in the table rather than a gap in the
// loop.
func TestRunCLI_MissingSubcommandIsAUsageError(t *testing.T) {
	for _, g := range commandGroups {
		if g.defaultAction {
			t.Logf("%s has a default action for a bare invocation, so a missing subcommand is not an error there", g.path)
			continue
		}
		t.Run(g.path, func(t *testing.T) {
			usage, registered := usageByCommand[g.path]
			if !registered {
				t.Fatalf("%q is a command group but is not in usageByCommand", g.path)
			}
			roots := isolatedHelpFS(t)
			resetDetectRemote(t)
			argv := pathWords(g.path)

			var code int
			stdout, stderr := captureStreams(t, func() { code = runCLI(argv, dispatchCommand) })

			if code != 2 {
				t.Errorf("`ghost %s` exit code = %d, want 2: the same usage-error code an unknown subcommand gets", g.path, code)
			}
			if stdout != "" {
				t.Errorf("`ghost %s` stdout = %q, want the diagnostic and the usage on stderr", g.path, stdout)
			}
			if want := `ghost ` + g.path + `: no subcommand given`; !strings.Contains(stderr, want) {
				t.Errorf("`ghost %s` stderr = %q, want the missing subcommand named: %q", g.path, stderr, want)
			}
			if !strings.Contains(stderr, usage) {
				t.Errorf("`ghost %s` stderr = %q, want that level's own usage (usageByCommand[%q])", g.path, stderr, g.path)
			}
			assertNoFiles(t, roots)
		})
	}
}

// TestRunCLI_UnknownTopLevelCommandIsAUsageError covers the two shapes #691
// names at the top level: a mistyped command, and no command at all. The second
// is a usage error too, decided rather than inherited — a wrapper running
// `ghost $SUB` with an unset or empty $SUB typed the same unusable command line
// as a misspelling, and reading it as success is the failure this fixes. The
// help paths are NOT this case: `ghost help` and `ghost -h` are questions, and
// TestRunCLI_HelpRequestStillExitsZero keeps them at 0.
func TestRunCLI_UnknownTopLevelCommandIsAUsageError(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{name: "no command at all", argv: nil, want: "ghost: no command given"},
		{name: "a misspelled command", argv: []string{"frobnicate"}, want: `ghost: unknown command: "frobnicate"`},
		{name: "a misspelled reflect with an operand", argv: []string{"reflecttt", "myproject"}, want: `ghost: unknown command: "reflecttt"`},
		{name: "an unknown flag in the command position", argv: []string{"--frobnicate"}, want: `ghost: unknown command: "--frobnicate"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			roots := isolatedHelpFS(t)
			resetDetectRemote(t)
			var code int
			stdout, stderr := captureStreams(t, func() { code = runCLI(tc.argv, dispatchCommand) })

			if code != 2 {
				t.Errorf("`ghost %s` exit code = %d, want 2", strings.Join(tc.argv, " "), code)
			}
			if stdout != "" {
				t.Errorf("`ghost %s` stdout = %q, want the diagnostic and the command list on stderr", strings.Join(tc.argv, " "), stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("`ghost %s` stderr = %q, want it to contain %q", strings.Join(tc.argv, " "), stderr, tc.want)
			}
			// The command list is where the reader is sent next, and it is the
			// only place the real names appear: the same text an unknown command
			// always answered with, unchanged.
			if !strings.Contains(stderr, "ghost <command>") {
				t.Errorf("`ghost %s` stderr = %q, want the top-level command list", strings.Join(tc.argv, " "), stderr)
			}
			assertNoFiles(t, roots)
		})
	}
}

// TestRunCLI_EmptyCommandWordIsAUsageError is the case where a word is PRESENT
// and empty, which is not the same as no word at all: a wrapper running
// `ghost mcp "$SUB"` with an unset $SUB passes one. Every level answers it as
// the unrecognised word it is — in particular `ghost mcp ""`, which is a group
// whose bare form starts the server, so an empty word took the bare invocation
// and connected on stdio instead of refusing. That is why the dispatch branches
// on PRESENCE rather than on the word being non-empty, and why "no subcommand
// given" stays reserved for a word that is genuinely absent.
func TestRunCLI_EmptyCommandWordIsAUsageError(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{name: "the command word", argv: []string{""}, want: `ghost: unknown command: ""`},
		{name: "a group with a default action", argv: []string{"mcp", ""}, want: `ghost mcp: unknown command: ""`},
		{name: "a group", argv: []string{"project", ""}, want: `ghost project: unknown command: ""`},
		{name: "a group whose usage is one line", argv: []string{"opencode", ""}, want: `ghost opencode: unknown command: ""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			roots := isolatedHelpFS(t)
			resetDetectRemote(t)
			var code int
			stdout, stderr := captureStreams(t, func() { code = runCLI(tc.argv, dispatchCommand) })

			if code != 2 {
				t.Errorf("`ghost %s` exit code = %d, want 2", strings.Join(tc.argv, " "), code)
			}
			if stdout != "" {
				t.Errorf("`ghost %s` stdout = %q, want the diagnostic on stderr", strings.Join(tc.argv, " "), stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("`ghost %s` stderr = %q, want an empty word reported as the unrecognised one it is: %q", strings.Join(tc.argv, " "), stderr, tc.want)
			}
			assertNoFiles(t, roots)
		})
	}
}

// TestRunCLI_HelpRequestStillExitsZero keeps the #630 gate in front of the
// usage error: a help request is a question, so it exits 0 and never reaches
// the dispatch — even when the subcommand next to the help flag is not a
// subcommand, and even for a name that matches no command at all. The
// alternative would fail `ghost project frobnicate -h`, and the reader who
// typed that wants the project's usage, which is what they get.
//
// Stream per case, because the top-level summary is the CLI's own: `ghost -h`
// prints it on stderr, where it has always gone, while a subcommand's usage
// goes to stdout (#630).
func TestRunCLI_HelpRequestStillExitsZero(t *testing.T) {
	for _, tc := range []struct {
		name   string
		argv   []string
		stdout string // a phrase the answered command's own usage prints
		stderr string
	}{
		{name: "top level, short form", argv: []string{"-h"}, stderr: "ghost <command>"},
		{name: "top level, long form", argv: []string{"--help"}, stderr: "ghost <command>"},
		{name: "help command", argv: []string{"help"}, stderr: "ghost <command>"},
		{name: "a group", argv: []string{"project", "-h"}, stdout: "ghost project delete"},
		{name: "a group with an unknown subcommand", argv: []string{"project", "frobnicate", "-h"}, stdout: "ghost project delete"},
		{name: "a group whose usage is one line", argv: []string{"opencode", "frobnicate", "--help"}, stdout: "ghost opencode cleanup-sessions"},
		// The history read takes a ref and its purge a whole id, so the usage
		// line names the two forms (#720) — which is also what this asserts: a
		// help text is pinned by the phrase it prints, not by a copy of it here.
		{name: "a leaf", argv: []string{"history", "-h"}, stdout: "ghost history <memory-ref>"},
		{
			name: "a name that matches no command", argv: []string{"help", "frobnicate"},
			stderr: `no command "frobnicate"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetDetectRemote(t)
			var code int
			stdout, stderr := captureStreams(t, func() { code = runCLI(tc.argv, dispatchCommand) })
			if code != 0 {
				t.Errorf("`ghost %s` exit code = %d, want 0: a question is not a mistake", strings.Join(tc.argv, " "), code)
			}
			if tc.stdout != "" {
				if !strings.Contains(stdout, tc.stdout) {
					t.Errorf("`ghost %s` stdout = %q, want the usage it names: %q", strings.Join(tc.argv, " "), stdout, tc.stdout)
				}
				if stderr != "" {
					t.Errorf("`ghost %s` stderr = %q, want a subcommand's usage answered on stdout alone", strings.Join(tc.argv, " "), stderr)
				}
			}
			if tc.stderr != "" && !strings.Contains(stderr, tc.stderr) {
				t.Errorf("`ghost %s` stderr = %q, want it to contain %q", strings.Join(tc.argv, " "), stderr, tc.stderr)
			}
			if strings.Contains(stderr, "unknown command") {
				t.Errorf("`ghost %s` stderr = %q: a help request is answered, not reported as a usage error", strings.Join(tc.argv, " "), stderr)
			}
		})
	}
}

// TestCommandGroupsMatchTheUsageTable holds commandGroups to the usage table
// in every direction, because the table in
// TestRunCLI_UnknownSubcommandIsAUsageError is only a guard while the two agree
// on what a group is:
//
//   - every group is a command usageByCommand registers, so a bare one has
//     usage text to print and an -h/--help that prints the same text;
//   - every command the usage table registers UNDER a level is a subcommand
//     word that level lists, so a new `ghost project rename` cannot be added to
//     the help without the dispatch learning to route — or refuse — it;
//   - every word a group lists is either a registered path under it or named in
//     that group's own usage text, so a typo in commandGroups fails here
//     instead of leaving a group that advertises a subcommand nothing can
//     reach (obsidian registers one usage text for both of its modes, so it is
//     covered by this direction alone).
//
// The second direction is the one that catches a new group added to the help
// without being taught about an unknown word. What it cannot catch, and what no
// test over these two tables can: a new group registered with only its own
// usage and no subcommand path under it — obsidian's shape — which nothing in
// the usage table distinguishes from a leaf whose first operand is data. That
// one has to be added to commandGroups by hand, where the table above then
// holds its dispatch to the same contract as every other group.
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
			t.Errorf("commandGroups lists %q, which usageByCommand does not register: there is no usage text for it", g.path)
		}
		groups[g.path] = g
	}

	for path := range usageByCommand {
		parts := pathWords(path)
		// Every prefix of a registered path is a level a subcommand word can
		// appear at, so each one has to be a group that lists that word.
		for n := 1; n < len(parts); n++ {
			level, word := strings.Join(parts[:n], " "), parts[n]
			g, isGroup := groups[level]
			if !isGroup {
				t.Errorf("usageByCommand registers %q under %q, so %q dispatches on a subcommand word, but commandGroups does not list it", path, level, level)
				continue
			}
			if !listsWord(g.subs, word) {
				t.Errorf("commandGroups[%q] does not list %q, but the usage table registers %q: `ghost %s frobnicate` would fall through to the top-level answer", level, word, path, level)
			}
		}
	}

	for level, g := range groups {
		for _, word := range g.subs {
			if _, ok := usageByCommand[level+" "+word]; ok {
				continue // a registered subcommand path
			}
			if !strings.Contains(usageByCommand[level], word) {
				t.Errorf("commandGroups[%q] lists %q, which is neither a registered path (%q) nor named in that group's usage text: nothing dispatches it",
					level, word, level+" "+word)
			}
		}
	}
}
