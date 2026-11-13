package supersede

import (
	"context"
	"database/sql"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestSelectCandidatesOrientsByAgeAfterAVerbatimReflect is #727's third
// memory's age.
//
// pairs whose created_at order was never in doubt.
func liveMemories(t *testing.T, store *memory.Store, project string) []memory.Memory {
	t.Helper()
	live, err := store.GetAll(context.Background(), project, -1)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	return live
}

func memoryUpdatedAt(t *testing.T, db *sql.DB, id string) string {
	t.Helper()
	var updatedAt string
	if err := db.QueryRow(`SELECT updated_at FROM memories WHERE id = ?`, id).Scan(&updatedAt); err != nil {
		t.Fatalf("read updated_at for %s: %v", id, err)
	}
	return updatedAt
}

func TestSelectCandidatesOrientsByAgeAfterAVerbatimReflect(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	newer := add(t, store, db,
		"the ledger changed its change feed back to Postgres LISTEN/NOTIFY",
		[]float32{1, 0, 0}, "2026-07-10 00:00:00")
	older := add(t, store, db,
		"the ledger used Postgres LISTEN/NOTIFY for its change feed",
		[]float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")

	// that can drift from what reflection actually sends.
	emitted := make([]memory.Memory, 0, 2)
	for _, m := range liveMemories(t, store, "p") {
		tags := m.Tags
		if tags == nil {
			tags = []string{}
		}
		emitted = append(emitted, memory.Memory{
			Category: m.Category, Content: m.Content, Importance: m.Importance, Tags: tags,
		})
	}
	if _, _, _, _, err := store.ReplaceNonManual(ctx, "p", emitted, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	// them equal, orient() falls to created_at (#778) rather than to the id, and
	if olderAge, newerAge := memoryUpdatedAt(t, db, older), memoryUpdatedAt(t, db, newer); olderAge >= newerAge {
		t.Fatalf("after a verbatim reflect the older memory's updated_at is %q and the newer one's is %q — "+
			"the pass stamped rows it did not change, and orient() now has no age to read", olderAge, newerAge)
	}

	sel, err := SelectCandidates(ctx, store, "p", 0.9)
	if err != nil {
		t.Fatalf("SelectCandidates: %v", err)
	}
	if len(sel.Candidates) != 1 {
		t.Fatalf("candidates = %d, want exactly the one pair: %+v", len(sel.Candidates), sel.Candidates)
	}
	c := sel.Candidates[0]
	if c.NewerID != newer || c.OlderID != older {
		t.Errorf("orientation after a verbatim reflect: newer=%s older=%s, want newer=%s older=%s — the "+
			"pass left both rows with one timestamp, so the direction came from a tie-break rather than "+
			"from the memories' ages, and a REVERSED verdict on the wrong pair is refused rather than flipped",
			c.NewerID, c.OlderID, newer, older)
	}
	if c.NewerCreatedAt != "2026-07-10 00:00:00" || c.OlderCreatedAt != "2026-01-01 00:00:00" {
		t.Errorf("candidate created_at = (%q, %q), want (2026-07-10 00:00:00, 2026-01-01 00:00:00)",
			c.NewerCreatedAt, c.OlderCreatedAt)
	}
}
