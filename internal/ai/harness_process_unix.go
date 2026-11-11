//go:build !windows

package ai

import (
	"os/exec"
	"time"

	"github.com/wcatz/ghost/internal/procgroup"
)

const harnessGracePoll = 20 * time.Millisecond

func setHarnessProcessGroup(cmd *exec.Cmd) {
	procgroup.SetGroup(cmd)
}

func terminateHarnessProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := procgroup.TerminateGroup(cmd); err != nil {
		return err
	}

	if !waitForHarnessGroup(cmd, harnessWaitDelay) {
		procgroup.KillGroup(cmd)
	}

	_ = waitForHarnessGroup(cmd, harnessWaitDelay)
	return nil
}

func sweepHarnessProcessGroup(cmd *exec.Cmd) {
	if !procgroup.GroupInhabited(cmd) {
		return
	}
	_ = terminateHarnessProcess(cmd)
}

func waitForHarnessGroup(cmd *exec.Cmd, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if !procgroup.GroupInhabited(cmd) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(harnessGracePoll)
	}
}
