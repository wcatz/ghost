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
	return b.String()
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
