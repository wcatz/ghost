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
// signaling the process. If haveToken is true, a live PID is additionally
// required to still carry wantToken as its creation-time token — a
// mismatch means the OS recycled pid to an unrelated process after the
// original one exited, which plain signal-0 can't distinguish from the
// original still running. A transient failure to read the fresh token
// fails open to "alive" rather than flapping a live process to "dead".
func isProcessAlive(pid int, wantToken string, haveToken bool) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if proc.Signal(syscall.Signal(0)) != nil {
		return false
	}
	if !haveToken {
		return true
	}
	token, ok := processStartTime(pid)
	if !ok {
		return true
	}
	return token == wantToken
}
