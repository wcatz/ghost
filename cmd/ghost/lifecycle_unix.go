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
// gracefully. exec.CommandContext calls this on deadline expiry; its WaitDelay
// escalates to a kill if the group has not exited in time.
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
