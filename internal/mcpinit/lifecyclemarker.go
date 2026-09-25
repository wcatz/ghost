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
	// lifecycleMarkerPrefix is the stem shared by every failure marker file in
	// config.DataDir(). Each project gets its own file named after it (see
	// projectMarkerFile), because one shared file failed two ways at once:
	// any project's successful run deleted it wholesale, erasing another
	// project's still-active failure, and two projects failing at the same
	// moment overwrote each other — so at most one project's problem was ever
	// recorded, frequently not the one being read (issue #540).
	lifecycleMarkerPrefix = "lifecycle-last-failure"
	// legacyMarkerFile is the pre-per-project name. Never written now; still
	// read as a last resort so a marker left by an older build keeps alerting
	// instead of becoming an orphan that only the 30-day self-clean ever
	// notices.
	legacyMarkerFile = lifecycleMarkerPrefix + ".json"
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
	// lifecycleMarkerErrorMaxBytes caps what the ALERT renders, so the block
	// printed at session start stays short enough to read.
	lifecycleMarkerErrorMaxBytes = 300
	// lifecycleMarkerDetailMaxBytes caps what the FILE stores. Larger than
	// the alert cap deliberately: the marker now carries the captured stderr
	// tail, and truncating it at write time to one line would discard exactly
	// the detail that captures exist to preserve. The alert still shows one
	// line; the file keeps enough to diagnose from.
	lifecycleMarkerDetailMaxBytes = 1500
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
	// Keep the whole bounded cause, not just its first line. The first line
	// used to be all that survived, which threw away the captured stderr tail
	// the moment it was recorded. The alert still renders one line
	// (lifecycleFailureAlert splits), so a multi-line marker costs the reader
	// nothing and gives anyone opening the file the actual evidence.
	text := strings.TrimSpace(firstErr)
	if text == "" {
		text = "phase failed"
	}
	m := lifecycleFailureMarker{
		Project:      resolved,
		PhasesFailed: append([]string(nil), phasesFailed...),
		Error:        truncateUTF8(text, lifecycleMarkerDetailMaxBytes),
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
	markerName := projectMarkerFile(resolved)
	tmp, err := os.CreateTemp(dataDir, markerName+".tmp*")
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
	if err := os.Rename(tmpPath, filepath.Join(dataDir, markerName)); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// ClearLifecycleFailure removes THIS project's marker. A missing marker is
// success, not an error; the data dir is never created just to delete nothing.
//
// Scoped deliberately. The old implementation removed the one shared file, so
// project A succeeding after project B failed erased B's failure — the marker
// recorded B's problem, and A's next clean run made it invisible (issue #540).
// Success heals its own project and nothing else.
//
// The legacy single-file marker is also removed when it names this project, so
// an older build's record does not keep alerting after the project recovers;
// one belonging to a different project is left alone.
func ClearLifecycleFailure(project string) error {
	dataDir, err := config.DataDirPath()
	if err != nil {
		return err
	}
	// Resolve exactly as WriteLifecycleFailure does when it keys the file.
	// Clearing by the raw name a caller passed would never find a marker the
	// writer stored under the resolved id — "success heals" would silently
	// heal nothing.
	if project != "" {
		if resolved := resolveMarkerProject(dataDir, project); resolved != "" {
			project = resolved
		}
	}
	if project != "" {
		if err := os.Remove(filepath.Join(dataDir, projectMarkerFile(project))); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	legacy := filepath.Join(dataDir, legacyMarkerFile)
	// Compared with no emptiness guard: an unattributed legacy marker
	// (Project == "") must still self-clean when told to clear "".
	if m := readMarkerAtPath(legacy); m != nil && m.Project == project {
		if err := os.Remove(legacy); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// projectMarkerFile is the marker file name for one project inside
// config.DataDir(). Sanitized because the project may be an unresolved name,
// which the filesystem will not accept as-is; resolved ids are already safe.
func projectMarkerFile(project string) string {
	return lifecycleMarkerPrefix + "-" + sanitizeMarkerProject(project) + ".json"
}

// sanitizeMarkerProject maps a project id or name onto a file-name-safe stem.
// Anything outside the common set becomes "_" rather than being dropped, so
// two names differing only in punctuation cannot collapse onto each other and
// silently share a marker. Capped only against absurd lengths.
func sanitizeMarkerProject(project string) string {
	var b strings.Builder
	for _, r := range project {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		// Not a placeholder word like "unattributed": a real project could be
		// named that, and the two would then share a marker — one project's
		// failure recorded under the other's name, which is the coupling this
		// change exists to remove. The empty project IS the legacy shared
		// file's meaning, so it maps there.
		return strings.TrimSuffix(legacyMarkerFile, ".json")
	}
	if len(out) > 80 {
		return out[:80]
	}
	return out
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
	// Scoped to THIS project. Success used to delete the one shared file, so
	// a clean run in project A erased project B's failure — the marker said B
	// had failed and A's next success made it disappear (issue #540).
	return ClearLifecycleFailure(project)
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
// readMarkerAtPath loads and validates a single marker file. A missing,
// unparseable or undated file reads as no marker rather than an error: this is
// a best-effort alert, and every failure mode of it must degrade to silence.
func readMarkerAtPath(path string) *lifecycleFailureMarker {
	b, err := os.ReadFile(path)
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

// readLifecycleFailure finds the marker relevant to a session: this project's
// own file first, then the name it may have been written under if the id was
// unknown at write time, and finally the legacy shared file so a record made
// by an older build still alerts instead of silently rotting on disk.
func readLifecycleFailure(projectID, projectName string) *lifecycleFailureMarker {
	dataDir, err := config.DataDirPath()
	if err != nil {
		return nil
	}
	if projectID == "" && projectName == "" {
		// A session that resolves to no project cannot say which marker is
		// its own. The single shared file used to be shown in exactly this
		// case, because some news beats none when there is nothing to filter
		// by — so keep it, and surface the most recent failure rather than
		// whatever sorts first.
		return newestMarker(filepath.Join(dataDir, lifecycleMarkerPrefix+"-*.json"),
			filepath.Join(dataDir, legacyMarkerFile))
	}
	for _, name := range markerCandidates(projectID, projectName) {
		if m := readMarkerAtPath(filepath.Join(dataDir, name)); m != nil {
			return m
		}
	}
	return nil
}

// newestMarker reads every path it can and returns the one recorded last.
func newestMarker(patterns ...string) *lifecycleFailureMarker {
	var newest *lifecycleFailureMarker
	newestAt := time.Time{}
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, path := range matches {
			m := readMarkerAtPath(path)
			if m == nil {
				continue
			}
			at, err := time.Parse(time.RFC3339, m.At)
			if err != nil {
				continue
			}
			if newest == nil || at.After(newestAt) {
				newest, newestAt = m, at
			}
		}
	}
	return newest
}

// markerCandidates is the lookup order: the resolved id, the display name (a
// marker written when the id could not be resolved keys on the name), then the
// legacy shared file. Duplicates collapse so a session whose id and name match
// reads the same file twice.
func markerCandidates(ids ...string) []string {
	out := make([]string, 0, len(ids)+1)
	seen := make(map[string]bool, len(ids)+1)
	for _, id := range ids {
		if id == "" {
			continue
		}
		f := projectMarkerFile(id)
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return append(out, legacyMarkerFile)
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
	m := readLifecycleFailure(projectID, projectName)
	if m == nil {
		return ""
	}
	at, err := time.Parse(time.RFC3339, m.At)
	if err != nil {
		return ""
	}
	age := time.Since(at)
	if age > lifecycleMarkerMaxAge {
		// Clear the marker that was actually read, keyed by its own recorded
		// project — never a blanket removal that could touch another's.
		_ = ClearLifecycleFailure(m.Project)
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
