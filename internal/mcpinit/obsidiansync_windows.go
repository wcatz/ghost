//go:build windows

package mcpinit

import (
	"os/exec"
	"syscall"
)

// detachProcess starts cmd in a new process group so it survives this
// short-lived hook process exiting and won't receive signals (e.g. console
// close) sent to Claude Code's process group. Windows has no setsid
// equivalent, so CREATE_NEW_PROCESS_GROUP is the closest analog.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}
