//go:build windows

package main

import "os/exec"

// setPhaseProcessGroup is a no-op on Windows: there is no process group to
// join, and the default CommandContext kill already targets the direct child.
func setPhaseProcessGroup(cmd *exec.Cmd) {}

// terminatePhaseProcess terminates the phase process. Windows has no portable
// SIGTERM equivalent for a detached child, so this kills it directly;
// exec.CommandContext's WaitDelay then bounds how long the wait may take.
func terminatePhaseProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
