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

	results, err := Run(ctx, store, queries)
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

// TestRunConditionTopRowSharesAreHandCountable: the two shares are
// computed from the same ranked lists the graded ratios read, so a
// fixture with known ranks makes them hand-countable. The rank function
// returns lists the test wrote rather than lists a search produced,
// which is what "known ranks" means here — the shares have to equal the
// counts a reader takes off those lists, and a share that disagrees with
// the list it was computed from is a bug this finds rather than a
// reading.
func TestRunConditionTopRowSharesAreHandCountable(t *testing.T) {
	store, _ := seedRunnerStore(t)
	ctx := context.Background()

	queries := []Query{
		{Name: "q1", ProjectID: "p", Text: "t", Vector: []float32{1, 0, 0}, Rel: Relevance{"a": 1, "b": 1}},
		{Name: "q2", ProjectID: "p", Text: "t", Vector: []float32{1, 0, 0}, Rel: Relevance{"a": 2, "b": 1}},
		{Name: "q3", ProjectID: "p", Text: "t", Vector: []float32{1, 0, 0}, Rel: Relevance{"a": 1}},
	}
	// q1: the top row is not labelled at all.
	// q2: the top row is labelled, but a carries a higher gain.
	// q3: the top row is labelled and nothing outranks it.
	lists := map[string][]string{
		"q1": {"x", "a", "b"},
		"q2": {"b", "a"},
		"q3": {"a"},
	}
	rank := func(q Query) ([]string, error) { return lists[q.Name], nil }

	res, err := runCondition(ctx, store, CondHybrid, queries, rank)
	if err != nil {
		t.Fatalf("runCondition: %v", err)
	}
	if res.Queries != 3 {
		t.Fatalf("scored %d queries, want 3", res.Queries)
	}
	// Two of the three top rows are labelled: q2 and q3.
	if res.TopRowRelevant != 2.0/3.0 {
		t.Errorf("top row relevant = %.3f, want 0.667 (q2 and q3 of 3)", res.TopRowRelevant)
	}
	// Only q3's top row carries the best label its query gives.
	if res.TopRowBestLabelled != 1.0/3.0 {
		t.Errorf("top row best-labelled = %.3f, want 0.333 (q3 of 3)", res.TopRowBestLabelled)
	}
	// The ceiling is a property of the labels: 1/2, 1/2 and 1/1.
	if res.Recall1Ceiling != 2.0/3.0 {
		t.Errorf("R@1 ceiling = %.3f, want 0.667 (mean of 1/2, 1/2, 1/1)", res.Recall1Ceiling)
	}
	// The shares and the ceiling are read against the same population the
	// graded ratios are, so a query the corpus does not answer is in none
	// of them.
	queries = append(queries, Query{Name: "q4", ProjectID: "p", Text: "t", Vector: []float32{1, 0, 0}})
	res, err = runCondition(ctx, store, CondHybrid, queries, rank)
	if err != nil {
		t.Fatalf("runCondition: %v", err)
	}
	if res.Queries != 3 {
		t.Errorf("scored %d queries, want 3 (the no-answer query is measured, not scored)", res.Queries)
	}
	if res.TopRowRelevant != 2.0/3.0 || res.TopRowBestLabelled != 1.0/3.0 || res.Recall1Ceiling != 2.0/3.0 {
		t.Errorf("the no-answer query moved a share or the ceiling: %+v", res)
	}
}

func TestRunNoQueries(t *testing.T) {
	store, _ := seedRunnerStore(t)
	results, err := Run(context.Background(), store, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, r := range results {
		if r.Queries != 0 || r.Recall10 != 0 {
			t.Errorf("%s: empty query set should yield zeroed metrics, got %+v", r.Condition, r)
		}
	}
}
