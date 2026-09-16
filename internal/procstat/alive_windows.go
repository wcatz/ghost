//go:build windows

package procstat

import (
	"golang.org/x/sys/windows"
)

// IsAlive reports whether pid names a running process. Unlike POSIX, Windows
// aggressively recycles PIDs, so a PID that opens successfully but has already
// exited (GetExitCodeProcess returns anything but STILL_ACTIVE) is treated as
// not alive — otherwise a reused PID belonging to an unrelated process would
// be mistaken for the one that owned the PID file. If haveToken is true, a
// live PID is additionally required to still carry wantToken as its
// creation-time token — a mismatch means the OS recycled pid to an unrelated
// process after the original one exited, which STILL_ACTIVE alone can't
// distinguish from the original still running. A transient failure to read
// the fresh token fails open to "alive" rather than flapping a live process
// to "dead".
func IsAlive(pid int, wantToken string, haveToken bool) bool {
	if pid <= 0 || int64(pid) > 0xFFFFFFFF {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	const stillActive = 259 // STILL_ACTIVE, per the Win32 GetExitCodeProcess docs

	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false
	}
	if exitCode != stillActive {
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
