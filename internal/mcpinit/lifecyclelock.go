package mcpinit

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wcatz/ghost/internal/config"
)

// pidInFile returns the pid recorded in pidPath, or 0 when the file is missing
// or malformed. It reads only the pid, ignoring the creation-time token that
// isProcessAlive uses.
func pidInFile(pidPath string) int {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return 0
	}
	pidStr, _, _ := strings.Cut(strings.TrimSpace(string(data)), ":")
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// AcquireLifecycleLock ensures this process holds the per-project lifecycle
// claim before it runs the phases.
//
// The stop hook claims the file on the child's behalf before spawning it (it
// writes the child's pid after Start), so a spawned coordinator normally finds
// the file already holding its own pid and proceeds without re-claiming — that
// is why the self-pid case is a success rather than a conflict. A coordinator
// no hook spawned, i.e. a manual `ghost lifecycle`, claims the file itself, so
// it can no longer overlap a hook-spawned run for the same project (observed
// while verifying: two reflect/supersede chains ran for one project at once).
//
// Returns (release, true) when this process holds the claim, (nil, false) when
// another live run holds it. It fails OPEN on any setup error: a lifecycle that
// cannot compute its lock path must still run, since the phases are convergent
// and a skipped maintenance run is worse than a rare overlap.
func AcquireLifecycleLock(projectID string) (func(), bool) {
	noop := func() {}
	dataDir, err := config.DataDir()
	if err != nil {
		return noop, true
	}
	pidPath := filepath.Join(dataDir, "lifecycle-"+projectID+".pid")

	if pidInFile(pidPath) == os.Getpid() {
		return noop, true // the hook already claimed this run
	}
	if !claimPidFile(pidPath) {
		return nil, false
	}
	return func() {
		// Remove the claim only while it is still ours; a later run re-claiming
		// the file must not have its pid deleted by this one's exit.
		if pidInFile(pidPath) == os.Getpid() {
			_ = os.Remove(pidPath)
		}
	}, true
}
