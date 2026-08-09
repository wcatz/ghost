//go:build linux

package mcpinit

import (
	"os"
	"strconv"
	"strings"
)

// processStartTime returns an opaque token identifying pid's process
// creation instant, or ("", false) if it can't be determined (process
// gone, unreadable /proc entry, permission denied).
//
// The raw source — /proc/<pid>/stat field 22 ("starttime") — is clock
// ticks since boot, not wall-clock, so it is NOT by itself reboot-safe:
// .pid files persist in dataDir across reboots, and a freshly-started,
// unrelated process after a reboot can coincidentally have the same
// boot-relative tick count a stale file recorded before the reboot. The
// machine's boot ID (a fresh UUID generated at every boot) is prefixed so
// a pre-reboot token can never match a post-reboot one, regardless of tick
// coincidence.
func processStartTime(pid int) (string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	line := string(data)

	// comm (field 2) is wrapped in parens and can itself contain spaces and
	// parens, so split on the *last* ')' to find where it ends unambiguously.
	idx := strings.LastIndexByte(line, ')')
	if idx == -1 || idx+2 >= len(line) {
		return "", false
	}
	// Fields after the split start at field 3 (state); field 22 (starttime)
	// is therefore remainder-index 22-3 = 19.
	fields := strings.Fields(line[idx+2:])
	const starttimeIdx = 19
	if starttimeIdx >= len(fields) {
		return "", false
	}
	starttime := fields[starttimeIdx]

	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(bootID)) + "-" + starttime, true
}
