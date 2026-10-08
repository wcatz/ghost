package assemble

import (
	"context"
	"reflect"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestQueryModeCollapseSurvivesAWindowPassThatRemovedNothing (#926, re-review
// of #940): dropNearDuplicates returns a NIL lostTo whenever it removes nothing,
// and "the window holds no near-duplicate pair" is one of those cases — the
// early return at demotion.go's len(penalty) == 0. The second pass over
// window+tail files each loser with `lostTo[loser] = lostToPass[loser]`, so on
// exactly that shape it panicked with "assignment to entry in nil map": a
// near-duplicate pair that only becomes visible once the tail hydrates beside
// the window took the whole query down instead of removing its loser.
//
// The fixture is therefore a window holding NO pair — two short, unlinked rows
// bm25 puts in the 2-row window — plus a long third row linked to one of them.
// The window's own pass has nothing to remove (lostTo comes back nil), the tail
// hydrates the linked row, and the second pass is the first to see the pair.
func TestQueryModeCollapseSurvivesAWindowPassThatRemovedNothing(t *testing.T) {
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
	mk := func(content string) string {
		t.Helper()
		id, err := s.Create(ctx, "proj", memory.Memory{
			Category: "fact", Content: content, Source: "manual", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", content, err)
		}
		return id
	}
	// Both window rows are short, so bm25 ranks them into the 2-row window, and
	// neither is linked to the other: the window pass has no pair to remove and
	// returns its nil attribution. The third row is far longer, so bm25 ranks it
	// below them (it hydrates into the tail), and it is linked to ONE window row
	// only — the pair the second pass is the first to see.
	windowA := mk("database configuration for the replica refresh runs hourly")
	windowX := mk("database configuration quorum zebra")
	tailC := mk("database configuration review for the staging cluster happens each quarter with the platform team, alongside the failover drill, the retention policy refresh and the backup restoration rehearsal")
	if err := s.CreateLink(ctx, windowA, tailC, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink(%s,%s): %v", windowA, tailC, err)
	}

	res := run(t, s, collapseRequest())
	ids := itemIDs(res.Items)

	// The answer is the window: both unlinked rows, and the tail row removed by
	// the second pass rather than answered beside its cluster mate.
	for _, id := range []string{windowA, windowX} {
		if !hasID(ids, id) {
			t.Errorf("items = %v, want the pair-free window row %s in the answer", ids, id)
		}
	}
	if hasID(ids, tailC) {
		t.Errorf("items = %v, want %s removed as the tail pass's near-duplicate loser", ids, tailC)
	}

	// The tail pass's removal is reported, naming the window row it lost to: a
	// removal that reaches the answer but not the verdict is the row-accounting
	// loss DroppedLosers exists to prevent.
	losers := collectLosers(t, res)
	if len(losers) != 1 {
		t.Fatalf("the trace holds %d dropped losers, want exactly the tail pass's one (%s): %+v",
			len(losers), tailC, res.Trace.Decisions)
	}
	l := losers[0]
	if l.ID != tailC {
		t.Errorf("the dropped loser is %s, want the tail row %s", l.ID, tailC)
	}
	if !reflect.DeepEqual(l.Against, []string{windowA}) {
		t.Errorf("decision for %s has Against = %v, want [%s] — the window row it is linked to", l.ID, l.Against, windowA)
	}
	for _, id := range []string{windowA, windowX} {
		if hasID(loserIDs(losers), id) {
			t.Errorf("the unlinked window row %s is filed as a dropped loser", id)
		}
	}
	for _, l := range losers {
		if hasID(ids, l.ID) {
			t.Errorf("%s is in Rows %v and filed as a dropped loser at the same time", l.ID, ids)
		}
	}

	// Explain is the same run projected: the tail loser excluded with the id it
	// lost to, both window rows included.
	row := explainRowByID(t, res.Explain, tailC)
	if row.Included {
		t.Errorf("the removed tail row %s is marked included", tailC)
	}
	if !reflect.DeepEqual(row.NearDuplicateOf, []string{windowA}) {
		t.Errorf("row %s: near_duplicate_of = %v, want [%s]", tailC, row.NearDuplicateOf, windowA)
	}
	for _, id := range []string{windowA, windowX} {
		if !explainRowByID(t, res.Explain, id).Included {
			t.Errorf("the window row %s is not included", id)
		}
	}
}

// loserIDs projects the ids of a set of dropped decisions, so an assertion can
// ask whether a particular row was removed without re-walking the decisions.
func loserIDs(losers []Decision) []string {
	ids := make([]string, 0, len(losers))
	for _, l := range losers {
		ids = append(ids, l.ID)
	}
	return ids
}
