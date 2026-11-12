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
)

func ensureObsidianSyncRunning() {
	if testing.Testing() {
		return
	}
	if !config.LoadForHook().Obsidian.AutoSync {
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
	if claimed, err := claimPidFile(pidPath); err != nil || !claimed {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		slog.Warn("obsidian sync spawn: cannot locate the ghost binary", "error", err)
		return
	}
	logFile, err := openLogForAppend(filepath.Join(dataDir, "obsidian-sync.log"))
	if err != nil {
		slog.Warn("obsidian sync spawn: cannot open log", "error", err)
		return
	}
	defer logFile.Close() //nolint:errcheck

	cmd := exec.Command(exe, "obsidian", "sync")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		slog.Warn("obsidian sync spawn: starting the detached process failed", "error", err)
		return
	}
	token, haveToken := processStartTime(cmd.Process.Pid)
	_ = atomicWritePID(pidPath, cmd.Process.Pid, token, haveToken)
	_ = cmd.Process.Release()
}

func isAlive(pidPath string) bool {
	data, ok := readPIDFile(pidPath)
	if !ok {
		return false
	}
	pidStr, token, haveToken := strings.Cut(strings.TrimSpace(string(data)), ":")
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return false
	}
	return isProcessAlive(pid, token, haveToken)
}
