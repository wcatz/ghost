//go:build !windows

package ai

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The harness funnel's deadline must bound the WHOLE call, not only the direct
// child. A harness is a CLI that spawns helpers, a helper that does not
// redirect its stdio inherits the child's stdout, and os/exec reads that pipe
// until EOF — so a cancelled call used to block until the last holder of the
// write end exited, however long that was. These two tests drive a fake harness
// that leaves exactly such a helper behind and assert the two mechanisms that
// close it: the group signal (a helper that respects it) and WaitDelay (a
// helper that does not).

// writeGrandchildHarness writes a POSIX fake harness whose body leaves a
// background helper holding the harness's stdout, and pins the scratch root and
// the passthrough the helper reports its pid through. The pid reaches the test
// in a FILE rather than a Go variable because it is written by a shell child,
// and through GHOST_PASSTHROUGH_ENV because a test-only name in the production
// allowlist would be a hole in the policy these tests sit beside.
func writeGrandchildHarness(t *testing.T, body string) (bin, pidFile string) {
	t.Helper()
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	pidFile = filepath.Join(t.TempDir(), "grandchild.pid")
	t.Setenv("HARNESS_GRANDCHILD_PID", pidFile)
	t.Setenv("GHOST_PASSTHROUGH_ENV", "HARNESS_GRANDCHILD_PID")
	bin = filepath.Join(t.TempDir(), "fake-harness")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write fake harness: %v", err)
	}
	return bin, pidFile
}

// startHarness runs the funnel-built command with stdout bound to a buffer,
// which is what makes os/exec copy the child's output through a pipe — the
// descriptor the grandchild inherits and the reason a cancelled Wait used to
// block. It returns once the fake has recorded its grandchild, so the context
// is cancelled with the helper already running rather than racing its start.
func startHarness(t *testing.T, ctx context.Context, bin, pidFile string) (*exec.Cmd, func(), int) {
	t.Helper()
	cmd, release, err := harnessCommand(ctx, bin, nil, os.Environ(), harnessGoose)
	if err != nil {
		t.Fatalf("harnessCommand: %v", err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		release()
		t.Fatalf("start the fake harness: %v", err)
	}
	return cmd, release, waitForRecordedPid(t, pidFile)
}

// waitForRecordedPid blocks until the fake has written its helper's pid. It
// polls rather than sleeping a fixed span because the write happens as soon as
// the shell reaches it, and a fixed sleep would either race it or slow every
// run down for no reason.
func waitForRecordedPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			if text := strings.TrimSpace(string(data)); text != "" {
				pid, err := strconv.Atoi(text)
				if err != nil {
					t.Fatalf("grandchild pid file %q is not a pid: %v", text, err)
				}
				return pid
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the fake harness never recorded a grandchild pid in %s", path)
	return 0
}

// processAlive reports whether pid still exists. EPERM means it exists but is
// not ours to signal, which is still alive; a pid of zero or below names no
// process, so it is reported as gone rather than asked about.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// waitGone polls until pid is gone, so a group signal the kernel has delivered
// but not yet had reaped is not read as the helper still running.
func waitGone(pid int, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("still running")
}

// killGrandchild ends a helper a test deliberately left alive, so the test does
// not leak a process into the rest of the package's run.
func killGrandchild(t *testing.T, pid int) {
	t.Helper()
	t.Cleanup(func() {
		if processAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

// TestHarnessDeadlineSweepsTheProcessGroup makes the group signal observable: a
// helper that takes the default SIGTERM is in the harness's process group, so
// the deadline's Cancel must reach it — a signal to the direct child alone
// leaves the helper running, and with it the child's stdout pipe, which is what
// held the call open past its deadline in the first place.
//
// The assertion is the helper being GONE, not a duration: a duration alone
// cannot tell the group signal apart from the WaitDelay backstop, which is
// covered by the test below.
func TestHarnessDeadlineSweepsTheProcessGroup(t *testing.T) {
	bin, pidFile := writeGrandchildHarness(t, "sleep 30 &\necho $! > \"$HARNESS_GRANDCHILD_PID\"\nwait\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, release, grandchild := startHarness(t, ctx, bin, pidFile)
	defer release()
	defer killGrandchild(t, grandchild)

	start := time.Now()
	cancel()
	_ = cmd.Wait()
	elapsed := time.Since(start)

	if err := waitGone(grandchild, 3*time.Second); err != nil {
		t.Errorf("a helper (pid %d) in the harness's process group survived the deadline (%v): signalling the direct child is not signalling the group", grandchild, err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the call returned %s after its context was done: something it spawned held it open", elapsed)
	}
}

// TestHarnessWaitDelayBoundsAPipeHoldingGrandchild covers the helper the group
// signal cannot end: one that ignores SIGTERM. Nothing in the group stops it,
// so the call is bounded by WaitDelay instead — the deadline plus the backstop,
// not the helper's lifetime.
//
// The helper is still alive when the call returns, and that is asserted: it is
// the positive control proving this run measured the backstop rather than a
// helper that happened to exit.
func TestHarnessWaitDelayBoundsAPipeHoldingGrandchild(t *testing.T) {
	// `exec` keeps the trap's disposition — an ignored signal survives exec —
	// so the process whose pid is recorded is itself the one ignoring SIGTERM.
	// The hold is well past twice the backstop so the assertion separates the
	// two by a wide margin rather than sharing one.
	bin, pidFile := writeGrandchildHarness(t, "sh -c 'trap \"\" TERM; exec sleep 10' &\necho $! > \"$HARNESS_GRANDCHILD_PID\"\nwait\n")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, release, grandchild := startHarness(t, ctx, bin, pidFile)
	defer release()
	defer killGrandchild(t, grandchild)

	start := time.Now()
	cancel()
	_ = cmd.Wait()
	elapsed := time.Since(start)

	if !processAlive(grandchild) {
		t.Fatalf("the helper (pid %d) died before the call returned, so this run did not exercise the backstop", grandchild)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the call was held open for %s by a helper that inherited the child's stdout, want it bounded by the %s backstop", elapsed, harnessWaitDelay)
	}
}
