package bench

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/wcatz/ghost/internal/memory"
)

// MemorySpec is one dataset memory. Key is a stable human-authored identifier
// used to reference the memory from query relevance maps and the embedding
// fixture; it is NOT the store's generated ID.
type MemorySpec struct {
	Key        string   `json:"key"`
	Category   string   `json:"category"`
	Content    string   `json:"content"`
	Importance float32  `json:"importance"`
	Tags       []string `json:"tags,omitempty"`
}

// QuerySpec is one dataset query. Rel maps memory Keys to graded relevance.
type QuerySpec struct {
	Name string         `json:"name"`
	Text string         `json:"text"`
	Rel  map[string]int `json:"rel"`
}

// Dataset is a self-contained benchmark: a project name, its memories, and the
// graded queries over them.
type Dataset struct {
	Project  string
	Memories []MemorySpec
	Queries  []QuerySpec
}

// Vectors maps a memory Key or query Name to its precomputed embedding. Stored
// as a committed fixture so CI can run the vector/hybrid conditions without
// Ollama; regenerate it from the live model when the dataset changes.
type Vectors map[string][]float32

// LoadMemories reads MemorySpec objects, one JSON per line.
func LoadMemories(r io.Reader) ([]MemorySpec, error) {
	var out []MemorySpec
	if err := decodeJSONL(r, func(raw json.RawMessage) error {
		var m MemorySpec
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		out = append(out, m)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadQueries reads QuerySpec objects, one JSON per line.
func LoadQueries(r io.Reader) ([]QuerySpec, error) {
	var out []QuerySpec
	if err := decodeJSONL(r, func(raw json.RawMessage) error {
		var q QuerySpec
		if err := json.Unmarshal(raw, &q); err != nil {
			return err
		}
		out = append(out, q)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// LoadVectors reads the embedding fixture (a single JSON object of key→vector).
func LoadVectors(r io.Reader) (Vectors, error) {
	var v Vectors
	if err := json.NewDecoder(r).Decode(&v); err != nil {
		return nil, fmt.Errorf("decode vectors: %w", err)
	}
	return v, nil
}

// decodeJSONL invokes fn once per non-blank line, decoded as raw JSON.
func decodeJSONL(r io.Reader, fn func(json.RawMessage) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		b := sc.Bytes()
		if len(b) == 0 || b[0] == '#' {
			continue
		}
		if err := fn(append(json.RawMessage(nil), b...)); err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
	}
	return sc.Err()
}

// LoadDatasetFiles loads a dataset from a directory containing memories.jsonl
// and queries.jsonl.
func LoadDatasetFiles(dir, project string) (Dataset, error) {
	mems, err := loadFile(dir+"/memories.jsonl", LoadMemories)
	if err != nil {
		return Dataset{}, err
	}
	qs, err := loadFile(dir+"/queries.jsonl", LoadQueries)
	if err != nil {
		return Dataset{}, err
	}
	return Dataset{Project: project, Memories: mems, Queries: qs}, nil
}

func loadFile[T any](path string, parse func(io.Reader) (T, error)) (T, error) {
	var zero T
	f, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer f.Close() //nolint:errcheck
	return parse(f)
}

// Seed inserts the dataset's memories into store (with their fixture
// embeddings) and returns the runnable queries with relevance maps translated
// from stable Keys to the store's generated IDs. It validates that every
// memory and query has a fixture vector and that every query references only
// known memory keys, so a malformed dataset fails loudly rather than scoring
// silently wrong.
func Seed(ctx context.Context, store *memory.Store, ds Dataset, vecs Vectors) ([]Query, error) {
	if err := store.EnsureProject(ctx, ds.Project, "/bench/"+ds.Project, ds.Project); err != nil {
		return nil, fmt.Errorf("ensure project: %w", err)
	}
	dim := 0 // shared embedding dimension; a mixed fixture is a hard error
	checkDim := func(what string, v []float32) error {
		if dim == 0 {
			dim = len(v)
		} else if len(v) != dim {
			return fmt.Errorf("%s has %d-dim vector, expected %d (regenerate embeddings)", what, len(v), dim)
		}
		return nil
	}

	keyToID := make(map[string]string, len(ds.Memories))
	for _, m := range ds.Memories {
		if m.Key == "" {
			return nil, fmt.Errorf("memory with empty key: %q", m.Content)
		}
		if _, dup := keyToID[m.Key]; dup {
			return nil, fmt.Errorf("duplicate memory key %q", m.Key)
		}
		vec, ok := vecs[m.Key]
		if !ok {
			return nil, fmt.Errorf("no fixture vector for memory key %q (regenerate embeddings)", m.Key)
		}
		if err := checkDim("memory "+m.Key, vec); err != nil {
			return nil, err
		}
		id, err := store.Create(ctx, ds.Project, memory.Memory{
			Category: m.Category, Content: m.Content, Importance: m.Importance,
			Tags: m.Tags, Source: "mcp",
		})
		if err != nil {
			return nil, fmt.Errorf("create memory %q: %w", m.Key, err)
		}
		if err := store.StoreEmbedding(ctx, id, vec, "bench"); err != nil {
			return nil, fmt.Errorf("embed memory %q: %w", m.Key, err)
		}
		keyToID[m.Key] = id
	}

	queries := make([]Query, 0, len(ds.Queries))
	for _, q := range ds.Queries {
		vec, ok := vecs[q.Name]
		if !ok {
			return nil, fmt.Errorf("no fixture vector for query %q (regenerate embeddings)", q.Name)
		}
		if err := checkDim("query "+q.Name, vec); err != nil {
			return nil, err
		}
		rel := make(Relevance, len(q.Rel))
		for key, gain := range q.Rel {
			id, ok := keyToID[key]
			if !ok {
				return nil, fmt.Errorf("query %q references unknown memory key %q", q.Name, key)
			}
			rel[id] = gain
		}
		queries = append(queries, Query{
			Name: q.Name, ProjectID: ds.Project, Text: q.Text, Vector: vec, Rel: rel,
		})
	}
	return queries, nil
}
