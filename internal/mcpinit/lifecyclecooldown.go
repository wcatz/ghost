package mcpinit

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
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

// UnfinishedLifecycleRunError is the cause recorded for a coordinator that
// claimed the project and wrote its start stamp, then died before it could
// record an outcome: SIGKILL, the OOM killer, a power loss. Nothing inside such
// a process can run, so the evidence is the pair of files it left behind — the
// stamp and the claim — and this is what both readers call that evidence.
const UnfinishedLifecycleRunError = "the previous run did not finish; the coordinator was killed before it could record an outcome"

// unfinishedLifecycleRun reports the start time of a run that claimed projectID
// and never finished. It reads the two files a run leaves at that point: the
// cooldown stamp (written once the run has work to do) and the per-project pid
// claim, which a run releases only on its way out with an outcome recorded.
// Both present and the claim naming no live process is a run that was killed.
//
// The stamp is required, not incidental: a run killed between the claim and the
// stamp burned no cooldown window, so the next stop hook retries at once and
// there is nothing paused to report. A live claim is a run in progress, not a
// failure. Every path is fail-silent: a missing, unreadable or non-regular file
// is not evidence, and this is called from the session-start alert where a
// bookkeeping problem must never become an error.
func unfinishedLifecycleRun(dataDir, projectID string) (time.Time, bool) {
	if dataDir == "" || projectID == "" || !safeProjectIDComponent(projectID) {
		return time.Time{}, false
	}
	startedAt, ok := lifecycleStartStamp(dataDir, projectID)
	if !ok {
		return time.Time{}, false
	}
	// Stat before isAlive: the liveness read opens the file, and a FIFO planted
	// where a claim belongs would block the session start until its writer
	// appeared. Only a regular file can be a claim.
	claimPath := filepath.Join(dataDir, "lifecycle-"+projectID+".pid")
	claim, err := os.Stat(claimPath)
	if err != nil || !claim.Mode().IsRegular() {
		return time.Time{}, false
	}
	if isAlive(claimPath) {
		return time.Time{}, false
	}
	// The stamp must BELONG to this claim. The stamp file is never removed, so
	// it outlives the run that wrote it, and a claim newer than the stamp is a
	// run that died before it stamped — one that burned no cooldown window and
	// has nothing to report. A run that stamped and then died leaves the stamp
	// newer than its own claim, which is the pair this looks for.
	if startedAt.Before(claim.ModTime()) {
		return time.Time{}, false
	}
	return startedAt, true
}

// lifecycleStartStamp returns the mtime of the project's cooldown stamp, and
// whether a regular stamp file is there at all. A directory, a device or a
// socket sitting where the stamp belongs is not a record of a run.
func lifecycleStartStamp(dataDir, projectID string) (time.Time, bool) {
	if dataDir == "" || projectID == "" || !safeProjectIDComponent(projectID) {
		return time.Time{}, false
	}
	stamp, err := os.Stat(filepath.Join(dataDir, lifecycleLastStartFile(projectID)))
	if err != nil || !stamp.Mode().IsRegular() {
		return time.Time{}, false
	}
	return stamp.ModTime(), true
}

// lifecycleStampCoversClaim reports whether the project's cooldown stamp is at
// least as new as a claim taken at claimAt — the same rule
// unfinishedLifecycleRun applies, for the reclaim path, where the claim file
// has already been replaced by this process's own and the dead claim's own
// timestamp is the one that matters.
//
// A claim newer than the stamp is a run that died before it stamped: it burned
// no cooldown window, so there is no paused consolidation to report and no
// marker to write.
func lifecycleStampCoversClaim(dataDir, projectID string, claimAt time.Time) bool {
	if claimAt.IsZero() {
		return false
	}
	startedAt, ok := lifecycleStartStamp(dataDir, projectID)
	if !ok {
		return false
	}
	return !startedAt.Before(claimAt)
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
//   - a stamp dated slightly in the FUTURE (clock skew) is not evidence that the
//     window expired, so it reads as inside the window — spawning on every turn
//     again is what this exists to fix. A stamp more than one window ahead reads
//     as stale, so a clock that was far ahead cannot hold consolidation off until
//     wall time catches up.
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
	return since < minInterval && since > -minInterval, since
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
		// `memory.ProjectArg` for the same reason as the refusal in
		// `acquireLifecycleLock`: this is the caller's `--project` operand, and a
		// session routinely supplies the clone URL it is standing in, credentials
		// included (#839). A value the guard recognises would otherwise be quoted
		// into the operator's stderr by the one package that never asked.
		return fmt.Errorf("cannot record a lifecycle start for %s: no project resolves to it", memory.RefusalArg("project", project))
	}
	if !safeProjectIDComponent(id) {
		return fmt.Errorf("project id %s is not a safe filename component", memory.RefusalArg("project id", id))
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
// appended to lifecycle.log — rotated first if that log has already reached the
// rotation cap, like every other open of it. The content is diagnostic only — the
// decision is the mtime — so the line names the project and both durations and
// nothing else has to read it.
//
// The project is a RESOLVED id from the store, not this process's operand, and a
// stored id is not token-guarded: `memory.CheckImportedProjectID` refuses control
// characters, backticks and delimiters but never asks the guard, and
// `memory.CheckImportedProject` asks it of a name and a path but not of the id.
// So an artifact's project id is stored verbatim, and a session standing in that
// project's recorded directory resolves to it. This file is read by operators and
// diagnostic tooling, so the value goes through `memory.RefusalArg` exactly as the
// neighbouring lifecycle surfaces do (the lock and the start stamp, the marker's
// alert through its own renderer): an ordinary id is named quoted — `project "p1"`,
// the spelling those neighbouring surfaces already use — and only a token-shaped
// one is withheld, behind a placeholder naming the argument and the format found.
//
// It goes to the log rather than the hook's stderr because the hook's stderr is
// the host's: one line per turn, forever, is noise nobody can act on. It is
// best-effort for the same reason — a log that cannot be opened is not a reason
// to change what the hook does.
func logLifecycleCooldownSkip(dataDir, projectID string, since, minInterval time.Duration) {
	f, err := openLogForAppend(filepath.Join(dataDir, "lifecycle.log"))
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck
	_, _ = fmt.Fprintf(f, "%s lifecycle: skipping spawn for project %s — last run started %s ago, inside lifecycle.min_interval=%s\n",
		time.Now().UTC().Format(time.RFC3339), memory.RefusalArg("project", projectID), since.Round(time.Second), minInterval)
}
