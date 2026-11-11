//go:build !windows

package procgroup

import (
	"os/exec"
	"syscall"
)

// SetGroup puts cmd in its own process group, so TerminateGroup and KillGroup
// can reach every helper the child spawns rather than only the direct child.
func SetGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// TerminateGroup asks cmd's whole process group to shut down gracefully. The
// negative PID targets the group SetGroup created. A group that is already
func TerminateGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	return nil
}

// KillGroup force-kills cmd's whole process group. Callers use this after a
// grace period for a group that ignored TerminateGroup, because os/exec's own
func KillGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// GroupInhabited reports whether anything is still in cmd's process group.
// Signal 0 asks the question without delivering anything, and ESRCH is the one
func GroupInhabited(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	return syscall.Kill(-cmd.Process.Pid, 0) != syscall.ESRCH
}
