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
