package scratch

import (
	"strings"
	"testing"
)

// TestIsReassessIDFile pins the name-only recogniser Reap's id-file rule asks.
// $GHOST_SCRATCH_DIR may name a directory somebody else writes to, and a match
// here authorizes a deletion, so the negatives are the near-miss names.
func TestIsReassessIDFile(t *testing.T) {
	const valid = "supersede-reassess-myproj-20260927T220356Z-4242-0123456789abcdef.ids"
	const tail = "-20260927T220356Z-4242-0123456789abcdef.ids"

	tests := []struct {
		name string
		want bool
	}{
		{valid, true},
		{"supersede-reassess-my-proj.v2_x" + tail, true},
		// The sanitizer's ceiling is 40 characters of project name.
		{"supersede-reassess-" + strings.Repeat("x", 40) + tail, true},
		{"supersede-reassess-" + strings.Repeat("x", 41) + tail, false},
		{"supersede-reassess" + tail, false}, // no project
		{"supersede-resolve-myproj" + tail, false},
		{strings.TrimSuffix(valid, ".ids"), false},
		{"supersede-reassess-myproj-20260927T220356Z-0123456789abcdef.ids", false}, // no run token
		{"supersede-reassess-myproj-20260927T220356-4242-0123456789abcdef.ids", false},
		{"supersede-reassess-myproj-20260927T220356Z-4242-0123456789ABCDEF.ids", false},
		{"supersede-reassess-myproj-20260927T220356Z-4242-0123456789abcdef0.ids", false},
		// A pid is never 0 and never carries a leading zero.
		{"supersede-reassess-myproj-20260927T220356Z-0-0123456789abcdef.ids", false},
		{"supersede-reassess-myproj-20260927T220356Z-007-0123456789abcdef.ids", false},
		{valid + ".txt", false},
		{" " + valid, false},
		{"123-0123456789abcdef", false}, // a per-invocation directory
		{"", false},
	}
	for _, tt := range tests {
		if got := isReassessIDFile(tt.name); got != tt.want {
			t.Errorf("isReassessIDFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestReassessTargetsNameIsRecognisedByIDFileRule: cmd/ghost writes the name and
// Reap collects it, so every name ReassessTargetsName can produce must be one
// isReassessIDFile accepts, or the file is silently never collected. The odd
// project names are the sanitizer's own cases.
func TestReassessTargetsNameIsRecognisedByIDFileRule(t *testing.T) {
	for _, project := range []string{"myproj", "../../escape/odd name", "", "-dash-leading", "unicode—name", strings.Repeat("x", 80)} {
		name, err := ReassessTargetsName(project)
		if err != nil {
			t.Fatalf("ReassessTargetsName(%q): %v", project, err)
		}
		if !isReassessIDFile(name) || isScratchDirName(name) {
			t.Errorf("ReassessTargetsName(%q) = %q, want an id-file name Reap does not read as a directory", project, name)
		}
	}
}
