//go:build windows

package main

import "os/exec"

// setPhaseProcessGroup is a no-op on Windows: there is no process group to
// join, and the default CommandContext kill already targets the direct child.
func setPhaseProcessGroup(cmd *exec.Cmd) {}

// terminatePhaseProcess terminates the phase process. Windows has no portable
// SIGTERM equivalent for a detached child, so this kills it directly.
func terminatePhaseProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// killPhaseProcess is the same as terminatePhaseProcess on Windows: there is
// no process group to target, so the direct child is all that can be killed.
func killPhaseProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
