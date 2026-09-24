package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/wcatz/ghost/internal/bench"
	"github.com/wcatz/ghost/internal/memory"
)

// runBench implements `ghost bench` — runs the built-in retrieval-quality
// benchmark (four ablations over the embedded dataset) and prints the metric
// table. With --sweep it instead grid-searches the fusion parameters and
// prints the ranked table. Judge-free, deterministic, no network. See
// docs/benchmarks.md.
func runBench() {
	sweep := false
	for _, arg := range os.Args[2:] {
		switch arg {
		case "--sweep":
			sweep = true
		default:
			fmt.Fprintf(os.Stderr, "error: unknown flag %q\n\nUsage: ghost bench [--sweep]\n", arg)
			os.Exit(1)
		}
	}

	ds, vecs, err := bench.BuiltinDataset()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Silence internal search diagnostics (e.g. FTS term-cap warnings that go
	// through the package-level default) so the benchmark table is the only
	// output; the table is on stdout, so `ghost bench` stays pipeable.
	slog.SetDefault(logger)
	store := memory.NewStore(db, logger)
	defer store.Close() //nolint:errcheck

	ctx := context.Background()
	queries, err := bench.Seed(ctx, store, ds, vecs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if sweep {
		points, err := bench.Sweep(ctx, store, queries, bench.SweepGrid())
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(bench.FormatSweep(points))
		return
	}

	results, err := bench.Run(ctx, store, queries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(bench.FormatResults(results))
}
