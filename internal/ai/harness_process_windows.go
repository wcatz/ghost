//go:build windows

package ai

import (
	"os/exec"

	"github.com/wcatz/ghost/internal/procgroup"
)

// setHarnessProcessGroup starts the harness child in its own process group.
// Windows has no portable positive group signal, so this does not let Cancel
// reach a grandchild the way the unix Setpgid does; what it does buy is that a
// Ctrl-C and its console events are not delivered to the child with the parent.
// CREATE_NEW_PROCESS_GROUP is the closest analogue of Setpgid. It is the
// HARNESS only: the lifecycle phase sibling deliberately does NOT take this flag
// on Windows (cmd/ghost's setPhaseProcessGroup is a no-op there), because a
// phase is ghost itself and must keep receiving the console Ctrl-C that
// maintenanceContext relies on.
func setHarnessProcessGroup(cmd *exec.Cmd) {
	procgroup.SetGroup(cmd)
}

// sweepHarnessProcessGroup has nothing to reach on Windows. There is no
// portable process group to ask about or signal, and Wait has already reaped
// the direct child; a helper that held the pipes was bounded by WaitDelay and
// cannot be signalled here, exactly as on the Cancel path.
func sweepHarnessProcessGroup(_ *exec.Cmd) {}

// terminateHarnessProcess kills the direct child. There is no portable way to
// signal a grandchild on Windows, so this is the same kill exec.CommandContext
// would have used by default; WaitDelay is what bounds a grandchild that keeps
// the child's stdout open.
func terminateHarnessProcess(cmd *exec.Cmd) error {
	return procgroup.TerminateGroup(cmd)
}
