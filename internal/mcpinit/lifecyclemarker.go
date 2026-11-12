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
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

const (
	lifecycleMarkerPrefix              = "lifecycle-last-failure"
	legacyMarkerFile                   = lifecycleMarkerPrefix + ".json"
	lifecycleMarkerVersion             = 1
	lifecycleAlertMaxAge               = 14 * 24 * time.Hour
	lifecycleMarkerMaxAge              = 30 * 24 * time.Hour
	lifecycleMarkerClockSkew           = 24 * time.Hour
	lifecycleSelfCleanMaxPasses        = 8
	lifecycleMarkerErrorMaxBytes       = 300
	lifecycleMarkerErrorRenderMaxBytes = 320
	lifecycleMarkerPhasesMaxBytes      = 100
	lifecycleMarkerPhaseMaxBytes       = 64
	lifecycleAlertMaxBytes             = 900
	lifecycleMarkerProjectMaxBytes     = 300
	lifecycleMarkerDetailMaxBytes      = 1500
)

// NoLLMBackendError is the marker error text for the specific failure this
// surface exists for: auto-reflect enabled but no claude/opencode/codex/goose
// binary reachable, so reflect was skipped instead of run. Shared by the stop
// hook's spawn guard and the lifecycle coordinator so both record the same
// diagnosable fact.
const NoLLMBackendError = "no CLI LLM backend available; reflect skipped"

// UnsafeProjectIDError is the marker cause for a project whose id cannot be a
// filename component. The stop hook's spawn guard records it, and the
// session-start alert recognises it so it asks for an id a lifecycle file can
// be keyed on instead of offering a retry that cannot work for such an id.
const UnsafeProjectIDError = "project id is not a safe filename component, so no lifecycle bookkeeping can be keyed on it"

// InterruptedBySignal opens the sentence the lifecycle coordinator records when
// a signal stops its chain ("interrupted by SIGTERM while running resolve",
// "... before supersede"). Shared with that writer so the alert can find the
// sentence wherever it lands in the stored error: on the first line when
// nothing failed before it, or at the end of an earlier phase's tail, joined
// with "; ", when something did.
const InterruptedBySignal = "interrupted by SIGTERM"

const phaseErrorPrefix = "error: "

var interruptSentence = regexp.MustCompile(
	"^" + regexp.QuoteMeta(InterruptedBySignal) + " (?:while running|before) [A-Za-z0-9_-]+$")

type lifecycleFailureMarker struct {
	Project      string   `json:"project"`
	PhasesFailed []string `json:"phases_failed"`
	Error        string   `json:"error"`
	At           string   `json:"at"` // RFC3339, UTC
	Version      int      `json:"version"`

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
func ClearLifecycleFailure(project string) error {
	dataDir, err := config.DataDirPath()
	if err != nil {
		return err
	}
	operand := project
	var name string
	if project != "" {
		resolved, resolvedName, resolveErr := resolveMarkerIdentity(dataDir, project)
		if errors.Is(resolveErr, memory.ErrAmbiguousProject) {
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
		old := filepath.Join(dataDir, oldProjectMarkerFile(project))
		if m := readMarkerAtPath(old); m != nil && m.Project == project {
			if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
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
	if m := readMarkerAtPath(legacy); m != nil && markerRecordsRun(dataDir, m.Project, project, name, operand) {
		if err := os.Remove(legacy); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

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

func clearCheckedMarker(dataDir, projectID, key string) error {
	owner, _, err := resolveMarkerIdentity(dataDir, key)
	if errors.Is(err, memory.ErrAmbiguousProject) {
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

func markerStem(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:markerStemBytes])
}

const markerStemBytes = 16

func oldProjectMarkerFile(project string) string {
	return lifecycleMarkerPrefix + "-" + sanitizeMarkerProject(project) + ".json"
}

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
		return strings.TrimSuffix(legacyMarkerFile, ".json")
	}
	if len(out) > 80 {
		return out[:80]
	}
	return out
}

// FinishLifecycleRun is the lifecycle coordinator's single end-of-run outcome
// record:
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
	return ClearLifecycleFailure(project)
}

func resolveMarkerProject(dataDir, project string) string {
	id, _, _ := resolveMarkerIdentity(dataDir, project)
	return id
}

func resolveMarkerIdentity(dataDir, project string) (id, name string, err error) {
	if project == "" {
		return "", "", nil
	}
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

var errNotRegularMarker = errors.New("marker is not a regular file")

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

func readLifecycleFailure(projectID, projectName string) *lifecycleFailureMarker {
	dataDir, err := config.DataDirPath()
	if err != nil {
		return nil
	}
	now := time.Now()
	if projectID == "" && projectName == "" {
		patterns := []string{
			filepath.Join(dataDir, lifecycleMarkerPrefix+"-*.json"),
			filepath.Join(dataDir, legacyMarkerFile),
		}
		blocked := sweepExpiredMarkers(now, patterns...)
		if m := newestMarker(now, blocked, patterns...); m != nil {
			return m
		}
		if len(blocked) > 0 {
			return blocked[0]
		}
		return nil
	}
	candidates := markerCandidates(projectID, projectName)
	blocked := sweepExpiredMarkers(now, markerCandidatePaths(dataDir, candidates)...)
	for _, c := range candidates {
		if blockedCandidate(blocked, c.name) {
			continue
		}
		m := readMarkerAtPath(filepath.Join(dataDir, c.name))
		if m == nil {
			continue
		}
		isLegacy := c.name == legacyMarkerFile
		if (c.checkProject || isLegacy) && m.Project != "" && m.Project != projectID && m.Project != projectName {
			continue
		}
		if at, err := time.Parse(time.RFC3339, m.At); err == nil && markerSilent(at, now) {
			continue
		}
		return m
	}
	if projectID != "" {
		if at, ok := unfinishedLifecycleRun(dataDir, projectID); ok && !markerSilent(at, now) {
			return &lifecycleFailureMarker{
				Project: projectID,
				Error:   UnfinishedLifecycleRunError,
				At:      at.UTC().Format(time.RFC3339),
			}
		}
	}
	if b := firstBlockedForSession(blocked, candidates, projectID, projectName); b != nil {
		return b
	}
	return nil
}

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

func markerSilent(at, now time.Time) bool {
	age := now.Sub(at)
	return age > lifecycleAlertMaxAge && age <= lifecycleMarkerMaxAge
}

func markerExpired(at, now time.Time) bool {
	age := now.Sub(at)
	return age > lifecycleMarkerMaxAge || age < -lifecycleMarkerClockSkew
}

var removeMarkerFile = os.Remove

func markerFileRemoved(path string) bool {
	err := removeMarkerFile(path)
	return err == nil || errors.Is(err, fs.ErrNotExist)
}

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

func newestMarker(now time.Time, blocked []*lifecycleFailureMarker, patterns ...string) *lifecycleFailureMarker {
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
			if blockedCandidate(blocked, filepath.Base(path)) {
				continue
			}
			if markerExpired(at, now) {
				continue
			}
			if newest == nil || ranksAbove(at, newestAt, now) {
				newest, newestAt = m, at
			}
		}
	}
	return newest
}

type markerCandidate struct {
	name         string
	checkProject bool
}

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

func markerCandidatePaths(dataDir string, candidates []markerCandidate) []string {
	paths := make([]string, 0, len(candidates))
	for _, c := range candidates {
		paths = append(paths, filepath.Join(dataDir, c.name))
	}
	return paths
}

func blockedCandidate(blocked []*lifecycleFailureMarker, name string) bool {
	for _, m := range blocked {
		if filepath.Base(m.path) == name {
			return true
		}
	}
	return false
}

func firstBlockedForSession(blocked []*lifecycleFailureMarker, candidates []markerCandidate, projectID, projectName string) *lifecycleFailureMarker {
	for _, m := range blocked {
		if blockedIsThisSessions(m, candidates, projectID, projectName) {
			return m
		}
	}
	return nil
}

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

func lifecycleFailureAlert(projectID, projectName string) string {
	var (
		m  *lifecycleFailureMarker
		at time.Time
	)
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
		if !markerExpired(at, time.Now()) {
			break
		}
		if m.path == "" {
			return ""
		}
		if !markerFileRemoved(m.path) {
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
	p, ok := markerAlertProject(m, projectID, projectName)
	if !ok {
		return ""
	}
	if alert := lifecycleUnclearableAlert(p, at); alert != "" {
		return alert
	}
	if strings.TrimSpace(m.Error) == UnsafeProjectIDError {
		rest := lifecycleAlertMaxBytes - len(fmt.Sprintf(lifecycleUnsafeAlertFormat, ""))
		if rest > lifecycleMarkerProjectMaxBytes {
			rest = lifecycleMarkerProjectMaxBytes
		}
		if rest < 0 {
			rest = 0
		}
		return fmt.Sprintf(lifecycleUnsafeAlertFormat, renderMarkerProject(p, rest))
	}
	ts := at.Format(time.RFC3339)
	phases := renderMarkerPhases(m.PhasesFailed)
	errLine := renderMarkerError(m.Error)
	rest := lifecycleAlertMaxBytes - len(fmt.Sprintf(lifecycleAlertFormat, "", ts, phases, errLine, ""))
	p = lifecycleAlertProject(p, rest)
	return fmt.Sprintf(lifecycleAlertFormat, p, ts, phases, errLine, p)
}

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

func lifecycleUnclearableAlert(p string, at time.Time) string {
	dataDir, err := config.DataDirPath()
	if err != nil {
		return ""
	}
	werr := lifecycleBookkeepingWritable(dataDir)
	if werr == nil {
		return ""
	}
	ts := at.Format(time.RFC3339)
	refusal := withoutPath(werr).Error()
	rest := lifecycleAlertMaxBytes - len(fmt.Sprintf(lifecycleUnclearableAlertFormat, refusal, "", ts, ""))
	p = lifecycleAlertProject(p, rest)
	return fmt.Sprintf(lifecycleUnclearableAlertFormat, refusal, p, ts, p)
}

const (
	lifecycleAlertFormat            = "**Ghost maintenance alert:** last automatic consolidation failed for project %s at %s (phase(s): %s; %s). Memory consolidation is paused until a run succeeds — run `ghost lifecycle --project %s` to retry, and check config cli.*_binary if no LLM CLI was found."
	lifecycleUnclearableAlertFormat = "**Ghost maintenance alert:** the data directory refuses new files (%s), so lifecycle bookkeeping cannot be written; the failure recorded for project %s at %s can be neither cleared nor confirmed as the current cause. Fix the directory's write access, then run `ghost lifecycle --project %s` to retry."
	lifecycleUnsafeAlertFormat      = "**Ghost maintenance alert:** project %s cannot be maintained unattended: its id is not a safe filename component, so no lifecycle bookkeeping can be keyed on it. Rename the project id to one a file name can hold."
)

func lifecycleAlertProject(p string, rest int) string {
	share := rest/2 - len("…")
	if share > lifecycleMarkerProjectMaxBytes {
		share = lifecycleMarkerProjectMaxBytes
	}
	if share < 0 {
		share = 0
	}
	return renderMarkerProject(p, share)
}

func markerPrefixWithin(s string, measure func(string) int, room int) int {
	if room < 0 || s == "" {
		return 0
	}
	lo, hi := 0, len(s)
	for lo < hi {
		mid := lo + (hi-lo+1)/2
		for mid > 0 && mid < len(s) && !utf8.RuneStart(s[mid]) {
			mid--
		}
		if mid <= lo {
			mid = lo + (hi-lo+1)/2
			for mid <= hi && mid < len(s) && !utf8.RuneStart(s[mid]) {
				mid++
			}
			if mid > hi {
				break
			}
		}
		if measure(s[:mid]) > room {
			hi = mid - 1
			continue
		}
		lo = mid
	}
	return lo
}

func markerPhaseEntry(p string, maxBytes int) (string, bool) {
	if t := assemble.Token(p); len(t) <= maxBytes {
		return t, true
	}
	room := maxBytes - len("…")
	if room < 0 {
		return "", false
	}
	cut := markerPrefixWithin(p, func(s string) int { return len(assemble.Token(s)) }, room)
	if cut == 0 {
		return "", false
	}
	return assemble.Token(p[:cut]) + "…", true
}

func renderMarkerPhases(phases []string) string {
	if len(phases) == 0 {
		return "none"
	}
	const countRoom = 32
	var (
		shown   []string
		size    int
		dropped int
	)
	for _, p := range phases {
		t, ok := markerPhaseEntry(p, lifecycleMarkerPhaseMaxBytes)
		if !ok {
			dropped++
			continue
		}
		add := len(t)
		if len(shown) > 0 {
			add += len(", ")
		}
		if size+add > lifecycleMarkerPhasesMaxBytes-countRoom {
			dropped++
			continue
		}
		shown = append(shown, t)
		size += add
	}
	list := strings.Join(shown, ", ")
	if dropped == 0 {
		return list
	}
	return list + fmt.Sprintf(" (+%d more)", dropped)
}

func renderMarkerError(text string) string {
	text = stripPaths(text)
	errLine := markerErrorField(markerExplainingLine(markerErrorField(text)))
	content := truncateUTF8(errLine, lifecycleMarkerErrorMaxBytes)
	return quoteData(markerErrorContent(content, lifecycleMarkerErrorRenderMaxBytes))
}

func markerErrorContent(content string, maxBytes int) string {
	if len(quoteData(content)) <= maxBytes {
		return content
	}
	room := maxBytes - len("«»…")
	if room < 0 {
		return ""
	}
	cut := markerPrefixWithin(content, func(s string) int { return len(quoteData(s)) }, room)
	if cut == 0 {
		return ""
	}
	return content[:cut] + "…"
}

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

func renderMarkerProject(p string, maxBytes int) string {
	if memory.RejectSecret("project", p) != nil {
		return memory.ProjectArg("project", p)
	}
	label := assemble.Label(p)
	if len(label) <= maxBytes {
		return label
	}
	room := maxBytes - len("…")
	if room < 0 {
		return ""
	}
	cut := markerPrefixWithin(p, func(s string) int { return len(assemble.Label(s)) }, room)
	if cut == 0 {
		return "…"
	}
	return assemble.Label(p[:cut]) + "…"
}
