//go:build !windows

package mcpinit

import (
	"os"
	"os/exec"
	"syscall"
)

// detachProcess starts cmd in a new session so it survives this short-lived
// hook process exiting and won't receive signals sent to Claude Code's
// process group.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// isProcessAlive reports whether pid names a running process, by sending
// it signal 0 — this checks existence and permission without actually
// signaling the process.
func isProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
