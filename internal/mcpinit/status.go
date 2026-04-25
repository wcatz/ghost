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

// Status checks the health of the Ghost ↔ Claude Code integration.
func Status(w io.Writer) error {
	fmt.Fprintf(w, "\nGhost ↔ Claude Code integration status:\n\n")

	healthy := true
	check := func(ok bool, pass, fail string) {
		if ok {
			fmt.Fprintf(w, "  ✓ %s\n", pass)
		} else {
			fmt.Fprintf(w, "  ✗ %s\n", fail)
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

			// autoMemoryEnabled must be false to prevent competing file-memory.
			autoMemVal, autoMemSet := sf.getAutoMemoryEnabled()
			autoMemOff := autoMemSet && !autoMemVal
			check(autoMemOff,
				"autoMemoryEnabled: false (built-in file-memory disabled)",
				"autoMemoryEnabled not set to false — run ghost mcp init")
		} else {
			fmt.Fprintf(w, "  ✗ cannot read settings: %v\n", err)
			healthy = false
		}
	}

	// 6. Project redirects.
	dataDir, err := config.DataDir()
	if err == nil {
		dbPath := filepath.Join(dataDir, "ghost.db")
		if _, err := os.Stat(dbPath); err == nil {
			db, err := memory.OpenDB(dbPath)
			if err == nil {
				defer db.Close()
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
			}
		} else {
			fmt.Fprintln(w, "  - no Ghost database (run ghost first)")
		}
	}

	fmt.Println()
	if healthy {
		fmt.Fprintln(w, "All checks passed.")
	} else {
		fmt.Fprintln(w, "Run `ghost mcp init` to fix issues.")
	}
	return nil
}
