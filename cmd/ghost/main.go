package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/bench"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/embedding"
	"github.com/wcatz/ghost/internal/linking"
	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/mcpserver"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/obsidian"
	"github.com/wcatz/ghost/internal/reflection"
	"github.com/wcatz/ghost/internal/selfupdate"
	"github.com/wcatz/ghost/internal/supersede"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-v", "--version", "version":
			fmt.Printf("ghost %s\n", version)
			return
		case "help", "--help", "-h":
			printUsage()
			return
		case "mcp":
			if len(os.Args) > 2 {
				switch os.Args[2] {
				case "init":
					runMCPInit()
					return
				case "status":
					runMCPStatus()
					return
				}
			}
			runMCP()
			return
		case "hook":
			runHook()
			return
		case "reflect":
			runReflect()
			return
		case "supersede":
			runSupersede()
			return
		case "upgrade":
			runUpgrade()
			return
		case "obsidian":
			runObsidian()
			return
		case "bench":
			runBench()
			return
		}
	}
	printUsage()
}

// runMCP starts ghost as an MCP server on stdio.
func runMCP() {
	if isTerminal() {
		fmt.Fprintln(os.Stderr, "Starting MCP server on stdio (meant to be called by Claude Code).")
		fmt.Fprintln(os.Stderr, "To set up the integration, run: ghost mcp init")
		fmt.Fprintln(os.Stderr, "")
	}
	cfg, logger, store := bootstrap()
	defer store.Close() //nolint:errcheck

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv := mcpserver.New(store, logger, version)

	if cfg.Embedding.Enabled {
		embedClient := embedding.NewClient(cfg.Embedding.OllamaURL, cfg.Embedding.Model, cfg.Embedding.Dimensions)
		embedWorker := embedding.NewWorker(embedClient, store, logger, 2*time.Minute)
		projectCh := make(chan string, 16)
		go embedWorker.Run(ctx, projectCh)
		srv.SetEmbedder(embedClient, projectCh)
		logger.Info("mcp: embedding enabled", "model", cfg.Embedding.Model)

		if cfg.Linking.Enabled {
			linkWorker := linking.NewWorker(store, logger, 2*time.Minute, float32(cfg.Linking.Threshold))
			go linkWorker.Run(ctx)
			logger.Info("mcp: memory linking enabled", "threshold", cfg.Linking.Threshold)
		}
	}

	if err := srv.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runMCPInit configures Claude Code to use Ghost as its memory system.
func runMCPInit() {
	dryRun := len(os.Args) > 3 && os.Args[3] == "--dry-run"
	if err := mcpinit.Run(os.Stdout, dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runMCPStatus checks the health of the Ghost ↔ Claude Code integration.
func runMCPStatus() {
	if err := mcpinit.Status(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runHook dispatches Claude Code hook events.
func runHook() {
	if len(os.Args) < 3 {
		os.Exit(0)
	}
	switch os.Args[2] {
	case "session-start":
		mcpinit.HandleSessionStartHook(os.Stdin, os.Stdout)
	case "stop":
		mcpinit.HandleStopHook(os.Stdin, os.Stdout)
	}
}

// runReflect manually triggers memory consolidation for a project.
// Defaults to dry-run (preview only). Use --apply to save results.
// Use --restore to undo the last consolidation from snapshot.
func runReflect() {
	var projectName, tierValue string
	var apply, restore bool
	tierValue = "auto"
	for i := 2; i < len(os.Args); i++ {
		switch {
		case os.Args[i] == "--tier" && i+1 < len(os.Args):
			tierValue = os.Args[i+1]
			i++
		case strings.HasPrefix(os.Args[i], "--tier="):
			tierValue = strings.TrimPrefix(os.Args[i], "--tier=")
		case os.Args[i] == "--apply":
			apply = true
		case os.Args[i] == "--restore":
			restore = true
		case !strings.HasPrefix(os.Args[i], "-"):
			projectName = os.Args[i]
		}
	}
	if projectName == "" {
		fmt.Fprintln(os.Stderr, `Usage: ghost reflect <project> [flags]

Flags:
  --tier string   Consolidation tier: auto, haiku, sqlite (default "auto")
  --apply         Save results (default is dry-run/preview only)
  --restore       Undo the last consolidation from snapshot`)
		os.Exit(1)
	}

	cfg, logger, store := bootstrap()
	defer store.Close() //nolint:errcheck

	ctx := context.Background()

	projectID, err := store.ResolveProjectByName(ctx, projectName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if projectID == "" {
		fmt.Fprintf(os.Stderr, "error: project %q not found\n", projectName)
		os.Exit(1)
	}

	if restore {
		n, err := store.RestoreSnapshot(ctx, projectID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Restored %d memories from snapshot for %s\n", n, projectName)
		return
	}

	var consolidator reflection.Consolidator
	switch tierValue {
	case "haiku":
		if cfg.API.Key == "" {
			fmt.Fprintln(os.Stderr, "error: haiku tier requires ANTHROPIC_API_KEY")
			os.Exit(1)
		}
		client := ai.NewClient(cfg.API.Key, logger)
		consolidator = reflection.NewHaikuConsolidator(client)
	case "sqlite":
		consolidator = reflection.NewSQLiteConsolidator()
	default: // "auto"
		var tiers []reflection.Consolidator
		if cfg.API.Key != "" {
			client := ai.NewClient(cfg.API.Key, logger)
			tiers = append(tiers, reflection.NewHaikuConsolidator(client))
		}
		tiers = append(tiers, reflection.NewSQLiteConsolidator())
		consolidator = reflection.NewTieredConsolidator(tiers, logger)
	}

	if !consolidator.Available(ctx) {
		fmt.Fprintf(os.Stderr, "error: consolidator %q is not available\n", consolidator.Name())
		os.Exit(1)
	}

	if !apply {
		fmt.Println("DRY RUN (use --apply to save results)")
		fmt.Println()
	}
	fmt.Printf("Project:      %s (%s)\n", projectName, projectID)
	fmt.Printf("Consolidator: %s\n", consolidator.Name())

	// Captured BEFORE fetching the consolidation input, then handed to
	// ReplaceNonManual: ghost reflect runs as a separate process from the live
	// MCP server, so any non-manual memory saved from this instant on wasn't
	// seen by the consolidator and must survive the replace. Capturing after
	// GetAll would leave a gap where a concurrent save is neither in the
	// snapshot nor recent enough to be preserved.
	consolidatedSince, err := store.CurrentTimestamp(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: get timestamp: %v\n", err)
		os.Exit(1)
	}
	existingMemories, err := store.GetAll(ctx, projectID, 200)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: get memories: %v\n", err)
		os.Exit(1)
	}
	currentContext, _ := store.GetLearnedContext(ctx, projectID)
	exchanges, _ := store.GetRecentExchanges(ctx, projectID, 15)

	fmt.Printf("Memories:     %d existing\n", len(existingMemories))
	fmt.Println("Running consolidation...")

	input := reflection.ReflectionInput{
		RecentExchanges:  exchanges,
		ExistingMemories: existingMemories,
		CurrentContext:   currentContext,
		ProjectName:      projectName,
	}

	consolidateCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	result, err := consolidator.Consolidate(consolidateCtx, input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: consolidation failed: %v\n", err)
		os.Exit(1)
	}

	var validMemories []reflection.ReflectMemory
	for _, m := range result.Memories {
		if strings.TrimSpace(m.Content) != "" {
			validMemories = append(validMemories, m)
		}
	}
	result.Memories = validMemories

	catCounts := make(map[string]int)
	for _, m := range result.Memories {
		catCounts[m.Category]++
	}
	var parts []string
	for cat, n := range catCounts {
		parts = append(parts, fmt.Sprintf("%d %s", n, cat))
	}
	fmt.Printf("Result:       %d memories (%s)\n", len(result.Memories), strings.Join(parts, ", "))
	fmt.Println()

	var projectMems, globalMems []reflection.ReflectMemory
	for _, m := range result.Memories {
		if m.Scope == "global" {
			globalMems = append(globalMems, m)
		} else {
			projectMems = append(projectMems, m)
		}
	}

	if len(projectMems) > 0 {
		fmt.Printf("  Project-scoped (%d):\n", len(projectMems))
		for _, m := range projectMems {
			truncated := m.Content
			if len(truncated) > 120 {
				truncated = truncated[:120] + "..."
			}
			fmt.Printf("    [%s] (%.1f) %s\n", m.Category, m.Importance, truncated)
		}
	}
	if len(globalMems) > 0 {
		fmt.Printf("  Global-scoped (%d):\n", len(globalMems))
		for _, m := range globalMems {
			truncated := m.Content
			if len(truncated) > 120 {
				truncated = truncated[:120] + "..."
			}
			fmt.Printf("    [%s] (%.1f) %s\n", m.Category, m.Importance, truncated)
		}
	}
	fmt.Println()

	if len(result.Memories) == 0 {
		fmt.Println("No memories returned from consolidation.")
		return
	}

	var existingNonManual int
	for _, m := range existingMemories {
		if m.Source != "manual" {
			existingNonManual++
		}
	}
	if existingNonManual >= 6 && len(projectMems) < existingNonManual/2 {
		fmt.Fprintf(os.Stderr, "WARNING: consolidation returned %d project memories vs %d existing non-manual (>50%% reduction)\n",
			len(projectMems), existingNonManual)
		if len(globalMems) > 0 {
			fmt.Fprintf(os.Stderr, "  (%d memories classified as global — check scope accuracy)\n", len(globalMems))
		}
	}

	if !apply {
		fmt.Println("Dry run complete. Re-run with --apply to save these results.")
		return
	}

	if len(globalMems) > 0 {
		if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
			fmt.Fprintf(os.Stderr, "warning: ensure _global project: %v\n", err)
		}
		for _, m := range globalMems {
			if _, _, err := store.Upsert(ctx, "_global", m.Category, m.Content, "reflection", m.Importance, m.Tags); err != nil {
				fmt.Fprintf(os.Stderr, "warning: upsert global memory: %v\n", err)
			}
		}
		fmt.Printf("Upserted %d global memories\n", len(globalMems))
	}

	if len(projectMems) > 0 {
		dbMemories := make([]memory.Memory, len(projectMems))
		for i, m := range projectMems {
			dbMemories[i] = memory.Memory{
				ProjectID:  projectID,
				Category:   m.Category,
				Content:    m.Content,
				Importance: m.Importance,
				Source:     "reflection",
				Tags:       m.Tags,
			}
		}

		if err := store.ReplaceNonManual(ctx, projectID, dbMemories, consolidatedSince); err != nil {
			fmt.Fprintf(os.Stderr, "error: save memories: %v\n", err)
			os.Exit(1)
		}

		summary := fmt.Sprintf("%d memories consolidated (%s)", len(dbMemories), strings.Join(parts, ", "))
		if len(globalMems) > 0 {
			summary += fmt.Sprintf(", %d promoted to global", len(globalMems))
		}
		fmt.Printf("Applied: %s\n", summary)
		fmt.Println("(use --restore to undo)")

		if result.LearnedContext != "" {
			if err := store.UpdateLearnedContext(ctx, projectID, result.LearnedContext, summary); err != nil {
				fmt.Fprintf(os.Stderr, "warning: update learned context: %v\n", err)
			}
		}
	}
}

// runSupersede implements `ghost supersede <project> [--apply]` — the creation
// half of staleness-aware ranking. It proposes newer→older 'supersedes' links
// over the project's live memories (cosine-similar candidates, Haiku-confirmed)
// and, with --apply, writes them. Dry-run by default. Re-runnable: it self-heals
// after `ghost reflect` cascade-deletes links. Consumed by search only when
// SupersedeDemote is set. See docs/benchmarks.md Phase 3.
func runSupersede() {
	var projectName string
	apply := false
	threshold := float32(0.80) // supersession candidates are the SAME fact — tighter than the 0.70 'related' floor
	for i := 2; i < len(os.Args); i++ {
		switch {
		case os.Args[i] == "--apply":
			apply = true
		case os.Args[i] == "--threshold" && i+1 < len(os.Args):
			if v, err := strconv.ParseFloat(os.Args[i+1], 32); err == nil {
				threshold = float32(v)
			}
			i++
		case strings.HasPrefix(os.Args[i], "--threshold="):
			if v, err := strconv.ParseFloat(strings.TrimPrefix(os.Args[i], "--threshold="), 32); err == nil {
				threshold = float32(v)
			}
		case !strings.HasPrefix(os.Args[i], "-"):
			projectName = os.Args[i]
		default:
			fmt.Fprintf(os.Stderr, "error: unknown flag %q\n", os.Args[i])
			os.Exit(1)
		}
	}
	if projectName == "" {
		fmt.Fprintln(os.Stderr, `Usage: ghost supersede <project> [flags]

Flags:
  --apply             Write the supersedes links (default is dry-run/preview)
  --threshold float   Min cosine similarity for a candidate pair (default 0.80)

Requires ANTHROPIC_API_KEY (uses Haiku to confirm each candidate).`)
		os.Exit(1)
	}

	cfg, logger, store := bootstrap()
	defer store.Close() //nolint:errcheck
	ctx := context.Background()

	projectID, err := store.ResolveProjectByName(ctx, projectName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if projectID == "" {
		fmt.Fprintf(os.Stderr, "error: project %q not found\n", projectName)
		os.Exit(1)
	}
	if cfg.API.Key == "" {
		fmt.Fprintln(os.Stderr, "error: ghost supersede requires ANTHROPIC_API_KEY (Haiku confirms each candidate)")
		os.Exit(1)
	}

	cls := supersede.NewHaikuClassifier(ai.NewClient(cfg.API.Key, logger))
	res, confirmed, err := supersede.Run(ctx, store, cls, projectID, threshold, apply, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	verb := "would link"
	if apply {
		verb = "linked"
	}
	short := func(id string) string {
		if len(id) > 8 {
			return id[:8]
		}
		return id
	}
	fmt.Printf("%s: %d candidate pairs, %d confirmed supersessions, %s %d\n",
		projectName, res.Candidates, res.Confirmed, verb, len(confirmed))
	for _, c := range confirmed {
		fmt.Printf("  %s  supersedes  %s\n", short(c.NewerID), short(c.OlderID))
	}
	if !apply && res.Confirmed > 0 {
		fmt.Println("\nRe-run with --apply to write these links.")
	}
}

// runUpgrade downloads and installs the latest ghost release.
func runUpgrade() {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot determine binary path: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Current: ghost %s (%s)\n", version, exe)
	fmt.Println("Checking for updates...")

	rel, err := selfupdate.LatestRelease()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	latest := strings.TrimPrefix(rel.TagName, "v")
	if latest == version {
		fmt.Printf("Already up to date (%s).\n", version)
		return
	}

	asset, err := selfupdate.FindAsset(rel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Fail closed: the archive digest must match the release's checksums.txt
	// manifest before a single byte reaches Replace. Without this, anyone able
	// to substitute a release asset gets arbitrary code execution on every
	// machine that runs `ghost upgrade`.
	checksumAsset, err := selfupdate.FindChecksumAsset(rel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	checksumBody, err := selfupdate.Download(checksumAsset.BrowserDownloadURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	checksumBytes, err := io.ReadAll(checksumBody)
	_ = checksumBody.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: reading checksums.txt: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Downloading %s...\n", asset.Name)
	body, err := selfupdate.Download(asset.BrowserDownloadURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	archiveBytes, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: downloading %s: %v\n", asset.Name, err)
		os.Exit(1)
	}

	if err := selfupdate.VerifyChecksum(archiveBytes, string(checksumBytes), asset.Name); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	bin, err := selfupdate.ExtractBinary(bytes.NewReader(archiveBytes))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Replacing %s...\n", exe)
	if err := selfupdate.Replace(exe, bin); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Updated: ghost %s → %s\n", version, latest)
}

// parseObsidianFlags parses the flags following `ghost obsidian <mode>`. It
// errors on an unknown flag or a value flag missing its argument rather than
// silently falling back to defaults (a misspelled --intervl would otherwise
// leave the user believing a cadence that isn't in effect).
func parseObsidianFlags(args []string) (out, project, interval string, err error) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--out", arg == "--project", arg == "--interval":
			if i+1 >= len(args) {
				return "", "", "", fmt.Errorf("flag %s needs a value", arg)
			}
			i++
			switch arg {
			case "--out":
				out = args[i]
			case "--project":
				project = args[i]
			case "--interval":
				interval = args[i]
			}
		case strings.HasPrefix(arg, "--out="):
			out = strings.TrimPrefix(arg, "--out=")
		case strings.HasPrefix(arg, "--project="):
			project = strings.TrimPrefix(arg, "--project=")
		case strings.HasPrefix(arg, "--interval="):
			interval = strings.TrimPrefix(arg, "--interval=")
		default:
			return "", "", "", fmt.Errorf("unknown or malformed flag %q", arg)
		}
	}
	return out, project, interval, nil
}

// roDSN builds a read-only DSN for the given database path. The file: URI
// form is required: modernc.org/sqlite honors mode=ro only on URI DSNs — a
// bare path opens silently read-write (verified against v1.53.0). The path is
// URI-escaped so a '?' or '#' in $XDG_DATA_HOME/$HOME can't be parsed as the
// query separator or a fragment (which would drop mode=ro or open the wrong
// file). No journal_mode pragma: setting it writes the DB header, which a
// read-only connection cannot do — it would fail against a non-WAL database
// and is a pure no-op against a WAL one.
func roDSN(dbPath string) string {
	u := url.URL{
		Scheme:   "file",
		Opaque:   (&url.URL{Path: dbPath}).EscapedPath(),
		RawQuery: "mode=ro&_pragma=busy_timeout(1000)",
	}
	return u.String()
}

// runObsidian implements `ghost obsidian export|sync` — a one-way mirror of
// the store into an Obsidian-readable Markdown vault.
func runObsidian() {
	if len(os.Args) < 3 || (os.Args[2] != "export" && os.Args[2] != "sync") {
		fmt.Fprintln(os.Stderr, `Usage: ghost obsidian <export|sync> [flags]

Flags:
  --out string       Vault directory (default ~/Documents/GhostVault or obsidian.vault_dir)
  --project string   Mirror a single project (plus Global)
  --interval string  sync only: poll cadence (default 30s or obsidian.interval)`)
		os.Exit(1)
	}
	mode := os.Args[2]
	out, project, interval, err := parseObsidianFlags(os.Args[3:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if out == "" {
		out = cfg.Obsidian.VaultDir
	}
	if out == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot resolve home dir: %v\n", err)
			os.Exit(1)
		}
		out = filepath.Join(home, "Documents", "GhostVault")
	}
	if interval == "" {
		interval = cfg.Obsidian.Interval
	}

	dataDir, err := config.DataDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "error: no database at %s — run ghost mcp init or start a session first\n", dbPath)
		os.Exit(1)
	}
	// Read-only: safe alongside a live MCP server.
	db, err := sql.Open("sqlite", roDSN(dbPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open %s: %v\n", dbPath, err)
		os.Exit(1)
	}
	// PRAGMA data_version (sync mode) is per-connection; pin the pool to one
	// connection so polls compare against a stable baseline. memory.OpenDB does
	// this for read-write opens — raw sql.Open here needs it explicitly.
	db.SetMaxOpenConns(1)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	store := memory.NewStore(db, logger)
	defer store.Close() //nolint:errcheck

	ex := &obsidian.Exporter{Store: store, Logger: logger}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if mode == "export" {
		if err := ex.Export(ctx, out, project); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				fmt.Println("Stopped.")
				return
			}
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Mirrored to %s\n", out)
		return
	}
	d, err := time.ParseDuration(interval)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: bad --interval %q: %v\n", interval, err)
		os.Exit(1)
	}
	if d <= 0 {
		fmt.Fprintf(os.Stderr, "error: --interval must be positive, got %s\n", d)
		os.Exit(1)
	}
	fmt.Printf("Syncing to %s every %s (Ctrl-C to stop)\n", out, d)
	if err := obsidian.Sync(ctx, ex, db, out, project, d); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			fmt.Println("Stopped.")
			return
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// printUsage displays the top-level help.
func printUsage() {
	fmt.Fprintf(os.Stderr, `ghost %s — MCP memory server for Claude Code

Usage:
  ghost <command>

Commands:
  mcp                         Start MCP server on stdio (used by Claude Code)
  mcp init [--dry-run]        Configure Claude Code integration
  mcp status                  Check Claude Code integration health
  reflect <project> [flags]   Memory consolidation (dry-run by default, --apply to save)
  supersede <project> [flags] Link superseded memories (dry-run by default, --apply to write)
  obsidian export [flags]     Mirror memories to an Obsidian vault (one-way)
  obsidian sync [flags]       Keep the vault mirror fresh (polls for DB changes)
  bench [--sweep]             Run the retrieval-quality benchmark (built-in dataset);
                              --sweep grid-searches the fusion parameters
  upgrade                     Update ghost to the latest release
  version                     Print version

Flags (reflect):
  --tier string   Consolidation tier: auto, haiku, sqlite (default "auto")
  --apply         Save results
  --restore       Undo last consolidation

Environment:
  ANTHROPIC_API_KEY           Required for reflect --tier haiku
  GHOST_DEBUG                 Enable debug logging
`, version)
}

// runBench implements `ghost bench` — runs the built-in retrieval-quality
// benchmark (four ablations over the embedded dataset) and prints the metric
// table. With --sweep it instead grid-searches the fusion parameters and
// prints the ranked table. Judge-free, deterministic, no network. See
// docs/benchmarks.md.
func runBench() {
	sweep := false
	for _, arg := range os.Args[2:] {
		switch arg {
		case "--sweep":
			sweep = true
		default:
			fmt.Fprintf(os.Stderr, "error: unknown flag %q\n\nUsage: ghost bench [--sweep]\n", arg)
			os.Exit(1)
		}
	}

	ds, vecs, err := bench.BuiltinDataset()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	threshold := float32(0.70)
	if cfg, err := config.Load(); err == nil {
		threshold = float32(cfg.Linking.Threshold)
	}
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Silence internal search diagnostics (e.g. FTS term-cap warnings that go
	// through the package-level default) so the benchmark table is the only
	// output; the table is on stdout, so `ghost bench` stays pipeable.
	slog.SetDefault(logger)
	store := memory.NewStore(db, logger)
	defer store.Close() //nolint:errcheck

	ctx := context.Background()
	queries, err := bench.Seed(ctx, store, ds, vecs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if sweep {
		// The sweep varies query-time fusion parameters over one prepared
		// store, so the link graph is built once up front.
		linking.NewWorker(store, logger, time.Hour, threshold).SweepOnce(ctx)
		points, err := bench.Sweep(ctx, store, queries, bench.SweepGrid())
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(bench.FormatSweep(points))
		return
	}

	results, err := bench.Run(ctx, store, queries, threshold)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(bench.FormatResults(results))
}

func bootstrap() (*config.Config, *slog.Logger, *memory.Store) {
	configPath, created, err := config.EnsureConfigFile()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create config file: %v\n", err)
	} else if created {
		fmt.Fprintf(os.Stderr, "Created config file: %s\n", configPath)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	logLevel := slog.LevelInfo
	if os.Getenv("GHOST_DEBUG") != "" {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))

	dataDir, err := config.DataDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot create data directory: %v\n", err)
		os.Exit(1)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: database: %v\n", err)
		os.Exit(1)
	}

	store := memory.NewStore(db, logger)

	if err := store.SeedGlobalMemories(context.Background()); err != nil {
		logger.Warn("seed global memories", "error", err)
	}

	return cfg, logger, store
}

// isTerminal reports whether stdin is connected to a terminal.
func isTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
