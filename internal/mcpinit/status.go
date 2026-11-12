package mcpinit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/claudeimport"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/embedding"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// Status checks the health of the Ghost ↔ Claude Code integration. The
// returned healthy bool reflects whether every check passed; err is reserved
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

	if PluginInstalled() {
		_, _ = fmt.Fprintln(w, "  ✓ integration managed by the ghost Claude Code plugin")
		_, _ = fmt.Fprintln(w, "    registration, hooks, and permissions are declarative — update with `/plugin update` in Claude Code")
		return true, nil
	}

	ghostBin := findGhostBinary()
	check(ghostBin != "",
		fmt.Sprintf("ghost binary: %s", ghostBin),
		"ghost binary not found in PATH")

	claudeBin := findBinary("claude")
	check(claudeBin != "",
		fmt.Sprintf("claude CLI: %s", claudeBin),
		"claude CLI not found in PATH")

	reportConfigFile(w)

	if claudeBin != "" {
		registered, why := claudeMCPEntryStatus(claudeBin, ghostBin)
		check(registered, "MCP server registered", why)
	}

	path, err := settingsPath()
	if err == nil {
		sf, err := loadSettings(path)
		if err == nil {
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

			hasHk := contractHookWired(sf, "SessionStart", "session-start", "claude-code", ghostBin)
			check(hasHk, "SessionStart hook configured", "SessionStart hook missing or pre-contract (run ghost mcp init)")

			hasStop := contractHookWired(sf, "Stop", "stop", "claude-code", ghostBin)
			check(hasStop, "Stop hook configured", "Stop hook missing or pre-contract (run ghost mcp init)")

			for _, c := range []struct{ event, token string }{{"UserPromptSubmit", "message-submit"}, {"PostToolUse", "edit"}} {
				if contractHookWired(sf, c.event, c.token, "claude-code", ghostBin) {
					_, _ = fmt.Fprintf(w, "  ✓ %s hook configured (working-moment context)\n", c.event)
				} else {
					_, _ = fmt.Fprintf(w, "  - %s hook not configured (optional; run ghost mcp init for working-moment context)\n", c.event)
				}
			}

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
				if data, err := readHostConfigFile(target); err == nil && strings.Contains(string(data), "stored in Ghost") {
					redirected++
				} else if errors.Is(err, errNotRegularFile) {
					_, _ = fmt.Fprintf(w, "  ! %s \u2014 %v\n", assemble.Label(p.Name), err)
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

func claudeMCPEntryStatus(claudeBin, ghostBin string) (bool, string) {
	out, err := exec.Command(claudeBin, "mcp", "get", "ghost").CombinedOutput()
	if err != nil {
		return false, "MCP server not registered"
	}
	present, current, why := claudeMCPVerdict(string(out), ghostBin)
	if !present {
		return false, "MCP server not registered"
	}
	if current {
		return true, ""
	}
	return false, why + " — " + claudeMCPRepair
}

func claudeMCPVerdict(out, ghostBin string) (present, current bool, why string) {
	reg, described := parseClaudeRegistration(out)
	command, _ := reg.config["command"].(string)
	if command == "" {
		if !described {
			return false, false, ""
		}
		return true, false, "MCP server registered, but it does not run the ghost binary"
	}
	resolved, rerr := exec.LookPath(command)
	if rerr != nil {
		return true, false, fmt.Sprintf("MCP server registered for a command that is not there (%s)", claudeMCPCommandLabel(command))
	}
	if !mcpCommandIsGhost(resolved, ghostBin) {
		return true, false, fmt.Sprintf("MCP server registered for a different binary (%s)", claudeMCPCommandLabel(command))
	}
	args, _ := reg.config["args"].([]string)
	if len(args) != 1 || args[0] != "mcp" {
		return true, false, fmt.Sprintf("MCP server registered with the wrong arguments (%s), expected \"mcp\"",
			claudeMCPArgsLabel(args))
	}
	return true, true, ""
}

const claudeMCPRepair = "run ghost mcp init --client claude"

func claudeMCPCommandLabel(command string) string {
	return memory.RefusalArg("command", command)
}

func claudeMCPArgsLabel(args []string) string {
	if len(args) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(args))
	for i, arg := range args {
		parts = append(parts, memory.RefusalArg(fmt.Sprintf("argument[%d]", i), arg))
	}
	return strings.Join(parts, " ")
}

func mcpCommandIsGhost(resolved, ghostBin string) bool {
	if ghostBin == "" {
		base := filepath.Base(resolved)
		return strings.EqualFold(strings.TrimSuffix(base, filepath.Ext(base)), "ghost")
	}
	want, err := os.Stat(ghostBin)
	if err != nil {
		return false
	}
	got, err := os.Stat(resolved)
	if err != nil {
		return false
	}
	return os.SameFile(got, want)
}

// StatusOpencode checks the health of the Ghost ↔ opencode integration.
// Unlike Status, it reports only opencode-relevant checks — the ghost binary,
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

	ghostBin := findGhostBinary()
	check(ghostBin != "",
		fmt.Sprintf("ghost binary: %s", ghostBin),
		"ghost binary not found in PATH")

	reportConfigFile(w)

	pluginPath, perr := opencodePluginPath()
	pluginOK := false
	if perr != nil {
		check(false, "", fmt.Sprintf("lifecycle plugin: %v", perr))
	} else if data, rerr := readHostConfigFile(pluginPath); rerr == nil && string(data) == renderOpencodeGhostPlugin(findGhostBinary()) {
		pluginOK = true
		check(true, fmt.Sprintf("lifecycle plugin installed: %s", pluginPath), "")
	} else if errors.Is(rerr, errNotRegularFile) {
		check(false, "", fmt.Sprintf("lifecycle plugin unreadable: %v", rerr))
	} else {
		check(false, "", "lifecycle plugin missing or outdated (run ghost mcp init --client opencode)")
	}

	mcpOK, mcpFail, unjudgeable := opencodeMCPEntryStatus(ghostBin)
	mcpFix := "" // footer line naming how to repair what failed above
	switch {
	case mcpOK:
		check(true, "ghost MCP server registered in opencode config", "")
	case unjudgeable != "":
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

	store := checkStoreHealth(w, check)
	if store != nil {
		defer store.Close() //nolint:errcheck
	}

	_, _ = fmt.Fprintln(w)
	if healthy {
		_, _ = fmt.Fprintln(w, "All checks passed.")
	} else {
		if mcpFix != "" {
			_, _ = fmt.Fprintln(w, mcpFix)
		} else {
			_, _ = fmt.Fprintln(w, "Run `ghost mcp init --client opencode` to fix issues.")
		}
	}
	return healthy, nil
}

func contractHookWired(sf *settingsFile, hookEvent, eventToken, wantSource, ghostBin string) bool {
	cmds, err := sf.hookCommands(hookEvent)
	if err != nil {
		return false
	}
	for _, cmd := range cmds {
		tail, ok := ghostHookTail(cmd, eventToken, ghostBin)
		if !ok {
			continue
		}
		for i := 0; i < len(tail); i++ {
			if tail[i] == "--source" && i+1 < len(tail) && tail[i+1] == wantSource {
				return true
			}
			if strings.HasPrefix(tail[i], "--source=") && strings.TrimPrefix(tail[i], "--source=") == wantSource {
				return true
			}
		}
	}
	return false
}

// ReportStaleIntegrations warns when existing client wiring predates the
// running binary. Pre-contract Claude hooks fail open on every fire (no
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
				if !contractHookWired(sf, c.event, c.token, "claude-code", findGhostBinary()) {
					hints = append(hints, fmt.Sprintf("%s hook is pre-contract or miswired — run `ghost mcp init` to migrate it", c.label))
				}
			}
		}
	}

	if _, statErr := os.Stat(opencodeDirOrEmpty()); statErr == nil {
		stale := true
		if pluginPath, perr := opencodePluginPath(); perr == nil {
			want := renderOpencodeGhostPlugin(findGhostBinary())
			if data, rerr := readHostConfigFile(pluginPath); rerr == nil && string(data) == want {
				stale = false
			} else if errors.Is(rerr, errNotRegularFile) {
				hints = append(hints, fmt.Sprintf("opencode lifecycle plugin unreadable: %v", rerr))
				stale = false // Don't also add the "missing or outdated" hint
			}
		}
		if stale {
			hints = append(hints, "opencode lifecycle plugin is missing or outdated — run `ghost mcp init --client opencode` to reinstall it")
		}
	}

	if codexDir, err := codexHomeDir(); err == nil {
		if _, statErr := os.Stat(codexDir); statErr == nil {
			bin := findGhostBinary()
			mcpOK, _ := codexMCPEntryStatus(bin)
			wired := false
			if rules, rerr := loadCodexHooksRules(); rerr == nil {
				wired = true
				for _, ev := range codexLifecycleEvents {
					if !codexContractHookWired(rules[ev.Key], ev.EventToken, "codex", bin) {
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

	if pluginDir, err := goosePluginDir(); err == nil {
		if _, statErr := os.Stat(filepath.Dir(filepath.Dir(pluginDir))); statErr == nil {
			bin := findGhostBinary()
			stale := false
			for _, f := range goosePackageFiles(bin) {
				if data, rerr := readHostConfigFile(filepath.Join(pluginDir, f.rel)); rerr != nil || string(data) != f.want {
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

func opencodeDirOrEmpty() string {
	dir, err := opencodeConfigDir()
	if err != nil {
		return ""
	}
	return dir
}

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

func checkStoreHealth(w io.Writer, check func(ok bool, pass, fail string)) *memory.Store {
	cfg := config.LoadForHook()
	if reach := checkOllama(w, cfg, check); cfg.Embedding.Enabled && reach == embedding.Unreachable {
		if dataDir, ddErr := config.DataDirPath(); ddErr == nil {
			reportOllamaDownDuration(w, dataDir)
		}
	}

	dataDir, err := config.DataDirPath()
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
		check(false, "", fmt.Sprintf("database: %v", err))
		return nil
	}
	db, err := memory.OpenReadDB(dbPath)
	if err != nil {
		check(false, "", fmt.Sprintf("database: %v", err))
		return nil
	}
	if version, verr := memory.DBUserVersion(db); verr != nil {
		_ = db.Close()
		check(false, "", fmt.Sprintf("database: %v", verr))
		return nil
	} else if version != memory.SchemaVersion() {
		_ = db.Close()
		if version > memory.SchemaVersion() {
			check(false, "", fmt.Sprintf("database: schema v%d is newer than this ghost build (v%d) — upgrade ghost", version, memory.SchemaVersion()))
		} else {
			_, _ = fmt.Fprintf(w, "  - database: schema v%d, this Ghost reads v%d — start a session or run `ghost mcp init` to migrate it\n", version, memory.SchemaVersion())
		}
		return nil
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := memory.NewStore(db, logger)
	store.SetEmbeddingIdentity(embedding.VectorIdentity(cfg.Embedding.Model, cfg.Embedding.Dimensions))
	if cfg.Embedding.Enabled {
		ctx := context.Background()
		embedded, stale, total, sErr := store.EmbeddingStats(ctx)
		if sErr == nil {
			checkEmbeddingStats(check, embedded, stale, total)
		} else {
			_, _ = fmt.Fprintf(w, "  ! embeddings: %v\n", sErr)
		}
		links, scans, lErr := store.LinkStats(ctx)
		if lErr == nil {
			_, _ = fmt.Fprintf(w, "  - memory links: %d links, %d memories scanned\n", links, scans)
		} else {
			_, _ = fmt.Fprintf(w, "  ! memory links: %v\n", lErr)
		}
	}
	reportHistoryGrowth(w, store)
	reportGlobalConsolidation(w, store)
	reportOverdueMaintenanceStatus(w, store, dataDir)
	reportReflectionExclusions(w, store)
	reportReflectOverBound(w, store)
	return store
}

func reportReflectionExclusions(w io.Writer, store *memory.Store) {
	ctx := context.Background()
	projects, err := store.ListProjects(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! consolidation exclusion: %v\n", err)
		return
	}
	for _, p := range projects {
		st, err := store.ReflectionExclusionStats(ctx, p.ID)
		if err != nil {
			_, _ = fmt.Fprintf(w, "  ! %s (%s): consolidation exclusion could not be read: %v\n",
				maintenanceProjectLabel(p.Name), maintenanceProjectID(p.ID), err)
			continue
		}
		if st.Total == 0 {
			continue
		}
		pct := float64(st.Frozen) * 100 / float64(st.Total)
		_, _ = fmt.Fprintf(w, "  - %s (%s): %d memories — %d frozen (%.0f%% excluded from consolidation: pinned %d, persistent %d, manual %d, builtin %d, resolved %d; %d consolidatable)\n",
			maintenanceProjectLabel(p.Name), maintenanceProjectID(p.ID), st.Total, st.Frozen, pct,
			st.Pinned, st.Persistent, st.Manual, st.Builtin, st.Resolved, st.Consolidatable)
	}
}

func reportReflectOverBound(w io.Writer, store *memory.Store) {
	ctx := context.Background()
	projects, err := store.ListProjects(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! reflect: could not list projects: %v\n", err)
		return
	}
	type over struct {
		name  string
		id    string
		count int
	}
	var overs []over
	for _, p := range projects {
		n, err := store.CountConsolidatableMemories(ctx, p.ID)
		if err != nil {
			_, _ = fmt.Fprintf(w, "  ! reflect: project %s (%s) consolidatable count could not be read\n", maintenanceProjectLabel(p.Name), maintenanceProjectID(p.ID))
			continue
		}
		if n > reflection.MaxConsolidationInput {
			overs = append(overs, over{name: p.Name, id: p.ID, count: n})
		}
	}
	if len(overs) == 0 {
		return
	}
	_, _ = fmt.Fprintf(w, "  ! reflect: %d project(s) above the %d consolidatable-memory limit for a single consolidation and skipped by unattended reflect — to consolidate, bring to at most %d: `ghost resolve <project> --apply` or `ghost prune --project <project> --apply` (pinned, manual and persistent memories do not count). Then run `ghost reflect <project> --apply`.\n",
		len(overs), reflection.MaxConsolidationInput, reflection.MaxConsolidationInput)
	for _, o := range overs {
		_, _ = fmt.Fprintf(w, "    - %s (%s): %d consolidatable memories\n", maintenanceProjectLabel(o.name), maintenanceProjectID(o.id), o.count)
	}
}

func reportHistoryGrowth(w io.Writer, store *memory.Store) {
	res, err := store.HistoryGrowth(context.Background())
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! history growth: %v\n", err)
		return
	}
	if res.TotalRows == 0 {
		_, _ = fmt.Fprintln(w, "  - history: no version rows recorded yet")
		return
	}
	_, _ = fmt.Fprintf(w,
		"  - history: %d version rows in %dh, %d restatements (%.0f%%), deepest memory %d/%d versions, store %d/%d rows\n",
		res.RowsInWindow, res.WindowHours, res.NoOpRows, res.NoOpShare*100,
		res.MaxVersions, res.PerMemoryCap, res.TotalRows, res.StoreCap)
	for _, warn := range res.Warnings {
		_, _ = fmt.Fprintf(w, "  ! %s\n", warn.Detail)
	}
}

func reportGlobalConsolidation(w io.Writer, store *memory.Store) {
	all, err := store.GetAll(context.Background(), memory.GlobalProjectID, -1)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! _global: could not be read: %v\n", err)
		return
	}
	live := globalConsolidatable(all)
	clusters := reflection.PlanGlobalFold(live)
	switch {
	case len(live) == 0:
		if len(clusters) == 0 {
			_, _ = fmt.Fprintf(w, "  - _global: %d memories, 0 reflection-written consolidatable\n", len(all))
		} else {
			folded := 0
			for _, c := range clusters {
				folded += len(c.Folded)
			}
			_, _ = fmt.Fprintf(w, "  ! _global: %d memories, %d reflection-written consolidatable, %d near-duplicate cluster(s) covering %d rows — run `ghost maintenance consolidate-global --apply` to fold\n", len(all), len(live), len(clusters), folded+len(clusters))
		}
	case len(clusters) == 0:
		_, _ = fmt.Fprintf(w, "  - _global: %d memories, %d reflection-written consolidatable — nothing to fold\n", len(all), len(live))
	default:
		folded := 0
		for _, c := range clusters {
			folded += len(c.Folded)
		}
		_, _ = fmt.Fprintf(w, "  ! _global: %d memories, %d reflection-written consolidatable, %d near-duplicate cluster(s) covering %d rows — run `ghost maintenance consolidate-global --apply` to fold\n", len(all), len(live), len(clusters), folded+len(clusters))
	}
	reportGlobalNearDuplicates(w, store)
}

const maxStatusGlobalNearDuplicatePairs = 5

func reportGlobalNearDuplicates(w io.Writer, store *memory.Store) {
	pairs, capped, err := store.NearDuplicatePairsForProject(context.Background(), memory.GlobalProjectID)
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! Near duplicates: _global could not be read: %v\n", err)
		return
	}
	if len(pairs) == 0 {
		return
	}
	sample := pairs
	more := 0
	if len(sample) > maxStatusGlobalNearDuplicatePairs {
		more = len(sample) - maxStatusGlobalNearDuplicatePairs
		sample = sample[:maxStatusGlobalNearDuplicatePairs]
	}
	var parts []string
	for _, pr := range sample {
		parts = append(parts, fmt.Sprintf("%s and %s", memory.RefusalArg("memory_id", pr[0]), memory.RefusalArg("memory_id", pr[1])))
	}
	line := fmt.Sprintf("  ! Near duplicates: %d live near-duplicate pair(s) in _global: %s", len(pairs), strings.Join(parts, ", "))
	if more > 0 {
		line += fmt.Sprintf(" ... and %d more", more)
	}
	line += " — run `ghost reflect _global --apply` to consolidate"
	_, _ = fmt.Fprintln(w, line)
	if capped {
		_, _ = fmt.Fprintf(w, "  ! near-duplicate scan capped at %d rows for _global; rows beyond the bound were not considered (scan capped/cut)\n", memory.MaxNearDuplicateScanRows)
	}
}

func globalConsolidatable(mems []memory.Memory) []memory.Memory {
	out := make([]memory.Memory, 0, len(mems))
	for _, m := range mems {
		if m.ResolvedAt != nil || m.Pinned || memory.RetentionExempt(m) || m.Source == "manual" || m.Source == "builtin" {
			continue
		}
		if m.Source != "reflection" {
			continue
		}
		out = append(out, m)
	}
	return out
}

func reportOverdueMaintenanceStatus(w io.Writer, store *memory.Store, dataDir string) {
	if store == nil || dataDir == "" {
		return
	}
	cfg := config.LoadForHook()
	minInterval := cfg.Lifecycle.MinIntervalDuration()
	overdue, err := OverdueMaintenanceProjects(context.Background(), store, dataDir, time.Now(), minInterval, cfg.Reflection.ClearsConsolidatableSet())
	if err != nil {
		_, _ = fmt.Fprintf(w, "  ! maintenance overdue: could not be read: %s\n", err)
		return
	}
	if len(overdue) == 0 {
		return
	}
	ReportOverdueMaintenance(w, overdue, time.Now())

}
