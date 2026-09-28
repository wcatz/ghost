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

// benchUsage is the help for `ghost bench`: stderr after an unknown flag (a
// usage error, exit 1), stdout for -h/--help (see handleHelp). One text for
// both, so the two can never drift.
const benchUsage = `Usage: ghost bench [--sweep]

Runs the built-in retrieval-quality benchmark (judge-free, deterministic, no
network) over the embedded dataset and prints the metric table. --sweep
grid-searches the fusion parameters and prints the ranked table instead. See
docs/benchmarks.md.
`

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
			fmt.Fprintf(os.Stderr, "error: unknown flag %q\n\n%s", arg, benchUsage)
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
	// The no-answer half of the same corpus goes into the SAME run, not a
	// second one: a query with an empty relevance map is undefined for every
	// graded ratio, so the runner measures it as a false positive instead of
	// skipping it, per condition. That is what puts the rate in the table
	// under the NDCG numbers rather than in a section about how recall cannot
	// see a leak.
	noAnswer, err := bench.NegativeQueries(ds, vecs)
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

	results, err := bench.Run(ctx, store, benchQuerySet(queries, noAnswer))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(bench.FormatResults(results))

	// The deeper half of the same measurement: the answerable contrast, the
	// per-flavor split, and the floor that would have to be set to refuse
	// every no-answer query — and what that floor costs. Report-only, the
	// baseline the abstention work needs rather than a gate.
	fp, err := bench.FalsePositives(ctx, store, bench.NoAnswerFor(results, bench.CondHybrid), queries)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(bench.FormatFalsePositives(fp, bench.CondHybrid))
}

// benchQuerySet is the graded queries and the no-answer queries as ONE set, which
// is how the runner takes them: an empty relevance map is what makes a query
// no-answer, and the runner measures those instead of skipping them. It is a
// named function because runBench cannot be reached from a test (it writes to
// stdout and exits), and this composition is the difference between a bench
// report carrying a false-positive rate and one that silently does not — a
// missing half that produces no error, only a shorter table.
func benchQuerySet(graded, noAnswer []bench.Query) []bench.Query {
	all := make([]bench.Query, 0, len(graded)+len(noAnswer))
	all = append(all, graded...)
	return append(all, noAnswer...)
}
