//go:build !linux

package mcpinit

// StaleGhostServers reports that stale-server checks are unavailable on non-Linux platforms.
func StaleGhostServers(_, _ string) ([]StaleGhostServer, bool) {
	return nil, false
}
