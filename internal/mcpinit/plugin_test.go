package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsUnderPluginCache(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/home/u/.claude/plugins/ghost/1.0.0/bin/ghost", true},
		{`C:\Users\u\.claude\plugins\ghost\1.0.0\bin\ghost.exe`, true},
		{"/usr/local/bin/ghost", false},
		{"/home/u/.claude/settings.json", false},
	}
	for _, tc := range cases {
		if got := isUnderPluginCache(tc.path); got != tc.want {
			t.Errorf("isUnderPluginCache(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

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

// TestFinalizePluginMarkerSkips pins the marker contract: once the finalize
// marker exists, a later session must do nothing at all.
func TestFinalizePluginMarkerSkips(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv(pluginDataEnv, dataDir)
	if err := os.WriteFile(filepath.Join(dataDir, finalizeMarkerName), []byte("done\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	finalizePlugin(&buf)
	if buf.Len() != 0 {
		t.Errorf("finalize with marker present should be a no-op, got output %q", buf.String())
	}
}

// TestFinalizePluginWritesMarker verifies the first run finalizes and writes
// the marker last, and that a second run is skipped. Diagnostics must land on
// the supplied stderr writer, never stdout.
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
