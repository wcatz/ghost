package memory

import (
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"real-infra", "real-infra"},
		{"/home/ada/work/infra", "/home/ada/work/infra"},
		{"/home/ada/work/inf ra", "'/home/ada/work/inf ra'"},
		{`C:\Users\ada\infra`, `'C:\Users\ada\infra'`},
		{"/work/$HOME/infra", "'/work/$HOME/infra'"},
		{"/work/`id`/infra", "'/work/`id`/infra'"},
		{"/work/inf;ra", "'/work/inf;ra'"},
		{"/work/inf*ra", "'/work/inf*ra'"},
		{"/work/inf'ra", `'/work/inf'\''ra'`},
		{"/work/à/infra", "'/work/à/infra'"},
		{"/work/inf\nra", "'/work/inf\nra'"},
		{"", "''"},
	} {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestShellQuoteRoundTripsThroughAPOSIXShell(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	for _, arg := range []string{"real-infra", "/home/ada/work/infra", "/home/ada/work/inf ra", `C:\Users\ada\infra`, "/work/$HOME/infra", "/work/`id`/infra", "/work/inf'ra", "/work/à/infra"} {
		out, err := exec.Command(sh, "-c", "printf '%s' "+shellQuote(arg)).Output()
		if err != nil {
			t.Fatalf("%q: %v", arg, err)
		}
		if string(out) != arg {
			t.Errorf("got %q want %q", out, arg)
		}
	}
}

func TestBindingRefusalNoticeShellQuotesItsCommands(t *testing.T) {
	for _, tc := range []struct {
		held, save string
		want       []string
	}{
		{"real-infra", "/home/ada/work/infra", []string{"ghost project merge /home/ada/work/infra real-infra", "ghost project bind real-infra /home/ada/work/infra"}},
		{`C:\work\infra`, `C:\Users\ada\Downloads\infra`, []string{`ghost project merge 'C:\Users\ada\Downloads\infra' 'C:\work\infra'`, `ghost project bind 'C:\work\infra' 'C:\Users\ada\Downloads\infra'`}},
		{"/work/à repo", "/work/$HOME/infra", []string{"ghost project merge '/work/$HOME/infra' '/work/à repo'", "ghost project bind '/work/à repo' '/work/$HOME/infra'"}},
	} {
		notice := (&BindingRefusal{Kind: RefusedPathMismatch, Name: "infra", ProjectIDs: []string{tc.held}, CandidateCount: 1, RecordedPath: "/somewhere/else", SavedTo: tc.save}).Notice()
		for _, want := range tc.want {
			if !strings.Contains(notice, want) {
				t.Errorf("missing %q in %q", want, notice)
			}
		}
		for _, arg := range []string{tc.save, tc.held} {
			if q := strconv.Quote(arg); strings.Contains(notice, q) {
				t.Errorf("notice still has Go-quoted %s", q)
			}
		}
	}
}
