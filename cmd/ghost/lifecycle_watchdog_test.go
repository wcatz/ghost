//go:build !windows

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// phaseExitsOnTerm is a phase that does what the grace period exists for: it
// takes the deadline's SIGTERM and is gone well inside it. The loop of short
// sleeps is the load-bearing part — a shell runs a trap only once its current
// foreground child has finished, so `trap 'exit 0' TERM; sleep 30` would sit
// out the whole deadline before exiting and prove nothing.
const phaseExitsOnTerm = "trap 'exit 0' TERM; while :; do sleep 0.05; done"

// phaseIgnoresTerm is a phase that never exits on its own: the case where the
// escalation is the only thing that ends the group.
const phaseIgnoresTerm = "trap '' TERM; while :; do sleep 0.05; done"

// newPhaseCmd builds a phase exactly the way the phase loop in runLifecycle
// does — its own process group, SIGTERM to that group as the deadline's
// Cancel, and a WaitDelay backstop so a test that fails fails quickly rather
// than hanging on a child nothing kills.
func newPhaseCmd(timeout time.Duration, script string) (context.Context, context.CancelFunc, *exec.Cmd) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	setPhaseProcessGroup(cmd)
	cmd.Cancel = func() error { return terminatePhaseProcess(cmd) }
	cmd.WaitDelay = 10 * time.Second
	return ctx, cancel, cmd
}

// realEscalation is the production phaseEscalation with one replacement: the
// kill records that it fired before it performs the real one, so a phase the
// assertion expects to be swept still dies and the wait returns. The count is
// atomic because the watchdog writes it from its own goroutine.
func realEscalation(grace time.Duration, attempts *atomic.Int64) phaseEscalation {
	return phaseEscalation{
		grace:          grace,
		after:          time.After,
		groupInhabited: phaseGroupInhabited,
		kill: func(cmd *exec.Cmd) {
			attempts.Add(1)
			killPhaseProcess(cmd)
		},
	}
}

// startPhaseAndWatchdog starts the phase, launches the watchdog once the phase
// exists, and hands back the wait for it. Start comes first for the reason the
// phase loop starts the phase first: the watchdog signals the phase's process
// group, and a goroutine that reads cmd.Process before Start writes it races
// with Start on the one field the kill is decided by.
func startPhaseAndWatchdog(t *testing.T, ctx context.Context, phaseDone chan struct{}, cmd *exec.Cmd, esc phaseEscalation) *sync.WaitGroup {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the phase: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		phaseWatchdog(ctx, phaseDone, cmd, esc)
	}()
	return &wg
}

// assertPhaseExitedItself checks that the phase went out on the deadline's
// SIGTERM rather than being force-killed by the escalation: a process the group
// SIGKILL landed on reports -1 and one that took the trap reports 0. Go
// injects the context error into the wait's result whenever the Cancel
// succeeded, whatever the exit code was, so the process state is what says
// which one happened.
func assertPhaseExitedItself(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("exit code %d: the phase was force-killed instead of exiting on the deadline's SIGTERM", code)
	}
}

// readPid reads a pid a phase wrote, polling briefly because the file may be
// written just as the phase is being reaped.
func readPid(t *testing.T, path string) int {
	t.Helper()
	var raw []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && len(bytes.TrimSpace(b)) > 0 {
			raw = b
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("read the recorded pid: %v (%q)", err, raw)
	}
	return pid
}

// processAlive reports whether pid still exists. EPERM means it exists but is
// not ours to signal, which still counts as alive.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// waitGone polls until pid is gone, so the test does not race the reparented
// process being reaped after the group SIGKILL.
func waitGone(pid int, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("still running after %s", within)
}

// TestThePhaseWatchdogSkipsTheKillWhenThePhaseExitsInsideTheGracePeriod is
// the deterministic path: the deadline fires, the phase exits on that SIGTERM
// inside the grace period, and the group is empty by the time the escalation
// asks. The escalation used to sleep out the grace period and kill
// unconditionally, so this run always signalled a process group id that no
// longer belonged to anything in it.
func TestThePhaseWatchdogSkipsTheKillWhenThePhaseExitsInsideTheGracePeriod(t *testing.T) {
	const (
		timeout = 100 * time.Millisecond
		grace   = 400 * time.Millisecond
	)
	ctx, cancel, cmd := newPhaseCmd(timeout, phaseExitsOnTerm)
	defer cancel()

	var attempts atomic.Int64
	phaseDone := make(chan struct{})
	wg := startPhaseAndWatchdog(t, ctx, phaseDone, cmd, realEscalation(grace, &attempts))

	// The deadline interrupted the phase, so the wait reports the context
	// error whatever the exit code was; what says the phase left on its own
	// is the exit code below.
	_ = cmd.Wait()
	close(phaseDone)
	wg.Wait()

	assertPhaseExitedItself(t, cmd)
	if n := attempts.Load(); n != 0 {
		t.Errorf("the escalation fired %d time(s) for a phase that had already exited and emptied its group", n)
	}
}

// TestThePhaseWatchdogSkipsTheKillWhenThePhaseEndedBeforeItRan is the racing
// path: the phase ends, and the loop closes phaseDone and cancels the context
// before the watchdog goroutine has parked in its select. Go's select then
// picks a ready case at random, so the old shape took the deadline branch about
// half the time and killed a dead group the rest of it later. Neither case
// escalates now, because the context the loop cancelled is not a deadline.
//
// The goroutine is held on a gate until both channels are ready, which makes
// the precondition deterministic rather than a coin toss, and it runs twenty
// times because a branch that escalated only half the time would be a flake,
// not a pass.
func TestThePhaseWatchdogSkipsTheKillWhenThePhaseEndedBeforeItRan(t *testing.T) {
	const grace = 50 * time.Millisecond

	for i := 0; i < 20; i++ {
		ctx, cancel, cmd := newPhaseCmd(time.Hour, "exit 0")
		if err := cmd.Start(); err != nil {
			cancel()
			t.Fatalf("run %d: start the phase: %v", i, err)
		}

		phaseDone := make(chan struct{})
		started, release := make(chan struct{}), make(chan struct{})
		var attempts atomic.Int64
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			close(started)
			<-release
			phaseWatchdog(ctx, phaseDone, cmd, realEscalation(grace, &attempts))
		}()
		<-started

		// Both channels closed before the goroutine runs, the same way the
		// phase loop leaves them when a phase ends before the watchdog
		// starts.
		if runErr := cmd.Wait(); runErr != nil {
			cancel()
			t.Fatalf("run %d: the phase should have exited on its own: %v", i, runErr)
		}
		close(phaseDone)
		cancel()
		close(release)
		wg.Wait()

		if n := attempts.Load(); n != 0 {
			t.Fatalf("run %d: the escalation fired %d time(s) for a phase that had already ended", i, n)
		}
	}
}

// TestThePhaseWatchdogAsksTheGroupBeforeItSignals pins the escalation's
// decision at the boundary where the grace is up, with the clock and the group
// question both injected so the state is set rather than waited for. An empty
// group must not be signalled — its id belongs to no run here and can already
// have been reused — and an inhabited one must be.
func TestThePhaseWatchdogAsksTheGroupBeforeItSignals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		groupAlive bool
		wantKills  int64
	}{
		{"an empty group is not signalled", false, 0},
		{"an inhabited group is signalled", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
			defer cancel()
			<-ctx.Done() // the deadline has fired

			graceUp := make(chan time.Time)
			close(graceUp) // and the grace is up

			var kills atomic.Int64
			phaseWatchdog(ctx, make(chan struct{}), &exec.Cmd{}, phaseEscalation{
				grace:          0,
				after:          func(time.Duration) <-chan time.Time { return graceUp },
				groupInhabited: func(*exec.Cmd) bool { return tc.groupAlive },
				kill:           func(*exec.Cmd) { kills.Add(1) },
			})

			if got := kills.Load(); got != tc.wantKills {
				t.Errorf("the escalation fired %d time(s), want %d", got, tc.wantKills)
			}
		})
	}
}

// TestThePhaseWatchdogKillsAPhaseThatOutlivesTheGracePeriod is the case where
// the phase process itself is still running when the escalation asks: the whole
// group is force-killed, or a phase that ignores SIGTERM keeps a harness
// grandchild running forever.
func TestThePhaseWatchdogKillsAPhaseThatOutlivesTheGracePeriod(t *testing.T) {
	const (
		timeout = 100 * time.Millisecond
		grace   = 150 * time.Millisecond
	)
	ctx, cancel, cmd := newPhaseCmd(timeout, phaseIgnoresTerm)
	defer cancel()

	var attempts atomic.Int64
	phaseDone := make(chan struct{})
	wg := startPhaseAndWatchdog(t, ctx, phaseDone, cmd, realEscalation(grace, &attempts))

	runErr := cmd.Wait()
	close(phaseDone)
	wg.Wait()

	if runErr == nil {
		t.Fatal("the escalation did not end a phase that ignored SIGTERM")
	}
	if n := attempts.Load(); n != 1 {
		t.Errorf("the escalation fired %d time(s), want exactly 1", n)
	}
}

// TestThePhaseWatchdogSweepsAHarnessThePhaseLeftBehind is the shape a real
// phase produces on a deadline, and the one a re-check on the phase process
// alone would skip: the phase has no SIGTERM handler, so it dies the moment the
// deadline's group signal lands and cmd.Wait returns at once, while the CLI
// harness it spawned — same group, its own stdio, so it does not hold the
// phase's pipes — ignores the signal and survives. The phase being reaped is
// not the group being empty, so the escalation must still reach the harness.
func TestThePhaseWatchdogSweepsAHarnessThePhaseLeftBehind(t *testing.T) {
	const (
		timeout = 150 * time.Millisecond
		grace   = 250 * time.Millisecond
	)
	pidFile := filepath.Join(t.TempDir(), "harness.pid")
	// The grandchild ignores SIGTERM and gets its own stdio, exactly as a CLI
	// harness does, so it neither dies on the group signal nor holds the
	// phase's stdout open past the phase's own exit.
	script := fmt.Sprintf(
		"(trap '' TERM; while :; do sleep 0.05; done) >/dev/null 2>&1 & echo $! > %s; "+
			"trap 'exit 0' TERM; while :; do sleep 0.05; done", pidFile)
	ctx, cancel, cmd := newPhaseCmd(timeout, script)
	defer cancel()

	var attempts atomic.Int64
	phaseDone := make(chan struct{})
	wg := startPhaseAndWatchdog(t, ctx, phaseDone, cmd, realEscalation(grace, &attempts))

	if runErr := cmd.Wait(); runErr == nil {
		t.Fatal("the phase exited on its own instead of taking the deadline's SIGTERM")
	}
	close(phaseDone)

	harness := readPid(t, pidFile)
	if !processAlive(harness) {
		t.Fatal("the harness was already gone before the escalation ran; this case is not covered")
	}

	wg.Wait()

	if n := attempts.Load(); n != 1 {
		t.Errorf("the escalation fired %d time(s), want exactly 1", n)
	}
	if err := waitGone(harness, 3*time.Second); err != nil {
		t.Errorf("the harness (pid %d) survived the escalation: the phase was reaped but its group was not swept (%v)", harness, err)
	}
}
