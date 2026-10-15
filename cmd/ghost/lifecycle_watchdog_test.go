package main

import (
	"context"
	"os/exec"
	"sync"
	"sync/atomic"
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
// escalation is the only thing that ends it.
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

// countingKill is the seam on killPhaseProcess: it records the escalation and
// then performs the real one, so a phase the assertion expects to survive it
// still dies when it is wrong to have been signalled, and a phase the
// escalation is right about still ends and the wait returns. The count is
// atomic because the watchdog writes it from its own goroutine.
func countingKill(attempts *atomic.Int64) func(*exec.Cmd) {
	return func(cmd *exec.Cmd) {
		attempts.Add(1)
		killPhaseProcess(cmd)
	}
}

// startPhaseAndWatchdog starts the phase, launches the watchdog once the phase
// exists, and hands back the wait for it. Start comes first for the reason the
// phase loop starts the phase first: the watchdog signals the phase's process
// group, and a goroutine that reads cmd.Process before Start writes it races
// with Start on the one field the kill is decided by.
func startPhaseAndWatchdog(t *testing.T, ctx context.Context, phaseDone chan struct{}, cmd *exec.Cmd, grace time.Duration, kill func(*exec.Cmd)) *sync.WaitGroup {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the phase: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		phaseWatchdog(ctx, phaseDone, cmd, grace, kill, time.After)
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

// TestThePhaseWatchdogSkipsTheKillWhenThePhaseExitsInsideTheGracePeriod is
// the deterministic path: the deadline fires, the phase exits on that SIGTERM
// inside the grace period, and the group is reaped before the escalation
// lands. The escalation used to sleep out the grace period and kill
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
	wg := startPhaseAndWatchdog(t, ctx, phaseDone, cmd, grace, countingKill(&attempts))

	// The deadline interrupted the phase, so the wait reports the context
	// error whatever the exit code was; what says the phase left on its own
	// is the exit code below.
	_ = cmd.Wait()
	close(phaseDone)
	wg.Wait()

	assertPhaseExitedItself(t, cmd)
	if n := attempts.Load(); n != 0 {
		t.Errorf("the escalation fired %d time(s) for a phase that had already exited and been reaped", n)
	}
}

// TestThePhaseWatchdogSkipsTheKillWhenThePhaseEndedBeforeItRan is the racing
// path: the phase ends, and the loop closes phaseDone and cancels the context,
// before the watchdog goroutine has parked in its select. Go's select then
// picks a ready case at random, so the old shape took the deadline branch
// about half the time and killed a dead group the rest of it later.
//
// The goroutine is held on a gate until both channels are ready, which makes
// the precondition deterministic rather than a coin toss; it runs twenty times
// because a random choice that favours the right case a twentieth of the time
// is a flake, not a pass.
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
			phaseWatchdog(ctx, phaseDone, cmd, grace, countingKill(&attempts), time.After)
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

// TestThePhaseWatchdogRechecksThePhaseAfterTheGracePeriod pins the boundary the
// grace period is a race against: the timer is up AND the phase has ended. The
// wait after the deadline will return on one or the other, so the escalation
// has to ask phaseDone once more before it signals — with the re-check removed
// the wait alone is enough to let a bare kill through, on a group whose phase
// is already reaped.
//
// The timer is injected so the boundary is a state the test sets, not a moment
// it waits for: `after` parks the watchdog in the wait with the phase still
// running, and the test then ends the phase and brings the timer up together.
func TestThePhaseWatchdogRechecksThePhaseAfterTheGracePeriod(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the deadline has already fired

	phaseDone := make(chan struct{})
	reached := make(chan struct{})
	graceUp := make(chan time.Time)
	after := func(time.Duration) <-chan time.Time {
		close(reached)
		return graceUp
	}

	var kills atomic.Int64
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		phaseWatchdog(ctx, phaseDone, &exec.Cmd{}, 0, func(*exec.Cmd) { kills.Add(1) }, after)
	}()

	// The watchdog is inside the wait, with the phase still running and the
	// grace timer not yet up. End the phase and bring the timer up together:
	// whichever of the two the wait returns on, phaseDone is closed by the
	// time the re-check runs.
	<-reached
	close(phaseDone)
	close(graceUp)
	<-finished

	if n := kills.Load(); n != 0 {
		t.Errorf("the escalation fired %d time(s) with the phase already ended and reaped", n)
	}
}

// TestThePhaseWatchdogKillsAPhaseThatOutlivesTheGracePeriod is the other half
// of the same rule: the re-check must not become an unconditional skip, or a
// phase that ignores SIGTERM keeps a harness grandchild running forever. A
// harness grandchild that survived the deadline is the whole reason the group
// is signalled at all.
func TestThePhaseWatchdogKillsAPhaseThatOutlivesTheGracePeriod(t *testing.T) {
	const (
		timeout = 100 * time.Millisecond
		grace   = 150 * time.Millisecond
	)
	ctx, cancel, cmd := newPhaseCmd(timeout, phaseIgnoresTerm)
	defer cancel()

	var attempts atomic.Int64
	phaseDone := make(chan struct{})
	wg := startPhaseAndWatchdog(t, ctx, phaseDone, cmd, grace, countingKill(&attempts))

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
