package main

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
		// Plain mode (no --audit) used to drop every one of these and render a
		// live block with exit 0 (#1195).
		{"plain unknown flag", []string{"context", "--bogus"}},
		{"plain unknown flag equals form", []string{"context", "--json=true"}},
		{"plain stray positional", []string{"context", "somewhere"}},
		{"plain project", []string{"context", "--project", "ghost"}},
		{"plain project equals form", []string{"context", "--project=ghost"}},
		{"plain since", []string{"context", "--since", "1h"}},
		{"plain since equals form", []string{"context", "--since=1h"}},
		{"plain cwd with no value", []string{"context", "--cwd"}},
		{"plain cwd twice", []string{"context", "--cwd", "/a", "--cwd=/b"}},
		{"plain as-of twice", []string{"context", "--as-of", "2026-01-01T00:00:00Z", "--as-of=2026-02-01T00:00:00Z"}},
		{"plain as-of with no value", []string{"context", "--as-of"}},
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
