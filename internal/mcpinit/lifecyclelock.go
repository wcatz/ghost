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

// pidInFile returns the pid recorded in pidPath, or 0 when the file is missing
// or malformed. Only the pid is read; the creation-time token that
// isProcessAlive uses is irrelevant for the ownership check in release.
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

	// A GHOST_DEV_FORBID_DATA_DIR refusal fails the claim, which is this
	// function's own answer to a store it cannot read: a lifecycle that cannot
	// see the project has nothing to lock, and the caller warns and continues
	// (#721).
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
		// The wrapped error is already safely rendered — it is the store's own
		// refusal, which goes through `memory.ProjectArg` — but the operand printed
		// beside it is the caller's, and `--project` is the session's own clone URL
		// about as often as it is a project name (#839). Quoting it here put a
		// credential-shaped operand into the operator's stderr in the one package
		// that had no call to the renderer.
		return noop, true, fmt.Errorf("resolve project %s: %w", memory.ProjectArg("project", project), err)
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

// LifecycleLockHeld reports whether a LIVE process holds projectID's per-project
// lifecycle lock, reading the claim AcquireLifecycleLock writes. It takes the
// RESOLVED project id, not a name, id or path: resolving an identifier is a
// database read, and a caller that has to ask about many projects has already
// resolved them.
//
// It is a reader and not a second claim, and the difference is the whole reason
// it exists. A command that must not interfere with a running lifecycle — #730's
// `ghost history compact` rewrites memory_history and moves memories.updated_at
// under an unattended pass that is appending to both — has to be able to ask
// whether one is running, and asking must not take the lock itself: a claim
// would turn the question into a second writer and would have to be released by
// a caller with no business holding it.
//
// It reads WITHOUT taking the claim file's flock, so it creates nothing: a check
// that wrote a ".lock" sibling would make a dry run change the store it is
// previewing. The claim itself is published by write-temp-then-rename
// (atomicWritePID), so a reader never sees a half-written one, and the window
// this leaves is the one a reader cannot close without becoming a writer: a
// lifecycle that claims immediately after this returns is not seen, and the worst
// that follows is that the repair and the run interleave — the repair only ever
// removes rows that changed nothing, so a row appended during it survives to the
// next run rather than being lost.
//
// Every unknown reads as NOT held. A missing, unreadable or unparseable claim is
// no run to wait for, and a reader that reported "held" on a corrupt file would
// refuse every future repair with no way for an operator to clear it. The
// project id is validated as a safe filename component for the same reason
// AcquireLifecycleLock validates it: the claim path is built from it, and an
// unconstrained id joined into a path can be pointed outside the data dir.
func LifecycleLockHeld(dataDir, projectID string) bool {
	if dataDir == "" || projectID == "" || !safeProjectIDComponent(projectID) {
		return false
	}
	return isAlive(filepath.Join(dataDir, "lifecycle-"+projectID+".pid"))
}
