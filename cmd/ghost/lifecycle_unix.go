//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// setPhaseProcessGroup puts a lifecycle phase in its own process group, so the
// whole group can be signalled at once. The phase is a `ghost` process that
// spawns CLI harnesses (claude/opencode/codex/goose); signalling only the
// direct child would orphan those harnesses on a timeout.
func setPhaseProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminatePhaseProcess asks the phase's whole process group to shut down
// gracefully.
func terminatePhaseProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	// Negative PID targets the process group created by setPhaseProcessGroup.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	return nil
}

// killPhaseProcess force-kills the phase's whole process group. WaitDelay's
// own escalation calls Process.Kill, which reaches only the direct child —
// a harness grandchild that ignored SIGTERM would then survive as an orphan,
// so the watchdog uses this instead.
func killPhaseProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// ESRCH (the group already exited) is the expected race and is ignored.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// phaseGroupInhabited reports whether anything is still in the phase's process
// group. It answers the question the escalation actually turns on: cmd.Wait
// returns as soon as the direct child is reaped, and a phase is a plain ghost
// process with no SIGTERM handler, so the phase process is gone within
// milliseconds of the deadline's group signal while the CLI harness it spawned
// — same group — can still be running. Signal 0 asks the question without
// delivering anything, and ESRCH is the one answer that means empty: a group
// whose id belongs to no run here, and which must not be signalled because on a
// busy machine it can already have been reused. EPERM (it exists but is not
// ours) counts as inhabited, and any other error is treated the same way rather
// than as an excuse to skip a group that may hold an orphan.
func phaseGroupInhabited(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	return syscall.Kill(-cmd.Process.Pid, 0) != syscall.ESRCH
}
