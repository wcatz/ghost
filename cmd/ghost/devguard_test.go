package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
)

// withVersion sets the build version the way the release ldflags do, and
// restores the one the test binary was built with, so one test cannot leave the
// next one believing it is a release.
func withVersion(t *testing.T, v string) {
	t.Helper()
	prev := version
	version = v
	t.Cleanup(func() { version = prev })
}

// TestApplyBuildVersionHandsTheCLIVersionToTheDataDirGuard is the whole of what
// package main owns for GHOST_DEV_FORBID_DATA_DIR (#721). The rule itself lives
// in config.DataDirPath, which every path into the data directory goes through,
// so the only wiring here is the value: the version the binary prints is the
// version the guard judges by. A guard applied with a different value than
// `ghost version` reports is a guard nobody can reason about — and a build that
// never applied one would fall back to config's "dev" default, which is the
// GUARDED side rather than the open one.
func TestApplyBuildVersionHandsTheCLIVersionToTheDataDirGuard(t *testing.T) {
	prev := config.BuildVersion()
	t.Cleanup(func() { config.SetBuildVersion(prev) })

	withVersion(t, "0.39.0")
	applyBuildVersion()
	if got := config.BuildVersion(); got != "0.39.0" {
		t.Fatalf("after applyBuildVersion with version = %q the config package sees %q", version, got)
	}

	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv(config.DevForbidDataDirEnv, dataDir)

	// A release build: the resolver creates and returns the directory it has
	// always created.
	got, err := config.DataDir()
	if err != nil {
		t.Fatalf("config.DataDir on a release build = %v, want the directory", err)
	}
	if got != dataDir {
		t.Errorf("config.DataDir = %q, want %q", got, dataDir)
	}
	if _, err := os.Stat(dataDir); err != nil {
		t.Errorf("config.DataDir did not create %s: %v", dataDir, err)
	}

	// The same binary's own dev version: refused, and nothing created.
	withVersion(t, "dev")
	applyBuildVersion()
	if got := config.BuildVersion(); got != "dev" {
		t.Fatalf("after applyBuildVersion with version = %q the config package sees %q", version, got)
	}
	other := filepath.Join(root, "other-data")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", other, err)
	}
	t.Setenv("XDG_DATA_HOME", other)
	// The listing names the NEW data home's ghost directory: the release half
	// above already created the first one, so pointing at it would prove nothing
	// about a directory that does not exist yet.
	t.Setenv(config.DevForbidDataDirEnv, filepath.Join(other, "ghost"))
	if _, err := config.DataDirPath(); err == nil {
		t.Error("config.DataDirPath = nil, want a refusal on a development build")
	} else if !strings.Contains(err.Error(), config.DevForbidDataDirEnv) {
		t.Errorf("the refusal does not name %s: %v", config.DevForbidDataDirEnv, err)
	}
	if _, err := os.Stat(filepath.Join(other, "ghost")); !os.IsNotExist(err) {
		t.Errorf("the refusal created the data directory (stat error: %v)", err)
	}
}
