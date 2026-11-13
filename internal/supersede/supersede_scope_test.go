package supersede

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func addScoped(t *testing.T, store *memory.Store, db *sql.DB, content string, vec []float32, createdAt string, scope map[string]string) string {
	t.Helper()
	id, err := store.Create(context.Background(), "p", memory.Memory{
		Category: "fact", Content: content, Importance: 0.7, Source: "mcp", Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(context.Background(), id, vec, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE memories SET created_at = ?, updated_at = ? WHERE id = ?`, createdAt, createdAt, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func envScope(env string) map[string]string { return map[string]string{"environment": env} }

func TestScopeConflictingPairs(t *testing.T) {
	ctx := context.Background()
	t.Run("select skips conflicting pair", func(t *testing.T) {
		store, db := seed(t)
		addScoped(t, store, db, "the service database pool timeout", []float32{1, 0, 0}, "2026-07-01 00:00:00", envScope("development"))
		addScoped(t, store, db, "the service database pool timeout", []float32{0.99, 0.02, 0}, "2026-01-01 00:00:00", envScope("production"))
		sel, err := SelectCandidates(ctx, store, "p", 0.9)
		if err != nil {
			t.Fatal(err)
		}
		if len(sel.Candidates) != 0 {
			t.Errorf("proposed %+v", sel.Candidates)
		}
	})
	t.Run("select finds compatible pair below conflicting ones", func(t *testing.T) {
		store, db := seed(t)
		newer := addScoped(t, store, db, "production pool timeout is 30s", []float32{1, 0}, "2026-07-01 00:00:00", envScope("production"))
		older := addScoped(t, store, db, "production pool timeout is 5s", []float32{0.85, 0.53}, "2026-01-01 00:00:00", envScope("production"))
		for i := 0; i < maxNeighbors; i++ {
			addScoped(t, store, db, fmt.Sprintf("development candidate %d", i), []float32{1, 0.02 * float32(i+1)}, "2026-01-01 00:00:00", envScope("development"))
		}
		sel, err := SelectCandidates(ctx, store, "p", 0.70)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range sel.Candidates {
			if c.NewerID == newer && c.OlderID == older {
				return
			}
		}
		t.Fatalf("compatible pair missing: %+v", sel.Candidates)
	})
	t.Run("run spends nothing on conflicting pair", func(t *testing.T) {
		store, db := seed(t)
		dev := addScoped(t, store, db, "the service database pool timeout", []float32{1, 0, 0}, "2026-07-01 00:00:00", envScope("development"))
		prod := addScoped(t, store, db, "the service database pool timeout", []float32{0.99, 0.02, 0}, "2026-01-01 00:00:00", envScope("production"))
		cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
		res, classified, err := Run(ctx, store, cls, "p", 0.9, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(cls.calls) != 0 || len(classified) != 0 || res.Candidates != 0 || res.Created != 0 || res.Confirmed != 0 {
			t.Fatalf("calls=%d classified=%d res=%+v", len(cls.calls), len(classified), res)
		}
		for _, id := range []string{dev, prod} {
			links, err := store.GetLinks(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(links) != 0 {
				t.Fatalf("%s has links %+v", id, links)
			}
		}
	})
	t.Run("run skips reclassifying conflicting existing link", func(t *testing.T) {
		store, db := seed(t)
		newer := addScoped(t, store, db, "reversed the NATS decision", []float32{1, 0, 0}, "2026-07-01 00:00:00", envScope("development"))
		older := addScoped(t, store, db, "unrelated gotcha about DNS caching", []float32{0, 1, 0}, "2026-01-01 00:00:00", envScope("production"))
		if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE memory_links SET created_at = '2020-01-01 00:00:00' WHERE source_id = ? AND target_id = ?`, newer, older); err != nil {
			t.Fatal(err)
		}
		cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
		res, _, err := Run(ctx, store, cls, "p", 0.9, true, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(cls.calls) != 0 || res.Candidates != 0 {
			t.Fatalf("calls=%d candidates=%d", len(cls.calls), res.Candidates)
		}
		links, err := store.GetLinks(ctx, older)
		if err != nil {
			t.Fatal(err)
		}
		if len(links) != 1 || links[0].Relation != "supersedes" {
			t.Fatalf("links=%+v", links)
		}
	})
}

type unscopedNeighborStore struct {
	*memory.Store
	forced []memory.ScoredMemory
}

func (u *unscopedNeighborStore) SearchVectorScoped(_ context.Context, _ string, _ []float32, _ int, _ map[string]string) ([]memory.ScoredMemory, error) {
	return u.forced, nil
}

func TestSelectCandidatesRefusesConflictingNeighbourFromUnscopedStore(t *testing.T) {
	store, db := seed(t)
	dev := addScoped(t, store, db, "the service database pool timeout", []float32{1, 0, 0}, "2026-07-01 00:00:00", envScope("development"))
	prod := addScoped(t, store, db, "the service database pool timeout", []float32{0.99, 0.02, 0}, "2026-01-01 00:00:00", envScope("production"))
	store2 := &unscopedNeighborStore{Store: store, forced: []memory.ScoredMemory{
		{MemoryID: dev, Score: 1.0, Scope: envScope("development")},
		{MemoryID: prod, Score: 0.99, Scope: envScope("production")},
	}}
	sel, err := SelectCandidates(context.Background(), store2, "p", 0.9)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Candidates) != 0 {
		t.Errorf("candidates=%+v", sel.Candidates)
	}
}
