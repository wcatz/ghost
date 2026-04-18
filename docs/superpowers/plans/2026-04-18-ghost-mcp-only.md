# Ghost: Strip to MCP-Only — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Strip Ghost down to a lean MCP memory server (16 tools, 4 resources, session hook, reflect, upgrade), removing all standalone assistant functionality, and release as v0.8.0.

**Architecture:** Delete 16 internal packages and vscode-ghost/. Rewrite cmd/ghost/main.go keeping only mcp/hook/reflect/upgrade/version subcommands. Run `go mod tidy` to drop ~20 orphaned dependencies. Tag v0.8.0 via goreleaser.

**Tech Stack:** Go 1.25, SQLite (modernc.org/sqlite), modelcontextprotocol/go-sdk, koanf config, goreleaser.

---

## File Map

**Deleted (16 packages + extension):**
- `internal/tui/` — BubbleTea interactive terminal UI
- `internal/telegram/` — Telegram bot
- `internal/voice/` — STT/TTS pipeline
- `internal/google/` — Google Calendar/Gmail OAuth
- `internal/server/` — HTTP REST/SSE daemon
- `internal/scheduler/` — Cron jobs + reminders
- `internal/github/` — GitHub notification monitor
- `internal/briefing/` — Daily briefing generator
- `internal/calendar/` — CalDAV client
- `internal/mdv2/` — Telegram MarkdownV2 escaping
- `internal/orchestrator/` — Session manager + AI orchestration
- `internal/mode/` — Operating mode definitions
- `internal/project/` — Project context gathering
- `internal/prompt/` — System prompt builder
- `internal/tool/` — Built-in tool registry (bash, file ops, grep, git)
- `internal/simulation/` — Empty test simulator
- `vscode-ghost/` — VS Code extension (depended on ghost serve)

**Kept (10 packages):**
- `internal/ai/` — Claude HTTP client (needed by reflection)
- `internal/claudeimport/` — Claude Code memory importer
- `internal/config/` — YAML config + env vars
- `internal/embedding/` — Ollama vector embedding worker
- `internal/mcpinit/` — `ghost mcp init/status`, SessionStart hook
- `internal/mcpserver/` — MCP server (16 tools, 4 resources)
- `internal/memory/` — SQLite store, tasks, decisions, FTS5
- `internal/provider/` — Interfaces (MemoryStore, LLMProvider)
- `internal/reflection/` — Memory consolidation (Haiku + SQLite tiers)
- `internal/selfupdate/` — Binary self-updater

**Modified:**
- `cmd/ghost/main.go` — Complete rewrite (keep 6 subcommands, drop everything else)
- `Dockerfile` — Change `CMD ["serve"]` → `CMD ["mcp"]`
- `README.md` — Remove serve/TUI/bot references, update tagline
- `go.mod` / `go.sum` — `go mod tidy` removes ~20 orphaned deps

---

### Task 1: Copy full repo to ~/git/gertrude

**Files:** N/A — filesystem operation before any changes

- [ ] **Step 1: Clone repo to gertrude**

```bash
git clone /home/wayne/git/ghost /home/wayne/git/gertrude
```

Expected: `Cloning into '/home/wayne/git/gertrude'...` then done.

- [ ] **Step 2: Verify all packages copied**

```bash
ls /home/wayne/git/gertrude/internal/ | wc -l
```

Expected: 26 (all original packages present).

---

### Task 2: Merge PR #148 and update local main

**Files:** N/A — git operations

- [ ] **Step 1: Wait for CI green, then merge**

```bash
gh pr checks 148
```

Expected: `build-and-test pass`, `analyze pass`, `extension pass`, `CodeQL pass`. If any fail, fix before proceeding.

- [ ] **Step 2: Merge and delete branch**

```bash
gh pr merge 148 --squash --delete-branch
```

- [ ] **Step 3: Pull main locally and clean up**

```bash
git checkout main && git pull origin main && git remote prune origin
```

---

### Task 3: Create the strip-down branch

**Files:** N/A

- [ ] **Step 1: Create branch from clean main**

```bash
git checkout -b refactor/mcp-only
```

---

### Task 4: Delete all removed packages and vscode-ghost

**Files:** 16 directories deleted + vscode-ghost/

- [ ] **Step 1: Delete in one shot**

```bash
rm -rf \
  internal/tui \
  internal/telegram \
  internal/voice \
  internal/google \
  internal/server \
  internal/scheduler \
  internal/github \
  internal/briefing \
  internal/calendar \
  internal/mdv2 \
  internal/orchestrator \
  internal/mode \
  internal/project \
  internal/prompt \
  internal/tool \
  internal/simulation \
  vscode-ghost
```

- [ ] **Step 2: Verify exactly 10 packages remain**

```bash
ls internal/
```

Expected output (exactly these 10, in any order):
```
ai  claudeimport  config  embedding  mcpinit  mcpserver  memory  provider  reflection  selfupdate
```

---

### Task 5: Rewrite cmd/ghost/main.go

**Files:** `cmd/ghost/main.go` — complete replacement

- [ ] **Step 1: Write the new main.go**

```go
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
```

- [ ] **Step 2: Verify it compiles**

```bash
go build ./cmd/ghost/
```

Expected: no output (success). If there are missing imports or type errors, check that all deleted packages are truly gone and no reference remains in the kept packages.

- [ ] **Step 3: Run go vet**

```bash
go vet ./...
```

Expected: no output.

---

### Task 6: Run go mod tidy

**Files:** `go.mod`, `go.sum`

- [ ] **Step 1: Tidy dependencies**

```bash
go mod tidy
```

Expected: no errors. The following direct deps should be removed from go.mod (verify with `cat go.mod`):
- `charm.land/bubbles/v2`
- `charm.land/bubbletea/v2`
- `charm.land/glamour/v2`
- `charm.land/lipgloss/v2`
- `github.com/BourgeoisBear/rasterm`
- `github.com/emersion/go-webdav`
- `github.com/go-chi/chi/v5`
- `github.com/go-co-op/gocron/v2`
- `github.com/go-telegram/bot`
- `github.com/google/go-github/v68`
- `github.com/olebedev/when`
- `golang.org/x/term`
- `google.golang.org/api`

- [ ] **Step 2: Build and test after tidy**

```bash
go build ./... && go test ./...
```

Expected: all tests pass. The test count will be significantly smaller (removed packages had tests).

---

### Task 7: Update Dockerfile and README

**Files:** `Dockerfile`, `README.md`

- [ ] **Step 1: Update Dockerfile CMD**

Change line 14 of `Dockerfile` from:
```dockerfile
CMD ["serve"]
```
to:
```dockerfile
CMD ["mcp"]
```

- [ ] **Step 2: Update README tagline and Quick Start**

In `README.md`:

a. The tagline line (around line 7) currently reads:
> MCP memory server for Claude Code, Cursor, and any MCP client. Pure Go. Single binary. No external services required.

Keep this line — it's already accurate.

b. Find any reference to `ghost serve` and remove it. Search with:
```bash
grep -n "ghost serve\|serve\|Telegram\|telegram\|HTTP daemon\|http daemon\|TUI\|bubbletea\|voice\|Google Calendar\|Gmail\|briefing" README.md
```

Remove or rewrite any section that describes `ghost serve`, the Telegram bot, TUI, voice, HTTP daemon, Google Calendar/Gmail, or briefing. The Docker section should now use `ghost mcp` not `ghost serve`.

c. Add a one-liner note near the top under the CI badges (before "## Why Ghost?"):
```markdown
> **v0.8.0:** Ghost is now MCP-only. The standalone AI assistant, TUI, Telegram bot, and HTTP server have been removed. Ghost's sole job is persistent memory for Claude Code and other MCP clients.
```

- [ ] **Step 3: Verify README has no serve/TUI references**

```bash
grep -n "ghost serve\|runServe\|-no-tui\|bubbletea\|Telegram\|telegram" README.md
```

Expected: no matches (or matches only in historical/changelog context).

---

### Task 8: Build, vet, test — final verification

**Files:** N/A

- [ ] **Step 1: Full build**

```bash
CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=v0.8.0-dev" -o /tmp/ghost-test ./cmd/ghost
```

Expected: binary produced at `/tmp/ghost-test`.

- [ ] **Step 2: Smoke test the binary**

```bash
/tmp/ghost-test version
/tmp/ghost-test help
/tmp/ghost-test mcp --help 2>&1 || true
```

Expected:
```
ghost v0.8.0-dev

ghost v0.8.0-dev — MCP memory server for Claude Code
...
```

- [ ] **Step 3: Full test suite**

```bash
go test ./... -count=1
```

Expected: all packages pass. If `internal/ai` tests fail due to missing API key, that's expected in CI — they should already be skipped with `t.Skip`.

- [ ] **Step 4: Vet**

```bash
go vet ./...
```

Expected: no output.

- [ ] **Step 5: Cleanup test binary**

```bash
rm /tmp/ghost-test
```

---

### Task 9: Commit, PR, merge, and tag v0.8.0

**Files:** all modified files

- [ ] **Step 1: Stage and commit**

```bash
git add -A
git commit -m "refactor: strip to MCP-only — remove TUI, serve, telegram, voice, google, scheduler, github, briefing

Removes 16 internal packages and vscode-ghost/. Ghost is now a focused
MCP memory server: ghost mcp, ghost mcp init/status, ghost hook, ghost
reflect, ghost upgrade. No standalone AI assistant, no HTTP daemon.

go mod tidy removes charm.land/*, go-telegram/bot, google.golang.org/api,
chi, gocron, go-github, go-webdav, and all transitive TUI/bot deps."
```

- [ ] **Step 2: Push and create PR**

```bash
git push -u origin refactor/mcp-only
gh pr create \
  --title "refactor: strip Ghost to MCP-only (v0.8.0)" \
  --body "Removes 16 internal packages and the VSCode extension. Ghost becomes a focused MCP memory server with 6 subcommands: mcp, mcp init, mcp status, hook, reflect, upgrade.

**Removed:** tui, telegram, voice, google, server, scheduler, github, briefing, calendar, mdv2, orchestrator, mode, project, prompt, tool, simulation, vscode-ghost/

**Kept:** memory, mcpserver, mcpinit, provider, embedding, reflection, claudeimport, config, ai, selfupdate

**go mod tidy:** drops charm.land/*, go-telegram/bot, google.golang.org/api, chi, gocron, go-github, go-webdav, and all TUI/bot transitive deps.

**Release:** v0.8.0 tagged after merge."
```

- [ ] **Step 3: Wait for CI, then merge**

```bash
gh pr checks <PR_NUMBER> --watch
gh pr merge <PR_NUMBER> --squash --delete-branch
```

- [ ] **Step 4: Pull main and tag v0.8.0**

```bash
git checkout main && git pull origin main
git tag v0.8.0
git push origin v0.8.0
```

Expected: goreleaser GitHub Actions workflow triggers on the tag push and produces 6 release artifacts (linux/darwin/windows × amd64/arm64) + checksums.

- [ ] **Step 5: Verify release**

```bash
gh release view v0.8.0
```

Expected: release page with 6 binary archives, checksums.txt, auto-generated changelog showing the refactor commit.
