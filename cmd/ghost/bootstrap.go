package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/embedding"
	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/memory"
)

type configHandling int

const (
	failOnConfig configHandling = iota
	warnOnConfig
)

func configOnLoadError(onBadConfig configHandling, err error) (*config.Config, bool) {
	if onBadConfig == failOnConfig {
		return nil, true
	}
	return config.FallbackConfig(), false
}

// bootstrap loads config, opens the database, and returns the shared command wiring.
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
		logger.Warn("config could not be loaded; serving the environment and built-in defaults", "error", err)
		cfg = fallback
	}

	dataDir, err := config.DataDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: database: %v\n", err)
		os.Exit(1)
	}

	readDB, readErr := memory.OpenReadDB(dbPath)
	if readErr != nil {
		logger.Warn("read-only handle unavailable; retrieval will take its snapshot on the primary connection", "error", readErr)
	}

	store := memory.NewStoreWithRead(db, readDB, logger)
	store.SetDemotionThreshold(cfg.Linking.DemotionThreshold)
	store.SetVectorMinSimilarity(float32(cfg.Search.MinSimilarity))
	store.SetEmbeddingIdentity(embedding.VectorIdentity(cfg.Embedding.Model, cfg.Embedding.Dimensions))

	if err := store.SeedGlobalMemories(context.Background()); err != nil {
		logger.Warn("seed global memories", "error", err)
	}

	return cfg, logger, store
}

// mcpLogConfig resolves the log writer and minimum level for ghost mcp.
func mcpLogConfig(stderrTerminal bool) (io.Writer, slog.Level, func()) {
	level := slog.LevelInfo
	writer := io.Writer(os.Stderr)
	var closeLog func()

	if path := os.Getenv("GHOST_LOG_FILE"); path != "" {
		if f, err := mcpinit.OpenRotatingLog(path); err != nil {
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

// cliLogLevel returns the stderr log level for interactive CLI commands.
func cliLogLevel() slog.Level {
	if os.Getenv("GHOST_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func isTerminal() bool {
	return isTerminalFile(os.Stdin)
}

func isTerminalFile(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
