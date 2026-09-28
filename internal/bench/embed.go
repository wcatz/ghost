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
	fmt.Fprintf(&b, "\n%d graded queries, %d memories. Retrieval-only, no LLM judge.\n", n, len(builtinMemoryKeys()))
	b.WriteString(FormatNoAnswer(summariesOf(results)))
	b.WriteString(FormatFusionGaps(results))
	return b.String()
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
		verdict := "not separable from the leg"
		if gap.Lo > 0 {
			verdict = "ahead of the leg"
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
