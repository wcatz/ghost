//go:build !windows

package procstat

import (
	"os"
	"syscall"
)

// IsAlive reports whether pid names a running process, by sending it signal
// 0 — this checks existence and permission without actually signaling the
// process. If haveToken is true, a live PID is additionally required to still
// carry wantToken as its creation-time token — a mismatch means the OS
// recycled pid to an unrelated process after the original one exited, which
// plain signal-0 can't distinguish from the original still running. A
// transient failure to read the fresh token fails open to "alive" rather than
// flapping a live process to "dead".
func IsAlive(pid int, wantToken string, haveToken bool) bool {
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
	token, ok := StartTime(pid)
	if !ok {
		return true
	}
	return token == wantToken
}
