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

// BuiltinDataset returns the dataset and embedding fixture compiled into the
// binary, so `ghost bench` runs without the source tree or Ollama.
func BuiltinDataset() (Dataset, Vectors, error) {
	mems, err := LoadMemories(bytes.NewReader(builtinMemories))
	if err != nil {
		return Dataset{}, nil, fmt.Errorf("builtin memories: %w", err)
	}
	qs, err := LoadQueries(bytes.NewReader(builtinQueries))
	if err != nil {
		return Dataset{}, nil, fmt.Errorf("builtin queries: %w", err)
	}
	vecs, err := LoadVectors(bytes.NewReader(builtinVectors))
	if err != nil {
		return Dataset{}, nil, fmt.Errorf("builtin vectors: %w", err)
	}
	return Dataset{Project: "bench", Memories: mems, Queries: qs}, vecs, nil
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
	fmt.Fprintf(&b, "\n%d graded queries, %d memories. Retrieval-only, no LLM judge.\n", n, len(builtinMemoryKeys()))
	return b.String()
}

// builtinMemoryKeys parses just the memory count for the report footer.
func builtinMemoryKeys() []MemorySpec {
	mems, _ := LoadMemories(bytes.NewReader(builtinMemories))
	return mems
}
