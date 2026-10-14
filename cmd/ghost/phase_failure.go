package main

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/wcatz/ghost/internal/memory"
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

	// phaseTailTrimmedMarker stands in for a kept tail that is one partial
	// line: the head cut landed inside it and no whole line follows.
	phaseTailTrimmedMarker = "[earlier output trimmed]"
)

// phaseTail keeps the last max bytes written to it and discards everything
// earlier, so a phase that prints for minutes contributes a bounded amount to
// the failure marker instead of an unbounded one.
//
// It is used behind an io.MultiWriter alongside the real stderr, so Write must
// always report the full byte count even when it discards: a short count would
// tell the child its pipe broke and make it retry or abort mid-run.
//
// The cut is from the HEAD, so it can land inside a line — inside a token, in
// the worst case, leaving a fragment without its ghp_ prefix that the token
// guard downstream cannot recognise, as the first line of the marker the
// session-start alert renders. String therefore drops a partial first line
// (see there); cutAfterNewline records whether the last byte discarded was a
// line break, which is what says the first kept line is whole.
type phaseTail struct {
	mu              sync.Mutex
	max             int
	buf             []byte
	trimmed         bool
	cutAfterNewline bool
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
		t.trimmed = true
		t.cutAfterNewline = t.buf[excess-1] == '\n'
		t.buf = append([]byte(nil), t.buf[excess:]...)
	}
	return len(p), nil
}

// String returns the kept tail. When the head cut landed mid-line, that
// partial first line is dropped, so no fragment of a line — of a token, in the
// case this exists for — is recorded; when nothing but that partial line (or
// only whitespace after it) was kept, phaseTailTrimmedMarker stands in for it.
// A tail that was never cut, or was cut just after a line break, is returned
// whole.
func (t *phaseTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.trimmed || t.cutAfterNewline {
		return string(t.buf)
	}
	i := bytes.IndexByte(t.buf, '\n')
	if i < 0 || len(bytes.TrimSpace(t.buf[i+1:])) == 0 {
		return phaseTailTrimmedMarker
	}
	return string(t.buf[i+1:])
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
//
// The value after `--project` is rendered through `memory.ProjectArg` (#1087):
// the marker is a durable data-directory file that persists for up to 30 days
// and is printed at every later session start, and the project operand is a
// value a session routinely supplies from the clone URL it is standing in, so a
// credential-shaped one has to be withheld rather than recorded. The renderer
// leaves the ordinary case as the quoted name the neighbouring refusal sentences
// use, so the marker still names the phase's scope. It is applied BEFORE the
// length test, so a credential-shaped value only a little past the bound is
// withheld by the guard rather than dropped as an anonymous omission.
func sanitizeArgv(argv []string) string {
	if len(argv) == 0 {
		return "(none)"
	}
	kept := make([]string, 0, len(argv))
	for i, a := range argv {
		if i > 0 && argv[i-1] == "--project" {
			a = memory.ProjectArg("project", a)
		}
		if len(a) > promptArgMaxBytes {
			kept = append(kept, promptOmittedMarker)
			continue
		}
		kept = append(kept, a)
	}
	return strings.Join(kept, " ")
}
