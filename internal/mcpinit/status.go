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
	"github.com/wcatz/ghost/internal/memory"
)

// Status checks the health of the Ghost ↔ Claude Code integration. The
// returned healthy bool reflects whether every check passed; err is reserved
// for actual failures (I/O, config parse) that prevented the checks from
// running at all.
func Status(w io.Writer) (bool, error) {
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
	ghostBin := findBinary("ghost")
	check(ghostBin != "",
		fmt.Sprintf("ghost binary: %s", ghostBin),
		"ghost binary not found in PATH")

	// 2. Claude CLI.
	claudeBin := findBinary("claude")
	check(claudeBin != "",
		fmt.Sprintf("claude CLI: %s", claudeBin),
		"claude CLI not found in PATH")

	reportConfigFile(w)

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

	// 7. Project redirects plus the shared store health (database, Ollama,
	// embedding, linking). checkStoreHealth returns the open store so the
	// Claude-specific redirect check can reuse it.
	store := checkStoreHealth(w, check)
	if store != nil {
		defer store.Close() //nolint:errcheck
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
	}

	fmt.Println()
	if healthy {
		_, _ = fmt.Fprintln(w, "All checks passed.")
	} else {
		_, _ = fmt.Fprintln(w, "Run `ghost mcp init` to fix issues.")
	}
	return healthy, nil
}

// StatusOpencode checks the health of the Ghost ↔ opencode integration.
// Unlike Status, it reports only opencode-relevant checks — the ghost binary,
// the mcp.ghost entry in the opencode config, Ollama, and the client-agnostic
// embedding/link stats. Claude-only checks (hooks, permissions, autoMemory,
// redirects) are never reported here, so a clean opencode setup prints
// "All checks passed." without the Claude CLI installed.
func StatusOpencode(w io.Writer) (bool, error) {
	_, _ = fmt.Fprintf(w, "\nGhost ↔ opencode integration status:\n\n")

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

	// 2. opencode MCP config.
	path, err := opencodeConfigPath()
	if err != nil {
		check(false, "", fmt.Sprintf("opencode MCP config: %v", err))
	} else if cfg, err := loadOpencodeConfig(path); err != nil {
		check(false, "", fmt.Sprintf("opencode MCP config: %v", err))
	} else {
		_, registered := mcpGhostCommand(cfg)
		check(registered,
			"opencode MCP config: ghost registered",
			"opencode MCP config: ghost missing or wrong command")
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
		_, _ = fmt.Fprintln(w, "Run `ghost mcp init --client opencode` to fix issues.")
	}
	return healthy, nil
}

// reportConfigFile prints the user config file's location informationally.
// It never fails the health check — the config file is optional, since
// compiled defaults work without one — so it deliberately doesn't take a
// check closure.
func reportConfigFile(w io.Writer) {
	path, err := config.ConfigFilePath()
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! config file: %v\n", err)
		return
	}
	if _, err := os.Stat(path); err == nil {
		_, _ = fmt.Fprintf(w, "  - config file: %s\n", path)
	} else {
		_, _ = fmt.Fprintf(w, "  - no config file (run ghost mcp init)\n")
	}
}

// checkEmbeddingStats reports embedding coverage via the check closure. An
// empty store is healthy — total == 0 passes with "(store empty)"; only a
// non-empty store with no embedded memories means vector search and linking
// are inactive. Shared by Status and StatusOpencode.
func checkEmbeddingStats(check func(ok bool, pass, fail string), embedded, total int) {
	if total == 0 {
		check(true, "embeddings: 0 memories (store empty)", "")
		return
	}
	check(embedded > 0,
		fmt.Sprintf("embeddings: %d/%d memories", embedded, total),
		fmt.Sprintf("embeddings: %d/%d memories — vector search and linking inactive", embedded, total))
}

// checkStoreHealth runs the database, Ollama, embedding, and linking health
// checks shared by Status and StatusOpencode. The Ollama reachability and
// model checks run even when no database exists yet, so a fresh install still
// reports embedding health. It returns the opened store (or nil) so callers
// can run additional store-dependent checks; the caller must Close a non-nil
// store.
func checkStoreHealth(w io.Writer, check func(ok bool, pass, fail string)) *memory.Store {
	// 8. Embedding & linking health — silent embed failures leave vector
	// search and memory linking inactive. Ollama checks run regardless of
	// whether a database exists.
	if cfg, cfgErr := config.Load(); cfgErr == nil {
		checkOllama(w, cfg, check)
	}

	dataDir, err := config.DataDir()
	if err != nil {
		return nil
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			_, _ = fmt.Fprintln(w, "  - no Ghost database (run ghost first)")
			return nil
		}
		// Permission, I/O, or other errors must fail the run rather than
		// masquerading as a fresh install.
		check(false, "", fmt.Sprintf("database: %v", err))
		return nil
	}
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		check(false, "", fmt.Sprintf("database: %v", err))
		return nil
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := memory.NewStore(db, logger)
	if cfg, cfgErr := config.Load(); cfgErr == nil && cfg.Embedding.Enabled {
		ctx := context.Background()
		if embedded, total, sErr := store.EmbeddingStats(ctx); sErr == nil {
			checkEmbeddingStats(check, embedded, total)
		}
		if links, scans, lErr := store.LinkStats(ctx); lErr == nil {
			_, _ = fmt.Fprintf(w, "  - memory links: %d links, %d memories scanned\n", links, scans)
		}
	}
	return store
}
