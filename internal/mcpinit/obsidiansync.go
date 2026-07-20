package mcpinit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/wcatz/ghost/internal/config"
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

	exe, err := os.Executable()
	if err != nil {
		return
	}
	logFile, err := os.OpenFile(filepath.Join(dataDir, "obsidian-sync.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
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
		return
	}
	_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	_ = cmd.Process.Release()
}

// isAlive reports whether pidPath names a PID file for a process that is
// still running. It never treats a stale or missing PID file as an error —
// the caller's only decision is "spawn a new one, or not".
func isAlive(pidPath string) bool {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
