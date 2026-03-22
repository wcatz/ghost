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
	"github.com/wcatz/ghost/internal/mcpserver"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/orchestrator"
	"github.com/wcatz/ghost/internal/scheduler"
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
	// Check for subcommands before flag parsing.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			runServe()
			return
		case "mcp":
			runMCP()
			return
		}
	}

	var (
		projects    stringSlice
		modeFlag    = flag.String("mode", "", "Operating mode: chat, code, debug, review, plan, refactor")
		modelFlag   = flag.String("model", "", "Model override (e.g. claude-opus-4-6-20250514)")
		yolo        = flag.Bool("yolo", false, "Skip all tool approval prompts")
		noMemory    = flag.Bool("no-memory", false, "Disable memory extraction for this session")
		noTUI       = flag.Bool("no-tui", false, "Force legacy REPL (no bubbletea)")
		cont        = flag.Bool("continue", false, "Resume last conversation")
		versionFlag = flag.Bool("version", false, "Print version and exit")
	)
	flag.Var(&projects, "project", "Project path (can be specified multiple times)")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("ghost %s\n", version)
		os.Exit(0)
	}

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
	cfg, logger, store, _ := bootstrap()
	defer store.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv := mcpserver.New(store, logger)

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
