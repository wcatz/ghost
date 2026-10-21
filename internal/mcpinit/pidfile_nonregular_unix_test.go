//go:build !windows

package mcpinit

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// claimFifoAt creates a FIFO at path — the claim path itself — or skips when
// the platform's filesystem refuses one.
func claimFifoAt(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
}

// unblockFIFO hands a blocked reader EOF by opening the FIFO for writing and
// closing it. O_NONBLOCK makes that open fail rather than wait when nobody is
// reading, and the loop that calls it repeats, because a read that blocked on
// one FIFO can block on the next one after it.
func unblockFIFO(path string) func() {
	return func() {
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
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
	withClaimDeadline(t, unblockFIFO(path), func() { alive = isAlive(path) })

	if alive {
		t.Error("isAlive() = true for a FIFO where a claim belongs, want false")
	}
}

// TestIsAliveRefusesASymlinkToAFIFOClaim: a link to a FIFO is the shape a
// planted path takes, and the open refuses the link itself rather than
// following it to something that is not a claim.
func TestIsAliveRefusesASymlinkToAFIFOClaim(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "planted.fifo")
	claimFifoAt(t, fifo)
	link := filepath.Join(t.TempDir(), "lifecycle-p1.pid")
	if err := os.Symlink(fifo, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	var alive bool
	withClaimDeadline(t, unblockFIFO(fifo), func() { alive = isAlive(link) })

	if alive {
		t.Error("isAlive() = true for a symlink to a FIFO where a claim belongs, want false")
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
	withClaimDeadline(t, unblockFIFO(path), func() { pid = pidInFile(path) })

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
	withClaimDeadline(t, unblockFIFO(path), func() { held = LifecycleLockHeld(dataDir, "p1") })

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

	withClaimDeadline(t, unblockFIFO(claim), func() {
		RunHostEvent("stop", "claude-code",
			io.Reader(bytes.NewReader([]byte(stopInputIn(t, projDir, transcript, "claude-jsonl")))),
			&out, &errOut)
	})

	if len(*spawned) != 1 {
		t.Fatalf("the hook started %d child(ren), want exactly 1: a path that is not a regular file is no claim",
			len(*spawned))
	}
}
