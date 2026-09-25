package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

const (
	// phaseFailureTailMax bounds how much of a phase's output travels into the
	// failure marker. Generous enough to include the last thing that was said
	// — which is where a failure shows up — and small enough that the marker,
	// which is rendered into a session-start alert, stays a block rather than
	// a transcript.
	phaseFailureTailMax = 1200

	// promptArgMaxBytes is the size above which an argv element is treated as
	// prompt content rather than a flag. The fallback records argv precisely
	// because stderr can be empty, so it must not become the one path that
	// writes the prompt into a file every session prints.
	promptArgMaxBytes = 200

	// promptOmittedMarker stands in for such an argument so the record still
	// shows that one was dropped, rather than silently misreporting the argv.
	promptOmittedMarker = "[prompt omitted]"
)

// phaseTail keeps the last max bytes written to it and discards everything
// earlier, so a phase that prints for minutes contributes a bounded amount to
// the failure marker instead of an unbounded one.
//
// It is used behind an io.MultiWriter alongside the real stderr, so Write must
// always report the full byte count even when it discards: a short count would
// tell the child its pipe broke and make it retry or abort mid-run.
type phaseTail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newPhaseTail(max int) *phaseTail {
	return &phaseTail{max: max}
}

func (t *phaseTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.buf = append(t.buf, p...)
	if excess := len(t.buf) - t.max; excess > 0 {
		// Copy rather than reslice so the retained window is a fresh backing
		// array; a reslice would keep the discarded prefix alive.
		t.buf = append([]byte(nil), t.buf[excess:]...)
	}
	return len(p), nil
}

func (t *phaseTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

// phaseFailureCause explains a failed phase in terms someone can act on.
//
// runErr.Error() alone is "exit status 1": 357 of those sat in lifecycle.log
// with nothing to distinguish them, and the run this issue was filed against
// failed reflect, resolve and supersede in one to two seconds each with
// completely empty stderr (issue #540) — a marker recording only the exit
// status tells a reader that a phase failed and nothing else, ever.
//
// So: output the child actually produced is the best evidence there is and is
// used verbatim. When there is none, record everything still knowable — the
// real exit code, the argv with any prompt-sized argument replaced, and this
// binary's build version — because a bare exit status is not actionable and
// the whole point of the marker is to say what went wrong.
func phaseFailureCause(argv []string, runErr error, output string) string {
	if tail := strings.TrimSpace(output); tail != "" {
		return tail
	}

	// Each branch produces a complete, self-contained cause — no shared
	// "exit " prefix, or a non-exit error would render as
	// "exit context deadline exceeded".
	cause := "unknown failure"
	var ee *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &ee) && ee.ProcessState != nil:
		// The real status, not the string: this is the value that tells a
		// timeout or a signal apart from an ordinary non-zero exit.
		cause = fmt.Sprintf("exit %d", ee.ExitCode())
	default:
		// A timeout, a missing binary, a spawn failure. Never an exit code,
		// because reporting one would point at the wrong thing entirely.
		cause = runErr.Error()
	}
	return fmt.Sprintf("%s; argv: %s; ghost %s", cause, sanitizeArgv(argv), version)
}

// sanitizeArgv renders argv for the marker, replacing any argument large
// enough to be prompt content. Flags and paths survive intact so the record
// still says which phase ran and with what options.
func sanitizeArgv(argv []string) string {
	if len(argv) == 0 {
		return "(none)"
	}
	kept := make([]string, 0, len(argv))
	for _, a := range argv {
		if len(a) > promptArgMaxBytes {
			kept = append(kept, promptOmittedMarker)
			continue
		}
		kept = append(kept, a)
	}
	return strings.Join(kept, " ")
}
