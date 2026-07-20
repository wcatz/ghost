package mcpinit

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

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
