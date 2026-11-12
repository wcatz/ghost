package mcpinit

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

func lifecycleLastStartFile(projectID string) string {
	return "lifecycle-" + projectID + ".last"
}

// UnfinishedLifecycleRunError is the cause recorded for a coordinator that
// claimed the project and wrote its start stamp, then died before it could
// record an outcome: SIGKILL, the OOM killer, a power loss. Nothing inside such
// a process can run, so the evidence is the pair of files it left behind — the
// stamp and the claim — and this is what both readers call that evidence.
const UnfinishedLifecycleRunError = "the previous run did not finish; the coordinator was killed before it could record an outcome"

func unfinishedLifecycleRun(dataDir, projectID string) (time.Time, bool) {
	if dataDir == "" || projectID == "" || !safeProjectIDComponent(projectID) {
		return time.Time{}, false
	}
	startedAt, ok := lifecycleStartStamp(dataDir, projectID)
	if !ok {
		return time.Time{}, false
	}

	claimPath := filepath.Join(dataDir, "lifecycle-"+projectID+".pid")
	claimFile, err := os.OpenFile(claimPath, markerOpenFlags, 0)
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = claimFile.Close() }()
	claim, err := claimFile.Stat()
	if err != nil || !claim.Mode().IsRegular() {
		return time.Time{}, false
	}
	if isAlive(claimPath) {
		return time.Time{}, false
	}

	if pidInFile(claimPath) == 0 {
		return time.Time{}, false
	}

	if !stampCoversClaim(startedAt, claim.ModTime()) {
		return time.Time{}, false
	}
	return startedAt, true
}

func stampCoversClaim(stamp, claim time.Time) bool {
	return !stamp.Before(claim)
}

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

func lifecycleStampCoversClaim(dataDir, projectID string, claimAt time.Time) bool {
	if claimAt.IsZero() {
		return false
	}
	startedAt, ok := lifecycleStartStamp(dataDir, projectID)
	if !ok {
		return false
	}
	return stampCoversClaim(startedAt, claimAt)
}

func lifecycleCooldownActive(dataDir, projectID string, minInterval time.Duration, now time.Time) (skip bool, since time.Duration) {
	if minInterval <= 0 || dataDir == "" || projectID == "" {
		return false, 0
	}
	st, err := os.Stat(filepath.Join(dataDir, lifecycleLastStartFile(projectID)))
	if err != nil {
		return false, 0
	}

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
func TouchLifecycleStart(project string) error {

	dataDir, err := config.DataDirPath()
	if err != nil {
		return fmt.Errorf("locate data dir: %w", err)
	}
	id := resolveMarkerProject(dataDir, project)
	if id == "" {

		return fmt.Errorf("cannot record a lifecycle start for %s: no project resolves to it", memory.RefusalArg("project", project))
	}
	if !safeProjectIDComponent(id) {
		return fmt.Errorf("project id %s is not a safe filename component", memory.RefusalArg("project id", id))
	}
	path := filepath.Join(dataDir, lifecycleLastStartFile(id))

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

func logLifecycleCooldownSkip(dataDir, projectID string, since, minInterval time.Duration) {
	f, err := openLogForAppend(filepath.Join(dataDir, "lifecycle.log"))
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck
	_, _ = fmt.Fprintf(f, "%s lifecycle: skipping spawn for project %s — last run started %s ago, inside lifecycle.min_interval=%s\n",
		time.Now().UTC().Format(time.RFC3339), memory.RefusalArg("project", projectID), since.Round(time.Second), minInterval)
}
