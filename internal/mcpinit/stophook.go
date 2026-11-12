package mcpinit

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/hostevent"
	"github.com/wcatz/ghost/internal/memory"
)

// RunHostEvent is the contract-v1 entrypoint for every host lifecycle event:
func RunHostEvent(eventArg, sourceArg string, stdin io.Reader, stdout io.Writer, stderr io.Writer) {
	data, err := hostevent.ReadPayload(stdin)
	if err != nil {
		logFailOpen(stderr, "read stdin", err)
		return
	}
	payload, err := hostevent.Parse(data, eventArg, sourceArg)
	if err != nil {
		logFailOpen(stderr, "validate contract", err)
		return
	}

	switch payload.Event() {
	case hostevent.EventSessionStart:
		if cap, _ := hostevent.CapabilityFor(payload.HostSource()); !cap.InjectContext {
			return
		}
		if RunningAsPlugin() {
			finalizePlugin(stderr)
		}
		runSessionStart(payload.Raw, payload.HostSource(), stdout)
	case hostevent.EventMessageSubmit, hostevent.EventEdit:
		if cap, _ := hostevent.CapabilityFor(payload.HostSource()); !cap.WorkingMoment {
			return
		}
		runWorkingMoment(payload, stdout)
	case hostevent.EventStop:
		runStop(payload, stdout, stderr, true)
	case hostevent.EventSessionEnd:
		runStop(payload, stdout, stderr, false)
	}
}

func logFailOpen(stderr io.Writer, stage string, err error) {
	if stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(stderr, "ghost hook: fail-open (%s: %v)\n", stage, err)
}

func runStop(p hostevent.Payload, stdout io.Writer, stderr io.Writer, nudge bool) {
	defer cleanupTransientTranscript(p)

	if nudge && p.StopHookActive {
		return
	}

	source := string(p.HostSource())
	spawnLifecycleIfConfigured(p.CWD, source, stderr)
}

func cleanupTransientTranscript(p hostevent.Payload) {
	if p.Contract == nil || p.TranscriptPath == "" {
		return
	}
	switch p.Contract.TranscriptFormat {
	case hostevent.FormatOpencodeMessages, hostevent.FormatOpencodeV2Messages:
	default:
		return
	}
	dir, ok := pluginSessionDir(p.TranscriptPath)
	if !ok {
		return
	}
	if os.Remove(p.TranscriptPath) != nil {
		return
	}
	_ = os.Remove(dir)
}

const (
	opencodeSessionDirPrefix = "ghost-"
	opencodeTranscriptName   = "messages.jsonl"
)

func isPluginSessionDirName(name string) bool {
	rest, ok := strings.CutPrefix(name, opencodeSessionDirPrefix)
	if !ok || len(rest) != 6 {
		return false
	}
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

func pluginSessionDir(transcriptPath string) (string, bool) {
	if filepath.Base(transcriptPath) != opencodeTranscriptName {
		return "", false
	}
	dir := filepath.Dir(transcriptPath)
	if !isPluginSessionDirName(filepath.Base(dir)) || !insideAHostTempDir(filepath.Dir(dir)) {
		return "", false
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || !ownedByCurrentUser(info) {
		return "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != opencodeTranscriptName {
		return "", false
	}
	fi, err := os.Lstat(transcriptPath)
	if err != nil || !fi.Mode().IsRegular() || !ownedByCurrentUser(fi) {
		return "", false
	}
	return dir, true
}

func hostTempDirCandidates() []string {
	candidates := []string{os.TempDir()}
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		if v := os.Getenv(key); v != "" {
			candidates = append(candidates, v)
		}
	}
	return candidates
}

func insideAHostTempDir(dir string) bool {
	for _, base := range hostTempDirCandidates() {
		if sameDir(dir, base) {
			return true
		}
	}
	return false
}

func sameDir(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func safeProjectIDComponent(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	if len(id) > maxProjectIDComponentBytes {
		return false
	}
	if strings.ContainsFunc(id, func(r rune) bool { return r < 0x20 }) {
		return false
	}
	if strings.ContainsAny(id, `\/`) {
		return false
	}
	if runtime.GOOS == "windows" && strings.ContainsAny(id, `:*?"<>|`) {
		return false
	}
	return filepath.Base(id) == id
}

const fileNameMaxBytes = 255

const longestDerivedNameOverhead = len("lifecycle-") + len(".last.tmp") + 10

const maxProjectIDComponentBytes = fileNameMaxBytes - longestDerivedNameOverhead

func spawnLifecycleIfConfigured(cwd, source string, stderr io.Writer) {
	if cwd == "" {
		return
	}
	cfg := config.LoadForHook()
	if !cfg.Reflection.AutoReflect && !cfg.Reflection.AutoResolve && !cfg.Reflection.AutoSupersede {
		return
	}
	if cfg.Reflection.AutoReflect && !cfg.Reflection.AutoResolve && !cfg.Reflection.AutoSupersede {
		if !ai.NewCLIProviderWithBinaries(cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary).Available() {
			sp := ai.NewSourceProviderForSource(source, cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary)
			if !sp.Available() {
				slog.Warn("lifecycle: skipping — no CLI binary available", "source", source)
				recordReflectSkipMarker(cwd, stderr)
				return
			}
		}
	}

	dataDir, err := config.DataDir()
	if err != nil {
		return
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	if _, err := os.Stat(dbPath); err != nil {
		return
	}
	db, err := sql.Open("sqlite", roDSN(dbPath))
	if err != nil {
		return
	}
	defer db.Close() //nolint:errcheck

	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	projectID, projectName := resolveSessionProject(context.Background(), store, cwd)
	if projectID == "" || projectName == "" {
		return
	}
	if projectID == memory.GlobalProjectID {
		return
	}

	if !safeProjectIDComponent(projectID) {
		slog.Warn("lifecycle spawn: refusing a project id that is not a safe filename component",
			"project_id", memory.RefusalArg("project id", projectID))
		recordSpawnFailure(projectID, cfg, errors.New(UnsafeProjectIDError), stderr)
		return
	}
	pidPath := filepath.Join(dataDir, "lifecycle-"+projectID+".pid")
	if isAlive(pidPath) {
		return
	}
	minInterval := cfg.Lifecycle.MinIntervalDuration()
	if skip, since := lifecycleCooldownActive(dataDir, projectID, minInterval, time.Now()); skip {
		logLifecycleCooldownSkip(dataDir, projectID, since, minInterval)
		return
	}

	exe, err := os.Executable()
	if err != nil {
		slog.Warn("lifecycle spawn: cannot locate the ghost binary", "error", err)
		if stderr != nil {
			_, _ = fmt.Fprintf(stderr, "ghost hook: lifecycle spawn skipped: cannot locate the ghost binary (%v)\n", err)
		}
		recordSpawnFailure(projectID, cfg, err, stderr)
		return
	}
	if err := lifecycleBookkeepingWritable(dataDir); err != nil {
		if stderr != nil {
			_, _ = fmt.Fprintf(stderr, "ghost hook: lifecycle spawn skipped: cannot write lifecycle bookkeeping files (%s)\n", withoutPath(err))
		}
		return
	}
	logFile, err := openLogForAppend(filepath.Join(dataDir, "lifecycle.log"))
	if err != nil {
		slog.Warn("lifecycle spawn: cannot open log", "error", err)
		if stderr != nil {
			_, _ = fmt.Fprintf(stderr, "ghost hook: lifecycle spawn skipped: cannot open the lifecycle log (%s)\n", withoutPath(err))
		}
		recordSpawnFailure(projectID, cfg, err, stderr)
		return
	}
	defer logFile.Close() //nolint:errcheck

	cmd := exec.Command(exe, "lifecycle", "--project", projectID)
	if source != "" {
		cmd.Args = append(cmd.Args, "--source", source)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := startLifecycleChild(cmd); err != nil {
		slog.Warn("lifecycle spawn: starting the detached process failed", "error", err)
		if stderr != nil {
			_, _ = fmt.Fprintf(stderr, "ghost hook: lifecycle spawn skipped: cannot start the detached process (%v)\n", err)
		}
		recordSpawnFailure(projectID, cfg, err, stderr)
		return
	}
}

var startLifecycleChild = func(cmd *exec.Cmd) error {
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	return nil
}

func recordReflectSkipMarker(cwd string, stderr io.Writer) {
	dataDir, err := config.DataDirPath()
	if err != nil {
		return
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	if _, err := os.Stat(dbPath); err != nil {
		return
	}
	db, err := sql.Open("sqlite", roDSN(dbPath))
	if err != nil {
		return
	}
	defer db.Close() //nolint:errcheck
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	projectID, _ := resolveSessionProject(context.Background(), store, cwd)
	if projectID == "" || projectID == memory.GlobalProjectID {
		return
	}
	if err := WriteLifecycleFailure(projectID, []string{"reflect"}, NoLLMBackendError); err != nil && stderr != nil {
		_, _ = fmt.Fprintf(stderr, "ghost hook: cannot record the reflect-skip marker: %s\n", withoutPath(err))
	}
}

func recordSpawnFailure(projectID string, cfg *config.Config, cause error, stderr io.Writer) {
	var phases []string
	if cfg.Reflection.AutoReflect {
		phases = append(phases, "reflect")
	}
	if cfg.Reflection.AutoResolve {
		phases = append(phases, "resolve")
	}
	if cfg.Reflection.AutoSupersede {
		phases = append(phases, "supersede")
	}
	if len(phases) == 0 {
		return
	}
	stripped := stripPaths(withoutPath(cause).Error())
	if err := WriteLifecycleFailure(projectID, phases, stripped); err != nil && stderr != nil {
		_, _ = fmt.Fprintf(stderr, "ghost hook: cannot record the lifecycle failure marker: %s\n", withoutPath(err))
	}
}

func atomicWritePID(path string, pid int, token string, haveToken bool) error {
	content := strconv.Itoa(pid)
	if haveToken {
		content += ":" + token
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write([]byte(content)); err != nil {
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

func claimPidFile(pidPath string) (bool, error) {
	claimed, _, err := claimPidFileReclaim(pidPath)
	return claimed, err
}

func claimPidFileReclaim(pidPath string) (claimed bool, reclaimedAt time.Time, err error) {
	lockFile, err := os.OpenFile(pidPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, time.Time{}, fmt.Errorf("open the claim lock: %w", withoutPath(err))
	}
	defer lockFile.Close() //nolint:errcheck

	if err := lockExclusive(lockFile); err != nil {
		return false, time.Time{}, fmt.Errorf("lock the claim: %w", withoutPath(err))
	}
	defer unlockFile(lockFile) //nolint:errcheck

	if priorFile, err := os.OpenFile(pidPath, markerOpenFlags, 0); err == nil {
		prior, statErr := priorFile.Stat()
		if statErr == nil && prior.Mode().IsRegular() {
			if isAlive(pidPath) {
				_ = priorFile.Close()
				return false, time.Time{}, nil
			}
			if pidInFile(pidPath) != 0 {
				reclaimedAt = prior.ModTime()
			}
		}
		_ = priorFile.Close()
	}
	token, haveToken := processStartTime(os.Getpid())
	if err := atomicWritePID(pidPath, os.Getpid(), token, haveToken); err != nil {
		return false, time.Time{}, fmt.Errorf("write the claim: %w", withoutPath(err))
	}
	return true, reclaimedAt, nil
}

func lifecycleBookkeepingWritable(dataDir string) error {
	f, err := os.CreateTemp(dataDir, ".ghost-lifecycle-writability-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

func withoutPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}

var pathTokenRegexp = regexp.MustCompile(`(^|[\s="',([;])((?:/[^\s"'(),;:\[\]](?:[^\s"'(),;:\[\]]|:[^\s])*|[A-Za-z]:[\\/][^\s"'(),;:\[\]](?:[^\s"'(),;:\[\]]|:[^\s])*))(:\s)?`)

func stripPaths(text string) string {
	result := pathTokenRegexp.ReplaceAllStringFunc(text, func(m string) string {
		sub := pathTokenRegexp.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		sep := sub[1]
		trailing := ""
		if len(sub) > 3 && sub[3] != "" {
			trailing = sub[3]
		}
		return sep + "<path>" + trailing
	})

	lines := strings.Split(result, "\n")
	for i, line := range lines {
		pathPos := []int{}
		for offset := 0; ; {
			idx := strings.Index(line[offset:], "<path>")
			if idx == -1 {
				break
			}
			pathPos = append(pathPos, offset+idx)
			offset += idx + len("<path>")
		}
		colonSpacePos := []int{}
		for offset := 0; ; {
			idx := strings.Index(line[offset:], ": ")
			if idx == -1 {
				break
			}
			colonSpacePos = append(colonSpacePos, offset+idx)
			offset += idx + 2
		}

		type dropRange struct{ start, end int }
		var toDrop []dropRange

		for _, cs := range colonSpacePos {
			var lastPathIdx = -1
			for _, pp := range pathPos {
				if pp < cs {
					lastPathIdx = pp
				}
			}
			if lastPathIdx == -1 {
				continue
			}
			hasOtherPath := false
			for _, pp := range pathPos {
				if pp > lastPathIdx && pp < cs {
					hasOtherPath = true
					break
				}
			}
			if hasOtherPath {
				continue
			}
			restStart := lastPathIdx + len("<path>")
			rest := line[restStart:cs]
			if rest != "" && !strings.Contains(rest, ": ") {
				toDrop = append(toDrop, dropRange{restStart, cs})
			}
		}

		for j := len(toDrop) - 1; j >= 0; j-- {
			dr := toDrop[j]
			line = line[:dr.start] + line[dr.end:]
		}
		lines[i] = line
	}
	return strings.Join(lines, "\n")
}
