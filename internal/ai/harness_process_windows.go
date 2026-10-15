//go:build windows

package ai

import (
	"os/exec"
	"syscall"
)

// setHarnessProcessGroup starts the harness child in its own process group.
// Windows has no portable positive group signal, so this does not let Cancel
// reach a grandchild the way the unix Setpgid does; what it does buy is that a
// console control event aimed at Ghost's own group does not reach the child.
// CREATE_NEW_PROCESS_GROUP is the closest analogue of Setpgid and is the same
// choice mcpinit.detachProcess makes.
func setHarnessProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// terminateHarnessProcess kills the direct child. There is no portable way to
// signal a Windows process group, so this is the same kill CommandContext would
// have used by default; WaitDelay is what bounds a grandchild that keeps the
// child's stdout pipe open after the deadline.
func terminateHarnessProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
