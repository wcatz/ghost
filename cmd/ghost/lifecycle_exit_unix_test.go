//go:build !windows

package main

import (
	"os"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/mcpinit"
)

// runCoordinatorToExit starts the chain and waits for it to end.
func runCoordinatorToExit(t *testing.T, s *signalSandbox) *coordinator {
	t.Helper()
	c := s.startCoordinator(t)
	c.waitExit(t, 90*time.Second)
	return c
}

// TestLifecycleExitStatusLockHeldIsZero: another live run holding the
// per-project lock turns this one away, which is a no-op and not a failure.
func TestLifecycleExitStatusLockHeldIsZero(t *testing.T) {
	s := newSignalSandbox(t)
	s.seed(t)
	if err := os.WriteFile(s.configPath(), []byte(signalTestConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	// This process is the live holder, in the sandbox's data directory.
	t.Setenv("XDG_DATA_HOME", s.data)
	release, ok, err := mcpinit.AcquireLifecycleLock(signalTestProject)
	if err != nil || !ok {
		t.Fatalf("take the lock: ok=%v err=%v", ok, err)
	}
	defer release()

	c := runCoordinatorToExit(t, s)
	if c.code != 0 {
		t.Errorf("a run the lock turned away exited %d, want 0\n%s", c.code, c.stderr.String())
	}
	if s.markerExists() {
		t.Errorf("a turned-away run wrote a failure marker\n%s", c.stderr.String())
	}
}
