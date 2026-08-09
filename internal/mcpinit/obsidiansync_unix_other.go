//go:build !windows && !linux && !darwin

package mcpinit

// processStartTime has no known implementation on this OS. Returning
// ("", false) degrades callers to legacy PID-only liveness checking,
// identical to today's behavior — no regression, just no reuse detection
// on whatever unlisted Unix this is.
func processStartTime(pid int) (string, bool) {
	return "", false
}
