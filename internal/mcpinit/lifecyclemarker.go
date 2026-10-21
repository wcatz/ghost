package mcpinit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
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
	// lifecycleMarkerMaxAge is the self-clean horizon on the AGE side: a
	// marker older than this is deleted at the next session-start so a
	// permanently-dead marker (its project deleted, its config changed by
	// hand) can never nag — or linger on disk — forever. The forward side has
	// its own, much tighter bound below: a marker dated ahead has a NEGATIVE
	// age, so no "older than" horizon ever reaches it.
	lifecycleMarkerMaxAge = 30 * 24 * time.Hour
	// lifecycleMarkerClockSkew is the tolerated clock skew: how far ahead of
	// this machine's clock a marker may be dated and still count as a real,
	// recent failure. A marker up to this far ahead still alerts — ordinary
	// skew between the writer's clock and this session's is not evidence of
	// anything. Further ahead than this is a bad stamp, not skew: a clock set
	// forward when the marker was written, a VM resumed with a skewed clock,
	// or a marker copied in from another machine. Its negative age never
	// reaches an "older than" horizon, so without this bound it would alert
	// at every session start, unchanged, until wall time caught up — so the
	// self-clean expires it on this bound, on the same path as an old one,
	// and newestMarker ranks a stamp dated ahead of the read's now BELOW every
	// stamp dated at or before it, so its future date cannot outrank a real
	// failure. This is a stamp-plausibility bound and deliberately NOT the
	// 30-day age horizon and not the cooldown's one-window rule; each bounds a
	// different thing.
	lifecycleMarkerClockSkew = 24 * time.Hour
	// lifecycleSelfCleanMaxPasses bounds how many expired markers one
	// session-start removes before giving up on finding a current one: more
	// than a project's candidate files (markerCandidates), so it never cuts a
	// real read short. It counts removals only — a kept-but-silent marker is
	// passed over inside the read (markerSilent) and spends no pass, and in a
	// session that resolves to no project the candidate set is one file per
	// project, so charging a skip here could exhaust the bound on files that
	// are never removed and hide the current failure behind them. The no-project
	// read's sweep of the expired markers counts against the same bound, so a
	// session start never spends an unbounded number of removals.
	lifecycleSelfCleanMaxPasses = 8
	// lifecycleMarkerErrorMaxBytes caps what the ALERT renders, so the block
	// printed at session start stays short enough to read.
	lifecycleMarkerErrorMaxBytes = 300
	// lifecycleMarkerProjectMaxBytes caps the project the ALERT renders, beside
	// the error cap above because it is caller text on the same line and the
	// same surface: the value is printed TWICE on the first line of every
	// session for the marker's lifetime, and nothing upstream bounds it —
	// parseLifecycleArgs accepts any size and WriteLifecycleFailure stores it
	// raw. It is applied AFTER the renderer, so a credential's placeholder is
	// never cut mid-word.
	lifecycleMarkerProjectMaxBytes = 300
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

// InterruptedBySignal opens the sentence the lifecycle coordinator records when
// a signal stops its chain ("interrupted by SIGTERM while running resolve",
// "... before supersede"). Shared with that writer so the alert can find the
// sentence wherever it lands in the stored error: on the first line when
// nothing failed before it, or at the end of an earlier phase's tail, joined
// with "; ", when something did.
const InterruptedBySignal = "interrupted by SIGTERM"

// phaseErrorPrefix opens the line a phase command prints just before it exits
// non-zero on an error it handled: reflect, resolve and supersede report those
// as "error: ..." on stderr. It is the line that says why the phase failed,
// where the phase's first line is a banner or a log record. A phase that ends
// without printing one — a Go panic, or a group SIGKILL after the grace period
// — leaves no such line, and the alert falls back to the first line.
const phaseErrorPrefix = "error: "

// interruptSentence is the whole of the sentence the coordinator writes when a
// signal stops its chain, and nothing else: InterruptedBySignal, then
// " while running " or " before ", then one phase name, to the end of the line.
// Phase output that only quotes the words does not match it.
var interruptSentence = regexp.MustCompile(
	"^" + regexp.QuoteMeta(InterruptedBySignal) + " (?:while running|before) [A-Za-z0-9_-]+$")

// lifecycleFailureMarker is the lifecycle-last-failure.json schema. Every
// field is a fact derivable from the run that wrote it: which project, which
// phases did not succeed, the first line of the first error, when.
type lifecycleFailureMarker struct {
	Project      string   `json:"project"`
	PhasesFailed []string `json:"phases_failed"`
	Error        string   `json:"error"`
	At           string   `json:"at"` // RFC3339, UTC
	Version      int      `json:"version"`

	// path is the file this marker was read from (readMarkerAtPath), never
	// stored. The self-clean removes it directly because a withheld project
	// (markerProjectField) names no file.
	path string
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
	recorded := resolved
	if resolved == "" {
		resolved = project
		recorded = markerProjectField(project)
	}
	if resolved == "" {
		return fmt.Errorf("cannot record lifecycle failure without a project")
	}
	// Keep the whole bounded cause, not just its first line. The first line
	// used to be all that survived, which threw away the captured stderr tail
	// the moment it was recorded. The alert still renders one line
	// (renderMarkerError picks it), so a multi-line marker costs the reader
	// nothing and gives anyone opening the file the actual evidence.
	//
	// The cause is the failed phase's raw output tail — whatever the harness or
	// the model printed last — so it is asked of the token guard before it is
	// stored, and judged whole, before the storage cap below, so a value that
	// cap would split is still recognised (markerErrorField). The caller's own
	// head cut is the caller's to handle: phaseTail drops the partial line it
	// leaves, so no prefix-less token fragment arrives here.
	text := strings.TrimSpace(firstErr)
	if text == "" {
		text = "phase failed"
	}
	m := lifecycleFailureMarker{
		Project:      recorded,
		PhasesFailed: append([]string(nil), phasesFailed...),
		Error:        truncateUTF8(markerErrorField(text), lifecycleMarkerDetailMaxBytes),
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

// markerProjectField is what the marker's project field records for an operand
// the store could not resolve: the operand itself, unless it is credential
// shaped, in which case the token guard's placeholder naming the format. The
// file stays keyed on the raw operand's digest (projectMarkerFile), so two
// token operands keep two markers and the same operand's clean run still
// clears its own; only the field, which is read back and printed, loses the
// value. Not empty: an empty project means an unattributed marker, which the
// alert would show in every session under that session's own project.
func markerProjectField(operand string) string {
	refusal := memory.RejectSecret("project", operand)
	if refusal == nil {
		return operand
	}
	var detected *memory.SecretContentError
	if errors.As(refusal, &detected) {
		return "<project withheld: it holds a " + detected.Format + ">"
	}
	return "<project withheld: it holds a credential>"
}

// markerErrorField is the marker's error text as stored and as rendered: the
// text itself, unless the token guard finds a credential in it, in which case
// the guard's placeholder naming the format. The whole text is withheld rather
// than the matching span, because the guard reports a format and not a
// position; the full output is still in lifecycle.log for whoever has to
// diagnose the run. Asked at write time, so the file never holds the value, and
// again at render time, because a marker an older build wrote does.
func markerErrorField(text string) string {
	refusal := memory.RejectSecret("error", text)
	if refusal == nil {
		return text
	}
	var detected *memory.SecretContentError
	if errors.As(refusal, &detected) {
		return "<error withheld: it holds a " + detected.Format + ">"
	}
	return "<error withheld: it holds a credential>"
}

// ClearLifecycleFailure removes every marker recording THIS project. A missing
// marker is success, not an error; the data dir is never created just to delete
// nothing.
//
// Scoped deliberately. The old implementation removed the one shared file, so
// project A succeeding after project B failed erased B's failure — the marker
// recorded B's problem, and A's next clean run made it invisible (issue #540).
// Success heals its own project and nothing else.
//
// "Its own" is more than the resolved id's file. A failure recorded while the
// operand resolved to nothing is keyed on the operand as given, and a project
// that later exists under that name with a different id (an imported one) never
// resolves back to that key: a clean run by name re-resolves to the id, a clean
// run by id never sees the name, and the alert kept saying consolidation was
// paused after the project had recovered. So the files keyed on the operand and
// on the project's name are cleared too, each only when it records this project
// AND its key is not another project's id — the recorded project alone cannot
// tell an unresolved "shared" from the project whose id IS "shared", since both
// record the bare string (see clearCheckedMarker).
//
// A reference that does not resolve CLEANLY — one two projects share, which is
// memory.ErrAmbiguousProject — clears nothing at all (#1268): the name is
// exactly what an unresolved write keyed its file on, so it may record either
// project, and an owner that cannot be determined is not read as "not mine".
// It returns before the legacy single-file check, so such an operand cannot
// clear the shared file either.
//
// The legacy single-file marker is also removed when it names this project by
// its resolved id, or by its stored name or the operand as given when that
// spelling resolves back to this project — an older build's record carried the
// unresolved operand — so it does not keep alerting after the project recovers;
// one naming another project is left alone.
func ClearLifecycleFailure(project string) error {
	dataDir, err := config.DataDirPath()
	if err != nil {
		return err
	}
	// Resolve exactly as WriteLifecycleFailure does when it keys the file.
	// Clearing by the raw name a caller passed would never find a marker the
	// writer stored under the resolved id — "success heals" would silently
	// heal nothing.
	operand := project
	var name string
	if project != "" {
		resolved, resolvedName, resolveErr := resolveMarkerIdentity(dataDir, project)
		if errors.Is(resolveErr, memory.ErrAmbiguousProject) {
			// The reference matched more than one project, so it names no
			// single project and every marker keyed on it — the digest file for
			// the operand itself included — may record another project's
			// failure. An owner that cannot be determined is not read as "not
			// mine" (#1268): success clears nothing rather than guessing. Any
			// OTHER error is the store-less situation the writer's own fallback
			// handles, so the operand stays the key and its marker is cleared.
			return nil
		}
		if resolved != "" {
			project, name = resolved, resolvedName
		}
	}
	if project != "" {
		if err := os.Remove(filepath.Join(dataDir, projectMarkerFile(project))); err != nil && !os.IsNotExist(err) {
			return err
		}
		// The name an older build gave this project. Removed only when the file
		// records THIS project: the old stem folded punctuation, so the same
		// file may hold another project's failure ("my project" and
		// "my_project" both keyed lifecycle-last-failure-my_project.json), and
		// deleting it would be the very coupling the digest stem removes.
		old := filepath.Join(dataDir, oldProjectMarkerFile(project))
		if m := readMarkerAtPath(old); m != nil && m.Project == project {
			if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		// The keys a failure recorded before the operand resolved may sit
		// under: the operand as given and the project's stored name.
		for _, key := range []string{operand, name} {
			if key == "" || key == project {
				continue
			}
			if err := clearCheckedMarker(dataDir, project, key); err != nil {
				return err
			}
		}
	}
	legacy := filepath.Join(dataDir, legacyMarkerFile)
	// The pre-per-project writer recorded the unresolved operand — often the
	// project's name or a path — in the one shared file, so a marker recording
	// this project under any spelling is this run's to clear. A spelling that
	// is not the resolved id must resolve back to this project first, because a
	// name two projects share (or a name that is another project's id) records
	// the same bare string (#1268).
	if m := readMarkerAtPath(legacy); m != nil && markerRecordsRun(dataDir, m.Project, project, name, operand) {
		if err := os.Remove(legacy); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// markerRecordsRun reports whether a marker's recorded project is the project a
// clean run is clearing. A recorded value equal to the resolved id is this
// project, full stop. A value equal to the project's stored name or to the
// operand as given is only a spelling — the legacy shared file predates the
// resolved id and carries whichever the caller passed, and a name two projects
// share (or a name that is another project's id) records the same string — so
// it counts only when that value RESOLVES back to this project. An unattributed
// marker (recorded "") names no project: only the empty run clears it, and
// letting a real project clear it would be one project's success erasing a
// record it cannot claim.
func markerRecordsRun(dataDir, recorded, id, name, operand string) bool {
	if recorded == id {
		return true
	}
	if recorded == "" || (recorded != name && recorded != operand) {
		return false
	}
	resolved, _, err := resolveMarkerIdentity(dataDir, recorded)
	return err == nil && resolved == id
}

// clearCheckedMarker removes the markers keyed on key — a name or operand that
// is not projectID itself — when they record the project being cleared. key is
// first asked of the store: when it is ANOTHER project's id, the file keyed on
// it is that project's own marker, recording the same bare string an unresolved
// write would, so it is left alone. A key the store cannot resolve CLEANLY — a
// name two projects share — is left for the same reason: the recorded project
// alone cannot say which of them it is (#1268). The digest file is compared
// against what the writer records for an unresolved operand (markerProjectField,
// so a withheld operand still matches its own file); the old-stem file against
// the raw key, because older builds never wrote the placeholder.
func clearCheckedMarker(dataDir, projectID, key string) error {
	owner, _, err := resolveMarkerIdentity(dataDir, key)
	if errors.Is(err, memory.ErrAmbiguousProject) {
		// The key matched more than one project, and a name two projects share
		// is exactly the key an unresolved write lands on. Ownership cannot be
		// determined, so it is not read as "not mine": the file is left for
		// whoever it records (#1268). Other errors — a store-less or unreadable
		// data dir — leave owner "" and fall through, because no other project
		// claims the key either.
		return nil
	}
	if owner != "" && owner != projectID {
		return nil
	}
	digest := filepath.Join(dataDir, projectMarkerFile(key))
	if m := readMarkerAtPath(digest); m != nil && m.Project == markerProjectField(key) {
		if err := os.Remove(digest); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	old := filepath.Join(dataDir, oldProjectMarkerFile(key))
	if m := readMarkerAtPath(old); m != nil && m.Project == key {
		if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// projectMarkerFile is the marker file name for one project inside
// config.DataDir(): the prefix and a digest of the project, never the project
// itself. The project may be an unresolved --project operand holding anything
// at all, and the stem has to be injective over it — the sanitized stem it
// replaces folded every byte outside [A-Za-z0-9._-] onto "_", so "my project"
// and "my_project" shared one file and one project's clean run erased the
// other's still-active failure. A digest has no folding to collide on, is
// lowercase hex (so a case-insensitive filesystem cannot merge "A" and "a"
// either), is a fixed length, and keeps the operand out of the directory
// listing entirely: whatever the operand is, the file name does not spell it.
//
// Markers written under the old stem are still read (oldProjectMarkerFile,
// markerCandidates) and cleared, so an upgrade neither loses nor strands a
// failure recorded before it.
func projectMarkerFile(project string) string {
	return lifecycleMarkerPrefix + "-" + markerStem(project) + ".json"
}

// LifecycleFailureMarkerFile is projectMarkerFile for callers outside the
// package — the lifecycle command's tests, which must name the file the
// writer really produces rather than spell a stem of their own (a stale
// spelling would make an "is absent" assertion pass vacuously).
func LifecycleFailureMarkerFile(project string) string {
	return projectMarkerFile(project)
}

// markerStem is the first lifecycleMarkerStemBytes of the project's SHA-256,
// hex-encoded. 128 bits makes an accidental collision between two projects'
// markers a non-event, and the prefix-plus-"-" shape keeps the file inside
// the lifecycle-last-failure-*.json glob the no-project read uses.
func markerStem(project string) string {
	sum := sha256.Sum256([]byte(project))
	return hex.EncodeToString(sum[:lifecycleMarkerStemBytes])
}

// lifecycleMarkerStemBytes is how much of the digest names the file.
const lifecycleMarkerStemBytes = 16

// oldProjectMarkerFile is the name an older build gave a project's marker:
// the sanitized stem. Never written now. It is NOT injective — a marker found
// under it may belong to another project whose name folded onto the same
// stem — so every reader and clearer of it checks the recorded project.
func oldProjectMarkerFile(project string) string {
	return lifecycleMarkerPrefix + "-" + sanitizeMarkerProject(project) + ".json"
}

// sanitizeMarkerProject maps an id onto a file-name-safe stem. Anything
// outside the common set becomes "_" rather than being dropped, and the
// result is capped against absurd lengths. It is NOT injective: "my project"
// and "my_project" both map to "my_project", which is why the lifecycle
// failure marker no longer keys on it (see projectMarkerFile) and only reads
// its old names through oldProjectMarkerFile.
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
// store's project ID, returning "" when no store exists, nothing matches, or
// the reference is ambiguous.
func resolveMarkerProject(dataDir, project string) string {
	id, _, _ := resolveMarkerIdentity(dataDir, project)
	return id
}

// resolveMarkerIdentity is resolveMarkerProject that also returns the project's
// stored name, which a clean run needs to find a marker keyed on the name
// before the project existed, and the resolution's error. The error is the
// difference between "no project answers to this" and "more than one does"
// (memory.ErrAmbiguousProject), and a caller deciding ownership must not read
// the second as the first: a name two projects share is what an unresolved
// write keyed its file on (#1268).
func resolveMarkerIdentity(dataDir, project string) (id, name string, err error) {
	if project == "" {
		return "", "", nil
	}
	// There is deliberately no data-dir check here. Every marker path resolves
	// its directory through config.DataDirPath, which is where the
	// GHOST_DEV_FORBID_DATA_DIR refusal happens (#721), so a forbidden directory
	// never reaches this function at all — and a "" from a caller that resolved
	// one it may not read is answered by that caller's own early return, never by
	// a fallback that writes the raw name (see WriteLifecycleFailure).
	db, err := sql.Open("sqlite", roDSN(filepath.Join(dataDir, "ghost.db")))
	if err != nil {
		return "", "", err
	}
	defer db.Close() //nolint:errcheck
	id, name, err = memory.NewStore(db, nil).ResolveProject(context.Background(), project)
	if err != nil {
		return "", "", err
	}
	return id, name, nil
}

// errNotRegularMarker is openMarker's refusal of an open descriptor that is
// not a regular file.
var errNotRegularMarker = errors.New("marker is not a regular file")

// openMarker opens a failure marker for reading and returns it only when the
// open descriptor is a regular file. The data dir is writable by every local
// process, so a FIFO planted at a marker path used to block os.ReadFile's open
// until a writer appeared — a hang in the session-start alert, in `ghost
// context` and in the clear path, where the host either waits or kills the
// hook at its own timeout — and a symlink was followed to whatever it named.
// The marker is always a regular file this package wrote, so anything else is
// no marker. The caller closes the file.
func openMarker(path string) (*os.File, error) {
	f, err := os.OpenFile(path, markerOpenFlags, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w (%s)", errNotRegularMarker, info.Mode().Type())
	}
	return f, nil
}

// readLifecycleFailure loads the marker, returning nil when there is none,
// when it is unparseable, or when its timestamp cannot be interpreted (a
// marker that cannot be aged safely is never printed and never deleted —
// fail silent, keep the file for manual inspection).
// readMarkerAtPath loads and validates a single marker file. A missing,
// unparseable or undated file reads as no marker rather than an error: this is
// a best-effort alert, and every failure mode of it must degrade to silence —
// including a path that is not a regular file at all (openMarker), which is
// no marker either.
func readMarkerAtPath(path string) *lifecycleFailureMarker {
	f, err := openMarker(path)
	if err != nil {
		return nil
	}
	defer f.Close() //nolint:errcheck
	b, err := io.ReadAll(f)
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
	m.path = path
	return &m
}

// readLifecycleFailure finds the marker relevant to a session: this project's
// own file first, then the name it may have been written under if the id was
// unknown at write time, and finally the legacy shared file so a record made
// by an older build still alerts instead of silently rotting on disk.
//
// A marker in the kept-but-silent band (markerSilent) is passed over here and
// not returned: it is kept on disk by design, so returning it would let it end
// the caller's read and hide a marker behind it. Passing it over inside the
// read costs the caller no self-clean pass.
//
// BOTH branches SWEEP their expired markers first, because both return ONE
// marker and the caller removes the file it read: an expired marker the read
// never reached was never returned and never removed, so the dead file stayed
// on disk for as long as the marker in front of it did. The no-project branch
// ranks every marker in the data directory and returns the one that ranks
// first, so the markers it ranked below were never reached; the project
// branch returns the FIRST candidate that is neither silent nor another
// project's, so the candidates behind it were never reached either.
func readLifecycleFailure(projectID, projectName string) *lifecycleFailureMarker {
	dataDir, err := config.DataDirPath()
	if err != nil {
		return nil
	}
	now := time.Now()
	if projectID == "" && projectName == "" {
		// A session that resolves to no project cannot say which marker is
		// its own. The single shared file used to be shown in exactly this
		// case, because some news beats none when there is nothing to filter
		// by — so keep it, and surface the most recent failure rather than
		// whatever sorts first.
		patterns := []string{
			filepath.Join(dataDir, lifecycleMarkerPrefix+"-*.json"),
			filepath.Join(dataDir, legacyMarkerFile),
		}
		// The expired markers go first: this read ranks what is left and
		// returns one marker, so the caller never reaches the expired ones
		// the ranking buried.
		blocked := sweepExpiredMarkers(now, patterns...)
		// The ranking decides what is shown: a current failure is never
		// hidden by a marker the sweep could not remove. The blocked marker
		// is the fallback, for the case where nothing current is left — it
		// can be neither cleared nor called stale, so the caller names the
		// directory as the blocker.
		if m := newestMarker(now, patterns...); m != nil {
			return m
		}
		if len(blocked) > 0 {
			return blocked[0]
		}
		return nil
	}
	candidates := markerCandidates(projectID, projectName)
	// The same sweep, over this project's own candidates: the lookup below
	// returns the FIRST candidate it can use and never reads the ones behind
	// it, so an expired marker behind the current one — a name-keyed digest,
	// an old-stem file, the shared legacy file — was never reached and never
	// removed, and stayed on disk until the marker in front of it aged out.
	blocked := sweepExpiredMarkers(now, markerCandidatePaths(dataDir, candidates)...)
	for _, c := range candidates {
		// A candidate the sweep could not remove is expired and stays on
		// disk. It must not hide a current failure behind it, so the lookup
		// carries on past it — past every one of them, not only the first
		// the sweep met; it is reported at the end instead, when there is
		// nothing current to show.
		if blockedCandidate(blocked, c.name) {
			continue
		}
		m := readMarkerAtPath(filepath.Join(dataDir, c.name))
		if m == nil {
			continue
		}
		// An old-stem file may hold another project's failure (the old stem
		// folded punctuation). Passing over it, rather than returning it for
		// the alert to suppress, lets this project's own legacy marker after
		// it still be found.
		if c.checkProject && m.Project != "" && m.Project != projectID && m.Project != projectName {
			continue
		}
		if at, err := time.Parse(time.RFC3339, m.At); err == nil && markerSilent(at, now) {
			continue
		}
		return m
	}
	// No marker, but a claimed run that wrote its start stamp and never came
	// back is a failure with no writer left to record it. The stamp is the time
	// the run started and the claim's dead pid is the evidence it never
	// finished; synthesised here so the session-start alert speaks for a
	// coordinator a kill signal reached before any of its own record-keeping.
	// A marker in the kept-but-silent band is aged the same way as a stored one:
	// old news is not news, and the stale claim stays for the next run to
	// reclaim.
	if projectID != "" {
		if at, ok := unfinishedLifecycleRun(dataDir, projectID); ok && !markerSilent(at, now) {
			return &lifecycleFailureMarker{
				Project: projectID,
				Error:   UnfinishedLifecycleRunError,
				At:      at.UTC().Format(time.RFC3339),
			}
		}
	}
	// Nothing current to show: the markers the sweep could not remove are
	// then the only thing left, and the first of them that is this session's
	// to report — another project's expired marker must not speak for this
	// session.
	if b := firstBlockedForSession(blocked, candidates, projectID, projectName); b != nil {
		return b
	}
	return nil
}

// ranksAbove reports whether the stamp at candidateAt wins newestMarker's read
// over the stamp at incumbentAt, for a read taken at now.
//
// A stamp dated AFTER now ranks below every stamp dated at or before it: a
// future date is not evidence of a later failure, so it must not outrank a real
// one, whatever the gap. Among stamps at or before now the newest wins. Among
// future stamps the earliest — the least skewed — wins, so being further ahead
// buys no rank. A future stamp still wins when it is the only candidate.
func ranksAbove(candidateAt, incumbentAt, now time.Time) bool {
	candidateFuture := candidateAt.After(now)
	incumbentFuture := incumbentAt.After(now)
	switch {
	case candidateFuture != incumbentFuture:
		return !candidateFuture
	case candidateFuture:
		return candidateAt.Before(incumbentAt)
	default:
		return candidateAt.After(incumbentAt)
	}
}

// markerSilent reports whether a stamp's age puts it in the kept-but-silent
// band: older than lifecycleAlertMaxAge, so it is no longer news, but not yet
// past lifecycleMarkerMaxAge, so it is kept on disk rather than swept. A read
// passes such a marker over (it neither alerts nor ends the read), and because
// it is kept it would otherwise be returned again on the next pass.
func markerSilent(at, now time.Time) bool {
	age := now.Sub(at)
	return age > lifecycleAlertMaxAge && age <= lifecycleMarkerMaxAge
}

// markerExpired reports whether a stamp is past an expiry bound, so the file
// holding it must be swept rather than kept: older than
// lifecycleMarkerMaxAge on the age side, or dated more than
// lifecycleMarkerClockSkew ahead on the forward side.
//
// It is ONE predicate for both readers of the rule — the alert's own age test
// and the no-project read's sweep — so a marker the sweep removes is one the
// alert would have removed, and a marker it keeps is one the alert would have
// kept. The kept-but-silent band (markerSilent) is inside these bounds, so a
// marker in it is not expired and stays on disk by design.
func markerExpired(at, now time.Time) bool {
	age := now.Sub(at)
	return age > lifecycleMarkerMaxAge || age < -lifecycleMarkerClockSkew
}

// removeMarkerFile removes the file holding a marker. It is a var so a test
// can put the file away between the read and the removal — the one race a
// caller can lose — without racing for it.
var removeMarkerFile = os.Remove

// markerFileRemoved removes the file holding a marker and reports whether the
// file is GONE.
//
// A file that is ALREADY GONE counts as gone: two session starts can walk the
// same expired marker, and the one that loses the race finds the file already
// removed rather than a directory that refuses. That is the sweep done, not a
// refusal — so the caller carries on to the next candidate instead of naming
// the directory as the blocker over a marker that is already gone, which
// would hide the current failure behind it.
func markerFileRemoved(path string) bool {
	err := removeMarkerFile(path)
	return err == nil || errors.Is(err, fs.ErrNotExist)
}

// sweepExpiredMarkers removes the expired markers the given paths name — a
// literal path or a pattern — and returns the markers the data directory
// refused to remove, in the order they were met.
//
// Every expired marker is tried, not only the first one that could not be
// removed: a refusal says nothing about the next file, and stopping at the
// first would leave the markers behind it unswept and unread — the lookup
// would meet one of them, fail to remove it again, and take the session's
// current failure with it.
//
// The expired ones are removed HERE, where the read reaches them, rather than
// left to the caller's loop: every read returns ONE marker, so a caller that
// removes the file it read never reached the expired markers behind it. The
// no-project read ranks every marker in the data directory and returns the one
// that ranks first; the project read returns the first candidate it can use.
// Neither reads the ones behind, so both sweep them here — over the data
// directory's markers and over this project's own candidates respectively.
//
// A marker the directory will not remove is returned rather than swallowed: it
// can be neither cleared nor called stale, so the caller reports the directory
// as the blocker instead of presenting a failure that may already be fixed as
// the current cause.
//
// The removals are bounded by lifecycleSelfCleanMaxPasses, the same bound the
// caller's loop works under, so one session start never spends an unbounded
// number of them.
func sweepExpiredMarkers(now time.Time, paths ...string) []*lifecycleFailureMarker {
	var blocked []*lifecycleFailureMarker
	removed := 0
	for _, path := range paths {
		matches, err := filepath.Glob(path)
		if err != nil {
			continue
		}
		for _, match := range matches {
			m := readMarkerAtPath(match)
			if m == nil {
				continue
			}
			at, err := time.Parse(time.RFC3339, m.At)
			if err != nil {
				continue
			}
			if !markerExpired(at, now) {
				continue
			}
			if !markerFileRemoved(m.path) {
				blocked = append(blocked, m)
				continue
			}
			removed++
			if removed >= lifecycleSelfCleanMaxPasses {
				return blocked
			}
		}
	}
	return blocked
}

// newestMarker reads every path it can and returns the marker that ranks first:
// a stamp dated after this read's now ranks BELOW every stamp dated at or
// before it, so a future-dated marker never outranks a real failure recorded at
// or before the read; among stamps at or before now the newest wins; among
// future stamps the earliest — the least skewed — wins, so being further ahead
// buys no rank. A future stamp is still returned when it is the only candidate.
//
// A marker in the kept-but-silent band (markerSilent) is dropped before the
// ranking, so it cannot end the read and hide a marker behind it; the caller
// needs no separate skip list for it.
func newestMarker(now time.Time, patterns ...string) *lifecycleFailureMarker {
	var newest *lifecycleFailureMarker
	var newestAt time.Time
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
			if markerSilent(at, now) {
				continue
			}
			if newest == nil || ranksAbove(at, newestAt, now) {
				newest, newestAt = m, at
			}
		}
	}
	return newest
}

// markerCandidate is one file readLifecycleFailure tries. checkProject marks a
// name that is not injective over projects, whose marker counts only when it
// records the session's own project.
type markerCandidate struct {
	name         string
	checkProject bool
}

// markerCandidates is the lookup order: the resolved id, the display name (a
// marker written when the id could not be resolved keys on the name), then
// the same two under the old sanitized stem an older build wrote, then the
// legacy shared file. Duplicates collapse so a session whose id and name
// match does not read the same file twice.
func markerCandidates(ids ...string) []markerCandidate {
	out := make([]markerCandidate, 0, 2*len(ids)+1)
	seen := make(map[string]bool, 2*len(ids)+1)
	add := func(name string, check bool) {
		if !seen[name] {
			seen[name] = true
			out = append(out, markerCandidate{name: name, checkProject: check})
		}
	}
	for _, id := range ids {
		if id != "" {
			add(projectMarkerFile(id), false)
		}
	}
	for _, id := range ids {
		if id != "" {
			add(oldProjectMarkerFile(id), true)
		}
	}
	add(legacyMarkerFile, false)
	return out
}

// markerCandidatePaths is where the candidates live, in the lookup order, for
// a caller that sweeps them instead of reading them. The names carry no glob
// metacharacter — the digest is hex and the old stem folds everything else to
// "_" — so each path is a literal the sweep can name.
func markerCandidatePaths(dataDir string, candidates []markerCandidate) []string {
	paths := make([]string, 0, len(candidates))
	for _, c := range candidates {
		paths = append(paths, filepath.Join(dataDir, c.name))
	}
	return paths
}

// blockedCandidate reports whether the candidate named name is one of the
// markers the sweep could not remove — every one of them, because a refusal
// says nothing about the files behind it.
func blockedCandidate(blocked []*lifecycleFailureMarker, name string) bool {
	for _, m := range blocked {
		if filepath.Base(m.path) == name {
			return true
		}
	}
	return false
}

// firstBlockedForSession returns the first of the markers the sweep could not
// remove that is this session's to report, or nil when none of them is.
func firstBlockedForSession(blocked []*lifecycleFailureMarker, candidates []markerCandidate, projectID, projectName string) *lifecycleFailureMarker {
	for _, m := range blocked {
		if blockedIsThisSessions(m, candidates, projectID, projectName) {
			return m
		}
	}
	return nil
}

// blockedIsThisSessions reports whether the marker the sweep could not remove
// is this session's to report, judged by the rules the lookup applies to the
// candidate it came from: a digest marker is this project's own file, so it
// counts unless it records another project; an old-stem file may hold another
// project's failure — the old stem folded punctuation — so it counts only
// when it records this one. Another project's expired marker must not speak
// for this session: it would name a blocker where the session has a failure
// of its own to show.
func blockedIsThisSessions(blocked *lifecycleFailureMarker, candidates []markerCandidate, projectID, projectName string) bool {
	name := filepath.Base(blocked.path)
	for _, c := range candidates {
		if c.name != name {
			continue
		}
		if !c.checkProject {
			_, ok := markerAlertProject(blocked, projectID, projectName)
			return ok
		}
		return blocked.Project == "" || blocked.Project == projectID || blocked.Project == projectName
	}
	return false
}

// lifecycleFailureAlert renders the one-line session-start alert for the
// current session, or "" when nothing should be printed.
//
// Print rules (each pinned by a test):
//   - no marker, or a marker that cannot be parsed/aged: no output;
//   - marker older than lifecycleAlertMaxAge (14d): no output from that marker
//     and the file is kept, but the read passes OVER it (never ends there), so
//     a marker behind it — a newer failure, or an in-tolerance future one the
//     ranking places below every stamp at or before now — still alerts;
//   - marker older than lifecycleMarkerMaxAge (30d), or dated more than
//     lifecycleMarkerClockSkew (24h) AHEAD: that file DELETED (self-clean) —
//     and EVERY expired file the read reaches is deleted, not only the one it
//     returns, because every read returns one marker and the expired ones
//     behind it were otherwise never reached: the no-project read over the
//     data directory's markers, the project read over its own candidates. The
//     next candidate is then read and judged by these same rules, so a
//     current marker behind it still alerts. When the removal fails because
//     the data directory refuses new entries, the marker cannot be cleared
//     either, so the alert names the directory as the blocker on the same line
//     a current marker gets (see lifecycleUnclearableAlert) instead of
//     returning silence;
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
// project is a raw `--project` operand the store could not resolve, so it is
// RENDERED rather than interpolated — see renderMarkerProject. The alert never
// includes paths — only the project identifier and one (≤300-byte) line of
// the recorded error, the one that explains it (markerExplainingLine),
// withheld by the token guard or framed as data — see renderMarkerError.
func lifecycleFailureAlert(projectID, projectName string) string {
	var (
		m  *lifecycleFailureMarker
		at time.Time
	)
	// Each pass either returns or removes the expired file it read, so the
	// loop ends when the candidates run out; the bound is only a backstop. A
	// kept-but-silent marker costs no pass: readLifecycleFailure passes the
	// silent band over itself (markerSilent) and never returns one here.
	for range lifecycleSelfCleanMaxPasses {
		m = readLifecycleFailure(projectID, projectName)
		if m == nil {
			return ""
		}
		var err error
		at, err = time.Parse(time.RFC3339, m.At)
		if err != nil {
			return ""
		}
		// Expired on EITHER side, each on its own bound: past
		// lifecycleMarkerMaxAge on the age side, or past
		// lifecycleMarkerClockSkew (24h of tolerated skew) on the forward
		// side. A marker dated ahead — a clock set forward when it was
		// written, a VM resumed with a skewed clock, a marker copied in from
		// another machine — has a NEGATIVE age and so would never satisfy an
		// "older than" horizon: as the only candidate it would alert at every
		// session start, unchanged, until wall time caught up. Within the 24h
		// tolerance the skew is ordinary and the marker still alerts; beyond
		// it the stamp is implausible and is swept by the removal below rather
		// than left to linger.
		if !markerExpired(at, time.Now()) {
			break
		}
		// Remove the expired file that was read, and only that file. Not
		// ClearLifecycleFailure: that is a clean run's clear, which removes
		// every marker recording the project, and from here it would delete a
		// CURRENT failure keyed on the project's name along with a stale one
		// keyed on its id. A withheld project names no file either, which is
		// the other reason the path is used. It came from config.DataDirPath
		// through readLifecycleFailure. The next pass reads what is left, so a
		// current marker behind the expired one is still shown.
		if m.path == "" {
			return ""
		}
		if !markerFileRemoved(m.path) {
			// An expired marker the directory refuses to remove is the same
			// un-clearable marker the check below reports: it cannot be
			// cleared, so it cannot be called stale either. The check below
			// sits after this loop and is never reached from here, so ask it
			// now rather than return silence and retry the removal at every
			// session start. A removal that fails for any other reason keeps
			// the old behaviour.
			if p, ok := markerAlertProject(m, projectID, projectName); ok {
				return lifecycleUnclearableAlert(p, at)
			}
			return ""
		}
		m = nil
	}
	if m == nil {
		return ""
	}
	// m is non-nil only via the break above, which requires an alertable age,
	// so a marker older than lifecycleAlertMaxAge never reaches the render.
	p, ok := markerAlertProject(m, projectID, projectName)
	if !ok {
		return ""
	}
	// A marker that cannot be cleared — the data directory refuses new
	// entries — is not presented as the current cause: it may be stale, and
	// the blocker is the directory, not the recorded phase. Surface that
	// instead, on the same session-start line (#1580).
	if alert := lifecycleUnclearableAlert(p, at); alert != "" {
		return alert
	}
	p = renderMarkerProject(p)
	return fmt.Sprintf(
		"**Ghost maintenance alert:** last automatic consolidation failed for project %s at %s (phase(s): %s; %s). Memory consolidation is paused until a run succeeds — run `ghost lifecycle --project %s` to retry, and check config cli.*_binary if no LLM CLI was found.",
		p, at.Format(time.RFC3339), renderMarkerPhases(m.PhasesFailed),
		renderMarkerError(m.Error), p)
}

// markerAlertProject decides whether a marker is this session's to report and
// names the project the alert renders: the marker's own project, or the
// session's when the marker is unattributed. ok is false when both sides name a
// project and they differ (another project's failure is not this
// conversation's business), and when neither side names one (degenerate: no
// retry command to give).
func markerAlertProject(m *lifecycleFailureMarker, projectID, projectName string) (string, bool) {
	if m.Project != "" && projectID != "" && m.Project != projectID && m.Project != projectName {
		return "", false
	}
	p := m.Project
	if p == "" {
		p = projectID
	}
	if p == "" {
		return "", false
	}
	return p, true
}

// lifecycleUnclearableAlert renders the session-start line for a marker the
// data directory will not let the alert clear, or "" when the directory is
// writable now and the marker can be handled normally. It is asked for a
// current marker and for an expired one whose self-clean removal failed, so
// both read the directory as the blocker rather than presenting a failure that
// may already be fixed as the current cause.
func lifecycleUnclearableAlert(p string, at time.Time) string {
	dataDir, err := config.DataDirPath()
	if err != nil {
		return ""
	}
	werr := lifecycleBookkeepingWritable(dataDir)
	if werr == nil {
		return ""
	}
	p = renderMarkerProject(p)
	return fmt.Sprintf(
		"**Ghost maintenance alert:** the data directory refuses new files (%s), so lifecycle bookkeeping cannot be written; the failure recorded for project %s at %s can be neither cleared nor confirmed as the current cause. Fix the directory's write access, then run `ghost lifecycle --project %s` to retry.",
		withoutPath(werr), p, at.Format(time.RFC3339), p)
}

// renderMarkerPhases renders the marker's failed-phase list for the alert, the
// third free-text field on that line beside the project and the error. Each
// entry is read from the marker file verbatim, so each is rendered as a token:
// a corrupt, hand-edited or older-build marker whose entry holds a line break
// would otherwise forge a line above the block's own «...» explainer — a second
// `## Ghost context:` heading or a fake `Use project_id:` instruction — the
// same class of caller text the two renderers beside this one exist for.
// assemble.Token writes an ordinary phase name byte for byte and quotes
// anything outside its bare set, so a line break prints as the Go escape `\n`
// and cannot start a line.
//
// An empty list is a legitimate shape now — a signal that landed between phases
// records the run, not a phase, and names none — so the field renders "none"
// rather than an empty gap between its two separators. The stand-in is a
// constant, not caller data, so it carries nothing through the token guard the
// entries do.
func renderMarkerPhases(phases []string) string {
	if len(phases) == 0 {
		return "none"
	}
	rendered := make([]string, len(phases))
	for i, p := range phases {
		rendered[i] = assemble.Token(p)
	}
	return strings.Join(rendered, ", ")
}

// renderMarkerError renders the marker's error for the alert. The error is the
// failed phase's output tail, so it is text a harness or a model wrote, printed
// at the top of every session start for the marker's lifetime: it goes through
// the token guard again (markerErrorField — the file may predate the write-side
// guard), the whole error rather than the line shown, so a token the line only
// begins is still caught; then ONE line of it (markerExplainingLine) is capped
// and framed as «...» data (quoteData), so instruction-like output reads as
// quoted text rather than as Ghost's own words. The cap applies before the
// frame, so the closing delimiter is never cut.
//
// Paths are stripped from the stored error text so that markers written by
// older builds (which stored the raw PathError/LinkError text containing
// absolute paths) also render without paths, keeping the alert's no-paths
// contract. stripPaths is the shared string-level helper also used at write time.
func renderMarkerError(text string) string {
	text = stripPaths(text)
	// The guard is asked twice: of the stored text, and of the line built from
	// it, because moving the interruption ahead of the report can put words
	// that were apart in the file next to each other on the shown line.
	errLine := markerErrorField(markerExplainingLine(markerErrorField(text)))
	return quoteData(truncateUTF8(errLine, lifecycleMarkerErrorMaxBytes))
}

// markerExplainingLine picks the one line of a stored error the alert shows.
//
// The error is a phase's output tail, and a phase's first line is its banner
// ("Project: ...") or a log record ("level=INFO msg=\"resolve prefilter\""),
// so the first line says that the phase started and not why it failed. The
// line that says why is the phase's own "error: ..." report (phaseErrorPrefix),
// and the FIRST one is taken, because the marker records the first cause: an
// interrupted phase appended after it can carry a cancellation report of its
// own. The report is found by its prefix rather than by being last, because the
// tail is fed by the phase's stdout and stderr through two pipes, so a stdout
// line can land after it. A stored error with no such line — the no-output
// fallback, NoLLMBackendError, a withheld placeholder, an interruption between
// phases — is shown by its first line, as before.
//
// An interruption is a fact of its own (markerInterrupt), and when it follows
// an earlier failure it sits at the end of whatever line that tail ended on,
// which need not be the report. It is put FIRST, joined to the report with
// "; ", so the cap applied after this cuts the report rather than the
// interruption. When the two share a line the interruption is moved off the
// end of it, not repeated. The result is rebuilt text, so renderMarkerError
// asks the token guard of it again; the whole tail stays in the marker file.
func markerExplainingLine(text string) string {
	lines := strings.Split(text, "\n")
	at := 0
	for i, l := range lines {
		if strings.HasPrefix(l, phaseErrorPrefix) {
			at = i
			break
		}
	}
	line := lines[at]
	sentence, on, ok := markerInterrupt(lines)
	if !ok {
		return line
	}
	if on == at {
		line = strings.TrimSuffix(strings.TrimSuffix(line, sentence), "; ")
	}
	if line = strings.TrimSpace(line); line == "" {
		return sentence
	}
	return sentence + "; " + line
}

// markerInterrupt finds the interruption sentence in a stored error, in the
// two places the coordinator writes it and no others: as the whole first line,
// when nothing failed before the signal (interruptOutcome), or at the end of a
// line after "; ", when it was appended to an earlier phase's cause
// (applyInterrupt). It returns the sentence and the index of its line.
func markerInterrupt(lines []string) (string, int, bool) {
	if interruptSentence.MatchString(lines[0]) {
		return lines[0], 0, true
	}
	for i, l := range lines {
		j := strings.LastIndex(l, "; "+InterruptedBySignal)
		if j < 0 {
			continue
		}
		if s := l[j+len("; "):]; interruptSentence.MatchString(s) {
			return s, i, true
		}
	}
	return "", -1, false
}

// renderMarkerProject renders the marker's project for the alert, and it is two
// renderers rather than one because the value needs both properties and neither
// renderer has both.
//
// The value is a raw `--project` operand: WriteLifecycleFailure falls back to it
// when the store cannot resolve it, so `ghost lifecycle --project $'evil\n## x'`
// stores the newline. assemble.Label folds it to one line — a newline would
// otherwise start a forged heading above the block's own «...» explainer, the
// same reason the block renders its project NAME through Label (#791) — and it
// returns an ordinary project byte-identically, quotes and all absent.
//
// Label is not enough on its own, and this is the half that is easy to miss:
// Label neutralises characters for a line, it does not keep a secret out of one
// (the bind refusal draws the same line). A clone URL with inline credentials is
// what an operator or an agent pastes into `--project` about as often as a name,
// and ProjectArg is the renderer that withholds it behind a placeholder naming
// the field and the credential's format — so a token is not replayed into every
// session's context until the marker ages out. The guard is asked FIRST, because
// the credential verdict has to win over the line verdict: a value that is both
// hostile and credential-shaped must come back as the placeholder, not as an
// escaped rendering of the token.
//
// The result is bounded beside the error line (lifecycleMarkerProjectMaxBytes),
// and the bound is applied AFTER the branch above rather than before it, so the
// one value that is already short — the placeholder — is never cut.
func renderMarkerProject(p string) string {
	if memory.RejectSecret("project", p) != nil {
		return memory.ProjectArg("project", p)
	}
	return truncateUTF8(assemble.Label(p), lifecycleMarkerProjectMaxBytes)
}
