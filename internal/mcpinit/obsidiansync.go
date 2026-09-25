package mcpinit

import (
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/fileguard"
	"github.com/wcatz/ghost/internal/maintenance"
)

// ensureObsidianSyncRunning starts `ghost obsidian sync` as a detached
// background process if one isn't already running, so the Obsidian vault
// mirror stays current for the rest of the machine's uptime instead of only
// reflecting whatever was in the DB the last time someone ran the command by
// hand. Opt-in via obsidian.auto_sync (default false) — most users never run
// Obsidian at all, and this must not create a vault directory or spawn a
// process on their machines without asking. Best-effort and silent
// otherwise: any failure here must never block or fail the session-start hook.
func ensureObsidianSyncRunning() {
	// os.Executable() below resolves to the running binary's own path. Under
	// `go test` that's the compiled test binary, not `ghost` — spawning it
	// with "obsidian sync" as args would just re-run the whole test suite,
	// which calls back into this function, recursively. Refuse unconditionally.
	if testing.Testing() {
		return
	}

	cfg, err := config.Load()
	if err != nil || !cfg.Obsidian.AutoSync {
		return
	}

	dataDir, err := config.DataDir()
	if err != nil {
		return
	}
	pidPath := filepath.Join(dataDir, "obsidian-sync.pid")
	if isAlive(pidPath) {
		return
	}
	// isAlive false is only a fast path: two hooks firing close together would
	// both pass it and both spawn a vault-writing process. claimPidFile
	// re-checks under an OS-level lock so exactly one wins, matching the
	// resolve/supersede/reflect lifecycle spawns.
	if !claimPidFile(pidPath) {
		return
	}

	exe, err := os.Executable()
	if err != nil {
		slog.Warn("obsidian sync spawn: cannot locate the ghost binary", "error", err)
		return
	}
	// Rotate before opening the detached sync log. This is file-only
	// maintenance; it never opens the database or changes auto-sync policy.
	if _, err := maintenance.RotateLogs(dataDir, cfg.Retention.LogMaxBytes); err != nil {
		slog.Warn("obsidian sync spawn: log rotation failed", "error", err)
	}
	logPath := filepath.Join(dataDir, "obsidian-sync.log")
	logLock, err := fileguard.AcquireLock(logPath + ".lock")
	if err != nil {
		slog.Warn("obsidian sync spawn: cannot lock log", "error", err)
		return
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	_ = logLock.Close()
	if err != nil {
		slog.Warn("obsidian sync spawn: cannot open log", "error", err)
		return
	}
	defer logFile.Close() //nolint:errcheck

	cmd := exec.Command(exe, "obsidian", "sync")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// New session: survives this short-lived hook process exiting, and won't
	// receive signals sent to Claude Code's process group.
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		slog.Warn("obsidian sync spawn: starting the detached process failed", "error", err)
		return
	}
	token, haveToken := processStartTime(cmd.Process.Pid)
	_ = atomicWritePID(pidPath, cmd.Process.Pid, token, haveToken)
	_ = cmd.Process.Release()
}

// isAlive reports whether pidPath names a PID file for a process that is
// still running. It never treats a stale or missing PID file as an error —
// the caller's only decision is "spawn a new one, or not". The file holds
// either a bare PID (legacy format, liveness-only) or "pid:token" (see
// processStartTime), in which case a live PID additionally has to still
// carry that token to be reported alive — see isProcessAlive in
// obsidiansync_unix.go / obsidiansync_windows.go.
func isAlive(pidPath string) bool {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	pidStr, token, haveToken := strings.Cut(strings.TrimSpace(string(data)), ":")
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return false
	}
	return isProcessAlive(pid, token, haveToken)
}
