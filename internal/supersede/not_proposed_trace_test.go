package supersede

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func addWithImportance(t *testing.T, store *memory.Store, db *sql.DB, content, stamp string, importance float32, vec []float32) string {
	t.Helper()
	ctx := context.Background()
	id, err := store.Create(ctx, "p", memory.Memory{
		Category: "fact", Content: content, Importance: importance, Source: "mcp",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StoreEmbedding(ctx, id, vec, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE memories SET created_at=?, updated_at=?, importance=? WHERE id=?`, stamp, stamp, importance, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestNotProposedIsOrientedWhenTheOlderRowIsScannedFirst: GetAll orders by
// importance then age, so a high-importance old row is visited before a newer
// one. The trace must still name the newer endpoint first, and the two visits
// of one unordered pair must spend only one entry of the bounded trace.
func TestNotProposedIsOrientedWhenTheOlderRowIsScannedFirst(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	older := addWithImportance(t, store, db,
		"The ledger reconciles accounts nightly at 02:00 UTC.",
		"2026-01-01 00:00:00", 0.9, []float32{1, 0, 0})
	newer := addWithImportance(t, store, db,
		"Build pipelines cache Go modules on a shared volume.",
		"2026-07-01 00:00:00", 0.5, []float32{0.4, 0.9, 0})

	sel, err := SelectCandidates(ctx, store, "p", 0.7)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Candidates) != 0 {
		t.Fatalf("unrelated pair became a candidate: %+v", sel.Candidates)
	}
	if len(sel.NotProposed) != 1 {
		t.Fatalf("NotProposed = %d entries for ONE unordered pair: %+v", len(sel.NotProposed), sel.NotProposed)
	}
	got := sel.NotProposed[0]
	if got.NewerID != newer || got.OlderID != older {
		t.Errorf("trace names %s -> %s, want the newer %s -> older %s", got.NewerID, got.OlderID, newer, older)
	}
}

// TestNotProposedTracesEveryObservedDropReason: a pair that is seen and dropped
// before classification must say why. A reader of a missed edge cannot be told
// "never proposed" only when the drop happened to be the cosine floor; a
// persistent endpoint and two rows with no usable chronology are different
// findings with different remedies.
func TestNotProposedTracesEveryObservedDropReason(t *testing.T) {
	t.Run("persistent endpoint", func(t *testing.T) {
		store, db := seed(t)
		ctx := context.Background()
		older := addWithImportance(t, store, db,
			"The ledger reconciles accounts nightly at 02:00 UTC.",
			"2026-01-01 00:00:00", 0.5, []float32{1, 0, 0})
		newer := addWithImportance(t, store, db,
			"The ledger reconciles accounts nightly at 02:05 UTC.",
			"2026-07-01 00:00:00", 0.5, []float32{0.95, 0.1, 0})
		if _, err := db.ExecContext(ctx, `UPDATE memories SET retention='persistent' WHERE id=?`, older); err != nil {
			t.Fatal(err)
		}
		sel, err := SelectCandidates(ctx, store, "p", 0.7)
		if err != nil {
			t.Fatal(err)
		}
		assertRejected(t, sel, newer, older, "persistent")
	})

	t.Run("no chronology", func(t *testing.T) {
		store, db := seed(t)
		ctx := context.Background()
		a := addWithImportance(t, store, db,
			"Build pipelines cache Go modules on a shared volume.",
			"2026-01-01 00:00:00", 0.5, []float32{1, 0, 0})
		b := addWithImportance(t, store, db,
			"Build pipelines cache Go modules on a mounted share.",
			"2026-01-01 00:00:00", 0.5, []float32{0.95, 0.1, 0})
		sel, err := SelectCandidates(ctx, store, "p", 0.7)
		if err != nil {
			t.Fatal(err)
		}
		if sel.Unoriented == 0 {
			t.Fatal("fixture did not tie the two stamps; the test would assert nothing")
		}
		// With no chronology there is no newer/older to name; the trace keeps
		// the scan order. Both ids must still be there so the pair is findable.
		if len(sel.NotProposed) != 1 {
			t.Fatalf("NotProposed = %+v, want one entry naming the drop", sel.NotProposed)
		}
		got := sel.NotProposed[0]
		if !strings.Contains(got.Reason, "timestamps") {
			t.Errorf("trace reason %q does not name the timestamps", got.Reason)
		}
		ids := map[string]bool{got.NewerID: true, got.OlderID: true}
		if !ids[a] || !ids[b] {
			t.Errorf("trace names %s -> %s, want both tied endpoints %s and %s", got.NewerID, got.OlderID, a, b)
		}
	})
}

func assertRejected(t *testing.T, sel Selection, newer, older, reason string) {
	t.Helper()
	if len(sel.NotProposed) != 1 {
		t.Fatalf("NotProposed = %+v, want one entry naming the drop", sel.NotProposed)
	}
	got := sel.NotProposed[0]
	if got.NewerID != newer || got.OlderID != older {
		t.Errorf("trace names %s -> %s, want %s -> %s", got.NewerID, got.OlderID, newer, older)
	}
	if !strings.Contains(got.Reason, reason) {
		t.Errorf("trace reason %q does not name %q", got.Reason, reason)
	}
}
