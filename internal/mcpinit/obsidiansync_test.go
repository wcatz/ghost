package mcpinit

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestEnsureObsidianSyncRunning_DisabledByDefault(t *testing.T) {
	// Isolate from the host: obsidian.auto_sync defaults to false, and this
	// must not touch a real home/config/data dir either way.
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)
	dataDir := filepath.Join(tmpDir, "data")
	t.Setenv("XDG_DATA_HOME", dataDir)

	ensureObsidianSyncRunning()

	if _, err := os.Stat(filepath.Join(dataDir, "ghost", "obsidian-sync.pid")); !os.IsNotExist(err) {
		t.Errorf("expected no pidfile to be created when auto_sync is off (err=%v)", err)
	}
}

func TestIsAlive_MissingFile(t *testing.T) {
	if isAlive(filepath.Join(t.TempDir(), "nope.pid")) {
		t.Error("isAlive() = true for a missing pidfile, want false")
	}
}

func TestIsAlive_Garbage(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "obsidian-sync.pid")
	if err := os.WriteFile(pidPath, []byte("not-a-pid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if isAlive(pidPath) {
		t.Error("isAlive() = true for a non-numeric pidfile, want false")
	}
}

func TestIsAlive_LiveProcess(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "obsidian-sync.pid")
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isAlive(pidPath) {
		t.Error("isAlive() = false for this test process's own PID, want true")
	}
}

func TestIsAlive_DeadProcess(t *testing.T) {
	// PID 1 belongs to init/systemd, never to us; a PID far above any real
	// process on a test runner is the reliable way to name a dead one.
	pidPath := filepath.Join(t.TempDir(), "obsidian-sync.pid")
	if err := os.WriteFile(pidPath, []byte("999999"), 0o600); err != nil {
		t.Fatal(err)
	}
	if isAlive(pidPath) {
		t.Error("isAlive() = true for an implausible PID, want false")
	}
}
