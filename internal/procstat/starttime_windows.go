//go:build windows

package procstat

import (
	"strconv"

	"golang.org/x/sys/windows"
)

// StartTime returns an opaque process-creation token for pid, or reports that it cannot be determined.
func StartTime(pid int) (string, bool) {
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
