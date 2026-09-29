//go:build !linux

package mcpinit

// The non-Linux half of the stale-server scan.
//
// There is no portable way to ask "which executable is this process running, and
// has that file been replaced" — macOS would need `ps` and lsof, Windows the
// process API, and each answers differently and partially. Rather than ship a
// partial answer, this reports UNANSWERED and lets every caller stay silent.
//
// That distinction is why the boolean is in the signature. A scan returning an
// empty list reads as "no stale servers", and an operator who believed that
// would conclude their install is healthy on a machine where the check never
// ran. Silence is the honest default, and it is what ReportStaleServers prints
// in both this case and the healthy one.

// StaleGhostServers cannot answer off Linux, and says so by returning false for
// `checked`. The arguments are accepted and ignored so the signature is the same
// on every platform, which is what lets callers call it unconditionally.
func StaleGhostServers(_, _ string) ([]StaleGhostServer, bool) {
	return nil, false
}
