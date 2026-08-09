//go:build windows

package mcpinit

import (
	"os/exec"
	"strconv"
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
// file. If haveToken is true, a live PID is additionally required to still
// carry wantToken as its creation-time token — a mismatch means the OS
// recycled pid to an unrelated process after the original one exited,
// which STILL_ACTIVE alone can't distinguish from the original still
// running. A transient failure to read the fresh token fails open to
// "alive" rather than flapping a live process to "dead".
func isProcessAlive(pid int, wantToken string, haveToken bool) bool {
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
	token, ok := processStartTime(pid)
	if !ok {
		return true
	}
	return token == wantToken
}

// processStartTime returns an opaque token identifying pid's process
// creation instant, or ("", false) if it can't be determined. The
// creation Filetime is wall-clock (100ns intervals since 1601-01-01), so
// it is already reboot-safe with no extra prefixing needed.
func processStartTime(pid int) (string, bool) {
	if pid <= 0 || int64(pid) > 0xFFFFFFFF {
		return "", false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	var creationTime, exitTime, kernelTime, userTime windows.Filetime
	if err := windows.GetProcessTimes(h, &creationTime, &exitTime, &kernelTime, &userTime); err != nil {
		return "", false
	}
	value := uint64(creationTime.HighDateTime)<<32 | uint64(creationTime.LowDateTime)
	return strconv.FormatUint(value, 10), true
}
