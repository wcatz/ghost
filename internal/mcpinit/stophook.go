package mcpinit

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/hostevent"
	"github.com/wcatz/ghost/internal/memory"
)

// stopReminder is emitted (as hook JSON) when a tool-using session ends
// without a single Ghost save. It is a non-blocking "approve" so no host
// renders it as a Stop hook error — the message is surfaced as a plain
// reminder, never as a failure. (Hosts that cannot honor a block — opencode —
// re-present it through their own channel; Claude/Codex/Goose show it as a
// normal system note, not an error.)
const stopReminder = `{"decision":"approve","reason":"Reminder: this session used tools but saved nothing to Ghost. If you learned anything worth keeping — commands, configs, gotchas, decisions — save it with ghost_memory_save before moving on."}`

// RunHostEvent is the contract-v1 entrypoint for every host lifecycle event:
//
//	ghost hook <event> --source <host>
//
// It validates the contract envelope strictly (hostevent.Parse) and dispatches
// per spec §2.2: session-start injects context, stop runs lifecycle spawns plus
// the capability-gated save nudge, session-end runs the spawns only. Fail-open
// is absolute: every validation, scan, or I/O failure logs one line to stderr,
// writes nothing to stdout, and returns — callers exit 0 so the host proceeds.
func RunHostEvent(eventArg, sourceArg string, stdin io.Reader, stdout io.Writer, stderr io.Writer) {
	data, err := io.ReadAll(stdin)
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
		// Output is capability-scoped (spec §2.1): hosts that cannot consume
		// injected context get a silent, successful no-op — never output they
		// would misread. Parse guarantees a known source.
		if cap, _ := hostevent.CapabilityFor(payload.HostSource()); !cap.InjectContext {
			return
		}
		runSessionStart(payload.Raw, stdout)
	case hostevent.EventStop:
		runStop(payload, stdout, stderr, true)
	case hostevent.EventSessionEnd:
		runStop(payload, stdout, stderr, false)
	}
}

// logFailOpen writes the single fail-open diagnostic line the outcome table
// promises. It never touches stdout and never returns an error upward: the
// hook must allow the stop regardless.
func logFailOpen(stderr io.Writer, stage string, err error) {
	if stderr == nil {
		return
	}
	_, _ = fmt.Fprintf(stderr, "ghost hook: fail-open (%s: %v)\n", stage, err)
}

// runStop executes the stop/session-end handler: opt-in lifecycle spawns, then
// — only on stop events (nudge=true) — the save-nudge reminder. The nudge is a
// non-blocking {"decision":"approve"} emitted for every host that reaches the
// nudge path, so no host surfaces it as a Stop hook error. opencode's plugin
// re-presents it through its own log channel; claude/codex/goose show it as a
// plain system note. session-end (nudge=false) never scans the transcript and
// never emits output.
func runStop(p hostevent.Payload, stdout io.Writer, stderr io.Writer, nudge bool) {
	// Adapter-materialized transcripts are swept once this invocation ends —
	// including on the guarded early-return below, so a leaked temp dir can't
	// outlive us regardless of which path fires.
	defer cleanupTransientTranscript(p)

	// stop_hook_active means the host is re-stopping immediately after our
	// reminder — spawning and nudging again would just loop. session-end
	// ignores the flag: it fires once per real session end, so its lifecycle
	// spawns always run (spec §2.2).
	if nudge && p.StopHookActive {
		return
	}

	source := string(p.HostSource())
	spawnResolveIfConfigured(p.CWD, source)
	spawnSupersedeIfConfigured(p.CWD, source)
	spawnReflectIfConfigured(p.CWD, source)

	if !nudge || p.TranscriptPath == "" {
		return
	}
	if _, ok := hostevent.CapabilityFor(p.HostSource()); !ok {
		logFailOpen(stderr, "unknown source "+string(p.HostSource()), fmt.Errorf("no capability entry"))
		return
	}
	f, err := os.Open(p.TranscriptPath)
	if err != nil {
		logFailOpen(stderr, "open transcript", err)
		return
	}
	defer f.Close() //nolint:errcheck

	res, ok, err := hostevent.Scan(p.Contract.TranscriptFormat, f)
	if !ok {
		logFailOpen(stderr, "transcript format", fmt.Errorf("no scanner registered for %q", p.Contract.TranscriptFormat))
		return
	}
	if err != nil {
		// Partial counts only: a save recorded after the cut would be missed,
		// so decide nothing — the stop passes.
		logFailOpen(stderr, "scan transcript", err)
		return
	}
	if res.ToolCalls == 0 || res.GhostSaves > 0 {
		return
	}
	// The reminder fires for every host that reaches here, but as a non-blocking
	// "approve" so hosts never present it as a Stop hook error. opencode's plugin
	// captures this same stdout and re-presents it through its own log channel.
	_, _ = fmt.Fprintln(stdout, stopReminder)
}

// cleanupTransientTranscript removes an adapter-materialized transcript once
// ghost is done with it. The opencode plugin writes under a mkdtemp
// `ghost-*` directory in the OS temp dir; because the plugin's own cleanup
// runs in its (possibly exiting) host process, a host that quits before the
// detached hook finishes — or exits right after spawning — would otherwise
// leak transcript dirs forever (`opencode run` does exactly this). Guarded
// twice: only adapter-materialized formats are considered, and only paths
// directly inside <os.TempDir()>/ghost-*/ are ever removed.
func cleanupTransientTranscript(p hostevent.Payload) {
	if p.Contract == nil || p.TranscriptPath == "" {
		return
	}
	switch p.Contract.TranscriptFormat {
	case hostevent.FormatOpencodeMessages, hostevent.FormatCodexRollout:
	default:
		return
	}
	dir := filepath.Dir(p.TranscriptPath)
	if !strings.HasPrefix(filepath.Base(dir), "ghost-") || filepath.Dir(dir) != os.TempDir() {
		return
	}
	_ = os.RemoveAll(dir)
}

// spawnResolveIfConfigured starts `ghost resolve <project> --apply` as a
// detached background process for the project matching cwd, if one isn't
// already running for that project. Opt-in via reflection.auto_resolve
// (default false) — most users never want an unattended write pass. Every
// failure path returns silently: this must never block or fail the stop hook.
// If the Anthropic API is out of credit, the spawned process itself fails
// fast and logs the failure to resolve.log — no local fallback runs in this
// path, so auto-resolve simply does nothing until credits are restored.
// Known limitation: resolution here depends on Store.ResolveProject's
// path/basename match against the stored project row; a cwd with no matching
// project is a silent no-op, same as an unconfigured user.
func spawnResolveIfConfigured(cwd, source string) {
	if cwd == "" {
		return
	}
	cfg, err := config.Load()
	if err != nil || !cfg.Reflection.AutoResolve {
		return
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

	pidPath := filepath.Join(dataDir, "resolve-"+projectID+".pid")
	if isAlive(pidPath) {
		return
	}
	// isAlive false above is only a fast path to skip locking in the common
	// case (no resolve running at all). It is NOT sufficient on its own: two
	// stop hooks firing close together for the same project could both pass
	// it and both decide to spawn a paid-API, DB-writing process. claimPidFile
	// re-checks liveness under an OS-level lock, serializing the
	// check-then-write against every other caller on the machine, so exactly
	// one of them wins the claim.
	if !claimPidFile(pidPath) {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}
	logFile, err := os.OpenFile(filepath.Join(dataDir, "resolve.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer logFile.Close() //nolint:errcheck

	cmd := exec.Command(exe, "resolve", projectName, "--apply")
	if source != "" {
		cmd.Args = append(cmd.Args, "--source", source)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return
	}
	token, haveToken := processStartTime(cmd.Process.Pid)
	_ = atomicWritePID(pidPath, cmd.Process.Pid, token, haveToken)
	_ = cmd.Process.Release()
}

// spawnSupersedeIfConfigured starts `ghost supersede <project> --apply` as a
// detached background process for the project matching cwd, if one isn't
// already running for that project. Opt-in via reflection.auto_supersede
// (default false) — most users never want an unattended write pass. Every
// failure path returns silently: this must never block or fail the stop hook.
// If the Anthropic API is out of credit, the spawned process itself fails
// fast and logs the failure to supersede.log — no local fallback runs in this
// path, so auto-supersede simply does nothing until credits are restored.
// Known limitation: resolution here depends on Store.ResolveProject's
// path/basename match against the stored project row; a cwd with no matching
// project is a silent no-op, same as an unconfigured user.
func spawnSupersedeIfConfigured(cwd, source string) {
	if cwd == "" {
		return
	}
	cfg, err := config.Load()
	if err != nil || !cfg.Reflection.AutoSupersede {
		return
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

	pidPath := filepath.Join(dataDir, "supersede-"+projectID+".pid")
	if isAlive(pidPath) {
		return
	}
	// isAlive false above is only a fast path to skip locking in the common
	// case (no supersede running at all). It is NOT sufficient on its own: two
	// stop hooks firing close together for the same project could both pass
	// it and both decide to spawn a paid-API, DB-writing process. claimPidFile
	// re-checks liveness under an OS-level lock, serializing the
	// check-then-write against every other caller on the machine, so exactly
	// one of them wins the claim.
	if !claimPidFile(pidPath) {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}
	logFile, err := os.OpenFile(filepath.Join(dataDir, "supersede.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer logFile.Close() //nolint:errcheck

	cmd := exec.Command(exe, "supersede", projectName, "--apply")
	if source != "" {
		cmd.Args = append(cmd.Args, "--source", source)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return
	}
	token, haveToken := processStartTime(cmd.Process.Pid)
	_ = atomicWritePID(pidPath, cmd.Process.Pid, token, haveToken)
	_ = cmd.Process.Release()
}

// spawnReflectIfConfigured starts `ghost reflect <project> --apply` as a
// detached background process for the project matching cwd, if one isn't
// already running for that project. Opt-in via reflection.auto_reflect
// (default false). Every failure path returns silently: this must never block
// or fail the stop hook.
//
// Unlike the resolve/supersede twins, this adds a no-LLM guard: consolidation
// is only worth an unattended write when a real LLM tier is available. Without
// an API key or a claude/opencode binary, --tier auto would fall through to the
// Jaccard-only sqlite tier and rewrite every non-manual memory for no quality
// gain, so the spawn is skipped entirely — before the DB is even opened, so
// this stays a cheap read-only no-op.
func spawnReflectIfConfigured(cwd, source string) {
	if cwd == "" {
		return
	}
	cfg, err := config.Load()
	if err != nil || !cfg.Reflection.AutoReflect {
		return
	}
	if cfg.API.Key == "" && !ai.NewCLIProviderWithBinaries(cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary).Available() {
		sp := ai.NewSourceProviderForSource(source, cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary)
		if !sp.Available() {
			slog.Warn("reflect: skipping — no CLI binary available", "source", source)
			return
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

	pidPath := filepath.Join(dataDir, "reflect-"+projectID+".pid")
	if isAlive(pidPath) {
		return
	}
	if !claimPidFile(pidPath) {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		return
	}
	logFile, err := os.OpenFile(filepath.Join(dataDir, "reflect.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer logFile.Close() //nolint:errcheck

	cmd := exec.Command(exe, "reflect", projectName, "--apply", "--require-llm")
	if source != "" {
		cmd.Args = append(cmd.Args, "--source", source)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return
	}
	token, haveToken := processStartTime(cmd.Process.Pid)
	_ = atomicWritePID(pidPath, cmd.Process.Pid, token, haveToken)
	_ = cmd.Process.Release()
}

// atomicWritePID writes pid (and, when haveToken is true, its creation-time
// token in "pid:token" form — see processStartTime) into path via
// write-temp-then-rename so a concurrent reader (e.g. another caller's
// claimPidFile, which reads this file under its own lock) never observes a
// truncated or partially-written file — os.WriteFile's
// open+truncate+write is not atomic and this call site runs outside any
// lock. When haveToken is false, the bare-PID legacy format is written,
// same as before this token support existed.
func atomicWritePID(path string, pid int, token string, haveToken bool) error {
	content := strconv.Itoa(pid)
	if haveToken {
		content += ":" + token
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// claimPidFile atomically claims pidPath for the current process. It takes
// an OS-level exclusive lock (flock/LockFileEx) on a sibling ".lock" file
// before checking liveness and writing the PID, so the whole
// check-then-write sequence is serialized against every other caller on the
// machine rather than just the final write. That serialization is the part
// a plain os.OpenFile(O_EXCL) or os.Link claim can't provide on its own:
// once a caller decides an existing claim is stale and moves to reclaim it,
// nothing stops a second caller from reaching the same decision against the
// same stale content and reclaiming it too. The lock is kernel-owned, so a
// process that dies mid-claim releases it automatically — a crash here can
// never deadlock a later caller. Returns false if the slot is genuinely held
// by a live process, or on any I/O failure.
func claimPidFile(pidPath string) bool {
	lockFile, err := os.OpenFile(pidPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false
	}
	defer lockFile.Close() //nolint:errcheck

	if err := lockExclusive(lockFile); err != nil {
		return false
	}
	defer unlockFile(lockFile) //nolint:errcheck

	if isAlive(pidPath) {
		return false
	}
	token, haveToken := processStartTime(os.Getpid())
	return atomicWritePID(pidPath, os.Getpid(), token, haveToken) == nil
}
