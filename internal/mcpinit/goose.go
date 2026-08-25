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

// Goose integration (spec §4 Phase 2). Ghost installs one Agent
// Plugins-conformant package under ~/.agents/plugins/ghost/ that gives goose
// everything at once: the ghost MCP server (mcp.json), and lifecycle hooks
// bridged onto the contract-v1 entrypoint (hooks/hooks.json).
//
// Verified against primary sources (2026-08-24):
//
//   - goose auto-discovers plugins in ~/.agents/plugins/<name>/ (user scope)
//     and <project>/.agents/plugins/<name>/ (project scope); a plugin's
//     hooks/hooks.json maps Open Plugins hook events to shell commands run
//     via `sh -c`, with the event payload as JSON on stdin.
//   - goose payloads use NATIVE field names — `event` and `working_dir` — not
//     the shared dialect's hook_event_name/cwd. Rather than ship a shim
//     script, hostevent.Parse aliases those names in core for --source goose,
//     so the hook command is `<ghost> hook <event> --source goose` directly:
//     cross-platform, nothing to interpret or repair.
//   - Stop emits a non-blocking {"decision":"approve","reason":…} reminder on
//     stdout (the same protocol as Claude Code) so goose never surfaces it as a
//     Stop hook error; SessionStart output is not injected by goose, which
//     RunHostEvent already gates on InjectContext=false for this source.
//   - Agent Plugins Spec v1.0.0 packaging: plugin.json requires exactly
//     $schema + name; mcp.json requires $schema + mcpServers with an explicit
//     "type" on every server. ${PLUGIN_ROOT} expansion exists for referencing
//     files bundled inside the package — ghost bundles no scripts (commands
//     invoke the resolved absolute ghost binary), so no placeholder appears.
//
// Like RunOpencode this never edits goose's own config (~/.config/goose/
// settings.json): uninstalling means deleting ~/.agents/plugins/ghost/, and
// every artifact is byte-compared on re-run so drift is repaired in place.

const (
	goosePluginName        = "ghost"
	gooseAgentPluginsVer   = "1.0.0"
	gooseManifestSchemaURL = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	gooseMCPSchemaURL      = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
)

// goosePluginManifest is the closed Agent Plugins manifest (§5). Only $schema
// and name are required; version/description are permitted metadata fields.
// The name must satisfy §5.5 (lowercase alphanumerics, hyphens, dots) —
// "ghost" conforms.
type goosePluginManifest struct {
	Schema      string `json:"$schema"`
	Name        string `json:"name"`
	Version     string `json:"version,omitempty"`
	Description string `json:"description,omitempty"`
}

// gooseStdioServer is one stdio server entry of mcp.json (§7.2.1). The spec
// requires an explicit type on every entry (unlike Claude Code's .mcp.json,
// which infers stdio) and resolves command as a single executable token — so
// the resolved absolute ghost binary path is baked in here, mirroring how the
// opencode plugin bakes GHOST_BIN_DEFAULT: desktop launchers may start goose
// with a narrower PATH than the shell that ran init.
type gooseStdioServer struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

// gooseMCPConfig is the closed mcp.json document (§7.2.1): required $schema +
// mcpServers, nothing else at the top level.
type gooseMCPConfig struct {
	Schema     string                      `json:"$schema"`
	MCPServers map[string]gooseStdioServer `json:"mcpServers"`
}

// gooseHookAction is one action inside a hooks.json rule (Open Plugins hooks
// shape, as implemented by goose). timeout is omitted — the 30s default
// comfortably covers ghost's spawn-and-return handlers.
type gooseHookAction struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// gooseHookRule omits matcher deliberately: lifecycle events have no matcher
// target, so each rule must fire for every fire of its event.
type gooseHookRule struct {
	Hooks []gooseHookAction `json:"hooks"`
}

// gooseHooksConfig is the top-level hooks object of hooks/hooks.json, keyed
// by the three v1 lifecycle events goose emits.
type gooseHooksConfig struct {
	Hooks map[string][]gooseHookRule `json:"hooks"`
}

// renderGoosePluginManifest returns plugin.json as it must exist on disk.
// Static content — byte-stable across runs by construction.
func renderGoosePluginManifest() string {
	return marshalGooseJSON(goosePluginManifest{
		Schema:      gooseManifestSchemaURL,
		Name:        goosePluginName,
		Version:     "0.1.0",
		Description: "Persistent cross-session memory for AI coding agents: MCP tools plus session lifecycle hooks.",
	})
}

// renderGooseMCPConfig returns mcp.json rendered with the resolved absolute
// ghost binary path.
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

// renderGooseHooksConfig returns hooks/hooks.json rendered with the resolved
// absolute ghost binary path. All three v1 events wire to the contract
// entrypoint with --source goose; hostevent.Parse handles goose's native
// payload field names in core (see applyGooseAliases), so no shim script sits
// between goose and ghost.
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

// marshalGooseJSON renders a config document deterministically: two-space
// indent, struct field order, sorted map keys, trailing newline. Byte-stable
// rendering is what makes install idempotency and status's byte-compare work.
func marshalGooseJSON(v any) string {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		// All render inputs are strings and structs of strings; failure is
		// impossible today, but a broken template must fail loudly rather
		// than install empty files.
		panic(fmt.Sprintf("render goose config: %v", err))
	}
	return string(data) + "\n"
}

// goosePluginDir returns the user-scope plugin package directory goose
// auto-discovers: ~/.agents/plugins/ghost/. Deliberately not XDG-aware —
// the Open Plugins discovery path is home-relative by specification.
func goosePluginDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".agents", "plugins", goosePluginName), nil
}

// RunGoose installs Ghost's goose integration: an Agent Plugins-conformant
// package under ~/.agents/plugins/ghost/ whose plugin.json satisfies the v1.0.0
// manifest schema, whose mcp.json registers the ghost stdio MCP server with
// the resolved absolute binary baked in, and whose hooks/hooks.json bridges
// SessionStart/Stop/SessionEnd onto `ghost hook <event> --source goose`. It
// never touches goose's own settings file.
func RunGoose(w io.Writer, dryRun bool) error {
	if dryRun {
		_, _ = fmt.Fprintf(w, "\nDry run — showing what would change:\n\n")
	}

	// Step 1: Prerequisites — only the ghost binary is required; its resolved
	// path is baked into the installed package.
	_, _ = fmt.Fprintln(w, "[1/3] Checking prerequisites...")
	ghostBin, _, err := checkPrereqs(w, "goose")
	if err != nil {
		return retryHint(err)
	}

	// Step 2: Ghost's own user config (not goose's settings).
	_, _ = fmt.Fprintln(w, "\n[2/3] Ensuring ghost config file...")
	if err := ensureConfigBootstrap(w, dryRun); err != nil {
		return retryHint(err)
	}

	// Step 3: The Agent Plugins package — MCP registration + lifecycle hooks.
	_, _ = fmt.Fprintln(w, "\n[3/3] Installing goose plugin package...")
	changed, err := installGoosePackage(w, ghostBin, dryRun)
	if err != nil {
		return retryHint(err)
	}
	if changed && !dryRun {
		_, _ = fmt.Fprintln(w, "Restart goose to activate.")
	}

	// Ollama embedding model.
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

// goosePackageFile describes one file of the installed plugin package.
type goosePackageFile struct {
	label string // human-readable name for step output
	rel   string // path relative to the plugin root
	want  string // exact bytes that must exist on disk
}

// goosePackageFiles renders every file of the package for this machine.
func goosePackageFiles(ghostBin string) []goosePackageFile {
	return []goosePackageFile{
		{label: "plugin.json", rel: "plugin.json", want: renderGoosePluginManifest()},
		{label: "mcp.json", rel: "mcp.json", want: renderGooseMCPConfig(ghostBin)},
		{label: "hooks/hooks.json", rel: filepath.Join("hooks", "hooks.json"), want: renderGooseHooksConfig(ghostBin)},
	}
}

// installGoosePackage writes the plugin package to ~/.agents/plugins/ghost/,
// rendered with the resolved ghost binary path. Idempotent: identical files
// are left untouched; missing or drifted files are overwritten with the
// rendered content (byte-compare, same semantics as installOpencodePlugin).
func installGoosePackage(w io.Writer, ghostBin string, dryRun bool) (bool, error) {
	dir, err := goosePluginDir()
	if err != nil {
		return false, err
	}

	changed := false
	for _, f := range goosePackageFiles(ghostBin) {
		path := filepath.Join(dir, f.rel)
		if existing, rerr := os.ReadFile(path); rerr == nil && string(existing) == f.want {
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
		if err := os.WriteFile(path, []byte(f.want), 0644); err != nil {
			return changed, fmt.Errorf("write %s: %w", f.label, err)
		}
		_, _ = fmt.Fprintf(w, "  + installed %s (%s)\n", f.label, path)
		changed = true
	}
	return changed, nil
}

// StatusGoose checks the health of the Ghost ↔ goose integration. It reports
// only goose-relevant checks — the ghost binary, the plugin package under
// ~/.agents/plugins/ghost/ (which is the whole integration: MCP registration
// plus lifecycle hooks), and the client-agnostic embedding/link stats tail
// shared with Status and StatusOpencode. Claude-only checks (hooks,
// permissions, autoMemory, redirects) are never reported here.
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

	// 1. Ghost binary.
	ghostBin := findBinary("ghost")
	check(ghostBin != "",
		fmt.Sprintf("ghost binary: %s", ghostBin),
		"ghost binary not found in PATH")

	reportConfigFile(w)

	// 2. Plugin package — it registers the ghost MCP server (mcp.json) and
	// bridges lifecycle events (hooks/hooks.json); without it goose has
	// neither tools nor reflection/resolve/supersede. Compared byte-for-byte
	// against the content rendered with the resolved ghost binary path: a
	// corrupted or hand-mangled file must not read healthy, and `ghost mcp
	// init --client goose` repairs any drift in place.
	dir, derr := goosePluginDir()
	switch {
	case derr != nil:
		check(false, "", fmt.Sprintf("goose plugin package: %v", derr))
	default:
		allCurrent := true
		for _, f := range goosePackageFiles(ghostBin) {
			data, rerr := os.ReadFile(filepath.Join(dir, f.rel))
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

	// 3. Embedding & linking health — silent embed failures leave vector
	// search and memory linking inactive.
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
