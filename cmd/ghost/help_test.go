package main

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

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

// TestHelpCommandPrintsTheCommandUsage is the third #637 finding: `ghost help
// <cmd>` printed the top-level summary whatever followed it, so the reader had
// to run the command to find out what it takes — and for a command whose parser
// errors on the arguments they typed, that is the wrong way round. The named
// command's own usage goes to stdout, the same text its -h prints, and a name
// that is not a command is reported rather than answered with a list that does
// not contain it.
func TestHelpCommandPrintsTheCommandUsage(t *testing.T) {
	restoreDetectRemote := func(t *testing.T) {
		t.Helper()
		t.Cleanup(func() { memory.SetDetectRemote(nil) })
	}

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

	t.Run("a help token in place of a command", func(t *testing.T) {
		for _, tc := range []struct {
			argv   []string
			want   string // a phrase only the named command's usage prints
			report bool   // the name that matched nothing is named on stderr
		}{
			{argv: []string{"help", "-h"}},
			{argv: []string{"help", "--help"}},
			{argv: []string{"-h"}},
			{argv: []string{"--help"}},
			{argv: []string{"-h", "upgrade"}, want: "ghost upgrade [--allow-downgrade]"},
			{argv: []string{"--help", "upgrade"}, want: "ghost upgrade [--allow-downgrade]"},
			{argv: []string{"help", "-h", "upgrade"}, want: "ghost upgrade [--allow-downgrade]"},
			{argv: []string{"help", "--help", "upgrade"}, want: "ghost upgrade [--allow-downgrade]"},
			{argv: []string{"help", "-h", "-h"}},
			{argv: []string{"help", "--help", "-h", "upgrade"}, want: "ghost upgrade [--allow-downgrade]"},
			{argv: []string{"help", "-h", "frobnicate"}, report: true},
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
			reported := strings.Contains(stderr, "no command")
			if reported != tc.report {
				t.Errorf("`ghost %s` reported a command that does not exist = %v, want %v (stderr: %q)", strings.Join(tc.argv, " "), reported, tc.report, strings.SplitN(stderr, "\n", 2)[0])
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
