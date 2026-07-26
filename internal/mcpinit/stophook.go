package mcpinit

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/wcatz/ghost/internal/config"
)

type stopHookInput struct {
	TranscriptPath string `json:"transcript_path"`
	StopHookActive bool   `json:"stop_hook_active"`
	CWD            string `json:"cwd"`
}

// transcriptLine is the minimal shape needed to spot tool_use entries in a
// Claude Code transcript JSONL line. Everything else in the line is ignored.
type transcriptLine struct {
	Type    string `json:"type"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
}

// ghostSaveTools are the tool names whose presence in a transcript proves the
// session saved knowledge to Ghost.
var ghostSaveTools = map[string]bool{
	"mcp__ghost__ghost_memory_save": true,
	"mcp__ghost__ghost_save_global": true,
}

// stopBlockMessage is emitted (as hook JSON) when a tool-using session ends
// without a single Ghost save. Claude Code shows the reason to Claude and the
// session continues once; stop_hook_active guarantees the second stop wins.
const stopBlockMessage = `{"decision":"block","reason":"This session used tools but saved nothing to Ghost. Review the session for discoveries worth keeping (commands, configs, gotchas, decisions) and save them with ghost_memory_save — or stop again if there is truly nothing to save."}`

// HandleStopHook is invoked by Claude Code when a session stops via:
//
//	ghost hook stop
//
// It blocks the stop once — via {"decision":"block"} on stdout — when the
// session used tools but never saved anything to Ghost. Every failure path
// returns silently, allowing the stop: the hook must never trap a session.
// The one exception to "no DB/LLM work on this path" is spawnResolveIfConfigured,
// which — best-effort, opt-in only — does a small synchronous read-only lookup
// (config + a project-ID query) before forking a detached `ghost resolve --apply`
// and returning immediately without waiting on it; no LLM call and no write ever
// happens inline here, only in the detached child.
func HandleStopHook(stdin io.Reader, stdout io.Writer) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return
	}
	var input stopHookInput
	if err := json.Unmarshal(data, &input); err != nil {
		return
	}
	// A prior block already fired this session — the second stop always wins.
	if input.StopHookActive {
		return
	}

	spawnResolveIfConfigured(input.CWD)

	if input.TranscriptPath == "" {
		return
	}
	f, err := os.Open(input.TranscriptPath)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck

	toolCalls, ghostSaves := scanTranscript(f)
	if toolCalls == 0 || ghostSaves > 0 {
		return
	}
	_, _ = fmt.Fprintln(stdout, stopBlockMessage)
}

// scanTranscript streams a transcript and counts assistant tool_use blocks,
// plus how many were Ghost save tools. Unparseable lines are skipped; a
// scanner error mid-file yields the counts seen so far — worst case the nudge
// fires once and the second stop passes through the stop_hook_active guard.
func scanTranscript(r io.Reader) (toolCalls, ghostSaves int) {
	sc := bufio.NewScanner(r)
	// Transcript lines carry full tool results and can be huge.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var line transcriptLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue
		}
		if line.Type != "assistant" {
			continue
		}
		for _, c := range line.Message.Content {
			if c.Type == "tool_use" {
				toolCalls++
				if ghostSaveTools[c.Name] {
					ghostSaves++
				}
			}
		}
	}
	return toolCalls, ghostSaves
}

// spawnResolveIfConfigured starts `ghost resolve <project> --apply` as a
// detached background process for the project matching cwd, if one isn't
// already running for that project. Opt-in via reflection.auto_resolve
// (default false) — most users never want an unattended write pass. Every
// failure path returns silently: this must never block or fail the stop hook.
// If the Anthropic API is out of credit, the spawned process itself fails
// fast and logs the failure to resolve.log — no local fallback runs in this
// path, so auto-resolve simply does nothing until credits are restored.
// Known limitation: resolution here depends on lookupProject's path/basename
// match against the stored project row; a cwd with no matching project is a
// silent no-op, same as an unconfigured user.
func spawnResolveIfConfigured(cwd string) {
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

	projectID, projectName := lookupProject(db, cwd)
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
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return
	}
	_ = atomicWritePID(pidPath, cmd.Process.Pid)
	_ = cmd.Process.Release()
}

// atomicWritePID writes pid into path via write-temp-then-rename so a
// concurrent reader (e.g. another caller's claimPidFile, which reads this
// file under its own lock) never observes a truncated or partially-written
// file — os.WriteFile's open+truncate+write is not atomic and this call site
// runs outside any lock.
func atomicWritePID(path string, pid int) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.Itoa(pid)), 0o600); err != nil {
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
	return atomicWritePID(pidPath, os.Getpid()) == nil
}
