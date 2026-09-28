package bench

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// MemorySpec is one dataset memory. Key is a stable human-authored identifier
// used to reference the memory from query relevance maps and the embedding
// fixture; it is NOT the store's generated ID.
//
// The three validity stamps are the corpus's exercise of validity itself: a
// corpus in which every row states no window cannot tell a retrieval stage that
// honours one from one that ignores it, because both produce the same answer.
// Pointers, because the store's own columns are nullable and a fixture that
// could not say "no claim" would have to state a claim instead — the same
// distinction the tools draw when a caller omits an argument.
type MemorySpec struct {
	Key        string   `json:"key"`
	Category   string   `json:"category"`
	Content    string   `json:"content"`
	Importance float32  `json:"importance"`
	Tags       []string `json:"tags,omitempty"`
	ValidFrom  *string  `json:"valid_from,omitempty"`
	ValidUntil *string  `json:"valid_until,omitempty"`
	VerifiedAt *string  `json:"verified_at,omitempty"`
	// AgeDays backdates created_at. It is 0 on the headline dataset, which is
	// what makes the decay factor identical across every candidate there and
	// the headline table blind to decay; the ranking-state suite sets it
	// (testdata/withstate_memories.jsonl).
	AgeDays int `json:"age_days,omitempty"`
	// Supersedes names the keys this memory REPLACES — the direction the demote
	// consumes (newer -> older), written through store.CreateLink with the same
	// relation and source `ghost supersede --apply` uses. Empty on the headline
	// dataset, whose corpus holds no supersession edge at all.
	Supersedes []string `json:"supersedes,omitempty"`
}

// QuerySpec is one dataset query. Rel maps memory Keys to graded relevance.
type QuerySpec struct {
	Name string         `json:"name"`
	Text string         `json:"text"`
	Rel  map[string]int `json:"rel"`
}

// Dataset is a self-contained benchmark: a project name, its memories, the
// graded queries over them, and the no-answer queries nothing in it answers (see
// falsepositive.go — those are excluded from every graded ratio and measured
// separately).
type Dataset struct {
	Project   string
	Memories  []MemorySpec
	Queries   []QuerySpec
	Negatives []NegativeQuery
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

// supersedesCycle returns a readable cycle in the corpus's supersedes graph, or
// "" when it is acyclic — a three-colour depth-first walk, so the graph's own
// size is what bounds it rather than the corpus being small by convention.
//
// Star links are acyclic and must stay legal: `k8s_ver_v3` supersedes both
// `k8s_ver_v2` and `k8s_ver_v1`, and `k8s_ver_v2` supersedes `k8s_ver_v1`. Only a
// path that comes back to a key still on the stack is a cycle.
func supersedesCycle(mems []MemorySpec) string {
	edges := make(map[string][]string, len(mems))
	for _, m := range mems {
		edges[m.Key] = append(edges[m.Key], m.Supersedes...)
	}
	const (
		white = 0 // unvisited
		grey  = 1 // on the current path
		black = 2 // finished
	)
	colour := make(map[string]int, len(mems))
	var path []string
	var walk func(string) string
	walk = func(key string) string {
		switch colour[key] {
		case grey:
			for i := len(path) - 1; i >= 0; i-- {
				if path[i] == key {
					return strings.Join(append(append([]string{}, path[i:]...), key), " -> ")
				}
			}
			return key
		case black:
			return ""
		}
		colour[key] = grey
		path = append(path, key)
		for _, next := range edges[key] {
			if next == key {
				continue // a self-edge is its own error, with a better message
			}
			if cycle := walk(next); cycle != "" {
				return cycle
			}
		}
		path = path[:len(path)-1]
		colour[key] = black
		return ""
	}
	for _, m := range mems {
		if cycle := walk(m.Key); cycle != "" {
			return cycle
		}
	}
	return ""
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

// LoadDatasetFiles loads a dataset from a directory containing memories.jsonl,
// queries.jsonl and negative_queries.jsonl.
func LoadDatasetFiles(dir, project string) (Dataset, error) {
	mems, err := loadFile(dir+"/memories.jsonl", LoadMemories)
	if err != nil {
		return Dataset{}, err
	}
	qs, err := loadFile(dir+"/queries.jsonl", LoadQueries)
	if err != nil {
		return Dataset{}, err
	}
	negs, err := loadFile(dir+"/negative_queries.jsonl", LoadNegatives)
	if err != nil {
		return Dataset{}, err
	}
	return Dataset{Project: project, Memories: mems, Queries: qs, Negatives: negs}, nil
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
//
// db is the same connection store was built on, and it is used for exactly one
// thing a store cannot be asked to do through its API: backdating created_at,
// because Create always stamps now. The supersedes edges go through
// store.CreateLink, the production writer, so a fixture describes a state a
// store could actually hold rather than one only raw SQL can produce.
func Seed(ctx context.Context, store *memory.Store, db *sql.DB, ds Dataset, vecs Vectors) ([]Query, error) {
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
			ValidFrom: m.ValidFrom, ValidUntil: m.ValidUntil, VerifiedAt: m.VerifiedAt,
		})
		if err != nil {
			return nil, fmt.Errorf("create memory %q: %w", m.Key, err)
		}
		if m.AgeDays > 0 {
			if err := backdate(ctx, db, id, m.AgeDays); err != nil {
				return nil, fmt.Errorf("backdate %q: %w", m.Key, err)
			}
		}
		if err := store.StoreEmbedding(ctx, id, vec, "bench"); err != nil {
			return nil, fmt.Errorf("embed memory %q: %w", m.Key, err)
		}
		keyToID[m.Key] = id
	}

	// After every row exists, so a supersedes edge can only name a key this
	// dataset really holds: a typo is a load error, not a demote that silently
	// does nothing. A self-edge and a cycle are refused here for the same reason
	// — a store `ghost supersede --apply` builds can hold neither, and the demote
	// cannot order a cycle: both of its rows would be penalised by the other and
	// neither would sink.
	//
	// The check is an acyclicity test over the whole key graph, NOT "one
	// superseder per target": a star update chain (v3 supersedes v2 and v1, v2
	// supersedes v1) is exactly the shape the demote is built for and has two
	// superseders on v1.
	if cycle := supersedesCycle(ds.Memories); cycle != "" {
		return nil, fmt.Errorf("supersedes cycle in the dataset: %s", cycle)
	}
	for _, m := range ds.Memories {
		for _, older := range m.Supersedes {
			target, ok := keyToID[older]
			if !ok {
				return nil, fmt.Errorf("memory %q supersedes unknown key %q", m.Key, older)
			}
			if older == m.Key {
				return nil, fmt.Errorf("memory %q supersedes itself", m.Key)
			}
			if err := store.CreateLink(ctx, keyToID[m.Key], target, "supersedes", 1.0, "llm"); err != nil {
				return nil, fmt.Errorf("link %q supersedes %q: %w", m.Key, older, err)
			}
		}
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
