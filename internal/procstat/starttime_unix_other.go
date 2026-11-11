//go:build !windows && !linux && !darwin

package procstat

// StartTime has no known implementation on this OS. Returning ("", false)
// degrades callers to legacy PID-only liveness checking, identical to the
func StartTime(pid int) (string, bool) {
	return "", false
}
