//go:build !windows

package ai

import (
	"os/exec"
	"time"

	"github.com/wcatz/ghost/internal/procgroup"
)

// harnessGracePoll is how often the escalation re-asks whether the harness's
// process group is empty while it waits out the grace period. The question is
// cheap (signal 0) and the answer arrives within a scheduling quantum of the
// signal, so polling separates a group that is winding down from one that
// ignored the signal without a second timeout to reason about.
const harnessGracePoll = 20 * time.Millisecond

// setHarnessProcessGroup puts a harness child in its own process group, so the
// deadline's Cancel can ask the WHOLE group to stop rather than only the direct
// child. A harness is a CLI that spawns helpers of its own — a server, a
// wrapper, a subcommand — and those helpers share its stdout, so signalling
// only the direct child leaves the inherited write end of the pipe open and
// os/exec's copy goroutine keeps Wait blocked for the helper's lifetime. This
// is the harness-level half of what cmd/ghost/lifecycle.go does one level up
// for a phase (setPhaseProcessGroup).
func setHarnessProcessGroup(cmd *exec.Cmd) {
	procgroup.SetGroup(cmd)
}

// terminateHarnessProcess asks the harness's whole process group to shut down,
// then force-kills whatever is still in it once the grace period has run out.
// The group signal is the first half; the force-kill is the half os/exec does
// not do, because its own escalation kills only the direct child and a helper
// that ignores SIGTERM would outlive the call as an orphan. Waiting out the
// grace and then killing on the same goroutine that os/exec's cancellation
// called is what makes the orphan gone by the time Wait returns, rather than
// racing a timer that fires beside it.
//
// The returned error is the group signal's, not the escalation's: a group that
// had already exited (ESRCH), or a direct child that has already been reaped
// (os.ErrProcessDone), is the expected race and the one error Wait reads as
// "use the exit status".
func terminateHarnessProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := procgroup.TerminateGroup(cmd); err != nil {
		return err
	}
	// Give the group the grace period before forcing it. A group that empties
	// on the SIGTERM — the well-behaved case — returns here at once.
	if !waitForHarnessGroup(cmd, harnessWaitDelay) {
		procgroup.KillGroup(cmd)
	}
	// Wait for the kill to land, so a caller that returns from Wait the moment
	// this function does not observe a helper that is no longer running.
	_ = waitForHarnessGroup(cmd, harnessWaitDelay)
	return nil
}

// waitForHarnessGroup polls until cmd's process group is empty, reporting
// whether it emptied within the bound. The last poll decides: a group that is
// still inhabited when the bound is up is reported as such rather than as
// empty, so the caller escalates instead of walking away from an orphan.
func waitForHarnessGroup(cmd *exec.Cmd, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if !procgroup.GroupInhabited(cmd) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(harnessGracePoll)
	}
}
