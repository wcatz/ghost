package scratch

import (
	"strings"
	"testing"
)

// TestIsReassessIDFile pins the recogniser Reap's id-file rule asks. It is a
// name-only test because that is all the rule ever sees: Reap pairs it with a
// regular-file check itself, so this decides "is this the name Ghost writes"
// and nothing more. The negatives are the names that live beside the real ones
// — a foreign repair, a longer suffix, uppercase hex, a timestamp in another
// spelling — because $GHOST_SCRATCH_DIR may name a directory somebody else
// writes to, and a match here is what authorizes a deletion.
func TestIsReassessIDFile(t *testing.T) {
	const valid = "supersede-reassess-myproj-20260927T220356Z-4242-0123456789abcdef.ids"

	tests := []struct {
		name string
		want bool
	}{
		// Exactly what ReassessTargetsName writes.
		{valid, true},
		{"supersede-reassess-my-proj.v2_x-20260927T220356Z-4242-0123456789abcdef.ids", true},
		{"supersede-reassess-a-20260927T220356Z-4242-0123456789abcdef.ids", true},
		// The sanitizer's length ceiling is 40 characters of project name, so 40
		// is the last accepted one and 41 is the first the writer can never produce.
		{"supersede-reassess-" + strings.Repeat("x", 40) + "-20260927T220356Z-4242-0123456789abcdef.ids", true},
		{"supersede-reassess-" + strings.Repeat("x", 41) + "-20260927T220356Z-4242-0123456789abcdef.ids", false},
		// Segments in the wrong place, or one of them missing.
		{"supersede-reassess-20260927T220356Z-4242-0123456789abcdef.ids", false}, // no project at all
		{"supersede-reassessmyproj-20260927T220356Z-4242-0123456789abcdef.ids", false},
		{"supersede-resolve-myproj-20260927T220356Z-4242-0123456789abcdef.ids", false},
		{"supersede-reassess-myproj-20260927T220356Z-4242-0123456789abcdef", false},
		{"supersede-reassess-myproj-20260927T220356Z-0123456789abcdef.ids", false}, // no run token
		// The timestamp's own spelling.
		{"supersede-reassess-myproj-20260927-220356Z-4242-0123456789abcdef.ids", false},
		{"supersede-reassess-myproj-20260927T220356-4242-0123456789abcdef.ids", false},
		// The run token's own spelling: a pid and 16 lowercase hex characters.
		{"supersede-reassess-myproj-20260927T220356Z-4x42-0123456789abcdef.ids", false},
		{"supersede-reassess-myproj-20260927T220356Z-4242-0123456789ABCDEF.ids", false},
		{"supersede-reassess-myproj-20260927T220356Z-4242-0123456789abcde.ids", false},
		{"supersede-reassess-myproj-20260927T220356Z-4242-0123456789abcdef0.ids", false},
		{"supersede-reassess-myproj-20260927T220356Z-4242-0123456789abcdeg.ids", false},
		{"supersede-reassess-myproj-20260927T220356Z-4242-0123456789abcdefX.ids", false},
		// The pid segment of the run token: a pid is never 0 and never carries
		// a leading zero, so neither spelling is one Ghost can have written.
		{"supersede-reassess-myproj-20260927T220356Z-0-0123456789abcdef.ids", false},
		{"supersede-reassess-myproj-20260927T220356Z-007-0123456789abcdef.ids", false},
		// Names that live in the same directory and must never qualify.
		{valid + ".txt", false},
		{valid + " ", false},
		{" " + valid, false},
		{"123-0123456789abcdef", false}, // a per-invocation directory
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReassessIDFile(tt.name); got != tt.want {
				t.Errorf("isReassessIDFile(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestReassessTargetsNameIsRecognisedByIDFileRule: the builder and the
// recogniser are one definition held in two places — cmd/ghost writes the name
// so it can read the list back, Reap reads it so it can collect it — so every
// name ReassessTargetsName can produce has to be a name isReassessIDFile
// accepts. The drift this guards against fails silently in exactly the
// direction nothing reports: the file is simply never collected. The awkward
// project names are the sanitizer's own cases, since it is the step that could
// widen the name past the signature.
func TestReassessTargetsNameIsRecognisedByIDFileRule(t *testing.T) {
	for _, project := range []string{
		"myproj",
		"my proj",
		"../../escape/odd name",
		"",
		"-dash-leading",
		"with.dots_and-dashes",
		"unicode—name",
		strings.Repeat("x", 80),
	} {
		name, err := ReassessTargetsName(project)
		if err != nil {
			t.Fatalf("ReassessTargetsName(%q): %v", project, err)
		}
		if !isReassessIDFile(name) {
			t.Errorf("ReassessTargetsName(%q) = %q, which Reap's id-file rule does not recognise", project, name)
		}
		if isScratchDirName(name) {
			t.Errorf("ReassessTargetsName(%q) = %q, which Reap reads as a per-invocation directory", project, name)
		}
		if !strings.HasPrefix(name, "supersede-reassess-") {
			t.Errorf("ReassessTargetsName(%q) = %q, want the repair name at its head", project, name)
		}
		if !strings.HasSuffix(name, ".ids") {
			t.Errorf("ReassessTargetsName(%q) = %q, want the .ids extension --only-file reads", project, name)
		}
	}
}
