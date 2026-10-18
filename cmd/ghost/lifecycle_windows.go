//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/wcatz/ghost/internal/procgroup"
)

// A Windows phase's group setup is deliberately a NO-OP, unlike the harness
// child's inside internal/ai. A phase is `ghost` itself, a console child that
// `maintenanceContext` relies on catching Ctrl-C/SIGTERM so it can cancel the
// harness calls it is blocked on before it exits; giving it its own process
// group would keep that console event from reaching it. The harness, by
// contrast, is a third-party CLI whose own children are swept by its own
// deadline, so it does take the flag (procgroup.SetGroup). The rest of the
// phase's moves delegate to the shared helpers: on Windows there is no portable
// group signal, so they all reduce to killing the direct child.
func setPhaseProcessGroup(_ *exec.Cmd) {}

// terminatePhaseProcess terminates the phase process. Windows has no portable
// SIGTERM equivalent for a detached child, so this kills it directly.
func terminatePhaseProcess(cmd *exec.Cmd) error {
	return procgroup.TerminateGroup(cmd)
}

// killPhaseProcess is the same as terminatePhaseProcess on Windows: there is
// no process group to target, so the direct child is all that can be killed.
func killPhaseProcess(cmd *exec.Cmd) {
	procgroup.KillGroup(cmd)
}

// phaseGroupInhabited is always false on Windows. There is no process group to
// join or signal, and the deadline's terminatePhaseProcess has already killed
// the direct child outright — there is no graceful grace period to wait out and
// nothing left to escalate against.
func phaseGroupInhabited(cmd *exec.Cmd) bool {
	return procgroup.GroupInhabited(cmd)
}

// maintenanceSignals is what maintenanceContext cancels on. Windows has no
// hangup to catch: os.Interrupt is the console Ctrl-C and SIGTERM is what Go
// maps a console close or shutdown event to.
func maintenanceSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
