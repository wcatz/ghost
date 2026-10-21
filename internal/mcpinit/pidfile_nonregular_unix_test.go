//go:build !windows

package mcpinit

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
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
// claimDeadline. On a timeout the FIFO is opened for writing and closed, which
// hands a blocked reader EOF, and the goroutine is joined before the failure,
// so nothing is left running.
func withClaimDeadline(t *testing.T, fifo string, fn func()) {
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
	for {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
		select {
		case <-done:
			t.Fatalf("the claim read blocked for more than %s on a path that is not a regular file", claimDeadline)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// claimFifoAt creates a FIFO at path — the claim path itself — or skips when
// the platform's filesystem refuses one.
func claimFifoAt(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
}

// TestIsAliveRefusesAFIFOClaim: the liveness read is the one that hung. It has
// to come back at once and say "no claim", because the caller's only decision
// is whether to spawn — and a run that cannot tell is a run that blocks the
// turn it is answering.
func TestIsAliveRefusesAFIFOClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle-p1.pid")
	claimFifoAt(t, path)

	var alive bool
	withClaimDeadline(t, path, func() { alive = isAlive(path) })

	if alive {
		t.Error("isAlive() = true for a FIFO where a claim belongs, want false")
	}
}

// TestPidInFileRefusesAFIFOClaim: the release path reads the same claim to
// decide whether the file is still this run's to remove. It is a reader of the
// same path, so it carries the same guard: a FIFO is not this run's claim, and
// reading it must not block the coordinator on its way out.
func TestPidInFileRefusesAFIFOClaim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle-p1.pid")
	claimFifoAt(t, path)

	var pid int
	withClaimDeadline(t, path, func() { pid = pidInFile(path) })

	if pid != 0 {
		t.Errorf("pidInFile() = %d for a FIFO where a claim belongs, want 0", pid)
	}
}

// TestLifecycleLockHeldRefusesAFIFOClaim: `ghost history compact` asks this
// reader whether a lifecycle run is in progress. A planted FIFO must read as
// no run: the repair it guards must not hang on the answer, and a claim that
// cannot be read is not a claim to wait for.
func TestLifecycleLockHeldRefusesAFIFOClaim(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "lifecycle-p1.pid")
	claimFifoAt(t, path)

	var held bool
	withClaimDeadline(t, path, func() { held = LifecycleLockHeld(dataDir, "p1") })

	if held {
		t.Error("LifecycleLockHeld() = true for a FIFO where a claim belongs, want false")
	}
}

// TestTheStopHookDoesNotBlockOnAFIFOClaim: the whole path the issue names. The
// hook reads the claim to decide whether a lifecycle is already running, and a
// FIFO there used to hang the stop until the host's own timeout killed it. The
// hook must return at once, and — a FIFO being no claim — it must go on to
// spawn the chain it was about to spawn.
func TestTheStopHookDoesNotBlockOnAFIFOClaim(t *testing.T) {
	dataHome, projDir := auditHookRun(t)
	spawned := captureLifecycleChild(t)

	claim := filepath.Join(dataHome, "ghost", "lifecycle-p1.pid")
	claimFifoAt(t, claim)

	transcript := writeTranscript(t, lineUser, lineGhostSave)
	var out, errOut bytes.Buffer

	withClaimDeadline(t, claim, func() {
		RunHostEvent("stop", "claude-code",
			io.Reader(bytes.NewReader([]byte(stopInputIn(t, projDir, transcript, "claude-jsonl")))),
			&out, &errOut)
	})

	if len(*spawned) != 1 {
		t.Fatalf("the hook started %d child(ren), want exactly 1: a path that is not a regular file is no claim",
			len(*spawned))
	}
}
