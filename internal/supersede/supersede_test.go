package supersede

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// mockClassifier confirms supersession per an injected rule — no LLM.
type mockClassifier struct {
	confirm func(newer, older string) bool
	calls   int
}

func (m *mockClassifier) Supersedes(_ context.Context, newer, older string) (bool, error) {
	m.calls++
	return m.confirm(newer, older), nil
}

// seed builds an in-memory store, returns it plus the raw db so tests can
// backdate created_at (Create always stamps now).
func seed(t *testing.T) (*memory.Store, *sql.DB) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.EnsureProject(context.Background(), "p", "/tmp/p", "p"); err != nil {
		t.Fatal(err)
	}
	return store, db
}

// add creates a memory with an embedding and a controlled created_at age.
func add(t *testing.T, store *memory.Store, db *sql.DB, content string, vec []float32, createdAt string) string {
	t.Helper()
	ctx := context.Background()
	id, err := store.Create(ctx, "p", memory.Memory{Category: "fact", Content: content, Importance: 0.7, Source: "mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(ctx, id, vec, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE memories SET created_at = ? WHERE id = ?`, createdAt, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSelectCandidates(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// Two similar memories (same subject) + one unrelated.
	newer := add(t, store, db, "postgres upgraded to 16", []float32{1, 0, 0}, "2026-07-10 00:00:00")
	older := add(t, store, db, "postgres runs version 14", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
	_ = add(t, store, db, "grafana listens on port 80", []float32{0, 0, 1}, "2026-06-01 00:00:00")

	cands, err := SelectCandidates(ctx, store, "p", 0.9)
	if err != nil {
		t.Fatalf("SelectCandidates: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("want exactly 1 candidate (the postgres pair), got %d: %+v", len(cands), cands)
	}
	c := cands[0]
	if c.NewerID != newer || c.OlderID != older {
		t.Errorf("wrong orientation: newer=%s older=%s (want newer=%s older=%s)", c.NewerID, c.OlderID, newer, older)
	}
}

func TestRunEmitsStarLinksAndFlipsRanking(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// Three versions of one fact, all mutually similar, distinct ages.
	v1 := add(t, store, db, "kubernetes cluster runs version 1.27", []float32{1, 0, 0}, "2026-01-01 00:00:00")
	v2 := add(t, store, db, "kubernetes upgraded to 1.29", []float32{0.99, 0.01, 0}, "2026-04-01 00:00:00")
	v3 := add(t, store, db, "kubernetes now on 1.31", []float32{0.98, 0.02, 0}, "2026-07-01 00:00:00")

	cls := &mockClassifier{confirm: func(_, _ string) bool { return true }} // all pairs are supersessions
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, slog.Default())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 3 mutually-similar versions → 3 ordered pairs (v3>v2, v3>v1, v2>v1).
	if res.Candidates != 3 || res.Confirmed != 3 || res.Created != 3 {
		t.Fatalf("want 3/3/3 candidates/confirmed/created, got %d/%d/%d", res.Candidates, res.Confirmed, res.Created)
	}

	// The star links must make the consumer rank v3 > v2 > v1.
	p := memory.DefaultSearchParams()
	p.SupersedeDemote = true
	results, err := store.SearchHybridParams(ctx, "p", "kubernetes version", nil, 10, p)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	rank := map[string]int{}
	for i, m := range results {
		rank[m.ID] = i
	}
	if rank[v3] >= rank[v2] || rank[v2] >= rank[v1] {
		t.Errorf("supersede demote should order v3<v2<v1, got v1=%d v2=%d v3=%d", rank[v1], rank[v2], rank[v3])
	}
}

func TestRunDryRunWritesNothing(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	n := add(t, store, db, "api rate limit raised to 500 rps", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	o := add(t, store, db, "api rate limit is 100 rps", []float32{0.99, 0, 0}, "2026-01-01 00:00:00")

	cls := &mockClassifier{confirm: func(_, _ string) bool { return true }}
	res, confirmed, err := Run(ctx, store, cls, "p", 0.9, false, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Confirmed != 1 || res.Created != 0 || len(confirmed) != 1 {
		t.Fatalf("dry run should confirm 1, create 0; got confirmed=%d created=%d", res.Confirmed, res.Created)
	}
	pairs, _ := store.SupersedesWithin(ctx, []string{n, o})
	if len(pairs) != 0 {
		t.Errorf("dry run must not write links, found %d", len(pairs))
	}
}

func TestRunRejectsParallelFacts(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	// Two similar-but-parallel facts (prod vs staging) — classifier says no.
	a := add(t, store, db, "prod database is postgres 16", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	b := add(t, store, db, "staging database is postgres 16", []float32{0.99, 0, 0}, "2026-06-01 00:00:00")

	cls := &mockClassifier{confirm: func(_, _ string) bool { return false }}
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Candidates != 1 || res.Confirmed != 0 || res.Created != 0 {
		t.Errorf("parallel facts: want 1 candidate, 0 confirmed/created; got %d/%d/%d", res.Candidates, res.Confirmed, res.Created)
	}
	pairs, _ := store.SupersedesWithin(ctx, []string{a, b})
	if len(pairs) != 0 {
		t.Errorf("rejected pair must not be linked, found %d", len(pairs))
	}
}

func TestRunIdempotent(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	add(t, store, db, "go version is 1.26", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	add(t, store, db, "go version is 1.24", []float32{0.99, 0, 0}, "2026-01-01 00:00:00")
	cls := &mockClassifier{confirm: func(_, _ string) bool { return true }}

	r1, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	r2, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Created != 1 || r2.Created != 1 {
		t.Errorf("both runs should (idempotently) create 1 link, got %d then %d", r1.Created, r2.Created)
	}
}
