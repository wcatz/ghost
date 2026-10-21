package mcpinit

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// TestIsAliveRefusesADirectoryClaim: a directory sits where the claim belongs
// as readily as a file does, and a read of it cannot mean anything.
func TestIsAliveRefusesADirectoryClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle-p1.pid")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	var alive bool
	withClaimDeadline(t, nil, func() { alive = isAlive(path) })

	if alive {
		t.Error("isAlive() = true for a directory where a claim belongs, want false")
	}
}

// TestIsAliveRefusesAnOverCapClaim: the read is bounded, and a file over the
// bound is no claim. The padding is SPACES, so an unbounded read still parses
// the pid the file starts with — the file is a live claim that is too long,
// which is exactly the case the bound has to refuse.
func TestIsAliveRefusesAnOverCapClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle-p1.pid")
	content := strconv.Itoa(os.Getpid()) + strings.Repeat(" ", maxPIDFileBytes)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var alive bool
	withClaimDeadline(t, nil, func() { alive = isAlive(path) })

	if alive {
		t.Errorf("isAlive() = true for a claim file of %d bytes, want false: the read is bounded", len(content))
	}
}
