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
	"github.com/wcatz/ghost/internal/fileguard"
	"github.com/wcatz/ghost/internal/memory"
)

// pidInFile returns the pid recorded in pidPath, or 0 when the file is missing
// or malformed. Only the pid is read; the creation-time token that
// isProcessAlive uses is irrelevant for the ownership check in release.
func pidInFile(pidPath string) int {
	data, err := fileguard.ReadSmallRegularFile(pidPath, 4096)
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

// AcquireLifecycleLock claims the per-project lifecycle lock for the project
// named by project, which may be a name, id, or path (as Store.ResolveProject
// accepts).
//
// The identifier is resolved to the project ID before the lock path is built,
// so a manual `ghost lifecycle --project <name>` and a hook-spawned run (which
// passes the ID) contend for the SAME file. The resolved ID is also validated
// as a safe filename component: project ids are unconstrained by the store, so
// a crafted identifier could otherwise make the pid path escape the data dir.
//
// The lock is claimed only by the coordinator process itself. The hook does not
// pre-claim on the child's behalf: it cannot know the child's pid until after
// Start, and a claim written between Start and that write would make the child
// see a foreign (parent) pid and abort — silently dropping the whole
// reflect/resolve/supersede cycle. Making the child the sole claimer removes
// that window; a redundant spawn simply loses the claim and exits.
//
// Returns (release, true, nil) when this process holds the claim;
// (nil, false, nil) when another live run holds it; and (noop, true, err)
// when the lock could not be evaluated at all — the caller should warn and
// continue, because the phases are convergent and skipping maintenance is
// worse than a rare overlap.
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
		return noop, true, fmt.Errorf("resolve project %q: %w", project, err)
	}
	if id == "" {
		// Unknown project: nothing to lock, and the phases will report it.
		return noop, true, nil
	}
	if !safeProjectIDComponent(id) {
		return noop, true, fmt.Errorf("project id %q is not a safe filename component", id)
	}

	pidPath := filepath.Join(dataDir, "lifecycle-"+id+".pid")
	if !claimPidFile(pidPath) {
		return nil, false, nil
	}
	return func() {
		// Remove the claim only while it is still ours; a later run re-claiming
		// the file must not have its pid deleted by this one's exit.
		if pidInFile(pidPath) == os.Getpid() {
			_ = os.Remove(pidPath)
		}
	}, true, nil
}
