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

// BuiltinDataset returns the dataset and embedding fixture compiled into the binary.
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

// FormatResults renders the ablation results as an aligned text table.
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
	b.WriteString(FormatTopRowShares(results))
	fmt.Fprintf(&b, "\n%d graded queries, %d memories. Retrieval-only, no LLM judge.\n", n, len(builtinMemoryKeys()))
	b.WriteString(FormatNoAnswer(summariesOf(results)))
	b.WriteString(FormatFusionGaps(results))
	return b.String()
}

// FormatTopRowShares renders the top-row shares and the R@1 ceiling.
func FormatTopRowShares(results []Result) string {
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
		fmt.Fprintf(&b, "  relevant row in top 5      %-12s %.3f\n", r.Condition, r.Top5Relevant)
	}
	if c, ok := ceilingOf(results); ok {
		fmt.Fprintf(&b, "  R@1 ceiling on these labels %.3f\n", c)
	}
	b.WriteString("R@1 divides by the labelled rows, not by 1, so a query grading four\n")
	b.WriteString("scores 0.250 at R@1 however well it ranks. The ceiling is what a perfect\n")
	b.WriteString("ranking posts on these labels; read every R@1 above against it.\n")
	return b.String()
}

func ceilingOf(results []Result) (float64, bool) {
	for _, r := range results {
		if r.Queries > 0 {
			return r.Recall1Ceiling, true
		}
	}
	return 0, false
}

// FormatFusionGaps renders the paired per-query NDCG@10 difference between hybrid and each leg.
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
			fmt.Fprintf(&b, "%-28s  not comparable: %v\n", "fused - "+lr.Condition, err)
			rows++
			continue
		}
		if gap.Queries == 0 {
			fmt.Fprintf(&b, "%-28s  not comparable: no paired queries\n", "fused - "+lr.Condition)
			rows++
			continue
		}
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

func resultFor(results []Result, cond string) (Result, bool) {
	for _, r := range results {
		if r.Condition == cond {
			return r, true
		}
	}
	return Result{}, false
}

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

func builtinMemoryKeys() []MemorySpec {
	mems, _ := LoadMemories(bytes.NewReader(builtinMemories))
	return mems
}
