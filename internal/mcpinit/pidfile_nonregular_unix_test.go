//go:build !windows

package mcpinit

import (
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
