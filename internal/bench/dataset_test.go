package bench

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestLoadJSONL(t *testing.T) {
	mems, err := LoadMemories(strings.NewReader(
		`# a comment line is skipped
{"key":"m1","category":"fact","content":"alpha","importance":0.7,"tags":["x"]}

{"key":"m2","category":"gotcha","content":"beta","importance":0.9}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 2 || mems[0].Key != "m1" || mems[1].Category != "gotcha" {
		t.Fatalf("parsed memories wrong: %+v", mems)
	}
	qs, err := LoadQueries(strings.NewReader(`{"name":"q1","text":"alpha","rel":{"m1":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 || qs[0].Rel["m1"] != 1 {
		t.Fatalf("parsed queries wrong: %+v", qs)
	}
	vecs, err := LoadVectors(strings.NewReader(`{"m1":[1,0],"q1":[1,0]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs["m1"]) != 2 {
		t.Fatalf("parsed vectors wrong: %+v", vecs)
	}
}

func newBenchStore(t *testing.T) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestSeedAndRun(t *testing.T) {
	ds := Dataset{
		Project: "p",
		Memories: []MemorySpec{
			{Key: "k8s", Category: "fact", Content: "kubernetes deployment", Importance: 0.7},
			{Key: "pg", Category: "fact", Content: "postgres database", Importance: 0.7},
		},
		Queries: []QuerySpec{
			{Name: "q_k8s", Text: "kubernetes", Rel: map[string]int{"k8s": 1}},
		},
	}
	vecs := Vectors{
		"k8s":   {1, 0, 0},
		"pg":    {0, 1, 0},
		"q_k8s": {0.9, 0.1, 0},
	}

	ctx := context.Background()
	queries, err := Seed(ctx, newBenchStore(t), ds, vecs)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if len(queries) != 1 {
		t.Fatalf("got %d queries, want 1", len(queries))
	}
	// Relevance keys were translated from "k8s" to a generated store ID.
	if len(queries[0].Rel) != 1 {
		t.Fatalf("relevance not translated: %+v", queries[0].Rel)
	}
	for id := range queries[0].Rel {
		if id == "k8s" {
			t.Error("relevance key was not translated to a store ID")
		}
	}
}

func TestSeedValidation(t *testing.T) {
	ctx := context.Background()
	base := Dataset{
		Project:  "p",
		Memories: []MemorySpec{{Key: "a", Category: "fact", Content: "x", Importance: 0.5}},
		Queries:  []QuerySpec{{Name: "q", Text: "x", Rel: map[string]int{"a": 1}}},
	}

	t.Run("missing memory vector", func(t *testing.T) {
		_, err := Seed(ctx, newBenchStore(t), base, Vectors{"q": {1}})
		if err == nil || !strings.Contains(err.Error(), "fixture vector for memory") {
			t.Fatalf("want missing-memory-vector error, got %v", err)
		}
	})
	t.Run("missing query vector", func(t *testing.T) {
		_, err := Seed(ctx, newBenchStore(t), base, Vectors{"a": {1}})
		if err == nil || !strings.Contains(err.Error(), "fixture vector for query") {
			t.Fatalf("want missing-query-vector error, got %v", err)
		}
	})
	t.Run("unknown relevance key", func(t *testing.T) {
		ds := base
		ds.Queries = []QuerySpec{{Name: "q", Text: "x", Rel: map[string]int{"nope": 1}}}
		_, err := Seed(ctx, newBenchStore(t), ds, Vectors{"a": {1}, "q": {1}})
		if err == nil || !strings.Contains(err.Error(), "unknown memory key") {
			t.Fatalf("want unknown-key error, got %v", err)
		}
	})
	t.Run("duplicate memory key", func(t *testing.T) {
		ds := base
		ds.Memories = []MemorySpec{
			{Key: "a", Category: "fact", Content: "x"},
			{Key: "a", Category: "fact", Content: "y"},
		}
		_, err := Seed(ctx, newBenchStore(t), ds, Vectors{"a": {1}, "q": {1}})
		if err == nil || !strings.Contains(err.Error(), "duplicate memory key") {
			t.Fatalf("want duplicate-key error, got %v", err)
		}
	})
}
