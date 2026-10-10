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

// benchUsage is the help for `ghost bench`: stderr after an unknown flag (a
// usage error, exit 1), stdout for -h/--help (see handleHelp). One text for
// both, so the two can never drift.
const benchUsage = `Usage: ghost bench [--sweep | --context | --passive | --audit | --cutoff-sweep | --no-answer-sweep]

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
reports withheld leakage, recall, contamination and header honesty. --audit scores
the retrieval audit instead: a scripted offline session with hand-written labels
(ids cited, memories restated, denied or saved) is judged by the audit's own
comparison, and the verdicts are scored against the labels per outcome. See
docs/benchmarks.md.
`

// The things `ghost bench` can print. A mode rather than a pair of bools
// because they are not independent: a sweep and a context measurement are two
// different reports over two different questions, and a caller asking for both
// gets neither rather than one of them.
const (
	benchModeResults  = "results"
	benchModeSweep    = "sweep"
	benchModeContext  = "context"
	benchModePassive  = "passive"
	benchModeAudit    = "audit"
	benchModeCutoff   = "cutoff-sweep"
	benchModeNoAnswer = "no-answer-sweep"
)

// benchModeOf reads the flags after the command. It is a named function because
// runBench is unreachable from a test (it writes to stdout and exits), and the
// flag table is exactly the kind of thing that rots untested: a flag added here
// and not in benchUsage is a mode a user cannot discover, and a flag parsed but
// not honoured is a mode that prints the wrong table while looking like it worked.
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
		case "--audit":
			err = set(benchModeAudit, "audit")
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

// runBench implements `ghost bench` — runs the built-in retrieval-quality
// benchmark (three ablations over the embedded dataset, plus the no-answer
// false-positive table under them) and prints the metric table. With --sweep it
// instead grid-searches the fusion parameters and prints the ranked table; with
// --context it prints the context-assembly table; with --passive the passive
// context surfaces' table. Judge-free, deterministic, no
// network. See docs/benchmarks.md.
func runBench() {
	mode, err := benchModeOf(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, benchUsage)
		os.Exit(1)
	}

	// The passive mode has its own corpus and its own store (an on-disk one, because
	// the session-start path opens a read-only handle on a path), so it returns
	// before the graded dataset is loaded and seeded: nothing below is its input.
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

	// The audit mode has its own corpus and its own scratch stores, so it returns
	// before the graded dataset is loaded too.
	if mode == benchModeAudit {
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		rep, err := bench.RunAuditTemp(context.Background())
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(bench.FormatAudit(rep))
		return
	}

	ds, vecs, err := bench.BuiltinDataset()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	// The context mode measures the block at a FIXED clock and the ablations at
	// the wall clock, and the split is deliberate in both directions. The
	// published ablation numbers depend on a corpus stamped at the moment the run
	// started — pinning that would age the corpus by however long ago the
	// constant was written and move NDCG for reasons that have nothing to do with
	// retrieval. The context table is a REPORT (#582) whose claim is that two runs
	// of one binary print the same bytes, and a report measured against the wall
	// clock cannot make it: a row's age and its validity window would both move.
	// One seed serves either mode, so the choice is a clock and nothing else.
	clock := time.Now().UTC()
	// The cutoff sweep reads the SAME clock the --context table is measured at,
	// not the wall clock: a sweep row is the block --context would print at that
	// share, and a table that moved with the calendar could neither reproduce nor
	// be compared against the report it chooses a default for.
	if mode == benchModeContext || mode == benchModeCutoff || mode == benchModeNoAnswer {
		clock = bench.ContextInstant()
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
	queries, stampedAt, err := bench.SeedAt(ctx, store, db, ds, vecs, clock)
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

	if mode == benchModeSweep {
		points, err := bench.Sweep(ctx, store, queries, bench.SweepGrid())
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(bench.FormatSweep(points))
		return
	}

	// The context mode reads the stamped instant SeedAt handed back rather than
	// the clock that was passed in: the stamp is truncated to the second, because
	// that is what memories.created_at holds, so a request measured at an
	// untruncated T would be measuring against a clock the corpus was not written
	// at. The two halves of the report are then about one instant.
	if mode == benchModeContext {
		rep, err := bench.RunContext(ctx, store, queries, stampedAt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(bench.FormatContext(rep))
		return
	}

	// The cutoff sweep reads the SAME graded corpus at the SAME fixed clock and
	// the SAME stamped instant --context uses, so a row of its table is the block
	// `ghost bench --context` would print at that share, one RunContext apart.
	if mode == benchModeCutoff {
		points, err := bench.ContextCutoffSweep(ctx, store, queries, stampedAt, bench.ContextCutoffGrid())
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(bench.FormatContextCutoffSweep(points))
		return
	}

	// The no-answer sweep reads the same graded corpus, clock and stamped instant
	// and adds the no-answer queries as the false-positive half.
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

	// The deeper half of the same measurement, read off the run above rather than
	// searched again: the answerable contrast, the per-flavor split, and the floor
	// that would have to be set to refuse every no-answer query — and what that
	// floor costs. Report-only, the baseline the abstention work needs rather than
	// a gate.
	fp, err := bench.FalsePositives(bench.NoAnswerFor(results, bench.CondHybrid), bench.PerQueryFor(results, bench.CondHybrid))
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
