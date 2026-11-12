//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/wcatz/ghost/internal/procgroup"
)

func setPhaseProcessGroup(_ *exec.Cmd) {}

func terminatePhaseProcess(cmd *exec.Cmd) error {
	return procgroup.TerminateGroup(cmd)
}

func killPhaseProcess(cmd *exec.Cmd) {
	procgroup.KillGroup(cmd)
}

func phaseGroupInhabited(cmd *exec.Cmd) bool {
	return procgroup.GroupInhabited(cmd)
}

func maintenanceSignals() []os.Signal {
	return []os.Signal{os.Interrupt, syscall.SIGTERM}
}
