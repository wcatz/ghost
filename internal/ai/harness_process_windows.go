//go:build windows

package ai

import (
	"os/exec"

	"github.com/wcatz/ghost/internal/procgroup"
)

func setHarnessProcessGroup(cmd *exec.Cmd) {
	procgroup.SetGroup(cmd)
}

func sweepHarnessProcessGroup(_ *exec.Cmd) {}

func terminateHarnessProcess(cmd *exec.Cmd) error {
	return procgroup.TerminateGroup(cmd)
}
