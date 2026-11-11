//go:build darwin

package procstat

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// StartTime returns an opaque token identifying pid's process creation
// instant, or ("", false) if it can't be determined. KinfoProc's P_starttime
func StartTime(pid int) (string, bool) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", false
	}
	tv := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%d", tv.Sec, tv.Usec), true
}
