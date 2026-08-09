//go:build darwin

package mcpinit

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// processStartTime returns an opaque token identifying pid's process
// creation instant, or ("", false) if it can't be determined. KinfoProc's
// P_starttime is a wall-clock Timeval (seconds+microseconds since epoch),
// so — unlike Linux's boot-relative ticks — it is already reboot-safe with
// no extra prefixing needed.
func processStartTime(pid int) (string, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", false
	}
	tv := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%d", tv.Sec, tv.Usec), true
}
