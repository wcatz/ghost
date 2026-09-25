package mcpinit

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wcatz/ghost/internal/config"
)

// The Stop hook fires after EVERY assistant turn, and the only guard on the
// auto-consolidation spawn used to be "is one already running". A session of a
// few dozen turns therefore paid for a few dozen reflect→resolve→supersede
// chains (issue #541 counted 543 runs in one lifecycle.log), even though
// reflect's input signature skipped the unchanged set most of the time. The
// cooldown below bounds how often a project's chain may START
// (lifecycle.min_interval, default 30m).
//
// The timestamp lives in a file, not a row. The hook's synchronous path must not
// open the store — it runs inside somebody else's turn and reads the config and
// (for project resolution) a read-only handle, and a question that costs one
// stat must not cost a connection. So the record is a per-project file in the
// Ghost data dir, beside the lifecycle-<project>.pid lock it complements:
//
//	lifecycle-<project>.last    mtime = when the last lifecycle STARTED
//
// The hook only ever READS it (os.Stat). The spawned `ghost lifecycle` WRITES
// it, when the run it actually starts begins. That asymmetry is deliberate: a
// stamp written by the hook would be burned by a spawn that failed to start, and
// the window would advance with no consolidation having happened.

// lifecycleLastStartFile is the stamp file name for one project in the Ghost
// data dir. Keyed on the RESOLVED project id, the same value the pid lock uses,
// so a manual `ghost lifecycle --project <name>` and a hook-spawned run (which
// passes the id) are the same project for both guards.
func lifecycleLastStartFile(projectID string) string {
	return "lifecycle-" + projectID + ".last"
}

// lifecycleCooldownActive reports whether this project's last lifecycle START is
// newer than minInterval, i.e. whether the caller must not spawn again yet. since
// is the age of that start, for the skip's log line; it is 0 when no start could
// be read.
//
// Every unknown is "run", never "skip":
//   - minInterval <= 0 is the documented opt-out, so nothing is consulted at all;
//   - a missing, unreadable or unstamped path means the first run (or one whose
//     write failed) — a cooldown that could silently stop maintenance is the
//     worse failure, so bookkeeping problems fail open;
//   - a stamp dated in the FUTURE (clock change, a file copied between hosts) is
//     not evidence that the window expired, so it reads as inside the window.
//     The alternative — spawning on every turn again — is what this exists to fix.
func lifecycleCooldownActive(dataDir, projectID string, minInterval time.Duration, now time.Time) (skip bool, since time.Duration) {
	if minInterval <= 0 || dataDir == "" || projectID == "" {
		return false, 0
	}
	st, err := os.Stat(filepath.Join(dataDir, lifecycleLastStartFile(projectID)))
	if err != nil {
		return false, 0
	}
	// A directory (or device, or socket) sitting where the stamp belongs is not
	// a record of a run; its mtime is whenever it was created, which would read
	// as "a lifecycle just started" and hold the project off indefinitely.
	if !st.Mode().IsRegular() {
		return false, 0
	}
	since = now.Sub(st.ModTime())
	return since < minInterval, since
}

// TouchLifecycleStart records that a lifecycle run for project has just STARTED.
// Called by the coordinator itself (cmd/ghost's runLifecycle) once it holds the
// per-project lock, so the stamp is written by the process that really runs and
// only for a run that was not turned away by another live run.
//
// project semantics match AcquireLifecycleLock: a name, id or path, resolved to
// the project id. An identifier that resolves to nothing writes nothing — without
// a resolved id there is no per-project file to key, and a stamp named after an
// arbitrary string would collide across projects.
//
// Best-effort by contract, like the failure marker beside it: a missing stamp
// only costs one extra spawn. Callers report the error and continue.
func TouchLifecycleStart(project string) error {
	// DataDirPath, not DataDir: same reason as the marker writers — the store
	// that would resolve the project is what created the directory, so a
	// missing one means there is nothing to stamp.
	dataDir, err := config.DataDirPath()
	if err != nil {
		return fmt.Errorf("locate data dir: %w", err)
	}
	id := resolveMarkerProject(dataDir, project)
	if id == "" {
		return fmt.Errorf("cannot record a lifecycle start for %q: no project resolves to it", project)
	}
	if !safeProjectIDComponent(id) {
		return fmt.Errorf("project id %q is not a safe filename component", id)
	}
	path := filepath.Join(dataDir, lifecycleLastStartFile(id))
	// Write-temp-then-rename, like the failure marker: the only mutation of the
	// stamp path is the rename, so a hook stat'ing it concurrently can never see
	// a half-written file. os.CreateTemp names the temp file uniquely, so two
	// writers cannot collide on it, and creates it 0600.
	tmp, err := os.CreateTemp(dataDir, lifecycleLastStartFile(id)+".tmp*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write([]byte(time.Now().UTC().Format(time.RFC3339Nano) + "\n")); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// logLifecycleCooldownSkip records the one line a skipped spawn leaves behind,
// appended to the existing lifecycle.log. The content is diagnostic only — the
// decision is the mtime — so the line names the project and both durations and
// nothing else has to read it.
//
// It goes to the log rather than the hook's stderr because the hook's stderr is
// the host's: one line per turn, forever, is noise nobody can act on. It is
// best-effort for the same reason — a log that cannot be opened is not a reason
// to change what the hook does.
func logLifecycleCooldownSkip(dataDir, projectID string, since, minInterval time.Duration) {
	f, err := os.OpenFile(filepath.Join(dataDir, "lifecycle.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck
	_, _ = fmt.Fprintf(f, "%s lifecycle: skipping spawn for project %s — last run started %s ago, inside lifecycle.min_interval=%s\n",
		time.Now().UTC().Format(time.RFC3339), projectID, since.Round(time.Second), minInterval)
}
