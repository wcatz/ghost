//go:build linux

package procstat

import (
	"os"
	"strconv"
	"strings"
)

// StartTime returns an opaque process-creation token for pid, or reports that it cannot be determined.
func StartTime(pid int) (string, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	line := string(data)

	idx := strings.LastIndexByte(line, ')')
	if idx == -1 || idx+2 >= len(line) {
		return "", false
	}
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
