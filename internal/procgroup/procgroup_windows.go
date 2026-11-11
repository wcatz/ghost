//go:build windows

package procgroup

import (
	"os/exec"
	"syscall"
)

// SetGroup starts cmd in its own process group on Windows.
func SetGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// TerminateGroup kills the direct child on Windows.
func TerminateGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// KillGroup is the same as TerminateGroup on Windows: there is no process group
// to target, so the direct child is all that can be killed.
func KillGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

// GroupInhabited reports false on Windows because process-group membership is not tracked.
func GroupInhabited(_ *exec.Cmd) bool { return false }
