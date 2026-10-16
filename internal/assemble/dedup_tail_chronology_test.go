package assemble

import (
	"context"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestQueryModeCollapseNeverTradesAWindowRowForACutTailRow: the removal's
// chronology rule (the later row of a pair is never removed in favour of the
// earlier one) also runs on the window-plus-tail pass. There it would remove a
// window row in favour of a later tail row that ranks behind another tail row,
// and the item budget then cuts that tail row: the answer would hold neither
// row of the pair. The subject must survive in one of them.
func TestQueryModeCollapseNeverTradesAWindowRowForACutTailRow(t *testing.T) {
	ctx := context.Background()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	if err := s.EnsureProject(ctx, "proj", "/src/proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	mk := func(content string, day int) string {
		t.Helper()
		id, err := s.Create(ctx, "proj", memory.Memory{
			Category: "fact", Content: content, Source: "manual", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", content, err)
		}
		stamp := time.Date(2026, 9, day, 10, 0, 0, 0, time.UTC).Format(memory.StoredStampLayout)
		if _, err := db.Exec(`UPDATE memories SET created_at = ?, updated_at = ? WHERE id = ?`, stamp, stamp, id); err != nil {
			t.Fatalf("stamp %s: %v", id, err)
		}
		return id
	}
	// The window is [A, X]; the tail is [D, C], D ranking ahead of C, so a
	// two-item answer never reaches C. Only C is later than its pair partner.
	windowA := mk("database configuration for the replica refresh runs hourly", 20)
	_ = mk("database configuration quorum zebra", 20)
	_ = mk("database configuration for the reporting warehouse is kept in the ops runbook", 20)
	tailC := mk("database configuration review for the staging cluster happens each quarter with the platform team, alongside the failover drill, the retention policy refresh and the backup restoration rehearsal", 24)
	if err := s.CreateLink(ctx, windowA, tailC, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink(%s,%s): %v", windowA, tailC, err)
	}

	res := run(t, s, collapseRequest())
	ids := itemIDs(res.Items)
	if !hasID(ids, windowA) && !hasID(ids, tailC) {
		t.Errorf("items = %v: neither row of the pair (%s, %s) is in the answer; losers %+v", ids, windowA, tailC, collectLosers(t, res))
	}
}
