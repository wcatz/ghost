package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/wcatz/ghost/internal/bench"
	"github.com/wcatz/ghost/internal/memory"
)

const benchUsage = `Usage: ghost bench [--sweep | --context | --passive | --cutoff-sweep | --no-answer-sweep]

Runs the built-in retrieval-quality benchmark (judge-free, deterministic, no
network) over the embedded dataset and prints the metric table. --sweep
grid-searches the fusion parameters and prints the ranked table instead.
--context prints the context-assembly table: what the block ghost_memory_search
returns costs, how much of it is relevant, and how much of it should never have
been in it. --cutoff-sweep sweeps the query-mode relevance cutoff over the same
--context corpus and prints, per share, the graded-relevant rows admitted,
context precision, result rate and estimated tokens per answer — the measurement
the shipped default is chosen from. --no-answer-sweep does the same for the
query-mode no-answer bar and prints, per rule and setting, the no-answer
false-positive rate, the answerable queries refused, the graded-relevant rows
admitted, context precision and estimated tokens. --passive measures the passive blocks instead
— session start, ghost context and ghost_project_context — over a synthetic
multi-project store with resolved, expired, out-of-scope and duplicate rows, and
reports withheld leakage, recall, contamination and header honesty. See
docs/benchmarks.md.
`

const (
	benchModeResults  = "results"
	benchModeSweep    = "sweep"
	benchModeContext  = "context"
	benchModePassive  = "passive"
	benchModeCutoff   = "cutoff-sweep"
	benchModeNoAnswer = "no-answer-sweep"
)

func benchModeOf(args []string) (string, error) {
	mode := benchModeResults
	set := func(next, flag string) error {
		if mode != benchModeResults && mode != next {
			return fmt.Errorf("--%s and --%s measure different things and cannot share a run", mode, flag)
		}
		mode = next
		return nil
	}
	for _, arg := range args {
		var err error
		switch arg {
		case "--sweep":
			err = set(benchModeSweep, "sweep")
		case "--context":
			err = set(benchModeContext, "context")
		case "--passive":
			err = set(benchModePassive, "passive")
		case "--cutoff-sweep":
			err = set(benchModeCutoff, "cutoff-sweep")
		case "--no-answer-sweep":
			err = set(benchModeNoAnswer, "no-answer-sweep")
		default:
			return "", fmt.Errorf("unknown flag %q", arg)
		}
		if err != nil {
			return "", err
		}
	}
	return mode, nil
}

func runBench() {
	mode, err := benchModeOf(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, benchUsage)
		os.Exit(1)
	}

	if mode == benchModePassive {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		rep, err := bench.RunPassiveTemp(context.Background(), bench.BlindNone)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(bench.FormatPassive(rep))
		return
	}

	ds, vecs, err := bench.BuiltinDataset()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	clock := time.Now().UTC()
	if mode == benchModeContext || mode == benchModeCutoff || mode == benchModeNoAnswer {
		clock = bench.ContextInstant()
	}
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	slog.SetDefault(logger)
	store := memory.NewStore(db, logger)
	defer store.Close() //nolint:errcheck

	ctx := context.Background()
	queries, stampedAt, err := bench.SeedAt(ctx, store, db, ds, vecs, clock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	noAnswer, err := bench.NegativeQueries(ds, vecs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if mode == benchModeSweep {
		points, err := bench.Sweep(ctx, store, queries, bench.SweepGrid())
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(bench.FormatSweep(points))
		return
	}

	if mode == benchModeContext {
		rep, err := bench.RunContext(ctx, store, queries, stampedAt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(bench.FormatContext(rep))
		return
	}

	if mode == benchModeCutoff {
		points, err := bench.ContextCutoffSweep(ctx, store, queries, stampedAt, bench.ContextCutoffGrid())
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(bench.FormatContextCutoffSweep(points))
		return
	}

	if mode == benchModeNoAnswer {
		points, err := bench.NoAnswerSweep(ctx, store, queries, noAnswer, stampedAt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(bench.FormatNoAnswerSweep(points))
		return
	}

	results, err := bench.Run(ctx, store, benchQuerySet(queries, noAnswer))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(bench.FormatResults(results))

	fp, err := bench.FalsePositives(bench.NoAnswerFor(results, bench.CondHybrid), bench.PerQueryFor(results, bench.CondHybrid))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(bench.FormatFalsePositives(fp, bench.CondHybrid))
}

func benchQuerySet(graded, noAnswer []bench.Query) []bench.Query {
	all := make([]bench.Query, 0, len(graded)+len(noAnswer))
	all = append(all, graded...)
	return append(all, noAnswer...)
}
