package mcpinit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	pluginRootEnv = "CLAUDE_PLUGIN_ROOT"
	pluginDataEnv = "CLAUDE_PLUGIN_DATA"

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

func isUnderPluginCache(p string) bool {
	s := strings.ReplaceAll(filepath.ToSlash(p), `\`, "/")
	home, err := os.UserHomeDir()
	if err != nil {
		return strings.Contains(s, pluginCacheDir)
	}
	roots := []string{slashJoin(home)}
	if resolved, rerr := filepath.EvalSymlinks(home); rerr == nil && resolved != home {
		roots = append(roots, slashJoin(resolved))
	}
	for _, root := range roots {
		if runtime.GOOS == "windows" {
			if strings.HasPrefix(strings.ToLower(s), strings.ToLower(root)) {
				return true
			}
			continue
		}
		if strings.HasPrefix(s, root) {
			return true
		}
	}
	return false
}

func slashJoin(base string) string {
	return strings.ReplaceAll(filepath.ToSlash(filepath.Join(base, ".claude", "plugins")), `\`, "/") + "/"
}

var managedPluginNames = []string{"ghost", "ghost-windows-amd64", "ghost-windows-arm64"}

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
	data, err := readHostConfigFile(filepath.Join(home, ".claude", "plugins", "installed_plugins.json"))
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
		name, _, _ := strings.Cut(key, "@")
		for _, n := range managedPluginNames {
			if strings.EqualFold(name, n) {
				return true
			}
		}
	}
	return false
}

func pluginMarkerPath() string {
	data := os.Getenv(pluginDataEnv)
	if data == "" {
		return ""
	}
	return filepath.Join(data, finalizeMarkerName)
}

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

	projects, err := importMemories(stderr, false)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "ghost plugin finalize: import memories: %v\n", err)
	} else {
		writeRedirects(stderr, projects, false)
	}

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
