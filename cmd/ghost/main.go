package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/briefing"
	"github.com/wcatz/ghost/internal/calendar"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/embedding"
	"github.com/wcatz/ghost/internal/github"
	goog "github.com/wcatz/ghost/internal/google"
	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/mcpserver"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/orchestrator"
	"github.com/wcatz/ghost/internal/reflection"
	"github.com/wcatz/ghost/internal/scheduler"
	"github.com/wcatz/ghost/internal/selfupdate"
	"github.com/wcatz/ghost/internal/server"
	"github.com/wcatz/ghost/internal/telegram"
	"github.com/wcatz/ghost/internal/tool"
	"github.com/wcatz/ghost/internal/tui"
	"github.com/wcatz/ghost/internal/voice"
)

var version = "dev"

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	// Check for subcommands and flags before flag parsing.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-v", "--version", "version":
			fmt.Printf("ghost %s\n", version)
			return
		case "help", "--help", "-h":
			printUsage()
			return
		case "serve":
			runServe()
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

	var (
		projects stringSlice
		modeFlag = flag.String("mode", "", "Operating mode: chat, code, debug, review, plan, refactor")
		modelFlag   = flag.String("model", "", "Model override (e.g. claude-opus-4-6-20250514)")
		yolo        = flag.Bool("yolo", false, "Skip all tool approval prompts")
		noMemory    = flag.Bool("no-memory", false, "Disable memory extraction for this session")
		noTUI       = flag.Bool("no-tui", false, "Force legacy REPL (no bubbletea)")
		cont        = flag.Bool("continue", false, "Resume last conversation")
	)
	flag.Usage = printUsage
	flag.Var(&projects, "project", "Project path (can be specified multiple times)")
	flag.Parse()

	cfg, logger, store, _ := bootstrap()
	defer store.Close()

	// Redirect logs early if TUI mode, before creating components that capture the logger.
	willUseTUI := tui.IsTerminal() && len(flag.Args()) == 0 &&
		!*noTUI && !cfg.Display.PlainMode &&
		os.Getenv("TERM") != "dumb" && os.Getenv("GHOST_PLAIN") == ""
	if willUseTUI {
		dataDir, _ := config.DataDir()
		if dataDir != "" {
			logFile, err := os.OpenFile(filepath.Join(dataDir, "ghost.log"),
				os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err == nil {
				defer logFile.Close()
				logger = slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: slog.LevelInfo}))
				slog.SetDefault(logger)
			}
		}
	}

	if cfg.API.Key == "" {
		fmt.Fprintln(os.Stderr, "error: ANTHROPIC_API_KEY not set")
		fmt.Fprintln(os.Stderr, "Set it via environment variable or in ~/.config/ghost/config.yaml")
		os.Exit(1)
	}

	// Initialize AI client.
	client := ai.NewClient(cfg.API.Key, logger)

	// Initialize tool registry.
	registry := tool.NewRegistry()
	tool.RegisterAll(registry, store)

	// Apply flag overrides.
	if *modeFlag != "" {
		cfg.Defaults.Mode = *modeFlag
	}
	if *modelFlag != "" {
		cfg.API.ModelQuality = *modelFlag
	}
	if *yolo {
		cfg.Defaults.ApprovalMode = "yolo"
	}
	if *noMemory {
		cfg.Defaults.AutoMemory = false
	}

	// Create orchestrator.
	orch := orchestrator.New(client, store, registry, cfg, logger)

	// Determine projects to load.
	if len(projects) == 0 {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot get working directory: %v\n", err)
			os.Exit(1)
		}
		projects = []string{cwd}
	}

	// Start sessions.
	var firstSession *orchestrator.Session
	for _, p := range projects {
		absPath, err := filepath.Abs(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: bad path %s: %v\n", p, err)
			continue
		}
		startFn := orch.StartSession
		if *cont {
			startFn = orch.ResumeSession
		}
		s, err := startFn(absPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot start session for %s: %v\n", absPath, err)
			continue
		}
		if firstSession == nil {
			firstSession = s
		}
	}

	if firstSession == nil {
		fmt.Fprintln(os.Stderr, "error: no valid project sessions")
		os.Exit(1)
	}

	// Determine run mode: one-shot, pipe, or interactive TUI.
	// One-shot check first — args take priority over pipe detection.
	if args := flag.Args(); len(args) > 0 {
		message := strings.Join(args, " ")
		tui.RunOneShot(firstSession, message, cfg.Display.ShowCost)
		return
	}

	if !tui.IsTerminal() {
		// Pipe mode: read all stdin and send as one message.
		input, _ := io.ReadAll(os.Stdin)
		if len(input) > 0 {
			tui.RunPipe(firstSession, string(input), cfg.Display.ShowCost)
		}
		return
	}

	// Interactive mode: bubbletea TUI unless forced plain.
	usePlainREPL := *noTUI || cfg.Display.PlainMode ||
		os.Getenv("TERM") == "dumb" || os.Getenv("GHOST_PLAIN") != ""

	if usePlainREPL {
		repl := tui.NewREPL(orch, cfg.Display.ShowCost)
		if err := repl.Run(firstSession); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	} else {
		if err := tui.RunApp(orch, cfg, firstSession); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
}

// runServe starts ghost as an HTTP daemon with all optional subsystems.
func runServe() {
	serveFlags := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := serveFlags.String("addr", "", "Listen address (overrides config)")
	serveFlags.Parse(os.Args[2:])

	cfg, logger, store, db := bootstrap()
	defer store.Close()

	if *addr != "" {
		cfg.Server.ListenAddr = *addr
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// --- Embedding worker (background vectorization via Ollama) ---
	var embedClient *embedding.Client
	var projectCh chan string
	if cfg.Embedding.Enabled {
		embedClient = embedding.NewClient(cfg.Embedding.OllamaURL, cfg.Embedding.Model, cfg.Embedding.Dimensions)
		embedWorker := embedding.NewWorker(embedClient, store, logger, 2*time.Minute)
		projectCh = make(chan string, 16)
		go embedWorker.Run(ctx, projectCh)
		// Notify embedding worker whenever a memory is saved from any path
		// (tool, MCP, HTTP, reflection, etc.)
		store.SetOnSave(func(projectID string) {
			select {
			case projectCh <- projectID:
			default: // non-blocking — if buffer full, periodic sweep catches it
			}
		})

		if embedClient.Alive(ctx) {
			logger.Info("embedding worker started", "model", cfg.Embedding.Model, "ollama", cfg.Embedding.OllamaURL)
		} else {
			logger.Info("embedding worker started (ollama offline, will retry)", "ollama", cfg.Embedding.OllamaURL)
		}
	}

	// --- Scheduler (cron jobs + reminders) ---
	sched, err := scheduler.New(db, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: scheduler: %v\n", err)
		os.Exit(1)
	}
	go sched.Start(ctx)
	logger.Info("scheduler started")

	// --- GitHub notification monitor ---
	var ghMonitor *github.Monitor
	if cfg.GitHub.Token != "" {
		interval := time.Duration(cfg.GitHub.Interval) * time.Second
		if interval < 30*time.Second {
			interval = 60 * time.Second
		}
		ghMonitor = github.NewMonitor(cfg.GitHub.Token, db, logger, interval)
		go ghMonitor.Run(ctx)
		logger.Info("github monitor started", "interval", interval)
	}

	// --- CalDAV calendar ---
	var calClient *calendar.Client
	if cfg.Calendar.URL != "" {
		var err error
		calClient, err = calendar.NewClient(ctx, calendar.Config{
			URL:      cfg.Calendar.URL,
			Username: cfg.Calendar.Username,
			Password: cfg.Calendar.Password,
		}, logger)
		if err != nil {
			logger.Warn("caldav init failed, calendar disabled", "error", err)
		} else {
			logger.Info("calendar connected", "url", cfg.Calendar.URL)
		}
	}

	// --- Google Calendar + Gmail (OAuth2) ---
	var googleClient *goog.Client
	credFile := expandHome(cfg.Google.CredentialsFile)
	if _, err := os.Stat(credFile); err == nil {
		googleClient, err = goog.NewClient(ctx, goog.Config{
			CredentialsFile: credFile,
			TokenFile:       expandHome(cfg.Google.TokenFile),
		}, logger)
		if err != nil {
			logger.Warn("google API init failed", "error", err)
		} else {
			logger.Info("google API connected (calendar + gmail)")
		}
	}

	// --- Telegram bot ---
	var tgBot *telegram.Bot
	if cfg.Telegram.Token != "" {
		allowedIDs := telegram.ParseAllowedIDs(cfg.Telegram.AllowedIDs)
		tgBot, err = telegram.New(telegram.Config{
			Token:      cfg.Telegram.Token,
			AllowedIDs: allowedIDs,
		}, store, ghMonitor, sched, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: telegram bot: %v\n", err)
			os.Exit(1)
		}

		// Wire P0/P1 GitHub alerts to Telegram.
		if ghMonitor != nil {
			ghMonitor.OnAlert(func(n github.Notification) {
				tgBot.SendAlertToAll(ctx, n)
			})
		}

		// Wire reminder alerts to Telegram.
		sched.OnAlert(func(message string) {
			tgBot.SendToAll(ctx, message)
		})

		// Wire Google Calendar/Gmail to Telegram.
		if googleClient != nil {
			tgBot.SetGoogle(googleClient)

			// Start meeting notifier.
			notifier := goog.NewMeetingNotifier(googleClient, func(msg string) {
				tgBot.SendToAll(ctx, msg)
			}, logger)
			go notifier.Run(ctx)
		}

		go tgBot.Run(ctx)
		logger.Info("telegram bot started", "allowed_users", len(allowedIDs))
	}

	// --- Voice STT for Telegram ---
	if cfg.Voice.Enabled && tgBot != nil {
		stt, err := voice.BuildSTT(voice.Options{
			STTBackend:       cfg.Voice.STTBackend,
			STTModel:         cfg.Voice.STTModel,
			AssemblyAIAPIKey: cfg.Voice.AssemblyAIAPIKey,
		})
		if err != nil {
			logger.Warn("stt unavailable for telegram", "error", err)
		} else {
			tgBot.SetSTT(stt)
			logger.Info("telegram voice transcription enabled", "backend", cfg.Voice.STTBackend)
		}
	}

	// --- Morning briefing cron ---
	briefingSources := briefing.Sources{
		GitHub:    ghMonitor,
		Calendar:  calClient,
		Google:    googleClient,
		Scheduler: sched,
	}
	if tgBot != nil {
		tgBot.SetBriefingSources(briefingSources)
	}
	if cfg.Briefing.Enabled && tgBot != nil {
		err := sched.AddCronJob(ctx, "morning-briefing", cfg.Briefing.Schedule, nil, func() {
			msg := briefing.Generate(ctx, briefingSources)
			tgBot.SendToAll(ctx, msg)
			logger.Info("morning briefing sent")
		})
		if err != nil {
			logger.Error("failed to schedule briefing", "error", err)
		} else {
			logger.Info("morning briefing scheduled", "cron", cfg.Briefing.Schedule)
		}
	}

	// --- Monthly cost report cron (reports previous month on 1st) ---
	if cfg.CostReport.Enabled && tgBot != nil {
		err := sched.AddCronJob(ctx, "monthly-cost-report", cfg.CostReport.Schedule, nil, func() {
			report := generateCostReport(ctx, store, logger)
			tgBot.SendToAll(ctx, report)
			logger.Info("monthly cost report sent")
		})
		if err != nil {
			logger.Error("failed to schedule cost report", "error", err)
		} else {
			logger.Info("monthly cost report scheduled", "cron", cfg.CostReport.Schedule)
		}
	}

	// --- Orchestrator for chat API (optional — requires API key) ---
	var orch *orchestrator.Orchestrator
	if cfg.API.Key != "" {
		aiClient := ai.NewClient(cfg.API.Key, logger)
		chatRegistry := tool.NewRegistry()
		tool.RegisterAll(chatRegistry, store)
		orch = orchestrator.New(aiClient, store, chatRegistry, cfg, logger)
		logger.Info("chat API enabled")
	} else {
		logger.Warn("no API key, chat endpoints disabled (memory API still available)")
	}

	// --- HTTP server (blocks) ---
	srv := server.New(store, &cfg.Server, logger)
	srv.SetOrchestrator(orch)
	if cfg.Voice.AssemblyAIAPIKey != "" {
		srv.SetAssemblyAIKey(cfg.Voice.AssemblyAIAPIKey)
		logger.Info("transcribe token endpoint enabled")
	}

	// Wire Telegram bot as approval notifier + give it the server address.
	if tgBot != nil {
		srv.SetApprovalNotifier(tgBot)
		tgBot.SetServerAddr(cfg.Server.ListenAddr)
		tgBot.SetServerToken(cfg.Server.AuthToken)
	}

	if err := srv.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runMCP starts ghost as an MCP server on stdio.
func runMCP() {
	if tui.IsTerminal() {
		fmt.Fprintln(os.Stderr, "Starting MCP server on stdio (this is meant to be called by Claude Code).")
		fmt.Fprintln(os.Stderr, "If you meant to set up the integration, run: ghost mcp init")
		fmt.Fprintln(os.Stderr, "")
	}
	cfg, logger, store, _ := bootstrap()
	defer store.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv := mcpserver.New(store, logger, version)

	// Wire embedding for hybrid search if configured.
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

// runReflect manually triggers memory consolidation for a project.
// Defaults to dry-run (preview only). Use --apply to save results.
// Use --restore to undo the last consolidation from snapshot.
func runReflect() {
	// Extract project name and flags from args (allow flags before or after project name).
	var projectName, tierValue string
	var apply, restore bool
	tierValue = "auto"
	for i := 2; i < len(os.Args); i++ {
		switch {
		case os.Args[i] == "--tier" && i+1 < len(os.Args):
			tierValue = os.Args[i+1]
			i++ // skip value
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

	cfg, logger, store, _ := bootstrap()
	defer store.Close() //nolint:errcheck

	ctx := context.Background()

	// Resolve project name to ID.
	projectID, err := store.ResolveProjectByName(ctx, projectName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if projectID == "" {
		fmt.Fprintf(os.Stderr, "error: project %q not found\n", projectName)
		os.Exit(1)
	}

	// Handle --restore.
	if restore {
		n, err := store.RestoreSnapshot(ctx, projectID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Restored %d memories from snapshot for %s\n", n, projectName)
		return
	}

	// Build consolidator for the requested tier.
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

	// Check availability.
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

	// Gather input.
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

	// Filter empty-content memories.
	var validMemories []reflection.ReflectMemory
	for _, m := range result.Memories {
		if strings.TrimSpace(m.Content) != "" {
			validMemories = append(validMemories, m)
		}
	}
	result.Memories = validMemories

	// Summary.
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

	// Split by scope.
	var projectMems, globalMems []reflection.ReflectMemory
	for _, m := range result.Memories {
		if m.Scope == "global" {
			globalMems = append(globalMems, m)
		} else {
			projectMems = append(projectMems, m)
		}
	}

	// Show each memory.
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

	// Empty-set guard (based on project-scoped only).
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

	// Apply global memories via upsert.
	if len(globalMems) > 0 {
		if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
			fmt.Fprintf(os.Stderr, "warning: ensure _global project: %v\n", err)
		}
		for _, m := range globalMems {
			_, _, err := store.Upsert(ctx, "_global", m.Category, m.Content, "reflection", m.Importance, m.Tags)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: upsert global memory: %v\n", err)
			}
		}
		fmt.Printf("Upserted %d global memories\n", len(globalMems))
	}

	// Apply project memories via replace.
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

// printUsage displays the top-level help.
func printUsage() {
	fmt.Fprintf(os.Stderr, `ghost %s — memory-first AI assistant

Usage:
  ghost [flags] [message]     Start interactive session or send one-shot message
  ghost <command>             Run a subcommand

Commands:
  serve                       Start HTTP daemon with all subsystems
  mcp                         Start MCP server on stdio (used by Claude Code)
  mcp init [--dry-run]        Configure Claude Code integration
  mcp status                  Check Claude Code integration health
  reflect <project> [flags]   Memory consolidation (dry-run by default, --apply to save)
  upgrade                     Update ghost to the latest release
  version                     Print version

Flags:
  -mode string                Operating mode: chat, code, debug, review, plan, refactor
  -model string               Model override (e.g. claude-opus-4-6-20250514)
  -project path               Project path (repeatable)
  -continue                   Resume last conversation
  -yolo                       Skip all tool approval prompts
  -no-memory                  Disable memory extraction for this session
  -no-tui                     Force legacy REPL (no bubbletea)

Environment:
  ANTHROPIC_API_KEY           Required for AI features
  GHOST_DEBUG                 Enable debug logging
  GHOST_PLAIN                 Force plain REPL (no bubbletea)
`, version)
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
	defer body.Close()

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

// bootstrap loads config, sets up logging, and opens the database.
func bootstrap() (*config.Config, *slog.Logger, *memory.Store, *sql.DB) {
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

	// Ensure seed global memories exist (pinned, manual source — consolidation-proof).
	if err := store.SeedGlobalMemories(context.Background()); err != nil {
		logger.Warn("seed global memories", "error", err)
	}

	return cfg, logger, store, db
}

// generateCostReport builds a Telegram-formatted monthly cost summary
// comparing API spend against Claude subscription tiers.
func generateCostReport(ctx context.Context, store *memory.Store, logger *slog.Logger) string {
	// Report on previous month.
	now := time.Now()
	year, month := now.Year(), int(now.Month())-1
	if month == 0 {
		month = 12
		year--
	}

	mc, err := store.GetMonthlyCost(ctx, year, month)
	if err != nil {
		logger.Error("cost report query failed", "error", err)
		return fmt.Sprintf("Failed to generate cost report: %v", err)
	}

	monthName := time.Month(mc.Month).String()
	noCacheCost := mc.TotalCost + mc.TotalSavings

	var b strings.Builder
	fmt.Fprintf(&b, "📊 *Ghost Cost Report — %s %d*\n\n", monthName, mc.Year)
	fmt.Fprintf(&b, "API Spend:      $%.2f\n", mc.TotalCost)
	fmt.Fprintf(&b, "Without cache:  $%.2f\n", noCacheCost)
	fmt.Fprintf(&b, "Cache savings:  $%.2f\n\n", mc.TotalSavings)

	if len(mc.ByModel) > 0 {
		b.WriteString("By model:\n")
		for _, m := range mc.ByModel {
			short := m.Model
			if i := strings.LastIndex(short, "-"); i > 10 {
				short = short[:i] // strip date suffix
			}
			fmt.Fprintf(&b, "  %s:  $%.2f\n", short, m.Cost)
		}
		b.WriteString("\n")
	}

	b.WriteString("vs Claude Subscription:\n")
	tiers := []struct {
		name  string
		price float64
	}{
		{"Pro ($20/mo)", 20},
		{"Max 5x ($100/mo)", 100},
		{"Max 20x ($200/mo)", 200},
	}
	for _, t := range tiers {
		if mc.TotalCost < t.price {
			fmt.Fprintf(&b, "  %s:  API is cheaper ✓\n", t.name)
		} else {
			fmt.Fprintf(&b, "  %s:  subscription cheaper ✗\n", t.name)
		}
	}

	return b.String()
}

// expandHome replaces a leading ~ with the user's home directory.
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}
