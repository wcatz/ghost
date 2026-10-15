//go:build !windows

package ai

import (
	"os/exec"
	"syscall"
)

// setHarnessProcessGroup puts a harness child in its own process group, so the
// deadline's Cancel can ask the WHOLE group to stop rather than only the direct
// child. A harness is a CLI that spawns helpers of its own — a server, a
// wrapper, a subcommand — and those helpers share its stdout, so signalling
// only the direct child leaves the inherited write end of the pipe open and
// os/exec's copy goroutine keeps Wait blocked for the helper's lifetime. This
// is the harness-level half of what cmd/ghost/lifecycle.go does one level up
// for a phase (setPhaseProcessGroup).
func setHarnessProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateHarnessProcess asks the harness's whole process group to shut down
// gracefully, so a well-behaved child and its helpers can flush and exit before
// WaitDelay is up. The negative PID targets the group setHarnessProcessGroup
// created. A group that is already gone (ESRCH) is the expected race: the
// direct child is signalled instead, and a process that has already exited
// reports os.ErrProcessDone, which is the one error Wait reads as "use the exit
// status" rather than as a cancellation failure.
func terminateHarnessProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	return nil
}
