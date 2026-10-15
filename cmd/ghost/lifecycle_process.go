package main

import (
	"os/exec"

	"github.com/wcatz/ghost/internal/procgroup"
)

// The lifecycle phase's process-group moves are the shared ones in
// internal/procgroup, the same package the harness spawn (internal/ai) uses, so
// the two cannot drift apart. The bodies below are named for the phase because
// that is the seam the watchdog and its tests are written against.

// setPhaseProcessGroup puts a lifecycle phase in its own process group, so the
// deadline's Cancel can signal it. The CLI harnesses the phase spawns are NOT
// in this group: each runs in its own (boundHarnessProcess, internal/ai) so the
// harness's own deadline can sweep it, and the phase command catches the signal
// through maintenanceContext, cancelling its work so each in-flight harness's
// own Cancel signals its group before the phase exits. This group signal is
// what starts that wind-down, and killPhaseProcess is the backstop for a phase
// that does not take it.
func setPhaseProcessGroup(cmd *exec.Cmd) {
	procgroup.SetGroup(cmd)
}

// terminatePhaseProcess asks the phase's whole process group to shut down
// gracefully.
func terminatePhaseProcess(cmd *exec.Cmd) error {
	return procgroup.TerminateGroup(cmd)
}

// killPhaseProcess force-kills the phase's whole process group. WaitDelay's
// own escalation calls Process.Kill, which reaches only the direct child — a
// harness grandchild that ignored SIGTERM would then survive as an orphan, so
// the watchdog uses this instead.
func killPhaseProcess(cmd *exec.Cmd) {
	procgroup.KillGroup(cmd)
}

// phaseGroupInhabited reports whether anything is still in the phase's process
// group. It answers the question the escalation actually turns on: cmd.Wait
// returns as soon as the direct child is reaped, and a phase is a plain ghost
// process with no SIGTERM handler, so the phase process is gone within
// milliseconds of the deadline's group signal while the CLI harness it spawned
// — same group — can still be running.
func phaseGroupInhabited(cmd *exec.Cmd) bool {
	return procgroup.GroupInhabited(cmd)
}
