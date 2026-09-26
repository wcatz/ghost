package main

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The #630 contract, tested behaviourally: a -h/--help request prints that
// command's own usage on stdout, exits 0, never reaches the command dispatch —
// the dispatch is where the side effects a help request must not have live —
// and creates nothing under an isolated HOME/XDG/TMPDIR. Nothing here re-execs
// the test binary or calls a run* function that could spawn a harness (that
// fork-bombs the machine).

// captureStreams runs fn with os.Stdout and os.Stderr redirected to pipes and
// returns what each captured. Readers run in goroutines so output that grew
// past a pipe buffer could not deadlock the test.
func captureStreams(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	or, ow, err := os.Pipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	er, ew, err := os.Pipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = ow, ew
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()

	outCh, errCh := make(chan string, 1), make(chan string, 1)
	go func() { b, _ := io.ReadAll(or); outCh <- string(b) }()
	go func() { b, _ := io.ReadAll(er); errCh <- string(b) }()

	fn()

	_ = ow.Close()
	_ = ew.Close()
	stdout, stderr = <-outCh, <-errCh
	_ = or.Close()
	_ = er.Close()
	return stdout, stderr
}

// isolatedHelpFS points every root a command could write through at a fresh
// temp directory and clears GHOST_*/ANTHROPIC_* overrides and the PATH, so a
// help request that wrongly ran its command writes somewhere visible here and
// can neither reach a real harness nor a client binary. Mirrors
// isolatedLifecycleEnv's env restore. Returns the roots to check afterwards.
func isolatedHelpFS(t *testing.T) []string {
	t.Helper()
	for _, e := range os.Environ() {
		if key, _, ok := strings.Cut(e, "="); ok && (strings.HasPrefix(key, "GHOST_") || key == "ANTHROPIC_API_KEY") {
			if old, ok := os.LookupEnv(key); ok {
				t.Setenv(key, old)
				_ = os.Unsetenv(key)
			}
		}
	}
	var roots []string
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "TMPDIR", "PATH"} {
		dir := t.TempDir()
		t.Setenv(key, dir)
		roots = append(roots, dir)
	}
	return roots
}

// assertNoFiles fails if anything was created under the isolated roots.
func assertNoFiles(t *testing.T, roots []string) {
	t.Helper()
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == root {
				return nil
			}
			t.Errorf("help request created %s", path)
			return nil
		})
		if err != nil {
			t.Errorf("walk %s: %v", root, err)
		}
	}
}

// helpCases is every subcommand that answers -h/--help, with a phrase only its
// own usage can print: routing a help request to the top-level summary or to a
// sibling's usage would pass a bare non-empty-stdout check. path holds the
// command words; pre holds arguments between them and the help flag.
var helpCases = []struct {
	name string
	path []string
	pre  []string
	want string
}{
	{name: "mcp", path: []string{"mcp"}, want: "Starts the MCP server on stdio"},
	{name: "mcp init", path: []string{"mcp", "init"}, want: "ghost mcp init [--client"},
	{name: "mcp init with client", path: []string{"mcp", "init"}, pre: []string{"--client", "claude"}, want: "ghost mcp init [--client"},
	{name: "mcp status", path: []string{"mcp", "status"}, want: "ghost mcp status [--client"},
	{name: "hook", path: []string{"hook"}, want: "ghost hook <event> --source <host>"},
	{name: "reflect", path: []string{"reflect"}, want: "ghost reflect <project> [flags]"},
	{name: "supersede", path: []string{"supersede"}, want: "ghost supersede <project> [flags]"},
	{name: "resolve", path: []string{"resolve"}, want: "ghost resolve <project> [flags]"},
	{name: "lifecycle", path: []string{"lifecycle"}, want: "ghost lifecycle --project <name>"},
	{name: "obsidian", path: []string{"obsidian"}, want: "ghost obsidian <export|sync> [flags]"},
	{name: "obsidian export", path: []string{"obsidian", "export"}, want: "ghost obsidian <export|sync> [flags]"},
	{name: "obsidian sync", path: []string{"obsidian", "sync"}, want: "ghost obsidian <export|sync> [flags]"},
	{name: "bench", path: []string{"bench"}, want: "ghost bench [--sweep]"},
	{name: "upgrade", path: []string{"upgrade"}, want: "ghost upgrade"},
	{name: "context", path: []string{"context"}, want: "ghost context [--cwd <dir>]"},
	{name: "maintenance", path: []string{"maintenance"}, want: "ghost maintenance status"},
	{name: "maintenance status", path: []string{"maintenance", "status"}, want: "ghost maintenance status"},
	{name: "maintenance clean-scratch", path: []string{"maintenance", "clean-scratch"}, want: "ghost maintenance clean-scratch"},
	{name: "project", path: []string{"project"}, want: "ghost project delete <name-or-id>"},
	{name: "project delete", path: []string{"project", "delete"}, want: "ghost project delete <name-or-id> [flags]"},
	{name: "project merge", path: []string{"project", "merge"}, want: "ghost project merge <old-name-or-id> <new-name-or-id>"},
	{name: "project bind", path: []string{"project", "bind"}, want: "ghost project bind <project-id> <checkout-directory>"},
	{name: "version", path: []string{"version"}, want: "ghost version"},
}

// TestRunCLI_HelpPrintsUsageExitsZeroWritesNothing drives the dispatch with
// each command's -h and --help. The injected dispatch records instead of
// running, so the test asserts the half that matters beyond the text: a help
// request returns 0 and never reaches a command, and nothing appears under the
// isolated HOME/XDG/TMPDIR roots.
func TestRunCLI_HelpPrintsUsageExitsZeroWritesNothing(t *testing.T) {
	roots := isolatedHelpFS(t)
	for _, tc := range helpCases {
		for _, flag := range []string{"-h", "--help"} {
			t.Run(tc.name+" "+flag, func(t *testing.T) {
				argv := append([]string{}, tc.path...)
				argv = append(argv, tc.pre...)
				argv = append(argv, flag)

				var dispatched []string
				stdout, stderr := captureStreams(t, func() {
					code := runCLI(argv, func(d []string) int {
						dispatched = d
						return 0
					})
					if code != 0 {
						t.Errorf("exit code = %d, want 0 (help is a question, not an error)", code)
					}
				})

				if dispatched != nil {
					t.Errorf("help request reached the command dispatch: %v", dispatched)
				}
				if !strings.HasPrefix(stdout, "Usage: ghost ") {
					t.Errorf("stdout = %q, want it to open with the usage line", stdout)
				}
				if !strings.Contains(stdout, tc.want) {
					t.Errorf("stdout = %q, want it to contain %q", stdout, tc.want)
				}
				if !strings.HasSuffix(stdout, "\n") {
					t.Errorf("stdout = %q, want a trailing newline", stdout)
				}
				if stderr != "" {
					t.Errorf("stderr = %q, want nothing on the error stream", stderr)
				}
				assertNoFiles(t, roots)
			})
		}
	}
}

// TestRunCLI_DispatchesWithoutHelp is the other half of the same seam: an
// ordinary invocation must still reach the dispatch untouched, so the test
// above cannot pass by never dispatching anything. It also pins that the
// operands of value-taking flags stay values — a project named "-h" is passed
// with the verbatim --project form and must run, not print help.
func TestRunCLI_DispatchesWithoutHelp(t *testing.T) {
	for _, argv := range [][]string{
		nil,
		{"frobnicate"},
		{"mcp", "init"},
		{"reflect", "myproject"},
		{"resolve", "--project", "-h"},
		{"supersede", "--project", "ghost", "--apply"},
		{"project", "bind", "infra", "/tmp/checkout"},
	} {
		var got []string
		stdout, stderr := captureStreams(t, func() {
			code := runCLI(argv, func(d []string) int {
				got = d
				return 0
			})
			if code != 0 {
				t.Errorf("runCLI(%v) code = %d, want 0", argv, code)
			}
		})
		if !reflect.DeepEqual(got, argv) {
			t.Errorf("dispatch got %v, want %v", got, argv)
		}
		if stdout != "" || stderr != "" {
			t.Errorf("runCLI(%v) wrote stdout %q / stderr %q, want nothing", argv, stdout, stderr)
		}
	}
}

// TestWantsHelp pins the flag scan itself, including the rule that makes a
// dash-leading project name pass through untouched: the token after a
// value-taking flag is that flag's value, never a help request.
func TestWantsHelp(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "short", args: []string{"-h"}, want: true},
		{name: "long", args: []string{"--help"}, want: true},
		{name: "after a boolean flag", args: []string{"--apply", "-h"}, want: true},
		{name: "after a value flag's value", args: []string{"--tier", "cli", "--help"}, want: true},
		{name: "after several value flags", args: []string{"--project", "p", "--source", "cli", "-h"}, want: true},
		{name: "inline value flag then help", args: []string{"--client=claude", "-h"}, want: true},
		{name: "project value is a name, not help", args: []string{"--project", "-h"}, want: false},
		{name: "source value is a name, not help", args: []string{"--source", "--help"}, want: false},
		{name: "inline project value is a name, not help", args: []string{"--project=--help"}, want: false},
		{name: "no args", args: nil, want: false},
		{name: "ordinary args", args: []string{"myproject", "--apply"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wantsHelp(tc.args); got != tc.want {
				t.Errorf("wantsHelp(%v) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestHelp_ProjectBindStaysByteIdentical: #612 gave `project bind` its own
// -h handling first. Centralising the check must not change what a user sees
// there — same text, same stream, still no dispatch.
func TestHelp_ProjectBindStaysByteIdentical(t *testing.T) {
	var dispatched []string
	stdout, stderr := captureStreams(t, func() {
		code := runCLI([]string{"project", "bind", "-h"}, func(d []string) int {
			dispatched = d
			return 1
		})
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	})
	if dispatched != nil {
		t.Errorf("help request reached the command dispatch: %v", dispatched)
	}
	if stdout != projectBindUsage {
		t.Errorf("stdout = %q, want exactly projectBindUsage (%q)", stdout, projectBindUsage)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want nothing", stderr)
	}
}
