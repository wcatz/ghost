package bench

import (
	"context"
	"database/sql"
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

// newBenchStoreWithDB returns the store and the connection behind it, which
// Seed needs: backdating created_at is not something a store can be asked to do
// through its API, and seeding a corpus with real ages is the whole point of the
// ranking-state suite.
func newBenchStoreWithDB(t *testing.T) (*memory.Store, *sql.DB) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(func() { _ = store.Close() })
	return store, db
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
	store, db := newBenchStoreWithDB(t)
	queries, err := Seed(ctx, store, db, ds, vecs)
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
		store, db := newBenchStoreWithDB(t)
		_, err := Seed(ctx, store, db, base, Vectors{"q": {1}})
		if err == nil || !strings.Contains(err.Error(), "fixture vector for memory") {
			t.Fatalf("want missing-memory-vector error, got %v", err)
		}
	})
	t.Run("missing query vector", func(t *testing.T) {
		store, db := newBenchStoreWithDB(t)
		_, err := Seed(ctx, store, db, base, Vectors{"a": {1}})
		if err == nil || !strings.Contains(err.Error(), "fixture vector for query") {
			t.Fatalf("want missing-query-vector error, got %v", err)
		}
	})
	t.Run("unknown relevance key", func(t *testing.T) {
		ds := base
		ds.Queries = []QuerySpec{{Name: "q", Text: "x", Rel: map[string]int{"nope": 1}}}
		store, db := newBenchStoreWithDB(t)
		_, err := Seed(ctx, store, db, ds, Vectors{"a": {1}, "q": {1}})
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
		store, db := newBenchStoreWithDB(t)
		_, err := Seed(ctx, store, db, ds, Vectors{"a": {1}, "q": {1}})
		if err == nil || !strings.Contains(err.Error(), "duplicate memory key") {
			t.Fatalf("want duplicate-key error, got %v", err)
		}
	})
}

// TestLoadersRefuseUnusableRows is one table for the loaders beyond the dataset
// pair: each bad line has to be refused with an error naming the defect, so a
// malformed fixture fails at load rather than skewing a report.
func TestLoadersRefuseUnusableRows(t *testing.T) {
	loaders := map[string]func(string) error{
		"negatives":    func(s string) error { _, err := LoadNegatives(strings.NewReader(s)); return err },
		"trap":         func(s string) error { _, err := LoadTrapScenarios(strings.NewReader(s)); return err },
		"maint-mems":   func(s string) error { _, err := LoadMaintenanceMemories(strings.NewReader(s)); return err },
		"maint-querys": func(s string) error { _, err := LoadMaintenanceQueries(strings.NewReader(s)); return err },
		"memories":     func(s string) error { _, err := LoadMemories(strings.NewReader(s)); return err },
		"queries":      func(s string) error { _, err := LoadQueries(strings.NewReader(s)); return err },
	}
	for name, load := range loaders {
		if err := load(`{"key": 3, "name": 3, "rel": 3, "text": 3}`); err == nil {
			t.Errorf("%s accepted a row of the wrong shape", name)
		}
	}
	for _, c := range []struct{ loader, line, want string }{
		{"negatives", `{"name":"a","text":"x","flavor":"near_miss","rel":{"m":1}}`, "grades"},
		{"negatives", `{"name":"a","text":"x"}`, "no flavor"},
		{"trap", `{"name":"a","category":"fact"}`, "at least one trap"},
		{"trap", `{"name":"a","category":"all probes","traps":[{"key":"t","content":"c"}]}`, "report row label"},
		{"maint-mems", `{"content":"c","category":"fact"}`, "empty key"},
		{"maint-mems", `{"key":"k","category":"fact"}`, "no content"},
		{"maint-mems", `{"key":"k","content":"c"}`, "no category"},
		{"maint-querys", `{"text":"t"}`, "empty name"},
		{"maint-querys", `{"name":"q"}`, "no text"},
		{"negatives", `{"text":"x","flavor":"f"}`, "empty name"},
		{"negatives", `{"name":"a","flavor":"f"}`, "no text"},
		{"maint-mems", `{"key":"k","content":"c","category":"fact","age_days":-1}`, "negative age_days"},
		{"maint-mems", "{\"key\":\"k\",\"content\":\"c\",\"category\":\"fact\"}\n{\"key\":\"k\",\"content\":\"d\",\"category\":\"fact\"}", "duplicate"},
		{"maint-querys", `{"name":"q","text":"t","rel":{}}`, "empty rel"},
		{"maint-querys", `{"name":"q","text":"t","rel":{"a":2,"b":2}}`, "ambiguous"},
		{"maint-querys", `{"name":"q","text":"t","rel":{"a":0}}`, "non-positive"},
		{"maint-querys", `{"name":"q","text":"t","rel":{"a":2},"probe":"bogus"}`, "probe"},
		{"maint-querys", `{"name":"q","text":"t","rel":{"a":2},"distractors":["a"]}`, "own answer"},
	} {
		err := loaders[c.loader](c.line)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %q: err = %v, want one containing %q", c.loader, c.line, err, c.want)
		}
	}
}
