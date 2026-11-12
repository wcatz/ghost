package mcpinit

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

func pidInFile(pidPath string) int {
	data, ok := readPIDFile(pidPath)
	if !ok {
		return 0
	}
	pidStr, _, _ := strings.Cut(strings.TrimSpace(string(data)), ":")
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// AcquireLifecycleLock claims the per-project lifecycle lock for the project
// named by project, which may be a name, id, or path (as Store.ResolveProject
// accepts).
func AcquireLifecycleLock(project string) (func(), bool, error) {
	noop := func() {}

	dataDir, err := config.DataDir()
	if err != nil {
		return noop, true, fmt.Errorf("locate data dir: %w", err)
	}
	db, err := sql.Open("sqlite", roDSN(filepath.Join(dataDir, "ghost.db")))
	if err != nil {
		return noop, true, fmt.Errorf("open database: %w", err)
	}
	defer db.Close() //nolint:errcheck

	id, _, err := memory.NewStore(db, nil).ResolveProject(context.Background(), project)
	if err != nil {

		return noop, true, fmt.Errorf("resolve project %s: %w", memory.RefusalArg("project", project), err)
	}
	if id == "" {

		return noop, true, nil
	}
	if !safeProjectIDComponent(id) {
		return noop, true, fmt.Errorf("project id %s is not a safe filename component", memory.RefusalArg("project id", id))
	}

	pidPath := filepath.Join(dataDir, "lifecycle-"+id+".pid")
	claimed, reclaimedAt, err := claimPidFileReclaim(pidPath)
	if err != nil {

		return noop, true, fmt.Errorf("claim the lifecycle lock for project id %s: %w", memory.RefusalArg("project id", id), err)
	}
	if !claimed {
		return nil, false, nil
	}

	if lifecycleStampCoversClaim(dataDir, id, reclaimedAt) {
		_ = WriteLifecycleFailure(id, nil, UnfinishedLifecycleRunError)
	}
	return func() {

		if pidInFile(pidPath) == os.Getpid() {
			_ = os.Remove(pidPath)
		}
	}, true, nil
}

// LifecycleLockHeld reports whether a LIVE process holds projectID's per-project
// lifecycle lock, reading the claim AcquireLifecycleLock writes. It takes the
// RESOLVED project id, not a name, id or path.
func LifecycleLockHeld(dataDir, projectID string) bool {
	if dataDir == "" || projectID == "" || !safeProjectIDComponent(projectID) {
		return false
	}
	return isAlive(filepath.Join(dataDir, "lifecycle-"+projectID+".pid"))
}
