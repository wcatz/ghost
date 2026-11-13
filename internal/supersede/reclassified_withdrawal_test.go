package supersede

import (
	"context"
	"database/sql"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// endpoint has moved since the edge was written (#787), so a fixture that leaves
//
// SUPERSEDES reports an edge rather than declining to write one (#845).
//
// neither states a rule the imperative veto would retire (#686) — the fixture has
func seedReclassifyPair(t *testing.T, store *memory.Store, db *sql.DB) (newer, older string) {
	t.Helper()
	ctx := context.Background()
	newer = add(t, store, db, "decision: the deploy pipeline now runs the migration check before staging", []float32{1, 0, 0}, "2026-07-01 00:00:00")
	older = add(t, store, db, "note: the deploy pipeline runs the unit suite before staging", []float32{0.99, 0, 0}, "2026-01-01 00:00:00")
	if err := store.CreateLink(ctx, newer, older, string(RelationSupersedes), 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE memory_links SET created_at = '2020-01-01 00:00:00' WHERE source_id = ? AND target_id = ?`,
		newer, older,
	); err != nil {
		t.Fatal(err)
	}
	return newer, older
}

// edge, and whether it claimed to have moved one.
func liveSupersedes(t *testing.T, store *memory.Store, projectID string) int {
	t.Helper()
	links, err := store.LinksByRelationSource(context.Background(), projectID, string(RelationSupersedes), "llm")
	if err != nil {
		t.Fatalf("LinksByRelationSource: %v", err)
	}
	return len(links)
}

// TestRunNamesTheEdgeAReclassificationWouldWithdraw is #785 at the layer the
// pass owns, under #845. A live 'supersedes' edge the pass re-judges and that
// operator after a repair that had already happened.
//
// (WithdrawSuppressed).
func TestRunNamesTheEdgeAReclassificationWouldWithdraw(t *testing.T) {
	for _, verdict := range []Relation{RelationNeither, RelationCauses, RelationReversed} {
		t.Run(string(verdict), func(t *testing.T) {
			store, db := seed(t)
			newer, older := seedReclassifyPair(t, store, db)
			cls := &mockClassifier{verdict: func(_, _ string) Relation { return verdict }}

			res, classified, err := Run(context.Background(), store, cls, "p", 0.9, true, nil)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(classified) != 1 {
				t.Fatalf("classified = %+v, want exactly the one pair", classified)
			}
			row := classified[0]
			if row.NewerID != newer || row.OlderID != older {
				t.Fatalf("the row names %s→%s, want %s→%s", row.NewerID, row.OlderID, newer, older)
			}
			if !row.Reclassified {
				t.Error("the row does not say the pair carried a live edge, so a caller cannot tell this withdrawal from a fresh pair's silence")
			}
			if row.Withdrawn {
				t.Error("the row claims a withdrawal although the edge is still in the graph")
			}
			if !row.WithdrawSuppressed {
				t.Error("the row does not say the withdrawal was DECLINED, so a caller cannot tell it from a concurrent pass that took the edge first — the same line of text, opposite advice")
			}
			if got := liveSupersedes(t, store, "p"); got != 1 {
				t.Errorf("live supersedes edge(s) = %d, want 1: the pass reported a %s verdict and left the edge live", got, verdict)
			}
			// change over a pass that made none.
			wantReclassified := 0
			if verdict == RelationCauses {
				wantReclassified = 1
			}
			if res.Reclassified != wantReclassified {
				t.Errorf("Reclassified = %d, want %d", res.Reclassified, wantReclassified)
			}
			if res.WithdrawSuppressed != 1 {
				t.Errorf("WithdrawSuppressed = %d, want 1", res.WithdrawSuppressed)
			}
		})
	}
}

// #845 the marker is false in BOTH modes — the withheld withdrawal is not a
// because the pass declines it either way.

// these rows are printed in its shape.
//
// claim the marker exists to prevent.
