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

			// Hook. Token-validated, not substring-matched: the command must
			// invoke `hook <event>` with an exact --source claude-code token
			// (either flag form). A pre-contract bare invocation fails open at
			// fire time and a wrong-source or lookalike value would too — all
			// must report as missing here so the fix is actionable via
			// `ghost mcp init`.
			hasHk := contractHookWired(sf, "SessionStart", "session-start", "claude-code")
			check(hasHk, "SessionStart hook configured", "SessionStart hook missing or pre-contract (run ghost mcp init)")

			hasStop := contractHookWired(sf, "Stop", "stop", "claude-code")
			check(hasStop, "Stop hook configured", "Stop hook missing or pre-contract (run ghost mcp init)")

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

	// 2. Lifecycle plugin — it both registers the ghost MCP server (config
	// hook) and bridges idle events to the contract; without it opencode has
	// neither tools nor reflection/resolve/supersede. Compared byte-for-byte
	// against the source rendered with the resolved ghost binary path: a
	// corrupted or hand-mangled file must not read healthy, and `ghost mcp
	// init --client opencode` repairs any drift in place.
	pluginPath, perr := opencodePluginPath()
	if perr != nil {
		check(false, "", fmt.Sprintf("lifecycle plugin: %v", perr))
	} else if data, rerr := os.ReadFile(pluginPath); rerr == nil && string(data) == renderOpencodeGhostPlugin(findBinary("ghost")) {
		check(true, fmt.Sprintf("lifecycle plugin installed: %s", pluginPath), "")
	} else {
		check(false, "", "lifecycle plugin missing or outdated (run ghost mcp init --client opencode)")
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

// contractHookWired reports whether any registered hook command for event
// invokes `hook <eventToken>` with an exact --source token equal to wantSource.
// Both accepted flag forms count: "--source X" and "--source=X". Token-based,
// not substring-based — a lookalike like "--source claude-code-extra" is
// rejected by hostevent.Parse at runtime, so status must never call it wired;
// conversely the equals form works and must not read as missing. Unparseable
// commands (exotic hand-edited quoting) conservatively don't count.
func contractHookWired(sf *settingsFile, hookEvent, eventToken, wantSource string) bool {
	cmds, err := sf.hookCommands(hookEvent)
	if err != nil {
		return false
	}
	for _, cmd := range cmds {
		_, rest, ok := splitHookCommand(cmd)
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 2 || fields[0] != "hook" || fields[1] != eventToken {
			continue
		}
		for i := 2; i < len(fields); i++ {
			if fields[i] == "--source" && i+1 < len(fields) && fields[i+1] == wantSource {
				return true
			}
			if strings.HasPrefix(fields[i], "--source=") && strings.TrimPrefix(fields[i], "--source=") == wantSource {
				return true
			}
		}
	}
	return false
}

// ReportStaleIntegrations warns when existing client wiring predates the
// running binary. Pre-contract Claude hooks fail open on every fire (no
// context injection, no save-nudge, no lifecycle spawns), and a drifted
// opencode lifecycle plugin means idle events never reach ghost at all —
// neither failure is visible to the user, so `ghost upgrade` surfaces them
// instead of silently disabling a working integration. Best-effort and
// completely silent when everything is current; read-only.
func ReportStaleIntegrations(w io.Writer) {
	var hints []string

	if path, err := settingsPath(); err == nil {
		if sf, err := loadSettings(path); err == nil {
			for _, c := range []struct {
				event string
				token string
				label string
			}{
				{"SessionStart", "session-start", "Claude Code SessionStart"},
				{"Stop", "stop", "Claude Code Stop"},
			} {
				cmds, err := sf.hookCommands(c.event)
				if err != nil || len(cmds) == 0 {
					continue // integration not in use, or unreadable — leave status to `ghost mcp status`
				}
				if !contractHookWired(sf, c.event, c.token, "claude-code") {
					hints = append(hints, fmt.Sprintf("%s hook is pre-contract or miswired — run `ghost mcp init` to migrate it", c.label))
				}
			}
		}
	}

	// opencode: the plugin is the whole integration (MCP registration via its
	// config hook + stop-event bridge), so drift or absence anywhere under an
	// existing opencode config dir warrants a hint. A missing plugin with no
	// opencode dir at all means opencode isn't in use — leave it to
	// `ghost mcp status --client opencode`.
	if _, statErr := os.Stat(filepath.Join(opencodeDirOrEmpty(), "opencode")); statErr == nil {
		stale := true
		if pluginPath, perr := opencodePluginPath(); perr == nil {
			want := renderOpencodeGhostPlugin(findBinary("ghost"))
			if data, rerr := os.ReadFile(pluginPath); rerr == nil && string(data) == want {
				stale = false
			}
		}
		if stale {
			hints = append(hints, "opencode lifecycle plugin is missing or outdated — run `ghost mcp init --client opencode` to reinstall it")
		}
	}

	if len(hints) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "\nIntegration wiring check:")
	for _, h := range hints {
		_, _ = fmt.Fprintf(w, "  ! %s\n", h)
	}
}

// opencodeDirOrEmpty returns the base config dir that opencode reads
// ($XDG_CONFIG_HOME or ~/.config), or "" when it cannot be resolved.
func opencodeDirOrEmpty() string {
	dir, err := opencodeConfigDir()
	if err != nil {
		return ""
	}
	return dir
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
		// reportOllamaDownDuration only fires when embedding is enabled AND
		// checkOllama's live probe just found Ollama unreachable — see that
		// function's doc comment for why a stale marker must never be printed
		// next to a currently-passing Ollama check.
		if alive := checkOllama(w, cfg, check); cfg.Embedding.Enabled && !alive {
			if dataDir, ddErr := config.DataDir(); ddErr == nil {
				reportOllamaDownDuration(w, dataDir)
			}
		}
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
