package main

// `ghost context` and `ghost context --audit` used to exit `2` when their own
// parsers rejected an argument (#1090); they exit `1` now. `2` is the code
// docs/cli.md and docs/invariants.md reserve for a command line the DISPATCH
// cannot route — an unknown command, a missing or unknown subcommand, no command
// — while an argument a command's own parser rejects is a run that started and
// failed, and exits `1`. Both modes are scripted (the block is what an adapter
// injects, the report is piped into a check), so a wrapper that maps `2` to "the
// command was typed wrong" misread a real argument error as a routing mistake.
//
// These drive the real dispatch through runCLI, with os.Args set the way
// asof_test.go sets it, because runContext reads os.Args rather than the argv
// it is dispatched with. The exit code is the one runCLI returns.

import (
	"os"
	"strings"
	"testing"
)

// TestContextArgumentRefusalsExitOne: every argument a `ghost context` parser
// rejects exits `1`, prints its one-line diagnostic on stderr naming the
// command, writes nothing to stdout and never prints a usage block — the usage
// block is what a real `2` carries, so printing it here would be the other half
// of the same mistake.
func TestContextArgumentRefusalsExitOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
	}{
		{"audit unknown flag", []string{"context", "--audit", "--wat"}},
		{"audit empty project", []string{"context", "--audit", "--project="}},
		{"audit non-duration since", []string{"context", "--audit", "--since", "7d"}},
		{"audit combined with as-of", []string{"context", "--audit", "--as-of", "2026-01-01T00:00:00Z"}},
		{"as-of not an RFC 3339 instant", []string{"context", "--as-of", "bogus"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			roots := isolatedHelpFS(t)
			resetDetectRemote(t)
			origArgs := os.Args
			// argv[0] is the program name and argv[1] the subcommand, which is
			// the shape runContext's hand-rolled scan indexes from.
			os.Args = append([]string{origArgs[0]}, tc.argv...)
			defer func() { os.Args = origArgs }()

			var code int
			stdout, stderr := captureStreams(t, func() {
				code = runCLI(os.Args[1:], dispatchCommand)
			})

			if code != 1 {
				t.Errorf("`ghost %s` exit code = %d, want 1: an argument its own parser rejects is a run that started and failed, not a routing error", strings.Join(tc.argv, " "), code)
			}
			if stdout != "" {
				t.Errorf("`ghost %s` stdout = %q, want nothing: the diagnostic goes to stderr", strings.Join(tc.argv, " "), stdout)
			}
			if !strings.HasPrefix(stderr, "ghost context") {
				t.Errorf("`ghost %s` stderr = %q, want a diagnostic naming the command", strings.Join(tc.argv, " "), stderr)
			}
			if strings.Contains(stderr, "Usage:") {
				t.Errorf("`ghost %s` stderr = %q, want no usage block: a usage block is what a routing error prints", strings.Join(tc.argv, " "), stderr)
			}
			if lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n"); len(lines) != 1 {
				t.Errorf("`ghost %s` stderr = %q, want a single diagnostic line", strings.Join(tc.argv, " "), stderr)
			}
			assertNoFiles(t, roots)
		})
	}
}

// TestContextArgumentRefusalsGuardEchoedValue: a `ghost context` refusal that
// names the caller's argument renders it through memory.RefusalArg, like
// --project does. A token-shaped value is withheld and a value longer than the
// 256 bytes a refusal repeats is not echoed back; the exit code stays 1. The
// parser errors time.ParseDuration and time.Parse return quote the input
// themselves, so the refusals must not wrap them either.
func TestContextArgumentRefusalsGuardEchoedValue(t *testing.T) {
	const key = "sk-ant-api03-Zz09XxYyWwVvUuTtSsRrQqPpOoNnMmLlKkJj01"
	long := strings.Repeat("x", 1000)
	for _, tc := range []struct {
		name string
		argv func(v string) []string
	}{
		{"since", func(v string) []string { return []string{"context", "--audit", "--since", v} }},
		{"since equals form", func(v string) []string { return []string{"context", "--audit", "--since=" + v} }},
		{"unknown flag", func(v string) []string { return []string{"context", "--audit", "--" + v} }},
		{"stray positional", func(v string) []string { return []string{"context", "--audit", v} }},
		{"as-of", func(v string) []string { return []string{"context", "--as-of", v} }},
	} {
		for _, val := range []struct{ name, v, needle string }{
			{"token", key, "sk-ant-api03"},
			{"oversized", long, "xxxxxxxxxx"},
		} {
			t.Run(tc.name+" "+val.name, func(t *testing.T) {
				roots := isolatedHelpFS(t)
				resetDetectRemote(t)
				origArgs := os.Args
				os.Args = append([]string{origArgs[0]}, tc.argv(val.v)...)
				defer func() { os.Args = origArgs }()

				var code int
				_, stderr := captureStreams(t, func() {
					code = runCLI(os.Args[1:], dispatchCommand)
				})
				if code != 1 {
					t.Errorf("exit code = %d, want 1", code)
				}
				if stderr == "" || !strings.HasPrefix(stderr, "ghost context") {
					t.Fatalf("stderr = %q, want a refusal naming the command", stderr)
				}
				if strings.Contains(stderr, val.needle) {
					t.Errorf("stderr echoes the caller's value: %q", stderr)
				}
				if len(stderr) > 600 {
					t.Errorf("stderr is %d bytes, want a bounded refusal: %q", len(stderr), stderr)
				}
				assertNoFiles(t, roots)
			})
		}
	}
}
