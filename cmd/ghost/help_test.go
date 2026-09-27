package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
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
	{name: "opencode", path: []string{"opencode"}, want: "ghost opencode cleanup-sessions [--grace <duration>]"},
	{name: "opencode cleanup-sessions", path: []string{"opencode", "cleanup-sessions"}, want: "ghost opencode cleanup-sessions [--grace <duration>]"},
	{name: "opencode cleanup-sessions with grace", path: []string{"opencode", "cleanup-sessions"}, pre: []string{"--grace", "24h"}, want: "ghost opencode cleanup-sessions [--grace <duration>]"},
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
		{"opencode", "cleanup-sessions"},
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
// value-taking flag of THAT command is that flag's value, never a help request,
// and a flag the command does not take is a typo rather than a value.
func TestWantsHelp(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		args    []string
		want    bool
	}{
		{name: "short", command: "reflect", args: []string{"-h"}, want: true},
		{name: "long", command: "reflect", args: []string{"--help"}, want: true},
		{name: "after a boolean flag", command: "reflect", args: []string{"--apply", "-h"}, want: true},
		{name: "after a value flag's value", command: "reflect", args: []string{"--tier", "cli", "--help"}, want: true},
		{name: "after several value flags", command: "reflect", args: []string{"--project", "p", "--source", "cli", "-h"}, want: true},
		{name: "inline value flag then help", command: "mcp init", args: []string{"--client=claude", "-h"}, want: true},
		{name: "project value is a name, not help", command: "reflect", args: []string{"--project", "-h"}, want: false},
		{name: "source value is a name, not help", command: "hook", args: []string{"--source", "--help"}, want: false},
		{name: "inline project value is a name, not help", command: "reflect", args: []string{"--project=--help"}, want: false},
		{name: "a value flag of another command is not a value", command: "upgrade", args: []string{"--cwd", "-h"}, want: true},
		{name: "another command's value flag, long form", command: "backup", args: []string{"--project", "--help"}, want: true},
		{name: "after end of options", command: "reflect", args: []string{"--", "-h"}, want: false},
		{name: "after end of options, long form", command: "reflect", args: []string{"--project", "p", "--", "--help"}, want: false},
		{name: "end of options for a command that takes no value flags", command: "upgrade", args: []string{"--", "--cwd", "-h"}, want: false},
		{name: "no args", command: "reflect", args: nil, want: false},
		{name: "ordinary args", command: "reflect", args: []string{"myproject", "--apply"}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wantsHelp(tc.command, tc.args); got != tc.want {
				t.Errorf("wantsHelp(%q, %v) = %v, want %v", tc.command, tc.args, got, tc.want)
			}
		})
	}
}

// TestRunCLI_ForeignValueFlagDoesNotSwallowHelp is the #637 finding at the seam
// it was reported through: `ghost upgrade --cwd -h` used to be read as --cwd
// taking "-h" as its value, because one table listed every value flag in the CLI
// and the scan did not know which command it was scanning. The consequence was
// not a missed usage line — it was the upgrade RUNNING, with the help flag
// consumed, on the one command that replaces the running binary. A flag a
// command does not take has to leave the next token to the scan, which is what
// makes the -h the user typed a help request.
func TestRunCLI_ForeignValueFlagDoesNotSwallowHelp(t *testing.T) {
	roots := isolatedHelpFS(t)
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{name: "upgrade with context's flag", argv: []string{"upgrade", "--cwd", "-h"}, want: "ghost upgrade"},
		{name: "upgrade with context's flag, long form", argv: []string{"upgrade", "--cwd", "--help"}, want: "ghost upgrade"},
		{name: "backup with mcp's flag", argv: []string{"backup", "--client", "-h"}, want: "ghost backup"},
		{name: "context with opencode's flag", argv: []string{"context", "--grace", "1h", "-h"}, want: "ghost context"},
		{name: "reflect with obsidian's flag", argv: []string{"reflect", "--interval", "5s", "-h"}, want: "ghost reflect"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dispatched []string
			stdout, stderr := captureStreams(t, func() {
				code := runCLI(tc.argv, func(d []string) int {
					dispatched = d
					return 0
				})
				if code != 0 {
					t.Errorf("exit code = %d, want 0", code)
				}
			})
			if dispatched != nil {
				t.Errorf("%v reached the command dispatch: %v", tc.argv, dispatched)
			}
			if !strings.Contains(stdout, tc.want) {
				t.Errorf("stdout = %q, want the usage of the command that was invoked (%q)", stdout, tc.want)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want nothing", stderr)
			}
			assertNoFiles(t, roots)
		})
	}
}

// TestRunCLI_EndOfOptionsIsNotHelp pins that "--" ends the option scan, which is
// what lets a project literally named --help be addressed at all: after it every
// token is an operand, so the scan must stop rather than read one as a request
// for usage. The dispatch still gets argv untouched, so the command's own parser
// decides what the operand means.
func TestRunCLI_EndOfOptionsIsNotHelp(t *testing.T) {
	for _, argv := range [][]string{
		{"reflect", "--", "-h"},
		{"reflect", "--project", "ghost", "--", "--help"},
		{"resolve", "--", "--help"},
	} {
		var got []string
		stdout, stderr := captureStreams(t, func() {
			if code := runCLI(argv, func(d []string) int {
				got = d
				return 0
			}); code != 0 {
				t.Errorf("runCLI(%v) code = %d, want 0", argv, code)
			}
		})
		if !reflect.DeepEqual(got, argv) {
			t.Errorf("runCLI(%v) dispatched %v, want the argv untouched: a help request must not swallow operands", argv, got)
		}
		if stdout != "" || stderr != "" {
			t.Errorf("runCLI(%v) wrote stdout %q / stderr %q, want nothing", argv, stdout, stderr)
		}
	}
}

// TestHelpCommandPrintsTheCommandUsage is the third #637 finding: `ghost help
// <cmd>` printed the top-level summary whatever followed it, so the reader had
// to run the command to find out what it takes — and for a command whose parser
// errors on the arguments they typed, that is the wrong way round. The named
// command's own usage goes to stdout, the same text its -h prints, and a name
// that is not a command is reported rather than answered with a list that does
// not contain it.
func TestHelpCommandPrintsTheCommandUsage(t *testing.T) {
	// restoreDetectRemote undoes the one global the dispatch wires on its way
	// to the help case (memory.SetDetectRemote), so a test that drives the real
	// dispatch leaves the package the way it found it: no repository detector,
	// which is the default the rest of this package's tests assume.
	restoreDetectRemote := func(t *testing.T) {
		t.Helper()
		t.Cleanup(func() { memory.SetDetectRemote(nil) })
	}

	// Each case names a phrase only that command's usage can print, so a
	// top-level summary or a sibling's usage cannot pass it. The real dispatch
	// is driven, because routing `help <cmd>` to that command's usage is the
	// change being tested, and reaching the help case in it touches nothing
	// else: no store, no config, no harness.
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{name: "upgrade", argv: []string{"help", "upgrade"}, want: "ghost upgrade [--allow-downgrade]"},
		{name: "reflect", argv: []string{"help", "reflect"}, want: "ghost reflect <project> [flags]"},
		{name: "mcp init", argv: []string{"help", "mcp", "init"}, want: "ghost mcp init [--client"},
		{name: "project bind", argv: []string{"help", "project", "bind"}, want: "ghost project bind <project-id> <checkout-directory>"},
		{name: "opencode cleanup-sessions", argv: []string{"help", "opencode", "cleanup-sessions"}, want: "ghost opencode cleanup-sessions [--grace <duration>]"},
		{name: "version", argv: []string{"help", "version"}, want: "ghost version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			roots := isolatedHelpFS(t)
			restoreDetectRemote(t)
			var code int
			stdout, stderr := captureStreams(t, func() { code = dispatchCommand(tc.argv) })
			if code != 0 {
				t.Errorf("exit code = %d, want 0: a question is not a mistake", code)
			}
			if !strings.HasPrefix(stdout, "Usage: ghost ") {
				t.Errorf("stdout = %q, want it to open with the usage line of the named command", stdout)
			}
			if !strings.Contains(stdout, tc.want) {
				t.Errorf("stdout = %q, want it to contain %q", stdout, tc.want)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want nothing", stderr)
			}
			assertNoFiles(t, roots)
		})
	}

	t.Run("no command", func(t *testing.T) {
		restoreDetectRemote(t)
		stdout, stderr := captureStreams(t, func() { dispatchCommand([]string{"help"}) })
		if stdout != "" {
			t.Errorf("stdout = %q, want the top-level summary to stay on stderr as it always was", stdout)
		}
		if !strings.Contains(stderr, "ghost <command>") {
			t.Errorf("stderr = %q, want the top-level command list", stderr)
		}
	})

	// A help token is not a mistyped command: `ghost help -h` is the same
	// question asked twice, and the diagnostic for a name that matched nothing
	// would be a lie about a token that is a request. `ghost -h <command>` is the
	// named-command form through the other spelling, and is documented as such.
	t.Run("a help token in place of a command", func(t *testing.T) {
		for _, tc := range []struct {
			argv []string
			want string // a phrase only the named command's usage prints
		}{
			{argv: []string{"help", "-h"}},
			{argv: []string{"help", "--help"}},
			{argv: []string{"-h"}},
			{argv: []string{"--help"}},
			{argv: []string{"-h", "upgrade"}, want: "ghost upgrade [--allow-downgrade]"},
			{argv: []string{"--help", "upgrade"}, want: "ghost upgrade [--allow-downgrade]"},
			// The same question asked twice, with the command after it: dropping
			// the token is what keeps the two spellings agreeing on which text
			// names the command, so this has to print upgrade's usage and not the
			// summary with the name silently discarded.
			{argv: []string{"help", "-h", "upgrade"}, want: "ghost upgrade [--allow-downgrade]"},
			{argv: []string{"help", "--help", "upgrade"}, want: "ghost upgrade [--allow-downgrade]"},
		} {
			restoreDetectRemote(t)
			var code int
			stdout, stderr := captureStreams(t, func() { code = dispatchCommand(tc.argv) })
			if code != 0 {
				t.Errorf("`ghost %s` exit code = %d, want 0", strings.Join(tc.argv, " "), code)
			}
			if tc.want != "" {
				if !strings.Contains(stdout, tc.want) {
					t.Errorf("`ghost %s` stdout = %q, want the usage of the command it names", strings.Join(tc.argv, " "), stdout)
				}
				continue
			}
			if stdout != "" {
				t.Errorf("`ghost %s` stdout = %q, want the top-level summary to stay on stderr as it always was", strings.Join(tc.argv, " "), stdout)
			}
			if !strings.Contains(stderr, "ghost <command>") {
				t.Errorf("`ghost %s` stderr = %q, want the top-level command list", strings.Join(tc.argv, " "), stderr)
			}
			if strings.Contains(stderr, "no command") {
				t.Errorf("`ghost %s` reported a help request as a command that does not exist: %q", strings.Join(tc.argv, " "), strings.SplitN(stderr, "\n", 2)[0])
			}
		}
	})

	t.Run("unknown command", func(t *testing.T) {
		restoreDetectRemote(t)
		var code int
		stdout, stderr := captureStreams(t, func() { code = dispatchCommand([]string{"help", "frobnicate"}) })
		if code != 0 {
			t.Errorf("exit code = %d, want 0: a question with no answer is not a mistake", code)
		}
		if stdout != "" {
			t.Errorf("stdout = %q, want the top-level summary to stay on stderr as it always was", stdout)
		}
		if !strings.Contains(stderr, `no command "frobnicate"`) {
			t.Errorf("stderr = %q, want the word that matched no command named", stderr)
		}
		if !strings.Contains(stderr, "ghost <command>") {
			t.Errorf("stderr = %q, want the top-level command list as the fallback", stderr)
		}
	})
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

// TestHelp_ValueFlagsSkipTheirValue checks the per-command table from the
// consumer's side: for every flag registered for a command, `-h`/`--help` in the
// value position is a VALUE (no help request), while a help request after a real
// value still reads as help — the skip must narrow the scan, not blind it.
func TestHelp_ValueFlagsSkipTheirValue(t *testing.T) {
	if len(helpValueFlagsByCommand) == 0 {
		t.Fatal("helpValueFlagsByCommand is empty; the value-flag table is missing")
	}
	for command, flags := range helpValueFlagsByCommand {
		if len(flags) == 0 {
			t.Errorf("%s is registered with no value flags; a command that takes none must not be a key", command)
		}
		for flag := range flags {
			t.Run(command+" "+flag, func(t *testing.T) {
				for _, help := range []string{"-h", "--help"} {
					if wantsHelp(command, []string{flag, help}) {
						t.Errorf("wantsHelp(%q, %v) = true, want false: %s in value position is a value, not help", command, []string{flag, help}, flag)
					}
					if !wantsHelp(command, []string{flag, "value", help}) {
						t.Errorf("wantsHelp(%q, %v) = false, want true: %s's value then help is still a help request", command, []string{flag, "value", help}, flag)
					}
				}
			})
		}
	}
}

// valueFlagOwners maps each function in this package that parses a value flag to
// the subcommand paths whose help scan has to honour those flags. One function
// can serve two commands (parseMCPClient is how both `mcp init` and `mcp status`
// read --client), and a command can parse inline rather than through a parse*
// helper (runContext, runHook), so the map is keyed by function name and every
// owner of that function has to register every flag it parses.
//
// A function absent from here that parses a value flag fails the scan: the table
// is a hand-maintained mirror of the parsers, and a parser missing from it is one
// whose --flag value the scan will read as a help request.
var valueFlagOwners = map[string][]string{
	"parseMCPClient":           {"mcp init", "mcp status"},
	"parseLifecycleArgs":       {"lifecycle"},
	"parseReflectArgs":         {"reflect"},
	"parseSupersedeArgs":       {"supersede"},
	"parseResolveArgs":         {"resolve"},
	"parseObsidianFlags":       {"obsidian"},
	"parseBackupArgs":          {"backup"},
	"parseExportArgs":          {"export"},
	"parseCleanupSessionsArgs": {"opencode cleanup-sessions"},
	"runContext":               {"context"},
	"runHook":                  {"hook"},
}

// TestHelp_ValueFlagsCoverEveryParser enforces both halves of the
// helpValueFlagsByCommand contract, per command, which the comment can only ask
// for: the map is a hand-maintained mirror of every flag that consumes the next
// token, and a value flag added to a parser without registering it would make
// `ghost <cmd> --newflag -h` print usage and exit 0 instead of running the
// command (or reporting the unknown flag). The scan reads this package's own
// sources for case clauses that match a flag-shaped literal (`-x` or `--x`;
// -h/--help excluded, they are the request itself) and advance the argument
// index — `++`, `+ 1` or `+= 1` on an identifier that also appears indexed
// inside the clause or its switch tag (`args[i]`), or an `args = args[1:]`
// reslice of that same identifier. Boolean flags like --apply never advance it,
// and an unrelated counter bump or string trim is not mistaken for parsing (see
// advancesIndex for why the loose reading would break the contract instead of
// protecting it), while the common refactors to the idiom stay covered.
//
// Both directions are checked for EVERY command rather than once for the whole
// CLI, which is the only level at which this is a guard: a flag registered for
// the wrong command is the bug the per-command table exists to fix, so a union
// that merely matched the scan would let it back in. Detection is a source-shape
// heuristic: a parser that reaches for its next token any other way has to keep
// this scan honest by not looking like a parser, or by extending it here in the
// same commit.
func TestHelp_ValueFlagsCoverEveryParser(t *testing.T) {
	// A bare flag token, optionally in the attached `--flag=value` form.
	// Error messages ("--project requires a value", "--client … (claude,
	// …)") can never match: the shape admits no whitespace. -h/--help are
	// excluded separately below — they are the request itself, never a
	// value flag.
	flagTokenRE := regexp.MustCompile(`^--?[a-zA-Z0-9][a-zA-Z0-9-]*(=[^ \t\n]*)?$`)
	neverValue := map[string]bool{"-h": true, "--help": true}
	fset := token.NewFileSet()
	// found maps a subcommand path to the value flags its own parsers consume.
	found := map[string]map[string]bool{}
	markFound := func(command, flag string) {
		if found[command] == nil {
			found[command] = map[string]bool{}
		}
		found[command][flag] = true
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			owners, owned := valueFlagOwners[fd.Name.Name]
			// A switch tag lives outside every CaseClause, so seed each clause
			// with the index expressions of its enclosing switch's tag:
			// `switch args[i] { case "--x": i++ }` has no IndexExpr in the
			// clause at all, and a parser written that way would otherwise be
			// invisible to the scan — the silent direction of this guard.
			tagIndexed := map[*ast.CaseClause]map[string]bool{}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				sw, ok := n.(*ast.SwitchStmt)
				if !ok || sw.Tag == nil {
					return true
				}
				tag := indexExprIdents(sw.Tag)
				for _, stmt := range sw.Body.List {
					if cc, ok := stmt.(*ast.CaseClause); ok {
						tagIndexed[cc] = tag
					}
				}
				return true
			})

			ast.Inspect(fd.Body, func(n ast.Node) bool {
				cc, ok := n.(*ast.CaseClause)
				if !ok {
					return true
				}
				indexed := indexExprIdents(cc)
				for name, v := range tagIndexed[cc] {
					indexed[name] = v
				}
				if !advancesIndex(indexed, cc) {
					return true
				}
				ast.Inspect(cc, func(m ast.Node) bool {
					lit, ok := m.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						return true
					}
					s, err := strconv.Unquote(lit.Value)
					if err != nil || !flagTokenRE.MatchString(s) {
						return true
					}
					if i := strings.IndexByte(s, '='); i >= 0 {
						s = s[:i]
					}
					if neverValue[s] {
						return true
					}
					if !owned {
						t.Errorf("%s parses %s as a value flag but is not in valueFlagOwners: "+
							"`ghost <cmd> %s -h` would print usage instead of running the command",
							fd.Name.Name, s, s)
						return true
					}
					for _, command := range owners {
						markFound(command, s)
					}
					return true
				})
				return true
			})
		}
	}

	for command, flags := range found {
		if _, registered := usageByCommand[command]; !registered {
			t.Errorf("valueFlagOwners names %q, which is not a command in usageByCommand: the help scan can never reach it", command)
		}
		for flag := range flags {
			if !helpValueFlagsByCommand[command][flag] {
				t.Errorf("%s consumes %s but it is missing from helpValueFlagsByCommand[%q]: "+
					"`ghost %s %s -h` would print usage instead of running the command", command, flag, command, command, flag)
			}
		}
	}
	for command, flags := range helpValueFlagsByCommand {
		if _, registered := usageByCommand[command]; !registered {
			t.Errorf("helpValueFlagsByCommand lists %q, which is not a command in usageByCommand: a dead entry", command)
		}
		for flag := range flags {
			if !found[command][flag] {
				t.Errorf("helpValueFlagsByCommand[%q] lists %s, but no parser owned by that command consumes the next token: "+
					"stale entry, or the parsers changed shape and this scan no longer sees them", command, flag)
			}
		}
	}
}

// advancesIndex reports whether cc advances the argument index — the signal
// that a matched flag consumed the following token instead of being a
// boolean flag of its own. The signal is tied to the index variable itself:
// an increment (`i++`), an `+= 1`, or a `+ 1` only counts when its
// identifier is in indexed — the identifiers appearing inside IndexExprs in
// the clause or its switch tag (`args[i]`, `os.Args[i]`, `args[i+1]`) — and
// a reslice only counts when it re-slices one of them with a literal low
// bound (`args = args[1:]`). Looser rules would mark unrelated clauses —
// `counts[m.Category]++`, `s = s[:i]` — as value-flag parsers, and the fix
// this test then suggests (register the flag) would make wantsHelp skip the
// token after a BOOLEAN flag, so `-h` behind it would run the command
// instead of printing usage: the very bug the guard exists to prevent.
func advancesIndex(indexed map[string]bool, cc *ast.CaseClause) bool {
	advance := false
	ast.Inspect(cc, func(n ast.Node) bool {
		if advance {
			return false
		}
		switch x := n.(type) {
		case *ast.IncDecStmt: // i++, idx++
			if id, ok := x.X.(*ast.Ident); ok && indexed[id.Name] {
				advance = true
			}
		case *ast.AssignStmt:
			switch {
			case x.Tok == token.ADD_ASSIGN && anyIntOne(x.Rhs) && anyLhsIndexed(x.Lhs, indexed): // i += 1
				advance = true
			default:
				for i, rhs := range x.Rhs {
					sl, ok := rhs.(*ast.SliceExpr)
					if !ok || i >= len(x.Lhs) {
						continue
					}
					lhs, okL := x.Lhs[i].(*ast.Ident)
					src, okR := sl.X.(*ast.Ident)
					if okL && okR && lhs.Name == src.Name && indexed[lhs.Name] && isIntOne(sl.Low) {
						advance = true // args = args[1:]
					}
				}
			}
		case *ast.BinaryExpr: // i + 1
			if x.Op != token.ADD {
				break
			}
			var other ast.Expr
			switch {
			case isIntOne(x.X) && !isIntOne(x.Y):
				other = x.Y
			case isIntOne(x.Y):
				other = x.X
			default:
				break
			}
			if id, ok := other.(*ast.Ident); ok && indexed[id.Name] {
				advance = true
			}
		}
		return true
	})
	return advance
}

// indexExprIdents returns every identifier appearing inside an IndexExpr
// under n — both the indexed slice (`args` in `args[i]`) and the index
// expression's variables, i.e. the names a parser advances or reslices.
// advancesIndex needs the slice name too: `args = args[1:]` matches on
// indexed[lhs.Name], where lhs is the slice, not the index.
func indexExprIdents(n ast.Node) map[string]bool {
	ids := map[string]bool{}
	ast.Inspect(n, func(m ast.Node) bool {
		ix, ok := m.(*ast.IndexExpr)
		if !ok {
			return true
		}
		collectIdents(ix.X, ids)
		collectIdents(ix.Index, ids)
		return true
	})
	return ids
}

// collectIdents adds every identifier appearing under n to ids.
func collectIdents(n ast.Node, ids map[string]bool) {
	ast.Inspect(n, func(m ast.Node) bool {
		if id, ok := m.(*ast.Ident); ok {
			ids[id.Name] = true
		}
		return true
	})
}

func anyLhsIndexed(lhs []ast.Expr, ids map[string]bool) bool {
	for _, e := range lhs {
		if id, ok := e.(*ast.Ident); ok && ids[id.Name] {
			return true
		}
	}
	return false
}

func anyIntOne(exprs []ast.Expr) bool {
	for _, e := range exprs {
		if isIntOne(e) {
			return true
		}
	}
	return false
}

func isIntOne(n ast.Node) bool {
	lit, ok := n.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "1"
}
