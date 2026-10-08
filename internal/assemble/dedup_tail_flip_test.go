package assemble

import (
	"context"
	"reflect"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestQueryModeCollapseProtectsAPersistentTailWinner (#926, review of #940):
// the second removal pass over window+tail decides losers by position, then
// FLIPS the decision when the positioned loser is protected and the winner is
// not (demotion.go: a pinned or retention-exempt row never loses to a plain
// one). So a persistent row hydrating into the tail can make the WINDOW row
// the loser of the second pass — and that is not a tail removal: the window's
// representative has to leave Rows too, and the real winner (the protected
// tail row) has to stay.
//
// Before the membership partition, the pass sliced keptAll by the window's
// length instead of by membership: the loser stayed in the answer while being
// filed as dropped, and the winner was silently cut from the answer with no
// verdict anywhere — the row-accounting loss DroppedLosers exists to prevent.
func TestQueryModeCollapseProtectsAPersistentTailWinner(t *testing.T) {
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
	mk := func(content, retention string) string {
		t.Helper()
		id, err := s.Create(ctx, "proj", memory.Memory{
			Category: "fact", Content: content, Source: "manual",
			Importance: 0.7, Retention: retention,
		})
		if err != nil {
			t.Fatalf("Create(%q): %v", content, err)
		}
		return id
	}
	// The plain pair ranks into the 2-row window (short rows outrank long
	// ones); the persistent row is lexically a member of the cluster but far
	// longer, so bm25 ranks it below the window and it hydrates into the tail.
	// The pair's own order is a bm25 TIE (same query terms, same length band,
	// same importance), so it is SQLite's sorter that picks it and nothing in
	// the repo pins that: the protected row therefore links to BOTH members —
	// whichever one survives the window is the one it beats in the second
	// pass — and the assertions below derive which member that was from the
	// answer instead of assuming it. The sibling
	// TestQueryModeCollapseSpansTheWindowAndTheTail holds the same discipline.
	dupA := mk("database configuration for the replica refresh runs hourly", "project")
	dupB := mk("database configuration of the standby snapshot runs nightly", "project")
	dupProtected := mk("database configuration of the standby snapshot replica pair runs nightly for the refresh cycle under the platform team's retention review", "persistent")
	for _, pair := range [][2]string{{dupA, dupB}, {dupA, dupProtected}, {dupB, dupProtected}} {
		if err := s.CreateLink(ctx, pair[0], pair[1], "related", 0.95, "auto"); err != nil {
			t.Fatalf("CreateLink(%s,%s): %v", pair[0], pair[1], err)
		}
	}

	req := collapseRequest()
	res := run(t, s, req)
	ids := itemIDs(res.Items)

	// The protected tail row is the real winner of the second pass and must be
	// in the answer; the plain window row is its loser and must not be.
	if !hasID(ids, dupProtected) {
		t.Errorf("items = %v, want the persistent row the second pass chose as representative", ids)
	}
	if hasID(ids, dupA) || hasID(ids, dupB) {
		t.Errorf("items = %v, want the plain cluster members removed (%s lost the second pass, %s the first)", ids, dupA, dupB)
	}

	// The contract on CandidateSet.DroppedLosers: not in Rows. A row filed as
	// dropped and still answered is two accounts of one retrieval disagreeing.
	for _, l := range collectLosers(t, res) {
		if hasID(ids, l.ID) {
			t.Errorf("%s is in Rows %v and filed as a dropped loser at the same time", l.ID, ids)
		}
	}

	// Both plain members are removed: the window's first-pass loser, and the
	// window representative the protected tail row beat in the second pass.
	// Which of the two was the window representative is a bm25 tie the sorter
	// decided, so derive it: the second-pass loser is the one that lost to the
	// protected row, and the first-pass loser is the one that lost to ITS
	// fellow plain member.
	losers := collectLosers(t, res)
	if len(losers) != 2 {
		t.Fatalf("the trace holds %d dropped losers, want 2 (first pass + second pass): %+v", len(losers), res.Trace.Decisions)
	}
	byID := map[string]Decision{}
	for _, l := range losers {
		byID[l.ID] = l
		if l.Stage != stageDedup || l.Reason != reasonNearDuplicate || l.Kept {
			t.Errorf("decision for %s = %+v, want a dropped %s/%s decision", l.ID, l, stageDedup, reasonNearDuplicate)
		}
	}
	rep, firstLoser := "", ""
	for _, id := range []string{dupA, dupB} {
		l, ok := byID[id]
		if !ok {
			t.Fatalf("plain cluster member %s has no dropped decision: %+v", id, losers)
		}
		if reflect.DeepEqual(l.Against, []string{dupProtected}) {
			rep = id
		} else {
			firstLoser = id
		}
	}
	if rep == "" || firstLoser == "" {
		t.Fatalf("cannot derive the window representative from Against: %s=%v %s=%v (protected %s)",
			dupA, byID[dupA].Against, dupB, byID[dupB].Against, dupProtected)
	}
	// The second pass's verdict is the one the membership partition exists to
	// keep visible: the window representative lost to the protected tail row.
	if got := byID[rep].Against; !reflect.DeepEqual(got, []string{dupProtected}) {
		t.Errorf("decision for the window representative %s has Against = %v, want [%s] — the protected row that won the second pass", rep, got, dupProtected)
	}
	// The first-pass loser lost to the member that represented the window
	// when the removal ran.
	if got := byID[firstLoser].Against; !reflect.DeepEqual(got, []string{rep}) {
		t.Errorf("decision for the first-pass loser %s has Against = %v, want [%s] — the window member it lost to", firstLoser, got, rep)
	}

	// Explain is the same run projected: both losers excluded with who they
	// lost to, the protected winner included.
	for _, tc := range []struct{ id, winner string }{{rep, dupProtected}, {firstLoser, rep}} {
		row := explainRowByID(t, res.Explain, tc.id)
		if row.Included {
			t.Errorf("the removed cluster member %s is marked included", tc.id)
		} else if !reflect.DeepEqual(row.NearDuplicateOf, []string{tc.winner}) {
			t.Errorf("row %s: near_duplicate_of = %v, want [%s]", tc.id, row.NearDuplicateOf, tc.winner)
		}
	}
	if !explainRowByID(t, res.Explain, dupProtected).Included {
		t.Errorf("the persistent representative %s is not included", dupProtected)
	}
}
