package mcpinit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wcatz/ghost/internal/claudeimport"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/embedding"
	"github.com/wcatz/ghost/internal/memory"
)

// Status checks the health of the Ghost ↔ Claude Code integration.
func Status(w io.Writer) error {
	_, _ = fmt.Fprintf(w, "\nGhost ↔ Claude Code integration status:\n\n")

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
	ghostBin, err := exec.LookPath("ghost")
	check(err == nil,
		fmt.Sprintf("ghost binary: %s", ghostBin),
		"ghost binary not found in PATH")

	// 2. Claude CLI.
	claudeBin, err := exec.LookPath("claude")
	check(err == nil,
		fmt.Sprintf("claude CLI: %s", claudeBin),
		"claude CLI not found in PATH")

	// 3. MCP server registration.
	if claudeBin != "" {
		out, err := exec.Command(claudeBin, "mcp", "get", "ghost").CombinedOutput()
		registered := err == nil && strings.Contains(string(out), "Command:")
		check(registered, "MCP server registered", "MCP server not registered")
	}

	// 4-6. Settings: permissions, hook, autoMemoryEnabled.
	path, err := settingsPath()
	if err == nil {
		sf, err := loadSettings(path)
		if err == nil {
			// Permissions.
			existing, _ := sf.getPermissions()
			set := make(map[string]bool, len(existing))
			for _, p := range existing {
				set[p] = true
			}
			var present int
			for _, p := range ghostPermissions {
				if set[p] {
					present++
				}
			}
			check(present == len(ghostPermissions),
				fmt.Sprintf("permissions: %d/%d", present, len(ghostPermissions)),
				fmt.Sprintf("permissions: %d/%d (run ghost mcp init)", present, len(ghostPermissions)))

			// Hook.
			hasHk := sf.hasHook("SessionStart", "ghost hook session-start")
			check(hasHk, "SessionStart hook configured", "SessionStart hook missing")

			hasStop := sf.hasHook("Stop", "hook stop")
			check(hasStop, "Stop hook configured", "Stop hook missing")

			// autoMemoryEnabled must be false to prevent competing file-memory.
			autoMemVal, autoMemSet := sf.getAutoMemoryEnabled()
			autoMemOff := autoMemSet && !autoMemVal
			check(autoMemOff,
				"autoMemoryEnabled: false (built-in file-memory disabled)",
				"autoMemoryEnabled not set to false — run ghost mcp init")
		} else {
			_, _ = fmt.Fprintf(w, "  ✗ cannot read settings: %v\n", err)
			healthy = false
		}
	}

	// 7. Project redirects.
	dataDir, err := config.DataDir()
	if err == nil {
		dbPath := filepath.Join(dataDir, "ghost.db")
		if _, err := os.Stat(dbPath); err == nil {
			db, err := memory.OpenDB(dbPath)
			if err == nil {
				defer db.Close() //nolint:errcheck
				logger := slog.New(slog.NewTextHandler(io.Discard, nil))
				store := memory.NewStore(db, logger)
				projects, err := store.ListProjects(context.Background())
				if err == nil {
					home, _ := os.UserHomeDir()
					var total, redirected int
					for _, p := range projects {
						if !filepath.IsAbs(p.Path) {
							continue
						}
						total++
						encoded := claudeimport.EncodeProjectPath(p.Path)
						target := filepath.Join(home, ".claude", "projects", encoded, "memory", "MEMORY.md")
						if data, err := os.ReadFile(target); err == nil && strings.Contains(string(data), "stored in Ghost") {
							redirected++
						}
					}
					check(redirected == total,
						fmt.Sprintf("project redirects: %d/%d", redirected, total),
						fmt.Sprintf("project redirects: %d/%d", redirected, total))
				}

				// 8. Embedding & linking health — silent embed failures
				// leave vector search and memory linking inactive.
				if cfg, cfgErr := config.Load(); cfgErr == nil {
					if !cfg.Embedding.Enabled {
						_, _ = fmt.Fprintln(w, "  - embedding disabled in config (FTS-only search)")
					} else {
						client := embedding.NewClient(cfg.Embedding.OllamaURL, cfg.Embedding.Model, cfg.Embedding.Dimensions)
						ctx := context.Background()
						if !client.Alive(ctx) {
							check(false, "", fmt.Sprintf("Ollama unreachable at %s — embeddings paused", cfg.Embedding.OllamaURL))
						} else {
							present, mErr := client.HasModel(ctx)
							check(mErr == nil && present,
								fmt.Sprintf("Ollama model %s installed", cfg.Embedding.Model),
								fmt.Sprintf("Ollama model %s missing — run: ollama pull %s", cfg.Embedding.Model, cfg.Embedding.Model))
						}
						if embedded, total, sErr := store.EmbeddingStats(ctx); sErr == nil {
							check(total == 0 || embedded > 0,
								fmt.Sprintf("embeddings: %d/%d memories", embedded, total),
								fmt.Sprintf("embeddings: %d/%d memories — vector search and linking inactive", embedded, total))
						}
						if links, scans, lErr := store.LinkStats(ctx); lErr == nil {
							_, _ = fmt.Fprintf(w, "  - memory links: %d links, %d memories scanned\n", links, scans)
						}
					}
				}
			}
		} else {
			_, _ = fmt.Fprintln(w, "  - no Ghost database (run ghost first)")
		}
	}

	fmt.Println()
	if healthy {
		_, _ = fmt.Fprintln(w, "All checks passed.")
	} else {
		_, _ = fmt.Fprintln(w, "Run `ghost mcp init` to fix issues.")
	}
	return nil
}
