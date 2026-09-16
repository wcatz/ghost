package mcpinit

import "github.com/wcatz/ghost/internal/procstat"

// processStartTime and isProcessAlive delegate to internal/procstat, which
// now owns the platform-specific process-identity code. The extraction was
// forced by internal/scratch: it needs the same PID-reuse token, but mcpinit
// already imports scratch, so a shared package is the only way to reuse the
// implementations instead of duplicating them. These unexported wrappers keep
// this package's call sites and platform tests on their existing names, so no
// behaviour changes here.
func processStartTime(pid int) (string, bool) { return procstat.StartTime(pid) }

// isProcessAlive is the delegation wrapper for procstat.IsAlive; see that
// function for the full semantics.
func isProcessAlive(pid int, wantToken string, haveToken bool) bool {
	return procstat.IsAlive(pid, wantToken, haveToken)
}
