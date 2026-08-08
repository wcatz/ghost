//go:build windows

package mcpinit

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachProcess starts cmd in a new process group so it survives this
// short-lived hook process exiting and won't receive signals (e.g. console
// close) sent to Claude Code's process group. Windows has no setsid
// equivalent, so CREATE_NEW_PROCESS_GROUP is the closest analog.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// isProcessAlive reports whether pid names a running process. Unlike POSIX,
// Windows aggressively recycles PIDs, so a PID that opens successfully but
// has already exited (GetExitCodeProcess returns anything but
// STILL_ACTIVE) is treated as not alive — otherwise a reused PID belonging
// to an unrelated process would be mistaken for the one that owned the PID
// file.
func isProcessAlive(pid int) bool {
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
	return exitCode == stillActive
}
