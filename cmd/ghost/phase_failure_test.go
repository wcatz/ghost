package main

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// TestPhaseFailureCausePrefersRealOutput: when a phase printed something, that
// is the evidence — the marker must carry it rather than the generic
// "exit status 1" that 357 entries in lifecycle.log carried with nothing to
// distinguish them (issue #540).
func TestPhaseFailureCausePrefersRealOutput(t *testing.T) {
	got := phaseFailureCause(
		[]string{"reflect", "--project", "ghost", "--apply"},
		errors.New("exit status 1"),
		"FileSystem.writeFile (/tmp/ghost-opencode-x/opencode/.gitignore): EACCES\n",
	)

	if strings.Contains(got, "exit status 1\n") && !strings.Contains(got, "EACCES") {
		t.Errorf("cause is the generic error rather than the child's output: %q", got)
	}
	if !strings.Contains(got, "EACCES") {
		t.Errorf("child output missing from the cause: %q", got)
	}
}

// TestPhaseFailureCauseOmitsPromptSizedArguments: the fallback records argv, so
// an argument carrying the prompt must not be echoed into a marker that gets
// printed at session start for every project session to read.
func TestPhaseFailureCauseOmitsPromptSizedArguments(t *testing.T) {
	long := "SYSTEM instructions: " + strings.Repeat("summarise this memory. ", 40)
	got := phaseFailureCause(
		[]string{"reflect", "--project", "ghost", long},
		errors.New("exit status 1"),
		"",
	)

	if strings.Contains(got, "summarise this memory") {
		t.Errorf("prompt-sized argument was written into the cause:\n%s", got)
	}
	if !strings.Contains(got, promptOmittedMarker) {
		t.Errorf("omitted argument left no marker, so the reader cannot tell one was dropped:\n%s", got)
	}
	// Short arguments must survive intact. The project operand is rendered
	// through memory.ProjectArg, so it keeps its quotes around the same value.
	if !strings.Contains(got, `--project "ghost"`) {
		t.Errorf("ordinary arguments were dropped too:\n%s", got)
	}
}

// TestPhaseFailureCauseCarriesRealExitCode: "exit 1" must come from the
// process's actual status, not from echoing runErr.Error() back — otherwise
// the fallback adds nothing over what the marker already recorded.
//
// A real subprocess rather than a constructed &exec.ExitError{}, which carries
// a nil ProcessState and panics in Error() and ExitCode().
func TestPhaseFailureCauseCarriesRealExitCode(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 3")
	runErr := cmd.Run()
	if runErr == nil {
		t.Fatal("precondition: expected the command to fail")
	}

	got := phaseFailureCause([]string{"reflect", "--project", "ghost"}, runErr, "")
	if !strings.Contains(got, "exit 3") {
		t.Errorf("exit code not recorded from the real status: %q", got)
	}
	if !strings.Contains(got, `--project "ghost"`) {
		t.Errorf("argv missing: %q", got)
	}
}

// splitTailToken is obviously fake, shaped by the detector's GitHub personal
// access token rule.
const splitTailToken = "ghp_" + "dddddddddddddddddddddddddddddddddddd"

// TestPhaseTailDropsAPartialFirstLine: the tail is cut from the head, so the
// cut can land inside a token. The fragment that survives has lost its ghp_
// prefix, so the token guard downstream cannot recognise it, and as the first
// line it is exactly what the session-start alert shows. A kept tail that
// starts mid-line must drop that partial line.
func TestPhaseTailDropsAPartialFirstLine(t *testing.T) {
	line := "harness: request rejected for " + splitTailToken + "\n"
	last := "error: consolidation failed\n"
	// Cap the tail so the cut lands 10 bytes into the token.
	cut := strings.Index(line, splitTailToken) + 10
	tail := newPhaseTail(len(line) - cut + len(last))
	for _, s := range []string{line, last} {
		if _, err := tail.Write([]byte(s)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	got := tail.String()
	if fragment := splitTailToken[10:]; strings.Contains(got, fragment) {
		t.Errorf("the tail keeps the split token's fragment %q:\n%q", fragment, got)
	}
	if got != last {
		t.Errorf("tail = %q, want only the whole line after the cut %q", got, last)
	}
}

// TestPhaseTailOfOnePartialLineIsAMarker: when the whole kept tail is one
// partial line there is no whole line to keep, so a fixed marker stands in
// rather than the fragment.
func TestPhaseTailOfOnePartialLineIsAMarker(t *testing.T) {
	tail := newPhaseTail(30)
	if _, err := tail.Write([]byte("harness: request rejected for " + splitTailToken)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := tail.String(); got != phaseTailTrimmedMarker {
		t.Errorf("tail = %q, want %q", got, phaseTailTrimmedMarker)
	}
}

// TestPhaseTailKeepsAWholeFirstLine: a cut that lands exactly after a newline
// leaves a whole first line, which must be kept.
func TestPhaseTailKeepsAWholeFirstLine(t *testing.T) {
	first, second := "dropped line\n", "kept line\nlast line\n"
	tail := newPhaseTail(len(second))
	for _, s := range []string{first, second} {
		if _, err := tail.Write([]byte(s)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if got := tail.String(); got != second {
		t.Errorf("tail = %q, want %q", got, second)
	}
}
