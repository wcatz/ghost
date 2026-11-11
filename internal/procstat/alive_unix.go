//go:build !windows

package procstat

import (
	"os"
	"syscall"
)

// IsAlive reports whether pid names a running process and still matches wantToken when provided.
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
