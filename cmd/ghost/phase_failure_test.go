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

// TestPhaseFailureCauseFallsBackToArgvWhenSilent is the case the issue leads
// with: opencode exiting 1 with EMPTY stderr, three phases failing in 1-2s
// each, and the marker recording nothing but "exit status 1". With no output,
// everything still knowable must be recorded — exit code, argv, version.
func TestPhaseFailureCauseFallsBackToArgvWhenSilent(t *testing.T) {
	// A real non-zero exit, not errors.New("exit status 1"): only an
	// *exec.ExitError carries a ProcessState, and the point of the fallback
	// is that it reports the actual code.
	cmd := exec.Command("sh", "-c", "exit 1")
	runErr := cmd.Run()
	if runErr == nil {
		t.Fatal("precondition: expected the command to fail")
	}

	got := phaseFailureCause(
		[]string{"reflect", "--project", "ghost", "--apply"},
		runErr,
		"   \n\t  ",
	)

	for _, want := range []string{"exit 1", "--project ghost", "reflect"} {
		if !strings.Contains(got, want) {
			t.Errorf("cause is missing %q — with empty stderr this is the only evidence:\n%s", want, got)
		}
	}
	if !strings.Contains(got, version) {
		t.Errorf("build version %q not recorded: %s", version, got)
	}
	// The whole point: not just "exit status 1".
	if strings.TrimSpace(got) == "exit status 1" {
		t.Errorf("cause degenerated to the generic error: %q", got)
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
	// Short arguments must survive intact.
	if !strings.Contains(got, "--project ghost") {
		t.Errorf("ordinary arguments were dropped too:\n%s", got)
	}
}

// TestPhaseFailureCauseNonExitError: a timeout or a missing binary never
// becomes an *exec.ExitError, and reporting "exit 1" for those would be a lie
// that points at the wrong thing entirely.
func TestPhaseFailureCauseNonExitError(t *testing.T) {
	got := phaseFailureCause(
		[]string{"reflect"},
		errors.New("context deadline exceeded"),
		"",
	)
	if !strings.Contains(got, "context deadline exceeded") {
		t.Errorf("non-exit error lost its cause: %q", got)
	}
	if strings.Contains(got, "exit 1") {
		t.Errorf("a timeout was reported as an exit code: %q", got)
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
	if !strings.Contains(got, "--project ghost") {
		t.Errorf("argv missing: %q", got)
	}
}

// TestPhaseTailKeepsOnlyTheBoundedTail: a phase that prints for minutes must
// not grow the marker without limit, and the LAST output is what matters —
// that is where the failure shows up.
func TestPhaseTailKeepsOnlyTheBoundedTail(t *testing.T) {
	tail := newPhaseTail(64)
	for i := 0; i < 100; i++ {
		if _, err := tail.Write([]byte(strings.Repeat("x", 10))); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	if _, err := tail.Write([]byte("FINAL")); err != nil {
		t.Fatalf("write final: %v", err)
	}

	got := tail.String()
	if len(got) > 64 {
		t.Errorf("tail grew to %d bytes, want <= 64", len(got))
	}
	if !strings.HasSuffix(got, "FINAL") {
		t.Errorf("the last thing written — the part that names the failure — is not at the end:\n%q", got)
	}
}

// TestPhaseTailPassesEverythingThrough: as an io.Writer in a MultiWriter, a
// short write must report success so the child's output still reaches the
// terminal. Returning a short count would make the child think the pipe broke.
func TestPhaseTailPassesEverythingThrough(t *testing.T) {
	tail := newPhaseTail(8)
	payload := "a much longer line than the tail can hold"
	n, err := tail.Write([]byte(payload))
	if err != nil {
		t.Fatalf("write returned an error: %v", err)
	}
	if n != len(payload) {
		t.Errorf("Write returned %d, want %d — a short count would make the child retry or abort", n, len(payload))
	}
	if len(tail.String()) > 8 {
		t.Errorf("tail exceeded its cap: %d bytes", len(tail.String()))
	}
}
