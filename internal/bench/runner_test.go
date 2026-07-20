package bench

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// seedRunnerStore builds an in-memory store with three well-separated memories
// and dim-3 hand vectors, returning the store and the relevant memory's ID.
func seedRunnerStore(t *testing.T) (*memory.Store, string) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p", "/tmp/p", "p"); err != nil {
		t.Fatal(err)
	}

	seed := []struct {
		content string
		vec     []float32
	}{
		{"kubernetes deployment alpha", []float32{1, 0, 0}},
		{"postgres database beta", []float32{0, 1, 0}},
		{"grafana dashboard gamma", []float32{0, 0, 1}},
	}
	var wantID string
	for i, s := range seed {
		id, err := store.Create(ctx, "p", memory.Memory{Category: "fact", Content: s.content, Importance: 0.7, Source: "mcp"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.StoreEmbedding(ctx, id, s.vec, "test"); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			wantID = id // the "kubernetes" memory
		}
	}
	return store, wantID
}

func TestRunAllConditions(t *testing.T) {
	store, wantID := seedRunnerStore(t)
	ctx := context.Background()

	queries := []Query{
		{
			Name: "k8s", ProjectID: "p", Text: "kubernetes",
			Vector: []float32{0.9, 0.1, 0}, // closest to the kubernetes memory
			Rel:    Relevance{wantID: 1},
		},
		{Name: "no-relevant", ProjectID: "p", Text: "nothing", Rel: Relevance{}}, // excluded from scoring
	}

	results, err := Run(ctx, store, queries, 0.70)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	wantConds := []string{CondFTS, CondVector, CondHybrid}
	if len(results) != len(wantConds) {
		t.Fatalf("got %d conditions, want %d", len(results), len(wantConds))
	}
	for i, r := range results {
		if r.Condition != wantConds[i] {
			t.Errorf("condition[%d] = %q, want %q", i, r.Condition, wantConds[i])
		}
		if r.Queries != 1 {
			t.Errorf("%s: scored %d queries, want 1 (no-relevant excluded)", r.Condition, r.Queries)
		}
		// The relevant memory is the unambiguous match on every path.
		if r.Recall10 != 1 {
			t.Errorf("%s: recall@10 = %.3f, want 1", r.Condition, r.Recall10)
		}
		if r.NDCG10 != 1 {
			t.Errorf("%s: ndcg@10 = %.3f, want 1 (relevant ranked first)", r.Condition, r.NDCG10)
		}
	}
}

func TestRunNoQueries(t *testing.T) {
	store, _ := seedRunnerStore(t)
	results, err := Run(context.Background(), store, nil, 0.70)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, r := range results {
		if r.Queries != 0 || r.Recall10 != 0 {
			t.Errorf("%s: empty query set should yield zeroed metrics, got %+v", r.Condition, r)
		}
	}
}
