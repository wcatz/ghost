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
	"runtime"
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
		// A plugin install owns its own wiring, so `ghost mcp init` never ran;
		// finish the one-time operations a plugin manifest cannot express
		// (auto-memory, memory import, redirects) before injecting context.
		// stderr-only — stdout here is the injected context.
		if RunningAsPlugin() {
			finalizePlugin(stderr)
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
	spawnLifecycleIfConfigured(p.CWD, source)

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
	case hostevent.FormatOpencodeMessages, hostevent.FormatOpencodeV2Messages, hostevent.FormatCodexRollout:
	default:
		return
	}
	dir := filepath.Dir(p.TranscriptPath)
	if !strings.HasPrefix(filepath.Base(dir), "ghost-") || filepath.Dir(dir) != os.TempDir() {
		return
	}
	_ = os.RemoveAll(dir)
}

// safeProjectIDComponent reports whether id can be embedded in a filename
// inside the data dir. Project ids originate from callers — an MCP client can
// send any project_id, and EnsureProject stores it verbatim — so the check
// rejects what could reshape the path (empty, ".", "..", path separators) and
// what a filename cannot hold (control characters, and on Windows the reserved
// :*?"<>| set). Everything else is a legitimate id and must keep working:
// rejecting spaces or non-ASCII names would silently switch off
// auto-consolidation for an existing project, which is a worse failure than the
// narrow path issue this guards.
func safeProjectIDComponent(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	// Control characters are unusable in a filename on Windows and have no
	// legitimate place in an id anywhere, so reject them unconditionally
	// (this covers NUL).
	if strings.ContainsFunc(id, func(r rune) bool { return r < 0x20 }) {
		return false
	}
	if strings.ContainsAny(id, `\/`) {
		return false
	}
	// Windows additionally forbids these in a filename component.
	if runtime.GOOS == "windows" && strings.ContainsAny(id, `:*?"<>|`) {
		return false
	}
	return filepath.Base(id) == id
}

// spawnLifecycleIfConfigured starts `ghost lifecycle <project>` as a single
// detached background process for the project matching cwd, if one isn't
// already running for that project. That process runs the enabled phases in
// order — reflect, then resolve, then supersede — so consolidation can never
// rewrite rows while resolve or supersede is classifying them. Spawned as
// three independent processes they raced, which surfaced as foreign-key aborts
// and lost resolved_at stamps. One PID file now guards all three phases.
//
// Opt in per phase via reflection.auto_reflect / auto_resolve / auto_supersede
// (all default false). Every failure path returns silently: this must never
// block or fail the stop hook.
func spawnLifecycleIfConfigured(cwd, source string) {
	if cwd == "" {
		return
	}
	cfg := config.LoadForHook()
	if !cfg.Reflection.AutoReflect && !cfg.Reflection.AutoResolve && !cfg.Reflection.AutoSupersede {
		return
	}
	// Consolidation is only worth an unattended write when a real LLM tier is
	// available: without one, --tier auto would fall through to the Jaccard-only
	// sqlite tier and rewrite every non-manual memory for no quality gain. When
	// reflect is the only enabled phase, skip the whole spawn before the DB is
	// even opened, so this stays a cheap read-only no-op. When resolve or
	// supersede are also enabled they still run — the coordinator skips its own
	// reflect phase in that case.
	if cfg.Reflection.AutoReflect && !cfg.Reflection.AutoResolve && !cfg.Reflection.AutoSupersede {
		if !ai.NewCLIProviderWithBinaries(cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary).Available() {
			sp := ai.NewSourceProviderForSource(source, cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary)
			if !sp.Available() {
				slog.Warn("lifecycle: skipping — no CLI binary available", "source", source)
				// This guard is exactly the historical silent death of
				// auto-reflect (no process ever starts, so lifecycle.log
				// never even appears). Record a failure marker when a store
				// exists to attribute it to, so the next session-start can
				// say so instead of the loss surfacing weeks later.
				recordReflectSkipMarker(cwd)
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

	// The project id is interpolated into a filename inside dataDir, and ids
	// are not constrained by the store: an MCP client may pass an arbitrary
	// project_id, which EnsureProject then uses verbatim. A value containing
	// path separators or ".." would move the pid file (and its .tmp sibling,
	// and the liveness reads) outside the data directory, so refuse rather
	// than build a path from it. Fail open: the hook must never block.
	if !safeProjectIDComponent(projectID) {
		slog.Warn("lifecycle spawn: refusing a project id that is not a safe filename component",
			"project_id", projectID)
		return
	}
	// Best-effort fast path: skip spawning when a lifecycle is visibly running.
	// It is deliberately NOT a claim — the coordinator claims the file itself
	// (AcquireLifecycleLock), because the hook cannot write the child's pid
	// before Start and a claim written in between would make the child see a
	// foreign pid and abort. Two hooks firing together may therefore both spawn;
	// one child wins the claim and the other exits.
	pidPath := filepath.Join(dataDir, "lifecycle-"+projectID+".pid")
	if isAlive(pidPath) {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		slog.Warn("lifecycle spawn: cannot locate the ghost binary", "error", err)
		recordSpawnFailure(projectID, cfg, err)
		return
	}
	logFile, err := os.OpenFile(filepath.Join(dataDir, "lifecycle.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		slog.Warn("lifecycle spawn: cannot open log", "error", err)
		recordSpawnFailure(projectID, cfg, err)
		return
	}
	defer logFile.Close() //nolint:errcheck

	// Pass the project ID, not the name: the coordinator keys its own lifecycle
	// claim on the same value, so the pid file the hook just claimed for this
	// child is the one the child checks. The phase subcommands resolve an id
	// as readily as a name (Store.ResolveProject tries id first).
	cmd := exec.Command(exe, "lifecycle", "--project", projectID)
	if source != "" {
		cmd.Args = append(cmd.Args, "--source", source)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		slog.Warn("lifecycle spawn: starting the detached process failed", "error", err)
		recordSpawnFailure(projectID, cfg, err)
		return
	}
	_ = cmd.Process.Release()
}

// Lock scope for the two hook-side marker writes below: the per-project pid
// lock (AcquireLifecycleLock) serializes only the CHILD's end-of-run
// write/clear — these hook-side writes fire while no child holds it,
// recordReflectSkipMarker before the isAlive(pidPath) fast path and
// recordSpawnFailure when the child never started. A hook-side marker can
// therefore be written and then cleared by a child that subsequently starts
// and succeeds (or the reverse ordering). The benign worst case is at most
// ONE missed alert cycle for that project — never a false alert, and
// temp+rename keeps the marker file itself intact throughout.
//
// recordReflectSkipMarker writes the failure marker for the reflect-only
// no-LLM guard above, where no lifecycle process ever starts. It runs only
// when a store ALREADY exists (DataDirPath never creates, and a store-less
// setup has nothing to consolidate — TestSpawnLifecycleIfConfigured_NoOpWithoutLLM
// pins that no phantom data dir appears), and only when cwd resolves to a
// project: an unattributable skip cannot be matched to a session later, so
// recording it would only produce a stray alert. Best-effort throughout.
func recordReflectSkipMarker(cwd string) {
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
	if projectID == "" {
		return
	}
	_ = WriteLifecycleFailure(projectID, []string{"reflect"}, NoLLMBackendError)
}

// recordSpawnFailure writes the failure marker for a chain that FAILED TO
// START after the project was resolved (binary not locatable, log not
// openable, process not spawnable): no phase will run, so every enabled
// phase is recorded as failed. Best-effort — a hook must never fail a stop.
func recordSpawnFailure(projectID string, cfg *config.Config, cause error) {
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
	_ = WriteLifecycleFailure(projectID, phases, cause.Error())
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
