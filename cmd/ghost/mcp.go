package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/embedding"
	"github.com/wcatz/ghost/internal/linking"
	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/mcpserver"
)

// runMCP starts ghost as an MCP server on stdio.
func runMCP() {
	if isTerminal() {
		fmt.Fprintln(os.Stderr, "Starting MCP server on stdio (meant to be called by Claude Code).")
		fmt.Fprintln(os.Stderr, "To set up the integration, run: ghost mcp init")
		fmt.Fprintln(os.Stderr, "")
	}
	logWriter, logLevel, closeLog := mcpLogConfig(isTerminalFile(os.Stderr))
	if closeLog != nil {
		defer closeLog()
	}
	// Config warnings go where this process's logs go, not to raw stderr: stderr
	// belongs to the client protocol here, which is exactly what GHOST_LOG_FILE
	// exists to keep clean. Set before any goroutine starts — SetWarningWriter
	// is not synchronised — and restored before closeLog closes the file.
	defer config.SetWarningWriter(logWriter)()
	cfg, logger, store := bootstrap(logWriter, logLevel, warnOnConfig)
	defer store.Close() //nolint:errcheck

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	srv := mcpserver.New(store, logger, version)
	// Hand the loaded CLI config to ghost_resolve: configured binaries win
	// over PATH, and cli.model_resolve is applied constructor-level per spawn
	// (the server is long-lived, so no env mutation).
	srv.SetResolveCLI(cfg.CLI)

	if cfg.Embedding.Enabled {
		embedClient := embedding.NewClient(cfg.Embedding.OllamaURL, cfg.Embedding.Model, cfg.Embedding.Dimensions)
		// A second config.DataDir() call here is deliberate and cheap (an
		// idempotent MkdirAll, same precedent as internal/mcpinit/hook.go): it
		// gives the embedding worker the data directory to write its
		// Ollama-down marker into without threading dataDir through
		// bootstrap()'s return values. If it fails, dataDir stays "" and the
		// worker just skips that bookkeeping (see embedding.NewWorker) — worth
		// warning about but never fatal, since bootstrap() already succeeded
		// with its own DataDir() call.
		dataDir, err := config.DataDir()
		if err != nil {
			logger.Warn("mcp: cannot resolve data dir, Ollama-down marker disabled", "error", err)
		}
		embedWorker := embedding.NewWorker(embedClient, store, logger, 2*time.Minute, dataDir)
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

// parseMCPClient parses the --client flag value (either "--client NAME" or
// "--client=NAME") from args, returning the name or an error when the flag is
// present with no value. The caller supplies the default for an absent flag.
func parseMCPClient(args []string) (string, error) {
	var client string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--client":
			if i+1 >= len(args) {
				return "", fmt.Errorf("--client requires a value (claude, opencode, codex, goose, or all)")
			}
			client = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--client="):
			client = strings.TrimPrefix(args[i], "--client=")
			if client == "" {
				return "", fmt.Errorf("--client requires a value (claude, opencode, codex, goose, or all)")
			}
		}
	}
	return client, nil
}

// clientTarget pairs a display name with its `mcp init` installer.
type clientTarget struct {
	name   string
	binary string // binary name to detect on PATH
	run    func(w io.Writer, dryRun bool) error
}

// mcpInitTargets lists every installable client in run order.
func mcpInitTargets() []clientTarget {
	return []clientTarget{
		{"claude", "claude", mcpinit.Run},
		{"opencode", "opencode", mcpinit.RunOpencode},
		{"codex", "codex", mcpinit.RunCodex},
		{"goose", "goose", mcpinit.RunGoose},
	}
}

// detectClients returns the names of clients whose binaries are found on PATH.
func detectClients() []string {
	var found []string
	for _, t := range mcpInitTargets() {
		if _, err := exec.LookPath(t.binary); err == nil {
			found = append(found, t.name)
		}
	}
	return found
}

// runAllClients installs ghost into every target sequentially, continuing past
// individual failures so one broken client never blocks the rest. Each target's
// installer output goes to stdout under a banner; failures go to stderr. Returns
// the names of failed targets (empty when all succeeded).
func runAllClients(stdout, stderr io.Writer, dryRun bool, targets []clientTarget) []string {
	var failed []string
	for _, t := range targets {
		_, _ = fmt.Fprintf(stdout, "\n=== %s ===\n", t.name)
		if err := t.run(stdout, dryRun); err != nil {
			_, _ = fmt.Fprintf(stderr, "error (%s): %v\n", t.name, err)
			failed = append(failed, t.name)
		}
	}
	return failed
}

// runMCPInit configures an MCP client to use Ghost as its memory system.
// When --client is omitted, detects which clients are on PATH and installs
// for all of them (or just the one found). --client all installs into every
// supported client unconditionally.
func runMCPInit() {
	client, err := parseMCPClient(os.Args[3:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	dryRun := false
	for _, a := range os.Args[3:] {
		if a == "--dry-run" {
			dryRun = true
		}
	}

	// Auto-detect when --client is omitted.
	if client == "" {
		detected := detectClients()
		switch len(detected) {
		case 0:
			fmt.Fprintf(os.Stderr, "error: no supported MCP client found on PATH\n"+
				"  Install one of: Claude Code, opencode, codex, goose\n"+
				"  Or specify explicitly: ghost mcp init --client <name>\n")
			os.Exit(1)
		case 1:
			client = detected[0]
		default:
			fmt.Fprintf(os.Stderr, "Detected clients: %s\n"+
				"  Installing for all detected clients.\n"+
				"  Use --client to target a specific one.\n\n",
				strings.Join(detected, ", "))
			failed := runAllClients(os.Stdout, os.Stderr, dryRun, mcpInitTargetsFor(detected))
			if len(failed) > 0 {
				fmt.Fprintf(os.Stderr, "\ncompleted with failures: %s\n", strings.Join(failed, ", "))
				os.Exit(1)
			}
			return
		}
	}

	if client == "all" {
		failed := runAllClients(os.Stdout, os.Stderr, dryRun, mcpInitTargets())
		if len(failed) > 0 {
			fmt.Fprintf(os.Stderr, "\ncompleted with failures: %s\n", strings.Join(failed, ", "))
			os.Exit(1)
		}
		return
	}

	// Look up the installer from the target registry — no hardcoded switch
	// needed; adding a new client to mcpInitTargets() is sufficient.
	target, ok := mcpInitTargetByName(client)
	if !ok {
		var names []string
		for _, t := range mcpInitTargets() {
			names = append(names, t.name)
		}
		fmt.Fprintf(os.Stderr, "error: unknown client %q (expected %s, or all)\n",
			client, strings.Join(names, ", "))
		os.Exit(1)
	}
	if err := target.run(os.Stdout, dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// mcpInitTargetsFor returns clientTargets filtered to the given names.
func mcpInitTargetsFor(names []string) []clientTarget {
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	var targets []clientTarget
	for _, t := range mcpInitTargets() {
		if nameSet[t.name] {
			targets = append(targets, t)
		}
	}
	return targets
}

// mcpInitTargetByName returns the clientTarget with the given name, or false
// if no such target exists. This keeps the name→installer mapping in one
// place (mcpInitTargets) so adding a new client never requires touching the
// switch statement.
func mcpInitTargetByName(name string) (clientTarget, bool) {
	for _, t := range mcpInitTargets() {
		if t.name == name {
			return t, true
		}
	}
	return clientTarget{}, false
}

// runMCPStatus checks the health of the Ghost ↔ MCP client integration.
// Defaults to Claude Code; --client opencode reports opencode-specific checks.
func runMCPStatus() {
	client, err := parseMCPClient(os.Args[3:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if client == "" {
		client = "claude"
	}

	var healthy bool
	switch client {
	case "opencode":
		healthy, err = mcpinit.StatusOpencode(os.Stdout)
	case "codex":
		healthy, err = mcpinit.StatusCodex(os.Stdout)
	case "goose":
		healthy, err = mcpinit.StatusGoose(os.Stdout)
	case "claude":
		healthy, err = mcpinit.Status(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "error: unknown client %q (expected claude, opencode, codex, or goose)\n", client)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if !healthy {
		os.Exit(1)
	}
}
