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

	// A plugin-managed integration does not go through `claude mcp get`: its
	// MCP server, hooks, and permissions are declarative in the plugin, and its
	// bundled binary lives in the plugin cache. Report that, rather than the
	// standalone registration checks below, which would all read as failures.
	if PluginInstalled() {
		_, _ = fmt.Fprintln(w, "  ✓ integration managed by the ghost Claude Code plugin")
		_, _ = fmt.Fprintln(w, "    registration, hooks, and permissions are declarative — update with `/plugin update` in Claude Code")
		return true, nil
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
	pluginOK := false
	if perr != nil {
		check(false, "", fmt.Sprintf("lifecycle plugin: %v", perr))
	} else if data, rerr := os.ReadFile(pluginPath); rerr == nil && string(data) == renderOpencodeGhostPlugin(findBinary("ghost")) {
		pluginOK = true
		check(true, fmt.Sprintf("lifecycle plugin installed: %s", pluginPath), "")
	} else {
		check(false, "", "lifecycle plugin missing or outdated (run ghost mcp init --client opencode)")
	}

	// 3. MCP server registration — the mcp.ghost entry across the config
	// layers opencode reads (the file in its config directory —
	// $OPENCODE_CONFIG_DIR when set — then $OPENCODE_CONFIG, then inline
	// $OPENCODE_CONFIG_CONTENT). Health weight follows check 2: the current
	// plugin registers ghost at startup (config hook in V1,
	// ctx.mcp.transform in V2) and overrides whatever the file says, so with
	// the plugin in place a broken or absent entry is inert — reported as
	// the fallback it is, never failed, or every documented plugin-only
	// install (init writes no config file) would exit red on a working
	// integration. When the plugin is missing the file is the only
	// registration surface, and a missing, disabled or wrong-path entry
	// fails the run with the edit that repairs it — validated against the
	// resolved binary the way StatusCodex validates config.toml.
	//
	// A layer that could not be read or parsed is neither: the plugin's
	// registration says nothing about an entry status could not judge, and
	// opencode drops such a layer outright, so it fails the run under both
	// gates and the footer points at the file rather than at the entry.
	mcpOK, mcpFail, unjudgeable := opencodeMCPEntryStatus(ghostBin)
	mcpFix := "" // footer line naming how to repair what failed above
	switch {
	case mcpOK:
		check(true, "ghost MCP server registered in opencode config", "")
	case unjudgeable != "":
		// Nothing about the registration can be asserted, so the run is red
		// under both gates — but the remediation depends on why. A config
		// directory that cannot be resolved has no file to repair and the
		// plugin check above already reports that root cause; only a layer
		// that exists and cannot be read or parsed names a file.
		if unjudgeable == opencodeMCPNoConfigDir {
			check(false, "", mcpFail)
			break
		}
		mcpFix = "The opencode config file shown above could not be read or parsed — repair it in place (`ghost mcp init` never writes that file)."
		check(false, "", mcpFail)
	case pluginOK:
		_, _ = fmt.Fprintf(w, "  - %s (the lifecycle plugin registers ghost at startup; the file entry is only the fallback)\n", mcpFail)
	default:
		mcpFix = "The mcp.ghost entry shown above is a config edit — apply it to opencode's config file (`ghost mcp init` never writes that file)."
		check(false, "", mcpFail)
	}

	// 4. Embedding & linking health — silent embed failures leave vector
	// search and memory linking inactive.
	store := checkStoreHealth(w, check)
	if store != nil {
		defer store.Close() //nolint:errcheck
	}

	_, _ = fmt.Fprintln(w)
	if healthy {
		_, _ = fmt.Fprintln(w, "All checks passed.")
	} else {
		// One remediation per failure, never two adjacent ones that
		// contradict: "run ghost mcp init" is the wrong advice for a
		// config repair, because init never writes that file.
		if mcpFix != "" {
			_, _ = fmt.Fprintln(w, mcpFix)
		} else {
			_, _ = fmt.Fprintln(w, "Run `ghost mcp init --client opencode` to fix issues.")
		}
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
	if _, statErr := os.Stat(opencodeDirOrEmpty()); statErr == nil {
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

	// codex: hooks are silently skipped until the user's one-time /hooks
	// trust review, and a stale MCP entry fails invisibly — hint when a
	// ~/.codex dir exists but wiring is missing or drifted.
	if codexDir, err := codexHomeDir(); err == nil {
		if _, statErr := os.Stat(codexDir); statErr == nil {
			bin := findBinary("ghost")
			mcpOK, _ := codexMCPEntryStatus(bin)
			wired := false
			if rules, rerr := loadCodexHooksRules(); rerr == nil {
				wired = true
				for _, ev := range codexLifecycleEvents {
					if !codexContractHookWired(rules[ev.Key], ev.EventToken, "codex") {
						wired = false
						break
					}
				}
			}
			if !mcpOK || !wired {
				hints = append(hints, "codex integration missing or miswired — run `ghost mcp init --client codex` (then approve the entries via /hooks)")
			}
		}
	}

	// goose: the plugin package under ~/.agents/plugins/ghost/ is the whole
	// integration — hint on any drift when an .agents/plugins dir exists.
	if pluginDir, err := goosePluginDir(); err == nil {
		if _, statErr := os.Stat(filepath.Dir(filepath.Dir(pluginDir))); statErr == nil {
			bin := findBinary("ghost")
			stale := false
			for _, f := range goosePackageFiles(bin) {
				if data, rerr := os.ReadFile(filepath.Join(pluginDir, f.rel)); rerr != nil || string(data) != f.want {
					stale = true
					break
				}
			}
			if stale {
				hints = append(hints, "goose plugin package missing or outdated — run `ghost mcp init --client goose` to reinstall it")
			}
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

// opencodeDirOrEmpty returns the config directory opencode itself resolves
// ($OPENCODE_CONFIG_DIR, else $XDG_CONFIG_HOME/opencode, else
// ~/.config/opencode), or "" when it cannot be resolved.
func opencodeDirOrEmpty() string {
	dir, err := opencodeConfigDir()
	if err != nil {
		return ""
	}
	return dir
}

// reportConfigFile prints the user config file's location, and any problem with
// reading it. It never fails the health check — the config file is optional,
// since compiled defaults work without one — so it deliberately doesn't take a
// check closure; the `!` line it prints for a broken file is the pointer, not a
// verdict. The rest of the status run continues on the environment plus the
// compiled defaults (see checkStoreHealth), so a broken file costs the user no
// other check.
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
	if _, err := config.Load(); err != nil {
		_, _ = fmt.Fprintf(w, "  ! config: %v (environment and built-in defaults in use)\n", err)
	}
}

// checkEmbeddingStats reports embedding coverage via the check closure. An
// empty store is healthy — total == 0 passes with "(store empty)"; only a
// non-empty store with no embedded memories means vector search and linking
// are inactive. Partial coverage is reported as such rather than as a bare
// fraction: with an identity configured, the gap is rows waiting to be
// re-embedded after a model, dimension or task-prefix change, and they are
// excluded from the vector leg until they are.
//
// That gap has two halves with different diagnoses — stale rows (a vector
// exists, under a retired identity: the re-embed worker has the work queued)
// and unembedded rows (no vector has ever existed: nothing has run, or
// embedding was only just enabled) — so when any of the gap is stale it is
// named. When nothing is stale the ordinary "(N awaiting re-embed)" line
// stands: "0 stale" beside the ordinary first-embed gap would read as a
// diagnosis that does not apply. Shared by Status and StatusOpencode.
func checkEmbeddingStats(check func(ok bool, pass, fail string), embedded, stale, total int) {
	if total == 0 {
		check(true, "embeddings: 0 memories (store empty)", "")
		return
	}
	detail := ""
	if gap := total - embedded; gap > 0 && stale > 0 {
		detail = fmt.Sprintf(": %d stale, %d unembedded", stale, gap-stale)
	}
	pass := fmt.Sprintf("embeddings: %d/%d memories", embedded, total)
	if embedded > 0 && embedded < total {
		pass = fmt.Sprintf("embeddings: %d/%d memories (%d awaiting re-embed%s)", embedded, total, total-embedded, detail)
	}
	check(embedded > 0, pass,
		fmt.Sprintf("embeddings: %d/%d memories — vector search and linking inactive%s", embedded, total, detail))
}

// checkStoreHealth runs the database, Ollama, embedding, and linking health
// checks shared by Status and StatusOpencode. The Ollama reachability and
// model checks run even when no database exists yet, so a fresh install still
// reports embedding health. It returns the opened store (or nil) so callers
// can run additional store-dependent checks; the caller must Close a non-nil
// store.
//
// The config is read once, through LoadForHook, and a file that does not parse
// must not cost the user these checks. reportConfigFile has already reported
// the parse error, and gating on config.Load here would drop the Ollama,
// embedding-coverage and link lines on exactly the run where a user needs them:
// a broken config silently disabling vector search is the thing this command
// exists to explain. On the fallback these checks still run and still
// report the truth about the store in front of them.
func checkStoreHealth(w io.Writer, check func(ok bool, pass, fail string)) *memory.Store {
	// 8. Embedding & linking health — silent embed failures leave vector
	// search and memory linking inactive. Ollama checks run regardless of
	// whether a database exists.
	cfg := config.LoadForHook()
	// reportOllamaDownDuration only fires when embedding is enabled AND
	// checkOllama's live probe just found Ollama unreachable — see that
	// function's doc comment for why a stale marker must never be printed
	// next to a currently-passing Ollama check.
	if alive := checkOllama(w, cfg, check); cfg.Embedding.Enabled && !alive {
		// A refused data dir must not have had a marker written into it before
		// the report says so (#721), so this resolves the directory the same way
		// the store open below does — and its `== nil` branch is the same skip.
		if dataDir, ddErr := config.DataDir(); ddErr == nil {
			reportOllamaDownDuration(w, dataDir)
		}
	}

	// A GHOST_DEV_FORBID_DATA_DIR refusal arrives from here, before the
	// read-write open below can migrate anything (#721). A status run that must
	// not open the store has something to report — that, and why — so this is a
	// failed check and not a silent nil: a run that exited non-zero having
	// printed nothing would be a run that diagnosed nothing.
	dataDir, err := config.DataDir()
	if err != nil {
		check(false, "", fmt.Sprintf("database: %v", err))
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
	// Same identity the MCP server embeds with, so the coverage line below
	// counts vectors that can actually be searched. Without it, a store mid
	// re-embed reports full coverage while the vector leg returns nothing —
	// which is exactly the state an operator runs `ghost mcp status` to catch.
	store.SetEmbeddingIdentity(embedding.VectorIdentity(cfg.Embedding.Model, cfg.Embedding.Dimensions))
	if cfg.Embedding.Enabled {
		ctx := context.Background()
		if embedded, stale, total, sErr := store.EmbeddingStats(ctx); sErr == nil {
			checkEmbeddingStats(check, embedded, stale, total)
		}
		if links, scans, lErr := store.LinkStats(ctx); lErr == nil {
			_, _ = fmt.Fprintf(w, "  - memory links: %d links, %d memories scanned\n", links, scans)
		}
	}
	return store
}
