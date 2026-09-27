package memory

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestShellQuote pins the quoting itself, because the notice's claim is that the
// command it prints can be pasted into a shell and run. Each case is a spelling
// a POSIX shell reads back as exactly the input: a plain word comes back
// unchanged, so the common notice stays readable, and everything else is wrapped
// in single quotes with each embedded quote closed, escaped and reopened. The
// Windows path, the `$` and the backtick are the three that Go's %q gets wrong
// in the direction that matters — a path that does not exist, and a word the
// shell expands before Ghost ever sees it.
func TestShellQuote(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{in: "real-infra", want: "real-infra"},
		{in: "/home/ada/work/infra", want: "/home/ada/work/infra"},
		{in: "/home/ada/work/inf ra", want: "'/home/ada/work/inf ra'"},
		{in: `C:\Users\ada\infra`, want: `'C:\Users\ada\infra'`},
		{in: "/work/$HOME/infra", want: "'/work/$HOME/infra'"},
		{in: "/work/`id`/infra", want: "'/work/`id`/infra'"},
		{in: "/work/inf;ra", want: "'/work/inf;ra'"},
		{in: "/work/inf*ra", want: "'/work/inf*ra'"},
		{in: "/work/inf'ra", want: `'/work/inf'\''ra'`},
		{in: "/work/à/infra", want: "'/work/à/infra'"},
		{in: "/work/inf\nra", want: "'/work/inf\nra'"},
		{in: "", want: "''"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			if got := shellQuote(tc.in); got != tc.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestShellQuoteRoundTripsThroughAPOSIXShell is the claim the notice rests on:
// what it prints has to come back out of a shell as the one argument Ghost meant.
// It runs `sh`, the same real-program precedent TestMultiProcessSharedDatabase
// sets, and skips where there is no POSIX shell to run (Windows CI) or under
// -short. The table above pins the spelling either way; this pins that the
// spelling means what it says.
func TestShellQuoteRoundTripsThroughAPOSIXShell(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a shell; skipped under -short")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no POSIX shell available: %v", err)
	}
	for _, arg := range []string{
		"real-infra",
		"/home/ada/work/infra",
		"/home/ada/work/inf ra",
		`C:\Users\ada\infra`,
		"/work/$HOME/infra",
		"/work/`id`/infra",
		"/work/inf'ra",
		"/work/à/infra",
	} {
		t.Run(arg, func(t *testing.T) {
			out, err := exec.Command(sh, "-c", "printf '%s' "+shellQuote(arg)).Output()
			if err != nil {
				t.Fatalf("sh -c with %s: %v", shellQuote(arg), err)
			}
			if string(out) != arg {
				t.Errorf("the shell read %s back as %q, want %q: the notice would name a project the reader cannot select", shellQuote(arg), out, arg)
			}
		})
	}
}

// TestBindingRefusalNoticeShellQuotesItsCommands pins the notice end to end, in
// the shape the reader gets it: a Windows path, a space and a `$` in another id,
// and a plain id beside them, so the test cannot pass by quoting everything or by
// quoting nothing. The Go-quoted form this replaces put `C:\\work\\infra` in the
// sentence — a path that does not exist — and left `/work/$HOME` inside double
// quotes, which the shell expands before the argument is ever passed.
func TestBindingRefusalNoticeShellQuotesItsCommands(t *testing.T) {
	for _, tc := range []struct {
		name string
		held string
		save string
		want []string
	}{
		{
			name: "plain ids are left bare",
			held: "real-infra",
			save: "/home/ada/work/infra",
			want: []string{
				"ghost project merge /home/ada/work/infra real-infra",
				"ghost project bind real-infra /home/ada/work/infra",
			},
		},
		{
			name: "a windows path is quoted, backslashes intact",
			held: `C:\work\infra`,
			save: `C:\Users\ada\Downloads\infra`,
			want: []string{
				`ghost project merge 'C:\Users\ada\Downloads\infra' 'C:\work\infra'`,
				`ghost project bind 'C:\work\infra' 'C:\Users\ada\Downloads\infra'`,
			},
		},
		{
			name: "a space and a shell metacharacter are quoted",
			held: "/work/à repo",
			save: "/work/$HOME/infra",
			want: []string{
				"ghost project merge '/work/$HOME/infra' '/work/à repo'",
				"ghost project bind '/work/à repo' '/work/$HOME/infra'",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notice := (&BindingRefusal{
				Kind:           RefusedPathMismatch,
				Name:           "infra",
				ProjectIDs:     []string{tc.held},
				CandidateCount: 1,
				RecordedPath:   "/somewhere/else",
				SavedTo:        tc.save,
			}).Notice()
			for _, want := range tc.want {
				if !strings.Contains(notice, want) {
					t.Errorf("notice does not suggest %q: %q", want, notice)
				}
			}
			// The Go-quoted spelling is the regression this replaced, and it is
			// absent from the prose too: the sentence quotes only the project
			// name, so the two forms cannot both be in the text.
			for _, arg := range []string{tc.save, tc.held} {
				if goQuoted := strconv.Quote(arg); strings.Contains(notice, goQuoted) {
					t.Errorf("notice still carries the Go-quoted form %s of %q: a shell would not read that back as the argument", goQuoted, arg)
				}
			}
		})
	}
}
