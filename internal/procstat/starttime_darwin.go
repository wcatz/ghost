//go:build darwin

package procstat

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// StartTime returns an opaque process-creation token for pid, or reports that it cannot be determined.
func StartTime(pid int) (string, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", false
	}
	tv := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%d", tv.Sec, tv.Usec), true
}
