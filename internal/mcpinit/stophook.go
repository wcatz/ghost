package mcpinit

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
// It performs no synchronous database access of its own; the one exception is
// spawnResolveIfConfigured, which — best-effort — forks a detached
// `ghost resolve --apply` and returns immediately without waiting on it, so
// this function's own hot path still does no DB/LLM work.
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
	// isAlive false means no pidfile, or a stale one left by a prior spawn that
	// never got as far as writing a real PID. Claim the slot before doing any
	// of the expensive setup below, so two stop hooks firing close together
	// (same project, e.g. near-simultaneous sessions) can't both pass the
	// liveness check and double-spawn a paid-API, DB-writing process.
	//
	// A plain os.OpenFile(O_CREATE|O_EXCL) claim is atomic for *existence* but
	// not for *content* — a second caller can observe the file the instant
	// after create, before this process's PID is written into it, and
	// mistake the still-empty file for a free slot. Avoiding that requires
	// content to exist at the moment the name is claimed, which os.Link
	// gives us: write our PID to a private temp file first, then hard-link it
	// onto pidPath — an atomic, all-or-nothing rename-like claim that fails
	// with ErrExist if the name is already taken, with no window where the
	// name exists but the content doesn't.
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
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	_ = cmd.Process.Release()
}

// claimPidFile atomically claims pidPath for the current process, retrying
// once against a stale claim. It writes a temp file containing our own PID,
// then hard-links it onto pidPath: os.Link fails with ErrExist if the name
// is taken, and unlike os.OpenFile(O_EXCL) there is no window where the name
// exists but the content is still empty, because the link target already
// has its final content at the moment the name is claimed. Returns false if
// the slot is genuinely held by a live process, or on any I/O failure.
func claimPidFile(pidPath string) bool {
	tmp, err := os.CreateTemp(filepath.Dir(pidPath), "resolve-*.pid.tmp")
	if err != nil {
		return false
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) //nolint:errcheck

	_, writeErr := tmp.WriteString(strconv.Itoa(os.Getpid()))
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		return false
	}

	if err := os.Link(tmpPath, pidPath); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return false
		}
		if isAlive(pidPath) {
			return false
		}
		_ = os.Remove(pidPath)
		if err := os.Link(tmpPath, pidPath); err != nil {
			return false
		}
	}
	return true
}
