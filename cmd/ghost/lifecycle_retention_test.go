package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/wcatz/ghost/internal/config"
)

func TestRunLifecycleDataDirMaintenance(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the test probe shim is POSIX-only")
	}
	dataHome := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", dataHome)
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "lsof"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	dataDir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dataDir, "lifecycle.log")
	if err := os.WriteFile(logPath, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}

	runLifecycleDataDirMaintenance(&config.Config{Retention: config.RetentionConfig{
		BackupCount: 1,
		LogMaxBytes: 4,
	}})
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "6789" {
		t.Fatalf("maintenance helper left log=%q, want newest tail %q", data, "6789")
	}
}
