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

// TestPluginInstalledRegistryNames pins the coexistence contract for the
// plugin family: installed_plugins.json keys are "<plugin>@<marketplace>",
// and native Windows is served by the per-arch entries
// "ghost-windows-amd64"/"ghost-windows-arm64", so the check must match the
// exact Ghost-managed names, not any plugin whose name starts with "ghost-".
func TestPluginInstalledRegistryNames(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"ghost@ghost", true},
		{"ghost-windows-amd64@ghost", true},
		{"ghost-windows-arm64@ghost", true},
		{"GHOST-Windows-AMD64@ghost", true},
		{"something-else@ghost", false},
		{"ghostwriter@ghost", false},
		{"ghost-tools@somewhere", false},
		{"ghost-windows-386@ghost", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv(pluginRootEnv, "")
			dir := filepath.Join(home, ".claude", "plugins")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			doc := `{"version":2,"plugins":{"` + tc.key + `":[]}}`
			if err := os.WriteFile(filepath.Join(dir, "installed_plugins.json"), []byte(doc), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := PluginInstalled(); got != tc.want {
				t.Errorf("PluginInstalled() with %q = %v, want %v", tc.key, got, tc.want)
			}
		})
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

// TestFinalizePluginNoMarkerWhenAutoMemoryFails pins the reviewer-caught
// contract: if disabling Claude's built-in file memory cannot be persisted,
// the marker must NOT be written, so the next session retries instead of
// silently leaving the competing memory enabled forever.
func TestFinalizePluginNoMarkerWhenAutoMemoryFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
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
