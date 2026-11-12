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
	phaseFailureTailMax    = 1200
	promptArgMaxBytes      = 200
	promptOmittedMarker    = "[prompt omitted]"
	phaseTailTrimmedMarker = "[earlier output trimmed]"
)

// phaseTail keeps the last max bytes written to it. Write must report len(p) even when it discards.
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

func phaseFailureCause(argv []string, runErr error, output string) string {
	if tail := strings.TrimSpace(output); tail != "" {
		return tail
	}

	cause := "unknown failure"
	var ee *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &ee) && ee.ProcessState != nil:
		cause = fmt.Sprintf("exit %d", ee.ExitCode())
	default:
		cause = runErr.Error()
	}
	return fmt.Sprintf("%s; argv: %s; ghost %s", cause, sanitizeArgv(argv), version)
}

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
