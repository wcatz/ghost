//go:build !windows && !linux && !darwin

package procstat

// StartTime has no known implementation on this OS. Returning ("", false)
// degrades callers to legacy PID-only liveness checking, identical to the
// behavior before tokens existed — no regression, just no reuse detection on
// whatever unlisted Unix this is.
func StartTime(pid int) (string, bool) {
	return "", false
}
