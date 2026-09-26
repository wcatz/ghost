package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
)

// usageCleanupSessions is the usage line shown for the command group and for
// any argument the parser rejects.
const usageCleanupSessions = "ghost opencode cleanup-sessions [--grace <duration>] [--limit <n>] [--apply]"

// parseCleanupSessionsArgs parses `ghost opencode cleanup-sessions` flags.
// Only --apply, --grace and --limit are recognized, and anything else is an
// error rather than being ignored: a silently misparsed flag here could turn
// a report into a deletion (or hide one), the same rule
// parseCleanScratchArgs applies to clean-scratch.
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

// runOpenCodeCleanupSessions implements `ghost opencode cleanup-sessions`: a
// one-shot removal of the pre-#568 backlog of lifecycle sessions titled
// exactly "[ghost]". Report-only by default; only --apply deletes, and a
// failed delete is reported (with a non-zero exit) after the rest have been
// attempted. The binary comes from cli.opencode_binary so the command talks to
// the same OpenCode installation the lifecycle runs use, falling back to PATH.
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
	res, err := ai.CleanupOpenCodeSessions(context.Background(), cfg.CLI.OpenCodeBinary, opts, os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if res.Failed > 0 {
		os.Exit(1)
	}
}
