package mcpinit

import (
	"testing"
	"time"
)

// The lifecycle claim is a regular file Ghost itself wrote, and it lives in the
// data directory, which every local process of the user's own can write. An
// entry wearing the claim's name is therefore as untrusted as its bytes: a FIFO
// there blocks an open for reading until a writer appears, so the stop hook,
// the session-end hook and `ghost history compact` all hung on it, and a
// character device was read without a bound. Only a regular file can be a
// claim, so anything else is no claim.

// claimDeadline bounds every read here that could block on a FIFO, so a
// regression fails the test instead of wedging the package run.
const claimDeadline = 5 * time.Second

// withClaimDeadline runs fn and fails the test if it has not returned within
// claimDeadline. unblock, when not nil, is called on a timeout to release a
// reader that is blocked on a FIFO, and again until fn returns, so the reader
// is joined before the failure and nothing is left running.
func withClaimDeadline(t *testing.T, unblock func(), fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		return
	case <-time.After(claimDeadline):
	}
	if unblock == nil {
		t.Fatal("the claim read blocked for more than 5s on a path that is not a regular file")
	}
	for {
		unblock()
		select {
		case <-done:
			t.Fatalf("the claim read blocked for more than %s on a path that is not a regular file", claimDeadline)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
