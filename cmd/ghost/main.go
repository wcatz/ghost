package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/embedding"
	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/mcpserver"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
	"github.com/wcatz/ghost/internal/selfupdate"
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
		case "upgrade":
			runUpgrade()
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

		if err := store.ReplaceNonManual(ctx, projectID, dbMemories); err != nil {
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

	fmt.Printf("Downloading %s...\n", asset.Name)
	body, err := selfupdate.Download(asset.BrowserDownloadURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer body.Close() //nolint:errcheck

	bin, err := selfupdate.ExtractBinary(body)
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

// bootstrap loads config, sets up logging, and opens the database.
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
