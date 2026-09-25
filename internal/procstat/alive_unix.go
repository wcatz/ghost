//go:build !windows

package procstat

import (
	"errors"
	"os"
	"syscall"
)

// Check reports the strongest process-identity conclusion the platform can
// prove. Permission and unsupported-platform errors are Unknown, never Dead.
func Check(pid int, wantToken string, haveToken bool) State {
	if pid <= 0 {
		return StateDead
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return StateUnknown
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			return StateDead
		}
		return StateUnknown
	}
	if !haveToken {
		return StateAlive
	}
	token, ok := StartTime(pid)
	if !ok {
		return StateUnknown
	}
	if token != wantToken {
		return StateDead
	}
	return StateAlive
}

// IsAlive preserves the historical boolean API while treating an indeterminate
// probe as alive. Destructive callers should use Check directly.
func IsAlive(pid int, wantToken string, haveToken bool) bool {
	return Check(pid, wantToken, haveToken) != StateDead
}
