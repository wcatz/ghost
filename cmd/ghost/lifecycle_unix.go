//go:build !windows

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/wcatz/ghost/internal/procgroup"
)

// The lifecycle phase's process-group moves are the shared ones in
// internal/procgroup, the same package the harness spawn (internal/ai) uses, so
// the two cannot drift apart. The bodies below are named for the phase because
// that is the seam the watchdog and its tests are written against. On unix the
// phase follows the harness's behaviour exactly: it sits in its own process
// group so the phase deadline can sweep it and its CLI helpers. The Windows
// group setup is deliberately kept a no-op (see lifecycle_windows.go); a ghost
// phase is its own process and must still receive console Ctrl-C alongside the
// rest of Ghost, which is what maintenanceContext is built for.
//
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

// maintenanceSignals is what maintenanceContext cancels on. SIGHUP is caught
// beside SIGINT and SIGTERM because closing the terminal or losing the SSH
// connection a foreground run is under sends it, and Go's default action for it
// is to exit: the coordinator would die and leave the phase, in a group of its
// own, running behind a claim that names a dead pid. It is caught only when it
// is not already ignored. A run started under nohup inherits SIGHUP ignored and
// asked to outlive its terminal, and signal.Notify would replace that ignore
// with a handler, turning the hangup nohup absorbs into an interruption.
func maintenanceSignals() []os.Signal {
	sigs := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		sigs = append(sigs, syscall.SIGHUP)
	}
	return sigs
}
