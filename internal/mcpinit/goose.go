package mcpinit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/hostevent"
)

const (
	goosePluginName        = "ghost"
	gooseAgentPluginsVer   = "1.0.0"
	gooseManifestSchemaURL = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	gooseMCPSchemaURL      = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
)

type goosePluginManifest struct {
	Schema      string `json:"$schema"`
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

type gooseStdioServer struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

type gooseMCPConfig struct {
	Schema     string                      `json:"$schema"`
	MCPServers map[string]gooseStdioServer `json:"mcpServers"`
}

type gooseHookAction struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type gooseHookRule struct {
	Hooks []gooseHookAction `json:"hooks"`
}

type gooseHooksConfig struct {
	Hooks map[string][]gooseHookRule `json:"hooks"`
}

func renderGoosePluginManifest() string {
	return marshalGooseJSON(goosePluginManifest{
		Schema:      gooseManifestSchemaURL,
		Name:        goosePluginName,
		Version:     "0.1.0",
		Description: "Persistent cross-session memory for AI coding agents: MCP tools plus session lifecycle hooks.",
	})
}

func renderGooseMCPConfig(ghostBin string) string {
	return marshalGooseJSON(gooseMCPConfig{
		Schema: gooseMCPSchemaURL,
		MCPServers: map[string]gooseStdioServer{
			goosePluginName: {
				Type:    "stdio",
				Command: ghostBin,
				Args:    []string{"mcp"},
			},
		},
	})
}

func renderGooseHooksConfig(ghostBin string) string {
	hookCmd := func(event string) string {
		return shellQuote(ghostBin) + " hook " + event + " --source " + string(hostevent.SourceGoose)
	}
	rule := func(event string) []gooseHookRule {
		return []gooseHookRule{{Hooks: []gooseHookAction{{Type: "command", Command: hookCmd(event)}}}}
	}
	return marshalGooseJSON(gooseHooksConfig{
		Hooks: map[string][]gooseHookRule{
			"SessionStart": rule("session-start"),
			"Stop":         rule("stop"),
			"SessionEnd":   rule("session-end"),
		},
	})
}

func marshalGooseJSON(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {

		panic(fmt.Sprintf("render goose config: %v", err))
	}
	return string(data) + "\n"
}

func goosePluginDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".agents", "plugins", goosePluginName), nil
}

// RunGoose installs Ghost's goose integration: an Agent Plugins-conformant
// package under ~/.agents/plugins/ghost/ whose plugin.json satisfies the v1.0.0

func RunGoose(w io.Writer, dryRun bool) error {
	if dryRun {
		_, _ = fmt.Fprintf(w, "\nDry run — showing what would change:\n\n")
	}

	_, _ = fmt.Fprintln(w, "[1/3] Checking prerequisites...")
	ghostBin, _, err := checkPrereqs(w, "goose")
	if err != nil {
		return retryHint(err)
	}

	_, _ = fmt.Fprintln(w, "\n[2/3] Ensuring ghost config file...")
	if err := ensureConfigBootstrap(w, dryRun); err != nil {
		return retryHint(err)
	}

	_, _ = fmt.Fprintln(w, "\n[3/3] Installing goose plugin package...")
	changed, err := installGoosePackage(w, ghostBin, dryRun)
	if err != nil {
		return stepError(err)
	}
	if changed && !dryRun {
		_, _ = fmt.Fprintln(w, "Restart goose to activate.")
	}

	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! load config: %v\n", err)
	} else {
		checkOllama(w, cfg, func(ok bool, pass, fail string) {
			if ok {
				_, _ = fmt.Fprintf(w, "  ✓ %s\n", pass)
			} else {
				_, _ = fmt.Fprintf(w, "  ✗ %s\n", fail)
			}
		})
	}

	if dryRun {
		_, _ = fmt.Fprintln(w, "\nNo changes made (dry run).")
	}
	return nil
}

type goosePackageFile struct {
	label string
	rel   string
	want  string
}

func goosePackageFiles(ghostBin string) []goosePackageFile {
	return []goosePackageFile{
		{label: "plugin.json", rel: "plugin.json", want: renderGoosePluginManifest()},
		{label: "mcp.json", rel: "mcp.json", want: renderGooseMCPConfig(ghostBin)},
		{label: "hooks/hooks.json", rel: filepath.Join("hooks", "hooks.json"), want: renderGooseHooksConfig(ghostBin)},
	}
}

func installGoosePackage(w io.Writer, ghostBin string, dryRun bool) (bool, error) {
	dir, err := goosePluginDir()
	if err != nil {
		return false, err
	}

	changed := false
	for _, f := range goosePackageFiles(ghostBin) {
		path := filepath.Join(dir, f.rel)
		existing, rerr := readHostConfigFile(path)
		if rerr != nil {
			if os.IsNotExist(rerr) {

			} else {
				return changed, fmt.Errorf("read %s: %w", path, rerr)
			}
		} else if string(existing) == f.want {
			_, _ = fmt.Fprintf(w, "  ✓ %s already current (%s)\n", f.label, path)
			continue
		}

		if dryRun {
			_, _ = fmt.Fprintf(w, "  ~ would install %s (%s)\n", f.label, path)
			changed = true
			continue
		}

		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return changed, fmt.Errorf("create plugin dir: %w", err)
		}
		if err := writeFileAtomic(path, []byte(f.want), 0644); err != nil {
			return changed, fmt.Errorf("write %s: %w", f.label, err)
		}
		_, _ = fmt.Fprintf(w, "  + installed %s (%s)\n", f.label, path)
		changed = true
	}
	return changed, nil
}

// StatusGoose checks the health of the Ghost ↔ goose integration. It reports
// only goose-relevant checks — the ghost binary, the plugin package under

func StatusGoose(w io.Writer) (bool, error) {
	_, _ = fmt.Fprintf(w, "\nGhost ↔ goose integration status:\n\n")

	healthy := true
	check := func(ok bool, pass, fail string) {
		if ok {
			_, _ = fmt.Fprintf(w, "  ✓ %s\n", pass)
		} else {
			_, _ = fmt.Fprintf(w, "  ✗ %s\n", fail)
			healthy = false
		}
	}

	ghostBin := findGhostBinary()
	check(ghostBin != "",
		fmt.Sprintf("ghost binary: %s", ghostBin),
		"ghost binary not found in PATH")

	reportConfigFile(w)

	dir, derr := goosePluginDir()
	switch {
	case derr != nil:
		check(false, "", fmt.Sprintf("goose plugin package: %v", derr))
	default:
		allCurrent := true
		for _, f := range goosePackageFiles(ghostBin) {
			data, rerr := readHostConfigFile(filepath.Join(dir, f.rel))
			if rerr != nil || string(data) != f.want {
				allCurrent = false
				break
			}
		}
		if allCurrent {
			check(true, fmt.Sprintf("goose plugin package installed: %s", dir), "")
		} else {
			check(false, "", "goose plugin package missing or outdated (run ghost mcp init --client goose)")
		}
	}

	store := checkStoreHealth(w, check)
	if store != nil {
		defer store.Close() //nolint:errcheck
	}

	_, _ = fmt.Fprintln(w)
	if healthy {
		_, _ = fmt.Fprintln(w, "All checks passed.")
	} else {
		_, _ = fmt.Fprintln(w, "Run `ghost mcp init --client goose` to fix issues.")
	}
	return healthy, nil
}
