//go:build !windows

package procgroup

import (
	"os/exec"
	"syscall"
)

// SetGroup puts cmd in its own process group so group termination reaches the whole child process tree.
func SetGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// TerminateGroup asks cmd's process group to shut down gracefully.
// It falls back to signalling the direct child if the group is already gone.
func TerminateGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	return nil
}

// KillGroup force-kills cmd's process group, falling back to the direct child if needed.
func KillGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// GroupInhabited reports whether cmd's process group still has members.
func GroupInhabited(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	return syscall.Kill(-cmd.Process.Pid, 0) != syscall.ESRCH
}
