package ai

import (
	"errors"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// harnessFailureOutputMax bounds the child output attached to a failure. It
// travels into the error, then into the failure marker, then into a
// session-start alert — a full JSON-lines transcript at any of those stops
// would stop being a message and start being a log file.
const harnessFailureOutputMax = 1200

// harnessFailureOutput chooses what to attach to a failed harness run.
//
// stderr first: it is the child's direct complaint, and that is what every
// client used to show. But when stderr is empty the stdout stream is used
// instead, because opencode reports failures through its --format json output
// rather than the console. All four clients appended stderr.String() alone, so
// a child that spoke on the other stream produced
//
//	opencode run: exit status 1:
//
// with nothing after the colon — 357 times in lifecycle.log, and every
// reflection, resolve and supersede failure for a day undiagnosable while the
// child had said exactly what went wrong one stream over (issue #540).
//
// Output is trimmed and taken from the tail: the error is what comes last,
// after the events leading up to it.
//
// The output is passed through the token guard before it reaches the error or
// the lifecycle log.
func harnessFailureOutput(stdout, stderr string) string {
	var full string
	if t := strings.TrimSpace(stderr); t != "" {
		full = t
	} else {
		full = strings.TrimSpace(stdout)
	}
	if full == "" {
		return ""
	}
	if refusal := memory.RejectSecret("harness output", full); refusal != nil {
		var detected *memory.SecretContentError
		if errors.As(refusal, &detected) {
			return "<harness output withheld: it holds a " + detected.Format + ">"
		}
		return "<harness output withheld: it holds a credential>"
	}
	return tail(full, harnessFailureOutputMax)
}

// tail returns the last n bytes of s, cut back to a character boundary so a
// multi-byte rune is never split.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut++
	}
	return s[cut:]
}
