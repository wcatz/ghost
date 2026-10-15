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

// phaseWithSweptHarness is the script for the shape a real phase produces on a
// deadline and the one a re-check on the phase process alone would skip: the
// phase has no SIGTERM handler of its own to keep, so it dies the moment the
// deadline's group signal lands and cmd.Wait returns at once, while the CLI
// harness it spawned — same group, its own stdio so it does not hold the
// phase's pipes — traps the signal away and survives.
func phaseWithSweptHarness(harnessPidFile string) string {
	return fmt.Sprintf(
		"(trap '' TERM; while :; do sleep 0.05; done) >/dev/null 2>&1 & echo $! > %s; "+
			"trap 'exit 0' TERM; while :; do sleep 0.05; done", harnessPidFile)
}

// newPhaseCmd builds a phase exactly the way the phase loop in runLifecycle
// does — its own process group, SIGTERM to that group as the deadline's
// Cancel, and a WaitDelay backstop so a test that fails fails quickly rather
// than hanging on a child nothing kills. Stdio and ExtraFiles stay unset, so
// the phase reads the null device and holds no pipe or extra descriptor the
// test would have to close behind it: a phase that outlives the test is
// holding nothing open when it does.
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
// not ours to signal, which still counts as alive. A pid of zero or below names
// no process at all — signal 0 to zero is a question about the caller's OWN
// group — so a caller holding no pid is told it has nothing alive rather than
// asking the kernel about itself.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
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

// waitGroupEmpty polls until nothing is left in the phase's process group.
// A group outlives its leader by a moment even after a group-wide SIGKILL —
// the phase's own short `sleep` is reparented and finishes — so a one-shot
// probe of an emptying group reports one that is still full.
func waitGroupEmpty(cmd *exec.Cmd, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !phaseGroupInhabited(cmd) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("still inhabited after %s", within)
}

// recordedPid reads a pid a phase wrote, or 0 when it wrote none. It is the
// reader the cleanup path uses, where the file is as likely to be absent as
// present: a test that aborted before the phase wrote anything still has to
// sweep, and a helper that ended the test from inside its own teardown would
// turn a leak into a crash.
func recordedPid(harnessPidFile string) int {
	raw, err := os.ReadFile(harnessPidFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return pid
}

// registerPhaseSweep ends the phase's whole process group at the end of the
// test, whether the assertions passed or not. The production escalation is not
// a substitute and never can be: it runs the grace period first, so a test
// that is over before that is up — an assertion that failed, a pid file that
// never appeared, a test this file adds later and forgets — leaves the phase
// and its harness reparented to init and still burning CPU. The sweep is
// therefore the test's own, not the watchdog's.
//
// The kill is the one the escalation makes, `killPhaseProcess` (the whole group
// by its id), behind the same question the escalation asks first — an empty
// group's id belongs to no run here and on a busy machine can already have been
// reused — plus the recorded harness by its pid: the group signal is what
// reaches a grandchild still in the group, and the pid file is the one record of
// it that exists when the group signal landed on nothing. The liveness re-check
// before the direct kill narrows the window in which the kernel could have
// handed the id to an unrelated process.
//
// The ASSERTION is registered first so the LIFO order runs it last, after the
// kill: a group can only be called empty by something that emptied it.
func registerPhaseSweep(t *testing.T, cmd *exec.Cmd, harnessPidFile string) {
	t.Helper()
	t.Cleanup(func() {
		// Polled rather than probed once: a process that was killed a moment
		// ago is still a live one for as long as it takes init to reap it, and
		// the question this answers is whether the sweep ended it, not whether
		// the reap has landed yet.
		if harnessPidFile != "" {
			harness := recordedPid(harnessPidFile)
			if err := waitGone(harness, 3*time.Second); err != nil {
				t.Errorf("the harness (pid %d) outlived the test's sweep of the phase's group (%v)", harness, err)
			}
		}
		if err := waitGroupEmpty(cmd, 3*time.Second); err != nil {
			t.Errorf("the phase's process group was not empty when the test ended: the sweep did not empty it (%v)", err)
		}
	})
	t.Cleanup(func() {
		// Asked, not assumed, for the reason the escalation asks it: a group
		// that is already empty has no run here behind its id, and on a busy
		// machine that id can belong to somebody else by now. The recorded
		// harness is killed either way, because the pid file is the one record
		// of it that survives the group signal landing on nothing.
		if phaseGroupInhabited(cmd) {
			killPhaseProcess(cmd)
		}
		if pid := recordedPid(harnessPidFile); pid > 0 && processAlive(pid) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
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
	registerPhaseSweep(t, cmd, "")

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
		registerPhaseSweep(t, cmd, "")
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
	// A phase that ignores SIGTERM is the one thing in this file that outlives
	// the deadline on its own, so it is the one that must never outlive the
	// test: an assertion above failing before the escalation's grace is up
	// would otherwise leave the phase running forever.
	registerPhaseSweep(t, cmd, "")

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
// phase produces on a deadline: the phase takes the deadline's SIGTERM and
// cmd.Wait returns at once, while the CLI harness it spawned — same group, its
// own stdio, so it does not hold the phase's pipes — ignores the signal and
// survives. The phase being reaped is not the group being empty, so the
// escalation must still reach the harness, and the group must be empty once it
// has.
func TestThePhaseWatchdogSweepsAHarnessThePhaseLeftBehind(t *testing.T) {
	const (
		timeout = 150 * time.Millisecond
		grace   = 250 * time.Millisecond
	)
	pidFile := filepath.Join(t.TempDir(), "harness.pid")
	ctx, cancel, cmd := newPhaseCmd(timeout, phaseWithSweptHarness(pidFile))
	defer cancel()
	// Registered whatever the assertions below do: the sweep is this test's,
	// and it runs after them.
	registerPhaseSweep(t, cmd, pidFile)

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
	// The GROUP, not only the harness the escalation was aimed at: a phase's
	// short `sleep` children are in the same group, and one still running is a
	// group the sweep did not finish.
	if err := waitGroupEmpty(cmd, 3*time.Second); err != nil {
		t.Errorf("the phase's group was still inhabited after the escalation swept it (%v)", err)
	}
}

// TestThePhaseGroupIsSweptWhenTheTestAbortsBeforeTheEscalation is the leak this
// file used to leave behind, and it is not the sweep test's alone. The
// production escalation runs the grace period first, so anything that ends a
// test before that is up — an assertion that failed, a pid file that never
// appeared, a phase that took the deadline's SIGTERM while its harness ignored
// it — leaves the harness running, reparented to init and holding every
// descriptor the test process had. Nothing in production can end it: the
// deadline only ever asked the group to shut down.
//
// So this test runs the same phase and harness with NO watchdog at all, leaves
// the way a failed assertion leaves (the same door, on the same cleanups), and
// asserts at the end that the sweep emptied the group.
func TestThePhaseGroupIsSweptWhenTheTestAbortsBeforeTheEscalation(t *testing.T) {
	const timeout = 150 * time.Millisecond
	pidFile := filepath.Join(t.TempDir(), "harness.pid")
	// The deadline is what ends the phase, so nothing here needs the context
	// itself: no watchdog is launched, and `cancel` is the only half of the
	// pair this test uses.
	_, cancel, cmd := newPhaseCmd(timeout, phaseWithSweptHarness(pidFile))
	defer cancel()
	registerPhaseSweep(t, cmd, pidFile)

	if err := cmd.Start(); err != nil {
		t.Fatalf("start the phase: %v", err)
	}
	harness := readPid(t, pidFile)
	if !processAlive(harness) {
		t.Fatal("the harness was already gone; this case is not covered")
	}

	// The test is over here: no watchdog was ever launched, so the escalation
	// cannot run, and the deadline's SIGTERM — which `defer cancel` delivers
	// once the body returns — ends the phase and is ignored by the harness.
	//
	// The subtest stands in for the failing assertion the issue describes. A
	// failed assertion and a skip leave the test function by the same door
	// (runtime.Goexit) and run the same cleanups, but a deliberately failing
	// subtest would fail this test and a skipped one does not: the property
	// under test is what the cleanups do on the way out, which both leave to
	// identically.
	t.Run("an assertion that ends the test before the grace period is up", func(t *testing.T) {
		t.Skip("this half only has to leave; the sweep has to happen anyway")
	})

	// Reap the phase, or the group still holds a zombie and the sweep cannot
	// be seen to have emptied anything.
	if runErr := cmd.Wait(); runErr == nil {
		t.Fatal("the phase exited on its own instead of taking the deadline's SIGTERM")
	}
}
