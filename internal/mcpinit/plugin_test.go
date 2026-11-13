package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunningAsPluginEnv(t *testing.T) {
	t.Setenv(pluginRootEnv, "")
	// The test binary does not live under the plugin cache, so only the env
	// var can flip this.
	if RunningAsPlugin() {
		t.Fatal("expected RunningAsPlugin false with no env and a non-plugin executable")
	}
	t.Setenv(pluginRootEnv, "/somewhere")
	if !RunningAsPlugin() {
		t.Fatal("expected RunningAsPlugin true when CLAUDE_PLUGIN_ROOT is set")
	}
}

func TestPluginMarkerPath(t *testing.T) {
	t.Setenv(pluginDataEnv, "")
	if got := pluginMarkerPath(); got != "" {
		t.Errorf("expected empty marker path without CLAUDE_PLUGIN_DATA, got %q", got)
	}
	t.Setenv(pluginDataEnv, "/data/ghost")
	if got, want := pluginMarkerPath(), filepath.Join("/data/ghost", finalizeMarkerName); got != want {
		t.Errorf("pluginMarkerPath() = %q, want %q", got, want)
	}
}

func TestFinalizePluginWritesMarker(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv(pluginDataEnv, dataDir)
	marker := filepath.Join(dataDir, finalizeMarkerName)

	var first bytes.Buffer
	finalizePlugin(&first)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected finalize marker to be written: %v", err)
	}
	if !strings.Contains(first.String(), "finalizing") {
		t.Errorf("first run should report finalizing on the stderr writer, got %q", first.String())
	}

	var second bytes.Buffer
	finalizePlugin(&second)
	if strings.Contains(second.String(), "finalizing") {
		t.Errorf("second run should be skipped by the marker, got %q", second.String())
	}
}

// TestFinalizePluginNoMarkerWhenAutoMemoryFails pins the reviewer-caught
// contract: if disabling Claude's built-in file memory cannot be persisted,
// the marker must NOT be written, so the next session retries instead of
// silently leaving the competing memory enabled forever.
func TestFinalizePluginNoMarkerWhenAutoMemoryFails(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	// Make ~/.claude a regular file, so settings save's MkdirAll fails.
	if err := os.WriteFile(filepath.Join(home, ".claude"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	dataDir := t.TempDir()
	t.Setenv(pluginDataEnv, dataDir)

	var buf bytes.Buffer
	finalizePlugin(&buf)
	if _, err := os.Stat(filepath.Join(dataDir, finalizeMarkerName)); err == nil {
		t.Fatal("marker must not be written when the auto-memory step failed")
	}
	if !strings.Contains(buf.String(), "will retry next session") {
		t.Errorf("expected a retry diagnostic, got %q", buf.String())
	}
}
