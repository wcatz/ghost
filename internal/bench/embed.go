package bench

import (
	"bytes"
	_ "embed"
	"fmt"
)

//go:embed testdata/memories.jsonl
var builtinMemories []byte

//go:embed testdata/queries.jsonl
var builtinQueries []byte

//go:embed testdata/embeddings.json
var builtinVectors []byte

//go:embed testdata/negative_queries.jsonl
var builtinNegativeQueries []byte

//go:embed testdata/negative_embeddings.json
var builtinNegativeVectors []byte

// BuiltinDataset returns the dataset and embedding fixture compiled into the
// binary, so `ghost bench` runs without the source tree or Ollama. The no-answer
// queries' vectors are merged into the same map: they are queried against this
// same corpus, and the maintenance-state suite keeps its own fixture.
func BuiltinDataset() (Dataset, Vectors, error) {
	mems, err := LoadMemories(bytes.NewReader(builtinMemories))
	if err != nil {
		return Dataset{}, nil, fmt.Errorf("builtin memories: %w", err)
	}
	qs, err := LoadQueries(bytes.NewReader(builtinQueries))
	if err != nil {
		return Dataset{}, nil, fmt.Errorf("builtin queries: %w", err)
	}
	negs, err := LoadNegatives(bytes.NewReader(builtinNegativeQueries))
	if err != nil {
		return Dataset{}, nil, fmt.Errorf("builtin no-answer queries: %w", err)
	}
	vecs, err := LoadVectors(bytes.NewReader(builtinVectors))
	if err != nil {
		return Dataset{}, nil, fmt.Errorf("builtin vectors: %w", err)
	}
	negVecs, err := LoadVectors(bytes.NewReader(builtinNegativeVectors))
	if err != nil {
		return Dataset{}, nil, fmt.Errorf("builtin no-answer vectors: %w", err)
	}
	for name, vec := range negVecs {
		vecs[name] = vec
	}
	return Dataset{Project: "bench", Memories: mems, Queries: qs, Negatives: negs}, vecs, nil
}

// FormatResults renders the ablation results as an aligned text table, with the
// no-answer false-positive table directly beneath it. Beside rather than
// elsewhere on purpose: a graded table on its own reads as if recall were the
// whole of retrieval quality, and the false-positive rate is the half that says
// what the recall was retrieved at the cost of.
func FormatResults(results []Result) string {
	var b bytes.Buffer
	n := 0
	if len(results) > 0 {
		n = results[0].Queries
	}
	fmt.Fprintf(&b, "%-14s %7s %7s %7s %8s %8s\n", "condition", "R@1", "R@5", "R@10", "MRR@10", "NDCG@10")
	for _, r := range results {
		fmt.Fprintf(&b, "%-14s %7.3f %7.3f %7.3f %8.3f %8.3f\n",
			r.Condition, r.Recall1, r.Recall5, r.Recall10, r.MRR10, r.NDCG10)
	}
	// The table above is unchanged, line for line, because other tools
	// parse it; the shares and the ceiling are new lines after it, which
	// is where a reader looks for "what else did this table not say".
	b.WriteString(FormatTopRowShares(results))
	fmt.Fprintf(&b, "\n%d graded queries, %d memories. Retrieval-only, no LLM judge.\n", n, len(builtinMemoryKeys()))
	b.WriteString(FormatNoAnswer(summariesOf(results)))
	b.WriteString(FormatFusionGaps(results))
	return b.String()
}

// FormatTopRowShares renders the two top-row shares and the R@1 ceiling,
// printed after the table above it. They are a separate block rather than
// two more columns in that table because the table's lines are fixed
// width other tools parse, and because the ceiling is a property of the
// labels rather than of a condition: it is the same figure for all three,
// so printing it per row would print it three times and invite reading it
// as a difference between them.
//
// Both shares are fractions of the ANSWERABLE queries, the same
// population the R@1 column divides by — a query nothing in the corpus
// answers has no top row to be relevant, and counting it in would put a
// ceiling on a share that has none. Conditions that scored nothing are
// skipped rather than printed as 0.000, which would read as a measured
// absence.
func FormatTopRowShares(results []Result) string {
	// The population the shares are read against is the first result that
	// actually scored something, which is the same source FormatResults
	// reads the graded count from.
	answerable := 0
	for _, r := range results {
		if r.Queries > 0 {
			answerable = r.Queries
			break
		}
	}
	if answerable == 0 {
		return ""
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "\ntop-row shares over the %d answerable queries\n", answerable)
	for _, r := range results {
		if r.Queries == 0 {
			continue
		}
		fmt.Fprintf(&b, "  top row relevant           %-12s %.3f\n", r.Condition, r.TopRowRelevant)
		fmt.Fprintf(&b, "  top row best-labelled      %-12s %.3f\n", r.Condition, r.TopRowBestLabelled)
	}
	if c, ok := ceilingOf(results); ok {
		fmt.Fprintf(&b, "  R@1 ceiling on these labels %.3f\n", c)
	}
	b.WriteString("R@1 divides by the labelled rows, not by 1, so a query grading four\n")
	b.WriteString("scores 0.250 at R@1 however well it ranks. The ceiling is what a perfect\n")
	b.WriteString("ranking posts on these labels; read every R@1 above against it.\n")
	return b.String()
}

// ceilingOf returns the R@1 ceiling of the first result that carries one,
// for the single line the shares report is read against. The ceiling is
// identical over every condition that scored the same query set, so a
// condition's own value is the run's; a result with no scored queries
// has none, and reporting one anyway would print a confident 0.000 for a
// set that was never measured.
func ceilingOf(results []Result) (float64, bool) {
	for _, r := range results {
		if r.Queries > 0 {
			return r.Recall1Ceiling, true
		}
	}
	return 0, false
}

// FormatFusionGaps renders the paired per-query NDCG@10 difference between the
// fused condition and each single leg, with a percentile bootstrap interval.
//
// It is here, and not only in the regression test's log, because the fusion
// margin is the headline number in docs/benchmarks.md and README.md: a published
// interval that no command prints is a claim a reader has to take on trust, which
// is the defect #561 exists to remove. `ghost bench` now prints the same figures
// TestBenchRegressionFloors gates on, from the same function, so the doc and the
// build cannot disagree about them.
//
// The gate is one-sided and deliberately weaker than what this prints — it
// requires the lower edge to clear -fusionTolerance — so a row here that excludes
// zero is the claim "fusion earns its keep", and a row that does not is the claim
// "fusion is not materially worse". Both are reported; neither is enforced here.
func FormatFusionGaps(results []Result) string {
	hybrid, ok := resultFor(results, CondHybrid)
	if !ok {
		return ""
	}
	var b bytes.Buffer
	b.WriteString("\nFused vs one leg at a time, paired per query (95% percentile bootstrap, ")
	fmt.Fprintf(&b, "%d resamples):\n", bootstrapResamples)
	fmt.Fprintf(&b, "%-28s %9s %9s %9s  %s\n", "comparison", "mean", "lo", "hi", "queries")
	rows := 0
	for _, leg := range []string{CondVector, CondFTS} {
		lr, ok := resultFor(results, leg)
		if !ok {
			continue
		}
		gap, err := CompareFusion(hybrid, lr)
		if err != nil {
			// The two conditions did not score the same query set, so there is no
			// pairing and no interval. Refusing to print a row is the honest
			// answer; a zero row would read as "identical".
			fmt.Fprintf(&b, "%-28s  not comparable: %v\n", "fused - "+lr.Condition, err)
			rows++
			continue
		}
		if gap.Queries == 0 {
			// CompareFusion is happy to pair two empty PerQuery slices — the names
			// all match, because there are none — and bootstrapMeanCI returns
			// (0, 0, 0) for an empty sample. The result is a confident-looking
			// all-zero row that reads as "identical" and is in fact "not measured",
			// printed under a header that says how many queries were graded.
			fmt.Fprintf(&b, "%-28s  not comparable: no paired queries\n", "fused - "+lr.Condition)
			rows++
			continue
		}
		// Three cases, not two: an interval entirely BELOW zero is separable too,
		// and it is the case fusionGate fails the build on. A two-case verdict
		// would have the command and the gate describing one measurement two
		// opposite ways, and would contradict the footnote this table prints.
		var verdict string
		switch {
		case gap.Lo > 0:
			verdict = "ahead of the leg"
		case gap.Hi < 0:
			verdict = "behind the leg"
		default:
			verdict = "not separable from the leg"
		}
		fmt.Fprintf(&b, "%-28s %+9.4f %+9.4f %+9.4f  %4d  %s\n",
			"fused - "+lr.Condition, gap.Mean, gap.Lo, gap.Hi, gap.Queries, verdict)
		rows++
	}
	if rows == 0 {
		return ""
	}
	b.WriteString("A row whose interval contains 0.0 does not separate the two conditions; the sort above cannot either.\n")
	return b.String()
}

// resultFor finds a condition's Result by name.
func resultFor(results []Result, cond string) (Result, bool) {
	for _, r := range results {
		if r.Condition == cond {
			return r, true
		}
	}
	return Result{}, false
}

// summariesOf reduces each condition's no-answer measurements to its summary, in
// the order the conditions were run.
func summariesOf(results []Result) []NoAnswerSummary {
	out := make([]NoAnswerSummary, 0, len(results))
	for _, r := range results {
		if len(r.NoAnswer) == 0 {
			continue
		}
		out = append(out, SummarizeNoAnswer(r.Condition, r.NoAnswer))
	}
	return out
}

// builtinMemoryKeys parses just the memory count for the report footer.
func builtinMemoryKeys() []MemorySpec {
	mems, _ := LoadMemories(bytes.NewReader(builtinMemories))
	return mems
}
