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
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// MemorySpec is one dataset memory.
type MemorySpec struct {
	Key        string   `json:"key"`
	Category   string   `json:"category"`
	Content    string   `json:"content"`
	Importance float32  `json:"importance"`
	Tags       []string `json:"tags,omitempty"`
	ValidFrom  *string  `json:"valid_from,omitempty"`
	ValidUntil *string  `json:"valid_until,omitempty"`
	VerifiedAt *string  `json:"verified_at,omitempty"`
	AgeDays    int      `json:"age_days,omitempty"`
	Supersedes []string `json:"supersedes,omitempty"`
}

// QuerySpec is one dataset query.
type QuerySpec struct {
	Name string         `json:"name"`
	Text string         `json:"text"`
	Rel  map[string]int `json:"rel"`
}

// Dataset is a self-contained benchmark.
type Dataset struct {
	Project   string
	Memories  []MemorySpec
	Queries   []QuerySpec
	Negatives []NegativeQuery
}

// Vectors maps a memory Key or query Name to its precomputed embedding.
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

// LoadVectors reads the embedding fixture.
func LoadVectors(r io.Reader) (Vectors, error) {
	var v Vectors
	if err := json.NewDecoder(r).Decode(&v); err != nil {
		return nil, fmt.Errorf("decode vectors: %w", err)
	}
	return v, nil
}

func supersedesCycle(mems []MemorySpec) string {
	edges := make(map[string][]string, len(mems))
	for _, m := range mems {
		edges[m.Key] = append(edges[m.Key], m.Supersedes...)
	}
	const (
		white = 0
		grey  = 1
		black = 2
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
				continue
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

// LoadDatasetFiles loads a dataset from a directory.
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

// Seed inserts the dataset's memories and returns runnable queries.
func Seed(ctx context.Context, store *memory.Store, db *sql.DB, ds Dataset, vecs Vectors) ([]Query, error) {
	queries, _, err := SeedAt(ctx, store, db, ds, vecs, time.Now().UTC())
	return queries, err
}

// SeedAt inserts the dataset's memories and returns runnable queries and the stamp instant.
func SeedAt(ctx context.Context, store *memory.Store, db *sql.DB, ds Dataset, vecs Vectors, at time.Time) ([]Query, time.Time, error) {
	stamp := newCorpusStampAt(at)
	if err := store.EnsureProject(ctx, ds.Project, "/bench/"+ds.Project, ds.Project); err != nil {
		return nil, stamp.instant(), fmt.Errorf("ensure project: %w", err)
	}
	dim := 0
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
			return nil, stamp.instant(), fmt.Errorf("memory with empty key: %q", m.Content)
		}
		if _, dup := keyToID[m.Key]; dup {
			return nil, stamp.instant(), fmt.Errorf("duplicate memory key %q", m.Key)
		}
		vec, ok := vecs[m.Key]
		if !ok {
			return nil, stamp.instant(), fmt.Errorf("no fixture vector for memory key %q (regenerate embeddings)", m.Key)
		}
		if err := checkDim("memory "+m.Key, vec); err != nil {
			return nil, stamp.instant(), err
		}
		id, err := store.CreateWithIDFromCorpus(ctx, ds.Project, corpusID(ds.Project, m.Key), memory.Memory{
			Category: m.Category, Content: m.Content, Importance: m.Importance,
			Tags: m.Tags, Source: "mcp",
			ValidFrom: m.ValidFrom, ValidUntil: m.ValidUntil, VerifiedAt: m.VerifiedAt,
		})
		if err != nil {
			return nil, stamp.instant(), fmt.Errorf("create memory %q: %w", m.Key, err)
		}
		if err := stamp.apply(ctx, db, id, m.AgeDays); err != nil {
			return nil, stamp.instant(), fmt.Errorf("stamp %q: %w", m.Key, err)
		}
		if err := store.StoreEmbedding(ctx, id, vec, "bench"); err != nil {
			return nil, stamp.instant(), fmt.Errorf("embed memory %q: %w", m.Key, err)
		}
		keyToID[m.Key] = id
	}

	if cycle := supersedesCycle(ds.Memories); cycle != "" {
		return nil, stamp.instant(), fmt.Errorf("supersedes cycle in the dataset: %s", cycle)
	}
	for _, m := range ds.Memories {
		for _, older := range m.Supersedes {
			target, ok := keyToID[older]
			if !ok {
				return nil, stamp.instant(), fmt.Errorf("memory %q supersedes unknown key %q", m.Key, older)
			}
			if older == m.Key {
				return nil, stamp.instant(), fmt.Errorf("memory %q supersedes itself", m.Key)
			}
			if err := store.CreateLink(ctx, keyToID[m.Key], target, "supersedes", 1.0, "llm"); err != nil {
				return nil, stamp.instant(), fmt.Errorf("link %q supersedes %q: %w", m.Key, older, err)
			}
		}
	}

	queries := make([]Query, 0, len(ds.Queries))
	for _, q := range ds.Queries {
		vec, ok := vecs[q.Name]
		if !ok {
			return nil, stamp.instant(), fmt.Errorf("no fixture vector for query %q (regenerate embeddings)", q.Name)
		}
		if err := checkDim("query "+q.Name, vec); err != nil {
			return nil, stamp.instant(), err
		}
		rel := make(Relevance, len(q.Rel))
		for key, gain := range q.Rel {
			id, ok := keyToID[key]
			if !ok {
				return nil, stamp.instant(), fmt.Errorf("query %q references unknown memory key %q", q.Name, key)
			}
			rel[id] = gain
		}
		queries = append(queries, Query{
			Name: q.Name, ProjectID: ds.Project, Text: q.Text, Vector: vec, Rel: rel,
		})
	}
	return queries, stamp.instant(), nil
}
