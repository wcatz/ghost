package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestIsUnderPluginCache pins the home-anchored match: only paths inside the
// current user's Claude Code plugin cache count, a /.claude/plugins/ segment
// under some OTHER root never does (it used to false-positive and refuse
// `ghost upgrade`), and when the home directory cannot be resolved the
// historical substring match stands.
func TestIsUnderPluginCache(t *testing.T) {
	t.Run("anchored to the resolved home", func(t *testing.T) {
		home := t.TempDir()
		setHome(t, home)
		inHome := filepath.ToSlash(filepath.Join(home, ".claude", "plugins", "ghost", "1.0.0", "bin", "ghost"))
		elsewhere := filepath.ToSlash(filepath.Join(home, "other-root", ".claude", "plugins", "ghost", "1.0.0", "bin", "ghost"))
		cases := []struct {
			path string
			want bool
		}{
			{inHome, true},
			{"/usr/local/bin/ghost", false},
			{"/home/u/.claude/settings.json", false},
			// The audit's false positive: same segment, foreign root.
			{elsewhere, false},
			// The root itself is not inside it: the prefix must not match
			// without the trailing segment.
			{filepath.ToSlash(filepath.Join(home, ".claude", "plugins")), false},
			// The trailing separator keeps `plugins` from matching
			// `plugins-evil` — a sibling prefix must not straddle.
			{filepath.ToSlash(filepath.Join(home, ".claude", "plugins-evil", "bin", "ghost")), false},
		}
		for _, tc := range cases {
			if got := isUnderPluginCache(tc.path); got != tc.want {
				t.Errorf("isUnderPluginCache(%q) = %v, want %v", tc.path, got, tc.want)
			}
		}
		if runtime.GOOS != "windows" {
			// On POSIX the match is case-sensitive; on Windows this same
			// path would be true via the folded branch, so assert it only
			// off Windows (deleting the runtime.GOOS guard in the
			// implementation fails here).
			up := strings.ToUpper(inHome)
			if isUnderPluginCache(up) {
				t.Errorf("isUnderPluginCache(%q) = true, want false; the POSIX match must be case-sensitive", up)
			}
		}
	})

	t.Run("windows-shaped paths under the same home", func(t *testing.T) {
		setHome(t, `C:\Users\u`)
		if !isUnderPluginCache(`C:\Users\u\.claude\plugins\ghost\1.0.0\bin\ghost.exe`) {
			t.Error("want true for the plugin cache under the resolved home")
		}
		if isUnderPluginCache(`D:\other\.claude\plugins\ghost\bin\ghost.exe`) {
			t.Error("want false for a foreign root even with backslashes")
		}
	})

	t.Run("case-insensitive prefix on Windows", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("case-folding branch only runs on Windows; asserted there and pinned as case-sensitive on POSIX above")
		}
		setHome(t, `C:\Users\u`)
		if !isUnderPluginCache(`C:/USERS/U/.CLAUDE/PLUGINS/ghost/1.0.0/bin/ghost.exe`) {
			t.Error("want true for a differently-cased executable path on Windows")
		}
	})

	t.Run("substring fallback when home is unresolvable", func(t *testing.T) {
		setHome(t, "")
		if !isUnderPluginCache("/x/.claude/plugins/ghost/bin/ghost") {
			t.Error("want historical substring match when UserHomeDir fails")
		}
		if isUnderPluginCache("/usr/local/bin/ghost") {
			t.Error("want false for a non-cache path even in the fallback")
		}
	})

	t.Run("symlinked home matches either spelling", func(t *testing.T) {
		real := t.TempDir()
		parent := t.TempDir()
		link := filepath.Join(parent, "home-link")
		if err := os.Symlink(real, link); err != nil {
			t.Skipf("cannot create symlink: %v", err)
		}
		setHome(t, link)
		// Lexical form: $HOME exactly as spelled.
		lexical := filepath.ToSlash(filepath.Join(link, ".claude", "plugins", "ghost", "bin", "ghost"))
		if !isUnderPluginCache(lexical) {
			t.Errorf("isUnderPluginCache(%q) = false, want true for the lexical home spelling", lexical)
		}
		// Symlink-resolved form: os.Executable() on Linux resolves through
		// symlinks, so the cache under the resolved $HOME must match too.
		resolvedHome, err := filepath.EvalSymlinks(real)
		if err != nil {
			t.Fatalf("EvalSymlinks: %v", err)
		}
		resolved := filepath.ToSlash(filepath.Join(resolvedHome, ".claude", "plugins", "ghost", "bin", "ghost"))
		if !isUnderPluginCache(resolved) {
			t.Errorf("isUnderPluginCache(%q) = false, want true for the symlink-resolved home spelling", resolved)
		}
		// A foreign /.claude/plugins/ root — a sibling of the link, so still
		// not under either spelling of home — stays false.
		foreign := filepath.ToSlash(filepath.Join(parent, "other-root", ".claude", "plugins", "ghost", "bin", "ghost"))
		if isUnderPluginCache(foreign) {
			t.Errorf("isUnderPluginCache(%q) = true, want false for a foreign root", foreign)
		}
	})
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
			setHome(t, home)
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
