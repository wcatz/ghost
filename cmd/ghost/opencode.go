package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
)

const usageCleanupSessions = "ghost opencode cleanup-sessions [--grace <duration>] [--limit <n>] [--apply]"

const opencodeUsage = "Usage: " + usageCleanupSessions + "\n"

// parseCleanupSessionsArgs parses flags for `ghost opencode cleanup-sessions`.
func parseCleanupSessionsArgs(args []string) (ai.OpenCodeCleanupOptions, error) {
	opts := ai.OpenCodeCleanupOptions{Grace: time.Hour}
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--apply":
			opts.Apply = true
		case arg == "--grace":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("--grace needs a duration, e.g. --grace 24h (usage: %s)", usageCleanupSessions)
			}
			if err := setCleanupGrace(&opts, args[i]); err != nil {
				return opts, err
			}
		case strings.HasPrefix(arg, "--grace="):
			if err := setCleanupGrace(&opts, strings.TrimPrefix(arg, "--grace=")); err != nil {
				return opts, err
			}
		case arg == "--limit":
			i++
			if i >= len(args) {
				return opts, fmt.Errorf("--limit needs a session count (usage: %s)", usageCleanupSessions)
			}
			if err := setCleanupLimit(&opts, args[i]); err != nil {
				return opts, err
			}
		case strings.HasPrefix(arg, "--limit="):
			if err := setCleanupLimit(&opts, strings.TrimPrefix(arg, "--limit=")); err != nil {
				return opts, err
			}
		default:
			return opts, fmt.Errorf("unknown argument %q (usage: %s)", arg, usageCleanupSessions)
		}
	}
	return opts, nil
}

func setCleanupGrace(opts *ai.OpenCodeCleanupOptions, value string) error {
	d, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("invalid --grace %q (want a duration such as 1h or 30m): %w", value, err)
	}
	if d < 0 {
		return fmt.Errorf("--grace must not be negative (got %s)", d)
	}
	opts.Grace = d
	return nil
}

func setCleanupLimit(opts *ai.OpenCodeCleanupOptions, value string) error {
	n, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("invalid --limit %q (want a session count): %w", value, err)
	}
	if n < 0 {
		return fmt.Errorf("--limit must not be negative (got %d)", n)
	}
	opts.Limit = n
	return nil
}

// runOpenCodeCleanupSessions runs `ghost opencode cleanup-sessions`.
func runOpenCodeCleanupSessions(args []string) {
	opts, err := parseCleanupSessionsArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}
	ctx, stopSignals := maintenanceContext()
	defer stopSignals()
	res, err := ai.CleanupOpenCodeSessions(ctx, cfg.CLI.OpenCodeBinary, opts, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if res.Failed > 0 {
		os.Exit(1)
	}
}
