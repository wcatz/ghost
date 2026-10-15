//go:build windows

package procgroup

import (
	"os/exec"
	"syscall"
)

// SetGroup starts cmd in its own process group. Windows has no portable
// positive group signal, so this does not let TerminateGroup or KillGroup reach
// a grandchild the way the unix Setpgid does; what it does buy is that a
// Ctrl-C and its console events are not delivered to the child with the
// parent, which CREATE_NEW_PROCESS_GROUP is the closest analogue of and the
// same flag the harness spawn already used.
func SetGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// TerminateGroup kills the direct child. Windows has no portable group signal,
// so this is the same as the default CommandContext cancellation and
// KillGroup; the caller's WaitDelay is what bounds a helper that keeps the
// child's stdout open.
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

// GroupInhabited is always false on Windows. There is no process group to join
// or signal, and the deadline's TerminateGroup has already killed the direct
// child outright — there is nothing left to escalate against.
func GroupInhabited(_ *exec.Cmd) bool { return false }
