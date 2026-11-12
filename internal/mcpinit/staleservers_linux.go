//go:build linux

package mcpinit

// StaleGhostServers lists running ghost MCP processes not using the installed binary.
func StaleGhostServers(root, installed string) ([]StaleGhostServer, bool) {
	return staleOnProc(root, installed)
}
