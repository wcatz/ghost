package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/wcatz/ghost/internal/replay"
)

const replayUsage = `Usage: ghost replay [--snapshot <db>] [--baseline <binary>] [--limit N] [--top-k N] [--max-queries N]
       ghost replay --rank --snapshot <db> --record <file.json>

Replays the search queries recorded in audit_log against a pinned snapshot.
The default baseline is the result set each query recorded when it ran, but
that set came from the live build's hybrid search while this command's current
side is keyword-only: a recorded-vs-current diff is only meaningful for
keyword-only recordings. On an embedding-enabled install use --baseline for a
true A/B against another build through the same rank mode. With --baseline a
second ghost build ranks the same queries through this command's rank mode
instead. The report shows, per query, top-k overlap, rank shifts and every
changed top hit. Ranking is keyword-only and offline: the snapshot's vectors
describe stored memories, not a fresh query, so no embedding endpoint is
called. --rank is that stable ranking contract for a baseline binary: it reads
one JSON query and prints a JSON array of ranked memory ids.
`

type replayOptions struct {
	snapshot   string
	baseline   string
	record     string
	limit      int
	topK       int
	maxQueries int
	rank       bool
}

// parseReplayArgs is a named function because runReplay prints to stdout and
// exits: a parser a test can drive is the only way to hold the flag contract.
func parseReplayArgs(args []string) (replayOptions, error) {
	opts := replayOptions{limit: 10, topK: 5}
	seen := map[string]bool{}
	set := func(name, value string) error {
		if seen[name] {
			return fmt.Errorf("%s given twice", name)
		}
		seen[name] = true
		if value == "" {
			return fmt.Errorf("%s requires a value", name)
		}
		return nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := strings.Cut(arg, "=")
		switch name {
		case "--rank":
			if hasValue {
				return replayOptions{}, fmt.Errorf("--rank takes no value")
			}
			if opts.rank {
				return replayOptions{}, fmt.Errorf("--rank given twice")
			}
			opts.rank = true
			continue
		default:
			if !strings.HasPrefix(name, "--") {
				return replayOptions{}, fmt.Errorf("unknown flag %q", arg)
			}
			if !hasValue {
				i++
				if i >= len(args) {
					return replayOptions{}, fmt.Errorf("%s requires a value", name)
				}
				value = args[i]
			}
		}
		switch name {
		case "--snapshot":
			if err := set(name, value); err != nil {
				return replayOptions{}, err
			}
			opts.snapshot = value
		case "--baseline":
			if err := set(name, value); err != nil {
				return replayOptions{}, err
			}
			opts.baseline = value
		case "--record":
			if err := set(name, value); err != nil {
				return replayOptions{}, err
			}
			opts.record = value
		case "--limit", "--top-k", "--max-queries":
			if err := set(name, value); err != nil {
				return replayOptions{}, err
			}
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return replayOptions{}, fmt.Errorf("%s needs a non-negative integer, got %q", name, value)
			}
			switch name {
			case "--limit":
				opts.limit = n
			case "--top-k":
				opts.topK = n
			case "--max-queries":
				opts.maxQueries = n
			}
		default:
			return replayOptions{}, fmt.Errorf("unknown flag %q", name)
		}
	}
	if opts.snapshot == "" {
		return replayOptions{}, fmt.Errorf("--snapshot is required")
	}
	if opts.rank {
		if opts.record == "" {
			return replayOptions{}, fmt.Errorf("--record is required with --rank")
		}
		return opts, nil
	}
	if opts.record != "" {
		return replayOptions{}, fmt.Errorf("--record is only valid with --rank")
	}
	return opts, nil
}

func runReplay() {
	opts, err := parseReplayArgs(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, replayUsage)
		os.Exit(1)
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if opts.rank {
		runReplayRank(opts)
		return
	}
	runReplayReport(opts)
}

func runReplayRank(opts replayOptions) {
	data, err := os.ReadFile(opts.record)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: read query record: %v\n", err)
		os.Exit(1)
	}
	var q replay.Query
	if err := json.Unmarshal(data, &q); err != nil {
		fmt.Fprintf(os.Stderr, "error: decode query record: %v\n", err)
		os.Exit(1)
	}
	store, closeFn, err := replay.OpenSnapshot(opts.snapshot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open snapshot: %v\n", err)
		os.Exit(1)
	}
	defer closeFn() //nolint:errcheck
	ids, err := (replay.StoreRunner{Store: store, Limit: opts.limit}).Rank(context.Background(), q)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: rank query: %v\n", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(ids); err != nil {
		fmt.Fprintf(os.Stderr, "error: write ranked ids: %v\n", err)
		os.Exit(1)
	}
}

func runReplayReport(opts replayOptions) {
	queries, closeFn, err := replay.LoadQueries(opts.snapshot, opts.maxQueries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: read recorded searches: %v\n", err)
		os.Exit(1)
	}
	defer closeFn() //nolint:errcheck

	baseline := replay.Runner(replay.RecordedRunner{})
	if opts.baseline != "" {
		baseline = replay.BinaryRunner{Binary: opts.baseline, Snapshot: opts.snapshot, Limit: opts.limit}
	}
	store, closeStore, err := replay.OpenSnapshot(opts.snapshot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open snapshot: %v\n", err)
		os.Exit(1)
	}
	defer closeStore() //nolint:errcheck

	if opts.baseline == "" {
		fmt.Println("note: comparing recorded hybrid result sets with a keyword-only current ranking; " +
			"a diff here is not by itself a retrieval regression — use --baseline for a true A/B")
	}
	results := replay.Run(context.Background(), queries, baseline,
		replay.StoreRunner{Store: store, Limit: opts.limit}, opts.topK)
	fmt.Print(replay.FormatReport(results))
}
