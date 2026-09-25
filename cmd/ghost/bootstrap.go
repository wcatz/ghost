package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// configHandling says what bootstrap does when the config file exists but does
// not parse.
type configHandling int

const (
	// failOnConfig is for CLI subcommands: the user asked for a specific
	// command, and running it against half the intended configuration — or
	// against defaults they did not choose — is worse than stopping with an
	// error that names the file.
	failOnConfig configHandling = iota

	// warnOnConfig is for `ghost mcp`, the long-lived server a host client
	// spawns on the user's behalf. Exiting there does not fail a command, it
	// leaves their editor with no Ghost tools at all, because of a typo in a
	// file they may not know exists. It warns and serves the defaults.
	warnOnConfig
)

// configOnLoadError returns what bootstrap should do when config.Load failed:
// the Config to carry on with, and whether the caller should exit on err
// instead. Split out of bootstrap so the policy is testable — the exit path
// itself cannot be exercised from a test.
func configOnLoadError(onBadConfig configHandling, err error) (*config.Config, bool) {
	if onBadConfig == failOnConfig {
		return nil, true
	}
	return config.FallbackConfig(), false
}

// bootstrap loads config, opens the database, and returns the wiring every
// command shares. logWriter/logLevel come from the caller: interactive CLI
// commands log INFO to stderr, while the MCP server resolves a quiet default
// (see mcpLogConfig) so stderr stays clean for MCP clients that surface it.
// onBadConfig decides what a config file that does not parse does to this
// command — see configHandling.
func bootstrap(logWriter io.Writer, logLevel slog.Level, onBadConfig configHandling) (*config.Config, *slog.Logger, *memory.Store) {
	logger := slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: logLevel}))

	configPath, created, err := config.EnsureConfigFile()
	if err != nil {
		logger.Warn("could not create config file", "error", err)
	} else if created {
		logger.Info("created config file", "path", configPath)
	}

	cfg, err := config.Load()
	if err != nil {
		fallback, fatal := configOnLoadError(onBadConfig, err)
		if fatal {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		// The server stays up on the environment plus the compiled defaults.
		// The Warn and every
		// config warning from the same Load call reach this process's log
		// channel — stderr, or GHOST_LOG_FILE when set, which runMCP has
		// already pointed the warning sink at (see config.SetWarningWriter) —
		// and `ghost mcp status` prints the same error on demand.
		logger.Warn("config could not be loaded; serving the environment and built-in defaults", "error", err)
		cfg = fallback
	}

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
	store.SetDemotionThreshold(cfg.Linking.DemotionThreshold)
	store.SetVectorMinSimilarity(float32(cfg.Search.MinSimilarity))

	if err := store.SeedGlobalMemories(context.Background()); err != nil {
		logger.Warn("seed global memories", "error", err)
	}

	return cfg, logger, store
}

// mcpLogConfig resolves the log writer and minimum level for `ghost mcp`.
// MCP protocol traffic occupies stdout, so stderr must stay clean: when the
// server is spawned by a client (stderr not a terminal), routine INFO logs
// are dropped and only WARN+ reaches stderr, unless GHOST_LOG_FILE redirects
// the full stream to a file. Interactive runs (stderr is a terminal) keep
// INFO on stderr. GHOST_DEBUG always restores Debug level. The returned
// closeLog is non-nil when a file was opened and must be called on shutdown.
func mcpLogConfig(stderrTerminal bool) (io.Writer, slog.Level, func()) {
	level := slog.LevelInfo
	writer := io.Writer(os.Stderr)
	var closeLog func()

	if path := os.Getenv("GHOST_LOG_FILE"); path != "" {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot open GHOST_LOG_FILE %q: %v\n", path, err)
			if !stderrTerminal {
				level = slog.LevelWarn
			}
		} else {
			writer = f
			closeLog = func() { _ = f.Close() }
		}
	} else if !stderrTerminal {
		level = slog.LevelWarn
	}

	if os.Getenv("GHOST_DEBUG") != "" {
		level = slog.LevelDebug
	}
	return writer, level, closeLog
}

// cliLogLevel is the stderr log level for interactive CLI commands: Info,
// or Debug when GHOST_DEBUG is set.
func cliLogLevel() slog.Level {
	if os.Getenv("GHOST_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

// isTerminal reports whether stdin is connected to a terminal.
func isTerminal() bool {
	return isTerminalFile(os.Stdin)
}

// isTerminalFile reports whether f is connected to a terminal.
func isTerminalFile(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
