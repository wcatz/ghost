package main

import (
	"context"
	"os/exec"
	"sync"
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
// escalation is right about still ends and cmd.Run returns.
func countingKill(attempts *int) func(*exec.Cmd) {
	return func(cmd *exec.Cmd) {
		*attempts++
		killPhaseProcess(cmd)
	}
}

// assertPhaseExitedItself checks that the phase went out on the deadline's
// SIGTERM rather than being force-killed by the escalation: a process the group
// SIGKILL landed on reports -1 and one that took the trap reports 0. Go
// injects the context error into Run's result whenever the Cancel succeeded,
// whatever the exit code was, so the process state is what says which one
// happened.
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

	phaseDone := make(chan struct{})
	attempts := 0
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		phaseWatchdog(ctx, phaseDone, cmd, grace, countingKill(&attempts))
	}()

	runErr := cmd.Run()
	close(phaseDone)
	wg.Wait()

	// The deadline interrupted the phase, so Run reports the context error
	// whatever the exit code was; what says the phase left on its own is the
	// exit code above.
	_ = runErr
	assertPhaseExitedItself(t, cmd)
	if attempts != 0 {
		t.Errorf("the escalation fired %d time(s) for a phase that had already exited and been reaped", attempts)
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

		phaseDone := make(chan struct{})
		started, release := make(chan struct{}), make(chan struct{})
		attempts := 0
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			close(started)
			<-release
			phaseWatchdog(ctx, phaseDone, cmd, grace, countingKill(&attempts))
		}()
		<-started

		// Both channels closed before the goroutine runs, the same way the
		// phase loop leaves them when a phase ends before the watchdog
		// starts.
		if runErr := cmd.Run(); runErr != nil {
			cancel()
			t.Fatalf("run %d: the phase should have exited on its own: %v", i, runErr)
		}
		close(phaseDone)
		cancel()
		close(release)
		wg.Wait()

		if attempts != 0 {
			cancel()
			t.Fatalf("run %d: the escalation fired %d time(s) for a phase that had already ended", i, attempts)
		}
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

	phaseDone := make(chan struct{})
	attempts := 0
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		phaseWatchdog(ctx, phaseDone, cmd, grace, countingKill(&attempts))
	}()

	runErr := cmd.Run()
	close(phaseDone)
	wg.Wait()

	if runErr == nil {
		t.Fatal("the escalation did not end a phase that ignored SIGTERM")
	}
	if attempts != 1 {
		t.Errorf("the escalation fired %d time(s), want exactly 1", attempts)
	}
}
