package mcpinit

import (
	"context"
	_ "embed"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/config"
)

// opencodeGhostPluginTS is the lifecycle adapter installed under
// <config>/opencode/plugins/. go:embed keeps the TypeScript source verbatim
// (template literals would fight a Go string constant) and makes this repo's
// copy the single source of truth for both in-repo installs and the future
// npm package (spec §4 Phase 3).
//
//go:embed opencode_ghost.ts
var opencodeGhostPluginTS string

// RunOpencode installs Ghost's opencode integration: one lifecycle plugin
// file that both registers the ghost MCP server (via the plugin config hook)
// and bridges idle events to `ghost hook stop --source opencode`. It never
// touches Claude Code's settings and never edits opencode's own config file —
// a single artifact under <config>/opencode/plugins/, so uninstalling is
// deleting one file.
func RunOpencode(w io.Writer, dryRun bool) error {
	if dryRun {
		_, _ = fmt.Fprintf(w, "\nDry run — showing what would change:\n\n")
	}

	// Step 1: Prerequisites — only the ghost binary is required.
	_, _ = fmt.Fprintln(w, "[1/3] Checking prerequisites...")
	if _, _, err := checkPrereqs(w, "opencode"); err != nil {
		return retryHint(err)
	}

	// Step 2: Ghost's own user config (not opencode's).
	_, _ = fmt.Fprintln(w, "\n[2/3] Ensuring ghost config file...")
	if err := ensureConfigBootstrap(w, dryRun); err != nil {
		return retryHint(err)
	}

	// Step 3: The lifecycle plugin — MCP registration + stop-event bridge.
	_, _ = fmt.Fprintln(w, "\n[3/3] Installing lifecycle plugin...")
	changed, err := installOpencodePlugin(w, dryRun)
	if err != nil {
		return retryHint(err)
	}
	if changed && !dryRun {
		_, _ = fmt.Fprintln(w, "Restart opencode to activate.")
		verifyOpencodeRegistration(w)
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

// opencodePluginPath resolves the installed lifecycle plugin file:
// <config>/opencode/plugins/ghost-opencode.ts (the plural "plugins" dir is
// what opencode auto-loads).
func opencodePluginPath() (string, error) {
	dir, err := opencodeConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "opencode", "plugins", "ghost-opencode.ts"), nil
}

// installOpencodePlugin writes the embedded lifecycle adapter to opencode's
// plugin directory. Idempotent: an identical file is left untouched; a
// missing, drifted, or outdated file is overwritten with the embedded source.
func installOpencodePlugin(w io.Writer, dryRun bool) (bool, error) {
	path, err := opencodePluginPath()
	if err != nil {
		return false, err
	}

	if existing, err := os.ReadFile(path); err == nil && string(existing) == opencodeGhostPluginTS {
		_, _ = fmt.Fprintf(w, "  ✓ lifecycle plugin already installed (%s)\n", path)
		return false, nil
	}

	if dryRun {
		_, _ = fmt.Fprintf(w, "  ~ would install lifecycle plugin (%s)\n", path)
		return true, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, fmt.Errorf("create plugin dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(opencodeGhostPluginTS), 0644); err != nil {
		return false, fmt.Errorf("write plugin: %w", err)
	}
	_, _ = fmt.Fprintf(w, "  + installed lifecycle plugin (%s)\n", path)
	return true, nil
}

// verifyOpencodeRegistration checks via `opencode mcp ls` that the ghost
// entry took effect. When the opencode CLI is absent it stays silent — the
// plugin is installed either way and will register on next start.
func verifyOpencodeRegistration(w io.Writer) {
	ocBin, err := exec.LookPath("opencode")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ocBin, "mcp", "ls").CombinedOutput()
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! could not verify registration (`opencode mcp ls` failed): %s\n", strings.TrimSpace(string(out)))
		return
	}
	if strings.Contains(string(out), "ghost") {
		_, _ = fmt.Fprintln(w, "  ✓ verified: ghost listed by `opencode mcp ls`")
	} else {
		_, _ = fmt.Fprintln(w, "  ! `opencode mcp ls` succeeded but ghost is not listed — restart opencode, or re-run `ghost mcp init --client opencode`")
	}
}

// opencodeConfigDir returns $XDG_CONFIG_HOME when set, else ~/.config.
func opencodeConfigDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return xdg, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("home dir: %w", err)
	}
	return filepath.Join(home, ".config"), nil
}
