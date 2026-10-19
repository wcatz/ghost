//go:build !windows

package main

// The exit status of a built `ghost lifecycle`, read where a script or
// supervisor reads it. The phases are separate ghost processes, so the run is
// driven through the built binary (see lifecycle_signal_unix_test.go) rather
// than runLifecycle in the test process. The interrupted-run status is pinned
// in the two interrupt tests, beside the marker those assert.

import (
	"os"
	"path/filepath"
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

// TestLifecycleExitStatusCleanRunIsZero: every enabled phase succeeded (none
// is enabled, which is the cheapest run with no failure) and the status is 0.
func TestLifecycleExitStatusCleanRunIsZero(t *testing.T) {
	s := newSignalSandbox(t)
	s.seed(t)
	cfg := "reflection:\n  auto_reflect: false\n  auto_resolve: false\n  auto_supersede: false\n"
	if err := os.WriteFile(s.configPath(), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	c := runCoordinatorToExit(t, s)
	if c.code != 0 {
		t.Errorf("a clean run exited %d, want 0\n%s", c.code, c.stderr.String())
	}
	if s.markerExists() {
		t.Errorf("a clean run left a failure marker\n%s", c.stderr.String())
	}
}

// TestLifecycleExitStatusFailedPhaseIsOne: a phase that exits non-zero is
// recorded in the marker, and the status says so. Resolve has no local
// fallback, so with no harness reachable it fails.
func TestLifecycleExitStatusFailedPhaseIsOne(t *testing.T) {
	s := newSignalSandbox(t)
	s.seed(t)
	if err := os.Remove(filepath.Join(s.bin, "claude")); err != nil {
		t.Fatalf("remove the fake harness: %v", err)
	}
	if err := os.WriteFile(s.configPath(), []byte(signalTestConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	c := runCoordinatorToExit(t, s)
	if !s.markerExists() {
		t.Fatalf("no phase failed, so the test does not exercise a failure\n%s", c.stderr.String())
	}
	if c.code != 1 {
		t.Errorf("a run with a failed phase exited %d, want 1\n%s", c.code, c.stderr.String())
	}
}

// TestLifecycleExitStatusSkippedPhaseIsOne: reflect skipped for want of an LLM
// backend writes a failure marker, so it is a failure.
func TestLifecycleExitStatusSkippedPhaseIsOne(t *testing.T) {
	s := newSignalSandbox(t)
	s.seed(t)
	if err := os.Remove(filepath.Join(s.bin, "claude")); err != nil {
		t.Fatalf("remove the fake harness: %v", err)
	}
	cfg := "reflection:\n  auto_reflect: true\n  auto_resolve: false\n  auto_supersede: false\n"
	if err := os.WriteFile(s.configPath(), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	c := runCoordinatorToExit(t, s)
	if !s.markerExists() {
		t.Fatalf("the skip wrote no marker\n%s", c.stderr.String())
	}
	if c.code != 1 {
		t.Errorf("a run with a skipped phase exited %d, want 1\n%s", c.code, c.stderr.String())
	}
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
