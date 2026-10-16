package mcpinit

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// nonRegularDeadline bounds a hook run whose transcript path is not a regular
// file. The hook is documented as fail-open, so it must return promptly: a
// regression that opens a FIFO blocks until something writes to it, and a
// directory read used to be attempted instead of refused.
const nonRegularDeadline = time.Second

// runWithDeadline runs f on its own goroutine and fails the test if it does not
// return. Anything f writes must be read only after it has returned.
func runWithDeadline(t *testing.T, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(nonRegularDeadline):
		t.Fatal("the hook did not return: it opened a transcript path that is not a regular file")
	}
}

// TestRunStopRefusesADirectoryTranscript: the payload names the transcript path,
// so it can be a directory as readily as a file. A directory is refused with the
// same fail-open line a missing file gets and is never read, so the hook returns
// rather than handing the scanner something it cannot parse.
func TestRunStopRefusesADirectoryTranscript(t *testing.T) {
	isolatedHome(t)
	dir := t.TempDir() // an empty directory sitting where the transcript belongs
	input := stopInput(t, dir, false)
	var out, errOut bytes.Buffer

	runWithDeadline(t, func() {
		RunHostEvent("stop", "claude-code", strings.NewReader(input), &out, &errOut)
	})

	if out.Len() != 0 {
		t.Errorf("expected silence for a directory transcript path, got %q", out.String())
	}
	if !strings.Contains(errOut.String(), "open transcript") {
		t.Errorf("the refusal is not attributed to the open; stderr was %q", errOut.String())
	}
}
