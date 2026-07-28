package supersede

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// mockClassifier returns a verdict per an injected rule — no LLM. calls
// records every (newer, older) content pair passed to Classify, so tests can
// assert a specific pair was (or wasn't) invoked — e.g. the skip-if-unchanged
// test.
type mockClassifier struct {
	verdict func(newer, older string) Relation
	calls   []struct{ Newer, Older string }
}

func (m *mockClassifier) Classify(_ context.Context, newer, older string) (Relation, error) {
	m.calls = append(m.calls, struct{ Newer, Older string }{newer, older})
	return m.verdict(newer, older), nil
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

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }} // all pairs are supersessions
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

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, false, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Confirmed != 1 || res.Created != 0 || len(classified) != 1 {
		t.Fatalf("dry run should confirm 1, create 0; got confirmed=%d created=%d classified=%d", res.Confirmed, res.Created, len(classified))
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

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationNeither }}
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
	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}

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

func TestRunSupersedesLinkDirection(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer := add(t, store, db, "api rate limit raised to 500 rps", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older := add(t, store, db, "api rate limit is 100 rps", []float32{0.99, 0, 0}, "2026-01-01 00:00:00")

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
	if _, _, err := Run(ctx, store, cls, "p", 0.9, true, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	links, err := store.GetLinks(ctx, newer)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	found := false
	for _, l := range links {
		if l.Relation == "supersedes" {
			found = true
			if l.SourceID != newer || l.TargetID != older {
				t.Errorf("supersedes link direction wrong: source=%s target=%s (want source=%s target=%s)", l.SourceID, l.TargetID, newer, older)
			}
		}
	}
	if !found {
		t.Error("expected a supersedes link, found none")
	}
}

func TestRunCausesVerdictWritesCausesLinkOlderToNewer(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer := add(t, store, db, "decision: reversed the NATS-first decision back to Postgres LISTEN/NOTIFY", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older := add(t, store, db, "gotcha: NATS delivers at-least-once and can reorder messages under partition rebalance", []float32{0.99, 0, 0}, "2026-01-01 00:00:00")

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationCauses }}
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CausesCreated != 1 {
		t.Fatalf("want CausesCreated=1, got %d", res.CausesCreated)
	}
	if len(classified) != 1 || classified[0].Relation != RelationCauses {
		t.Fatalf("want 1 classified pair with RelationCauses, got %+v", classified)
	}

	links, err := store.GetLinks(ctx, older)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	found := false
	for _, l := range links {
		if l.Relation == "causes" {
			found = true
			if l.SourceID != older || l.TargetID != newer {
				t.Errorf("causes link direction wrong: source=%s target=%s (want source=%s target=%s, i.e. older->newer)", l.SourceID, l.TargetID, older, newer)
			}
		}
	}
	if !found {
		t.Error("expected a causes link, found none")
	}

	pairs, _ := store.SupersedesWithin(ctx, []string{newer, older})
	if len(pairs) != 0 {
		t.Errorf("CAUSES verdict must not write a supersedes link, found %d", len(pairs))
	}
}

func TestRunReclassifiesExistingSupersedesToCauses(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer := add(t, store, db, "decision: replaced NATS with Postgres LISTEN/NOTIFY", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older := add(t, store, db, "gotcha: NATS can reorder messages under partition rebalance", []float32{0.99, 0, 0}, "2026-01-01 00:00:00")

	if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationCauses }}
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reclassified != 1 {
		t.Errorf("want Reclassified=1, got %d", res.Reclassified)
	}

	pairs, _ := store.SupersedesWithin(ctx, []string{newer, older})
	if len(pairs) != 0 {
		t.Errorf("stale supersedes link should be invalidated, found %d", len(pairs))
	}

	links, _ := store.GetLinks(ctx, older)
	found := false
	for _, l := range links {
		if l.Relation == "causes" && l.SourceID == older && l.TargetID == newer {
			found = true
		}
	}
	if !found {
		t.Error("expected causes link older->newer after reclassification")
	}
}

func TestRunNeitherInvalidatesExistingCausesLink(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer := add(t, store, db, "decision: use gRPC for the service mesh", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older := add(t, store, db, "note: gRPC requires HTTP/2", []float32{0.99, 0, 0}, "2026-01-01 00:00:00")

	if err := store.CreateLink(ctx, older, newer, "causes", 0.9, "llm"); err != nil {
		t.Fatal(err)
	}

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationNeither }}
	if _, _, err := Run(ctx, store, cls, "p", 0.9, true, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	links, _ := store.GetLinks(ctx, older)
	for _, l := range links {
		if l.Relation == "causes" {
			t.Errorf("causes link should have been invalidated, still present: %+v", l)
		}
	}
	pairs, _ := store.SupersedesWithin(ctx, []string{newer, older})
	if len(pairs) != 0 {
		t.Error("NEITHER must not create a supersedes link")
	}
}

func TestRunSkipsUnchangedExistingLink(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	// Dissimilar vectors so SelectCandidates never rediscovers this pair on
	// its own — the only way it's classified is via the existing-link
	// reclassify path, which is exactly what skip-if-unchanged should suppress.
	newer := add(t, store, db, "reversed the NATS decision back to Postgres LISTEN/NOTIFY", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older := add(t, store, db, "unrelated gotcha about DNS caching", []float32{0, 1, 0}, "2026-01-01 00:00:00")

	if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
	if _, _, err := Run(ctx, store, cls, "p", 0.9, true, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, call := range cls.calls {
		if call.Newer == "reversed the NATS decision back to Postgres LISTEN/NOTIFY" {
			t.Errorf("Classify should not have been called for the unchanged existing-link pair, but was called with newer=%q older=%q", call.Newer, call.Older)
		}
	}
}

func TestRunRetriggersReclassifyWhenEndpointChanges(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	newer := add(t, store, db, "reversed the NATS decision back to Postgres LISTEN/NOTIFY", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older := add(t, store, db, "unrelated gotcha about DNS caching", []float32{0, 1, 0}, "2026-01-01 00:00:00")

	if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	// Backdate the link so the memory's later update is unambiguously newer.
	if _, err := db.ExecContext(ctx, `UPDATE memory_links SET created_at = '2020-01-01 00:00:00' WHERE source_id = ? AND target_id = ?`, newer, older); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE memories SET updated_at = '2026-07-15 00:00:00' WHERE id = ?`, older); err != nil {
		t.Fatal(err)
	}

	cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
	if _, _, err := Run(ctx, store, cls, "p", 0.9, true, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	called := false
	for _, call := range cls.calls {
		if call.Newer == "reversed the NATS decision back to Postgres LISTEN/NOTIFY" {
			called = true
		}
	}
	if !called {
		t.Error("Classify should have been called after older endpoint's updated_at moved past the link's created_at")
	}
}
