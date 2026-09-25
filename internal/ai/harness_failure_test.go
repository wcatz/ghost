package ai

import (
	"context"
	"strings"
	"testing"
)

// TestHarnessFailureOutputFallsBackToStdout is the defect behind issue #540's
// "opencode exit 1 with empty stderr".
//
// Every client reported a failed run as `fmt.Errorf("...: %w: %s", err,
// stderr.String())` — and opencode reports failures through its `--format
// json` stream on STDOUT, not the console. So the message came out as
//
//	opencode run: exit status 1:
//
// with nothing after the colon, 357 times in lifecycle.log, and every
// reflection, resolve and supersede failure for the day was undiagnosable:
// the child had said exactly what went wrong, on the other stream, and Ghost
// dropped it.
func TestHarnessFailureOutputFallsBackToStdout(t *testing.T) {
	got := harnessFailureOutput(`{"error":"writeFile /x/.gitignore: ENOENT"}`, "")
	if !strings.Contains(got, "ENOENT") {
		t.Errorf("stdout was discarded when stderr was empty — the failure is invisible:\n%q", got)
	}
}

// TestHarnessFailureOutputPrefersStderr: stderr is the child's direct
// complaint and is what the old code showed. Falling back must not displace it
// — a long JSON stream on stdout must not bury the real diagnostic.
func TestHarnessFailureOutputPrefersStderr(t *testing.T) {
	got := harnessFailureOutput(`{"noise":"large json stream"}`, "permission denied writing config")
	if !strings.Contains(got, "permission denied") {
		t.Errorf("stderr missing: %q", got)
	}
	if strings.Contains(got, "large json stream") {
		t.Errorf("stdout displaced stderr — the direct complaint must win: %q", got)
	}
}

// TestHarnessFailureOutputIsBounded: stdout on a successful-looking but
// non-zero run can be a whole JSON-lines transcript. Attaching all of it to an
// error that then travels into a marker rendered at session start would turn
// the alert into a transcript too.
func TestHarnessFailureOutputIsBounded(t *testing.T) {
	big := strings.Repeat(`{"line":"filler"}`+"\n", 5000)
	got := harnessFailureOutput(big, "")

	if len(got) > harnessFailureOutputMax+64 {
		t.Errorf("failure output grew to %d bytes, want <= %d", len(got), harnessFailureOutputMax)
	}
	// The TAIL is what matters: that is where the error sits, after the
	// events that led to it.
	if !strings.Contains(got, `{"line":"filler"}`) {
		t.Errorf("no output survived at all: %q", got)
	}
}

// TestHarnessFailureOutputBothEmpty: a child that says nothing must not panic
// or return a lie — empty is the honest answer, and the caller's own
// "exit status N" still carries the failure.
func TestHarnessFailureOutputBothEmpty(t *testing.T) {
	if got := harnessFailureOutput("", ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestHarnessFailureOutputTrimsNoise: leading and trailing whitespace from a
// captured stream makes the message look like it was mangled; the marker
// renders the first line, so that line should be the child's first real one.
func TestHarnessFailureOutputTrimsNoise(t *testing.T) {
	got := harnessFailureOutput("   \n\n  actual failure line\nmore\n", "")
	if !strings.HasPrefix(got, "actual failure line") {
		t.Errorf("leading blank lines survived into the message: %q", got)
	}
}

// TestCLIClientReportsAFailureTheChildPutOnStdout is the wiring test the
// helper alone cannot give: it drives a real subprocess, so it fails if any
// client stops routing through harnessFailureOutput and goes back to appending
// stderr.String() — which is how every one of them reported opencode's failures
// as "exit status 1: " with nothing after the colon.
func TestCLIClientReportsAFailureTheChildPutOnStdout(t *testing.T) {
	bin := fakeClaudeBinary(t, `
printf 'config write failed: ENOENT'
exit 3
`)
	c := &CLIClient{binary: bin}
	_, _, err := c.Reflect(context.Background(), "prompt")
	if err == nil {
		t.Fatal("expected the failing child to surface an error")
	}
	if !strings.Contains(err.Error(), "ENOENT") {
		t.Errorf("the child's own explanation was dropped:\n%s", err.Error())
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("the exit status is missing too:\n%s", err.Error())
	}
}
