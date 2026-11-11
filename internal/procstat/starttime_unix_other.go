//go:build !windows && !linux && !darwin

package procstat

// StartTime reports that no process-creation token is available on this platform.
func StartTime(pid int) (string, bool) {
	return "", false
}
