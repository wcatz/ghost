package mcpinit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// The auto-consolidation chain normally runs as a DETACHED child of the stop
// hook: nobody reads its exit status and its stderr lands only in
// lifecycle.log, so a chain that keeps failing (most often because no LLM CLI
// binary is reachable on the non-login PATH) used to vanish for weeks. The
// marker below is the durable, machine-readable half of the fix: written
// whenever a run fails, read (and printed as one labeled alert) by the next
// session-start.
const (
	// lifecycleMarkerFile is the marker's fixed name inside config.DataDir().
	lifecycleMarkerFile = "lifecycle-last-failure.json"
	// lifecycleMarkerVersion is the marker schema version as written; readers
	// ignore it (unknown fields and versions parse into the same shape).
	lifecycleMarkerVersion = 1
	// lifecycleAlertMaxAge bounds the alert: a failure older than this is no
	// longer news, and re-printing it every session would train the reader to
	// ignore the block. Between lifecycleAlertMaxAge and
	// lifecycleMarkerMaxAge the file is kept but stays silent.
	lifecycleAlertMaxAge = 14 * 24 * time.Hour
	// lifecycleMarkerMaxAge is the self-clean horizon: markers older than
	// this are deleted at the next session-start so a permanently-dead
	// marker (its project deleted, its config changed by hand) can never
	// nag — or linger on disk — forever.
	lifecycleMarkerMaxAge = 30 * 24 * time.Hour
	// lifecycleMarkerErrorMaxBytes caps the recorded error line (first line
	// only) so the alert stays one short block of context.
	lifecycleMarkerErrorMaxBytes = 300
)

// NoLLMBackendError is the marker error text for the specific failure this
// surface exists for: auto-reflect enabled but no claude/opencode/codex/goose
// binary reachable, so reflect was skipped instead of run. Shared by the stop
// hook's spawn guard and the lifecycle coordinator so both record the same
// diagnosable fact.
const NoLLMBackendError = "no CLI LLM backend available; reflect skipped"

// lifecycleFailureMarker is the lifecycle-last-failure.json schema. Every
// field is a fact derivable from the run that wrote it: which project, which
// phases did not succeed, the first line of the first error, when.
type lifecycleFailureMarker struct {
	Project      string   `json:"project"`
	PhasesFailed []string `json:"phases_failed"`
	Error        string   `json:"error"`
	At           string   `json:"at"` // RFC3339, UTC
	Version      int      `json:"version"`
}

// WriteLifecycleFailure records a failed (or never-started) consolidation
// chain as an atomically-replaced marker in config.DataDir(). project may be
// a name, id, or path (Store.ResolveProject semantics); it is resolved to the
// project ID so the next session-start can match it against the session's own
// resolution, falling back to the raw value when the store cannot resolve it.
// Best-effort by contract: callers ignore the error — failing to record a
// failure must never itself fail a run.
func WriteLifecycleFailure(project string, phasesFailed []string, firstErr string) error {
	// DataDirPath, not DataDir: recording a failure is best-effort bookkeeping
	// and must not MkdirAll a ghost/ directory that no store ever created —
	// same reason as recordReflectSkipMarker and ClearLifecycleFailure. With no
	// store the project cannot resolve and this returns an error the callers
	// already ignore.
	dataDir, err := config.DataDirPath()
	if err != nil {
		return fmt.Errorf("locate data dir: %w", err)
	}
	resolved := resolveMarkerProject(dataDir, project)
	if resolved == "" {
		resolved = project
	}
	if resolved == "" {
		return fmt.Errorf("cannot record lifecycle failure without a project")
	}
	text := strings.SplitN(firstErr, "\n", 2)[0]
	if strings.TrimSpace(text) == "" {
		text = "phase failed"
	}
	m := lifecycleFailureMarker{
		Project:      resolved,
		PhasesFailed: append([]string(nil), phasesFailed...),
		Error:        truncateUTF8(text, lifecycleMarkerErrorMaxBytes),
		At:           time.Now().UTC().Format(time.RFC3339),
		Version:      lifecycleMarkerVersion,
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// Write-temp-then-rename, with a UNIQUE temp name (os.CreateTemp), so a
	// reader — or a second writer for another project — can never observe a
	// torn file: the rename is the only mutation of the marker path.
	tmp, err := os.CreateTemp(dataDir, lifecycleMarkerFile+".tmp*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(dataDir, lifecycleMarkerFile)); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// ClearLifecycleFailure removes the marker. A missing marker is success, not
// an error; the data dir is never created just to delete nothing.
func ClearLifecycleFailure() error {
	dataDir, err := config.DataDirPath()
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dataDir, lifecycleMarkerFile)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// FinishLifecycleRun is the lifecycle coordinator's single end-of-run outcome
// record:
//
//   - any failed phase (phasesFailed non-empty) writes/overwrites the marker
//     with those phases — a PARTIAL run (2 of 3 ok) therefore keeps the
//     marker recording just the failed phase(s);
//   - a fully successful run (at least one phase ran, none failed) clears the
//     marker — success heals;
//   - a run in which no phase ran at all (every auto phase disabled) has no
//     opinion and leaves any existing marker untouched, so a no-op run can
//     never erase another run's real failure record.
//
// project semantics match WriteLifecycleFailure. Returns the underlying
// error for the caller to surface on stderr; callers in the run path treat it
// as a warning.
func FinishLifecycleRun(project string, phasesRan int, failedPhases []string, firstErr string) error {
	if len(failedPhases) > 0 {
		if strings.TrimSpace(firstErr) == "" {
			firstErr = "phase failed"
		}
		return WriteLifecycleFailure(project, failedPhases, firstErr)
	}
	if phasesRan == 0 {
		return nil
	}
	return ClearLifecycleFailure()
}

// resolveMarkerProject best-effort resolves project (name/id/path) to the
// store's project ID, returning "" when no store exists or nothing matches.
func resolveMarkerProject(dataDir, project string) string {
	if project == "" {
		return ""
	}
	db, err := sql.Open("sqlite", roDSN(filepath.Join(dataDir, "ghost.db")))
	if err != nil {
		return ""
	}
	defer db.Close() //nolint:errcheck
	id, _, err := memory.NewStore(db, nil).ResolveProject(context.Background(), project)
	if err != nil {
		return ""
	}
	return id
}

// readLifecycleFailure loads the marker, returning nil when there is none,
// when it is unparseable, or when its timestamp cannot be interpreted (a
// marker that cannot be aged safely is never printed and never deleted —
// fail silent, keep the file for manual inspection).
func readLifecycleFailure() *lifecycleFailureMarker {
	dataDir, err := config.DataDirPath()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dataDir, lifecycleMarkerFile))
	if err != nil {
		return nil
	}
	var m lifecycleFailureMarker
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, m.At); err != nil {
		return nil
	}
	return &m
}

// lifecycleFailureAlert renders the one-line session-start alert for the
// current session, or "" when nothing should be printed.
//
// Print rules (each pinned by a test):
//   - no marker, or a marker that cannot be parsed/aged: no output;
//   - marker older than lifecycleAlertMaxAge (14d): no output, file kept;
//   - marker older than lifecycleMarkerMaxAge (30d): no output, file DELETED
//     (self-clean);
//   - otherwise print when the marker's project matches the session's
//     resolved project ID OR its resolved name (the lifecycle command may
//     have been run by either), when the marker has no project (cannot be
//     attributed — show it everywhere), or when the session resolved to NO
//     project (the user still needs to know). Suppressed only when both
//     sides name a project and they differ: another project's failure is
//     not this conversation's business.
//
// The marker's own project is used in the text; an unattributed marker falls
// back to the session's project so the retry command is still copyable. The
// alert never includes paths — only the project identifier and the first
// (≤300-byte) line of the recorded error.
func lifecycleFailureAlert(projectID, projectName string) string {
	m := readLifecycleFailure()
	if m == nil {
		return ""
	}
	at, err := time.Parse(time.RFC3339, m.At)
	if err != nil {
		return ""
	}
	age := time.Since(at)
	if age > lifecycleMarkerMaxAge {
		_ = ClearLifecycleFailure()
		return ""
	}
	if age > lifecycleAlertMaxAge {
		return ""
	}
	if m.Project != "" && projectID != "" && m.Project != projectID && m.Project != projectName {
		return ""
	}
	p := m.Project
	if p == "" {
		p = projectID
	}
	if p == "" {
		// Neither side can name a project: nothing to report, no retry
		// command to give. Degenerate — stay silent.
		return ""
	}
	errLine := strings.SplitN(m.Error, "\n", 2)[0]
	return fmt.Sprintf(
		"**Ghost maintenance alert:** last automatic consolidation failed for project %s at %s (phase(s): %s; %s). Memory consolidation is paused until a run succeeds — run `ghost lifecycle --project %s` to retry, and check config cli.*_binary if no LLM CLI was found.",
		p, at.Format(time.RFC3339), strings.Join(m.PhasesFailed, ", "),
		truncateUTF8(errLine, lifecycleMarkerErrorMaxBytes), p)
}
