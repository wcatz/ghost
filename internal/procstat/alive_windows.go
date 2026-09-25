//go:build windows

package procstat

import (
	"errors"

	"golang.org/x/sys/windows"
)

// Check reports the strongest process-identity conclusion the platform can
// prove. Access failures are Unknown, never Dead.
func Check(pid int, wantToken string, haveToken bool) State {
	if pid <= 0 || int64(pid) > 0xFFFFFFFF {
		return StateDead
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			return StateDead
		}
		return StateUnknown
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	const stillActive = 259 // STILL_ACTIVE, per the Win32 GetExitCodeProcess docs
	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return StateUnknown
	}
	if exitCode != stillActive {
		return StateDead
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
