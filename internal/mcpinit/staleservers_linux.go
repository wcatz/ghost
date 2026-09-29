//go:build linux

package mcpinit

// StaleGhostServers is the entry point for the Linux scan, and the only
// platform where it can answer.
//
// The build tag is load-bearing rather than a convenience: the scan reads
// /proc/<pid>/{comm,cmdline,exe,stat}, and there is no portable equivalent.
// Everywhere else the honest answer is "not checked" — see the other
// implementation, which says exactly that instead of returning an empty list
// that would read as "nothing is stale".

// StaleGhostServers lists the running `ghost mcp` processes that are not
// running the binary named by installed. root is the proc tree to read, so a
// test can point it at a fixture; production passes procRoot ("/proc").
// installed is the path of THIS command's own binary — os.Executable() in both
// production call sites — and not a discovered installation, so a finding means
// "a different file", never "an older build" (see the header in
// staleservers.go).
//
// The boolean reports whether the scan could run at all. False means the
// question is UNANSWERED, which is not the same as an empty answer.
func StaleGhostServers(root, installed string) ([]StaleGhostServer, bool) {
	return staleOnProc(root, installed)
}
