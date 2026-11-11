package mcpinit

import (
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
