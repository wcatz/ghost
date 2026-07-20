//go:build !windows

package mcpinit

import (
	"os/exec"
	"syscall"
)

// detachProcess starts cmd in a new session so it survives this short-lived
// hook process exiting and won't receive signals sent to Claude Code's
// process group.
func detachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
