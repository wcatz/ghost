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
// gone (ESRCH) is the expected race: the direct child is signalled instead, and
// a process that has already exited reports os.ErrProcessDone, which os/exec
// reads as "use the exit status" rather than as a cancellation failure.
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
// escalation kills only the direct child and would leave a helper running as
// an orphan.
func KillGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	// ESRCH (the group already exited) is the expected race and is ignored.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
}

// GroupInhabited reports whether anything is still in cmd's process group.
// Signal 0 asks the question without delivering anything, and ESRCH is the one
// answer that means empty: a group whose id belongs to no run here, and which
// must not be signalled because on a busy machine it can already have been
// reused. EPERM (it exists but is not ours) counts as inhabited, and any other
// error is treated the same way rather than as an excuse to skip a group that
// may hold an orphan.
func GroupInhabited(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	return syscall.Kill(-cmd.Process.Pid, 0) != syscall.ESRCH
}
