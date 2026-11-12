package supersede

import (
	"context"
	"database/sql"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestSelectCandidatesPersistentEndpoints(t *testing.T) {
	ctx := context.Background()
	t.Run("skips when any endpoint is persistent", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			keepNewer bool
			keepOlder bool
		}{
			{name: "persistent target", keepOlder: true},
			{name: "persistent source", keepNewer: true},
			{name: "both endpoints", keepNewer: true, keepOlder: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				store, db := seed(t)
				newer := add(t, store, db, "postgres upgraded to 16", []float32{1, 0, 0}, "2026-07-10 00:00:00")
				older := add(t, store, db, "postgres runs version 14", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
				_ = add(t, store, db, "grafana listens on port 80", []float32{0, 0, 1}, "2026-06-01 00:00:00")
				markPersistent(t, db, map[string]bool{newer: tc.keepNewer, older: tc.keepOlder})
				sel, err := SelectCandidates(ctx, store, "p", 0.9)
				if err != nil {
					t.Fatalf("SelectCandidates: %v", err)
				}
				if len(sel.Candidates) != 0 {
					t.Fatalf("got %d candidate(s): %+v", len(sel.Candidates), sel.Candidates)
				}
			})
		}
	})
	t.Run("still proposes ordinary pair", func(t *testing.T) {
		store, db := seed(t)
		newer := add(t, store, db, "postgres upgraded to 16", []float32{1, 0, 0}, "2026-07-10 00:00:00")
		older := add(t, store, db, "postgres runs version 14", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
		_ = add(t, store, db, "grafana listens on port 80", []float32{0, 0, 1}, "2026-06-01 00:00:00")
		sel, err := SelectCandidates(ctx, store, "p", 0.9)
		if err != nil {
			t.Fatalf("SelectCandidates: %v", err)
		}
		if len(sel.Candidates) != 1 || sel.Candidates[0].NewerID != newer || sel.Candidates[0].OlderID != older {
			t.Fatalf("candidates = %+v, want (%s,%s)", sel.Candidates, newer, older)
		}
	})
}

func TestRunPersistentEndpoints(t *testing.T) {
	ctx := context.Background()
	t.Run("writes no edge beside persistent row", func(t *testing.T) {
		store, db := seed(t)
		newer := add(t, store, db, "postgres upgraded to 16", []float32{1, 0, 0}, "2026-07-10 00:00:00")
		older := add(t, store, db, "postgres runs version 14", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
		markPersistent(t, db, map[string]bool{newer: false, older: true})
		cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
		res, _, err := Run(ctx, store, cls, "p", 0.9, true, discardLogger())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Candidates != 0 || cls.batchCalls != 0 {
			t.Errorf("candidates=%d batchCalls=%d, want 0,0", res.Candidates, cls.batchCalls)
		}
		var edges int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM memory_links WHERE relation = 'supersedes' AND invalidated_at IS NULL`).Scan(&edges); err != nil {
			t.Fatal(err)
		}
		if edges != 0 {
			t.Errorf("wrote %d edges", edges)
		}
	})
	t.Run("refuses to re-affirm existing edge", func(t *testing.T) {
		store, db := seed(t)
		newer := add(t, store, db, "postgres upgraded to 16 in staging", []float32{1, 0, 0}, "2026-07-10 00:00:00")
		older := add(t, store, db, "postgres runs version 14 in staging", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
		if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
			t.Fatal(err)
		}
		markPersistent(t, db, map[string]bool{older: true})
		if _, err := db.ExecContext(ctx, `UPDATE memory_links SET created_at = '2026-08-01 00:00:00'`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE memories SET updated_at = '2026-09-01 00:00:00' WHERE id = ?`, older); err != nil {
			t.Fatal(err)
		}
		cls := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
		res, _, err := Run(ctx, store, cls, "p", 0.9, true, discardLogger())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Candidates != 0 || cls.batchCalls != 0 {
			t.Errorf("candidates=%d batchCalls=%d, want 0,0", res.Candidates, cls.batchCalls)
		}

		// control: ordinary reclassify pair is still proposed
		store2, db2 := seed(t)
		newer2 := add(t, store2, db2, "postgres upgraded to 16 in staging", []float32{1, 0, 0}, "2026-07-10 00:00:00")
		older2 := add(t, store2, db2, "postgres runs version 14 in staging", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")
		if err := store2.CreateLink(ctx, newer2, older2, "supersedes", 0.95, "llm"); err != nil {
			t.Fatal(err)
		}
		if _, err := db2.ExecContext(ctx, `UPDATE memory_links SET created_at = '2026-08-01 00:00:00'`); err != nil {
			t.Fatal(err)
		}
		if _, err := db2.ExecContext(ctx, `UPDATE memories SET updated_at = '2026-09-01 00:00:00' WHERE id = ?`, older2); err != nil {
			t.Fatal(err)
		}
		cls2 := &mockClassifier{verdict: func(_, _ string) Relation { return RelationSupersedes }}
		res2, _, err := Run(ctx, store2, cls2, "p", 0.9, true, discardLogger())
		if err != nil {
			t.Fatalf("Run control: %v", err)
		}
		if res2.Candidates == 0 {
			t.Error("control pair was not re-proposed")
		}
	})
}

func markPersistent(t *testing.T, db *sql.DB, which map[string]bool) {
	t.Helper()
	for id, want := range which {
		if !want {
			continue
		}
		if _, err := db.Exec(`UPDATE memories SET retention = 'persistent' WHERE id = ?`, id); err != nil {
			t.Fatalf("mark %s persistent: %v", id, err)
		}
		got, err := memory.NewStore(db, discardLogger()).GetByIDs(context.Background(), []string{id})
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if len(got) != 1 || got[0].Retention != memory.RetentionPersistent {
			t.Fatalf("fixture: %s reads %+v", id, got)
		}
	}
}
