package mcpinit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Claude Code plugin integration. The plugin ships Ghost's wiring
// declaratively (.mcp.json + hooks/hooks.json), so `ghost mcp init` never runs;
// the one-time operations a manifest cannot express are finalized by Ghost's
// own SessionStart hook on first contact. See
// docs/superpowers/specs/2026-08-20-ghost-claude-plugin-design.md.

const (
	// pluginRootEnv is set by Claude Code for every process a plugin owns —
	// including hook processes — and is the primary plugin-mode signal.
	pluginRootEnv = "CLAUDE_PLUGIN_ROOT"
	// pluginDataEnv is a persistent, per-plugin directory that survives
	// updates (unlike pluginRootEnv, whose value changes on every update).
	// It holds the one-time finalize marker.
	pluginDataEnv = "CLAUDE_PLUGIN_DATA"

	// pluginCacheDir is the path segment Claude Code installs plugin caches
	// under. The bundled binary lives inside it, so its executable path is a
	// reliable plugin signal even when the env var is absent.
	pluginCacheDir = "/.claude/plugins/"

	finalizeMarkerName = "finalized"
)

// RunningAsPlugin reports whether this ghost process is the binary Claude Code
// installed inside the plugin cache, as opposed to a standalone install. It is
// the safety gate: only a plugin-managed process may finalize plugin wiring or
// refuse `ghost upgrade`.
func RunningAsPlugin() bool {
	if os.Getenv(pluginRootEnv) != "" {
		return true
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	return isUnderPluginCache(exe)
}

// isUnderPluginCache reports whether p lies inside Claude Code's plugin cache.
// Both path separators are normalized to forward slashes so the check is
// testable cross-platform and holds on Windows, whose paths use backslashes.
func isUnderPluginCache(p string) bool {
	s := strings.ReplaceAll(filepath.ToSlash(p), `\`, "/")
	return strings.Contains(s, pluginCacheDir)
}

// PluginInstalled reports whether a Ghost plugin is installed for Claude Code
// at all — detected by running-as-plugin OR by an entry in Claude Code's
// installed-plugins registry. `ghost mcp init` uses this to defer rather than
// double-wire alongside a plugin. A registry read failure is not fatal: it
// degrades to the running-as-plugin signal.
func PluginInstalled() bool {
	if RunningAsPlugin() {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"))
	if err != nil {
		return false
	}
	var doc struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return false
	}
	for key := range doc.Plugins {
		// Keys are "<plugin>@<marketplace>". The POSIX plugin is "ghost";
		// native Windows is served by the per-arch entries
		// "ghost-windows-amd64" and "ghost-windows-arm64", each bundling one
		// .exe. Match the whole ghost family so a standalone `ghost mcp init`
		// defers to any of them instead of double-wiring.
		name, _, _ := strings.Cut(key, "@")
		if strings.EqualFold(name, "ghost") || strings.HasPrefix(strings.ToLower(name), "ghost-") {
			return true
		}
	}
	return false
}

// pluginMarkerPath returns the finalize marker path inside CLAUDE_PLUGIN_DATA,
// or "" when the host did not provide a persistent data dir (in which case
// finalize still runs — every step is idempotent — it just cannot be skipped).
func pluginMarkerPath() string {
	data := os.Getenv(pluginDataEnv)
	if data == "" {
		return ""
	}
	return filepath.Join(data, finalizeMarkerName)
}

// finalizePlugin performs the one-time, idempotent plugin-wiring finalization:
// the operations `ghost mcp init` owns that a plugin manifest cannot express.
//
//  1. disable Claude Code's built-in file memory (autoMemoryEnabled: false)
//  2. import existing Claude Code memories into Ghost
//  3. write MEMORY.md redirects for known projects
//
// A marker in CLAUDE_PLUGIN_DATA skips all three on later sessions. Every step
// is idempotent, so a crash before the marker is written self-heals on the next
// session.
//
// Diagnostics go to stderr only. The SessionStart hook's stdout is injected
// into the model's context, so nothing here may ever be written there.
func finalizePlugin(stderr io.Writer) {
	if stderr == nil {
		stderr = io.Discard
	}

	marker := pluginMarkerPath()
	if marker != "" {
		if _, err := os.Stat(marker); err == nil {
			return // already finalized for this plugin install
		}
	}

	_, _ = fmt.Fprintln(stderr, "ghost plugin: finalizing first-run wiring")

	// Step 1: disable Claude Code's competing file memory. This is the one step
	// whose failure would otherwise be permanent: the marker would skip it on
	// every later session, leaving Claude's built-in file memory enabled and
	// competing with Ghost. Track success and gate the marker on it.
	autoMemoryOK := false
	if path, err := settingsPath(); err != nil {
		_, _ = fmt.Fprintf(stderr, "ghost plugin finalize: settings path: %v\n", err)
	} else if sf, err := loadSettings(path); err != nil {
		_, _ = fmt.Fprintf(stderr, "ghost plugin finalize: load settings: %v\n", err)
	} else if err := ensureAutoMemoryDisabled(stderr, sf, false); err != nil {
		_, _ = fmt.Fprintf(stderr, "ghost plugin finalize: auto-memory: %v\n", err)
	} else if err := sf.save(); err != nil {
		_, _ = fmt.Fprintf(stderr, "ghost plugin finalize: save settings: %v\n", err)
	} else {
		autoMemoryOK = true
	}

	// Steps 2 and 3 are idempotent and self-healing (memories also auto-import
	// on the first ghost_project_context call), so their failures are logged
	// but do not block finalization. importMemories returns the project list
	// writeRedirects needs, so redirects run only when that list is available.
	projects, err := importMemories(stderr, false)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ghost plugin finalize: import memories: %v\n", err)
	} else {
		writeRedirects(stderr, projects, false)
	}

	// Marker last, and only if the one non-self-healing step succeeded. A
	// failed auto-memory step leaves the marker unwritten so the next session
	// retries; a crash mid-finalize behaves the same way.
	if marker == "" {
		return
	}
	if !autoMemoryOK {
		_, _ = fmt.Fprintln(stderr, "ghost plugin finalize: auto-memory step failed; will retry next session")
		return
	}
	if err := os.MkdirAll(filepath.Dir(marker), 0755); err != nil {
		_, _ = fmt.Fprintf(stderr, "ghost plugin finalize: create marker dir: %v\n", err)
		return
	}
	stamp := time.Now().UTC().Format(time.RFC3339) + "\n"
	if err := os.WriteFile(marker, []byte(stamp), 0644); err != nil {
		_, _ = fmt.Fprintf(stderr, "ghost plugin finalize: write marker: %v\n", err)
	}
}
