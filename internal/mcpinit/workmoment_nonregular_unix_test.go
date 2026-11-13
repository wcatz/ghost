//go:build !windows

package mcpinit

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// withMomentMarkerDeadline runs fn and fails the test if it has not returned
// within three seconds. A FIFO at the marker path is unblocked by opening it
// for both reading and writing, so a blocked call on origin/main is released
// rather than wedging the suite.
func withMomentMarkerDeadline(t *testing.T, unblock []string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return
	case <-time.After(3 * time.Second):
	}
	for {
		for _, p := range unblock {
			if w, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = w.Close()
			}
			if r, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
				_ = r.Close()
			}
		}
		select {
		case <-done:
			t.Fatalf("the marker call blocked for more than 3s on a path that is not a regular file")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
