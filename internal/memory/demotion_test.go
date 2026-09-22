package memory

import (
	"context"
	"testing"
)

func TestDemotionPenaltiesDemotesLowerRanked(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "alpha fact")
	b := makeMemory(t, s, "alpha fact restated")

	if err := s.CreateLink(ctx, a, b, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	penalty, err := DemotionPenalties(ctx, s.db, []string{a, b}, map[string]bool{}, 0.90)
	if err != nil {
		t.Fatalf("DemotionPenalties: %v", err)
	}
	if penalty[b] != 1 {
		t.Errorf("penalty[b] = %d, want 1", penalty[b])
	}
	if penalty[a] != 0 {
		t.Errorf("penalty[a] = %d, want 0", penalty[a])
	}
}

// TestSupersedePenalties: the shared injection/search helper penalizes only
// the superseded side, and only when both endpoints are in the window —
// same rule as SupersedesWithin.
func TestSupersedePenalties(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	stale := makeMemory(t, s, "stale payments region")
	fresh := makeMemory(t, s, "fresh payments region")
	outside := makeMemory(t, s, "unrelated memory")

	if err := s.CreateLink(ctx, fresh, stale, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	// Edge whose counterpart is outside the window must not count.
	if err := s.CreateLink(ctx, outside, stale, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink outside: %v", err)
	}

	penalty, err := SupersedePenalties(ctx, s.db, []string{fresh, stale})
	if err != nil {
		t.Fatalf("SupersedePenalties: %v", err)
	}
	if penalty[stale] != 1 {
		t.Errorf("penalty[stale] = %d, want 1 (only the in-window edge)", penalty[stale])
	}
	if penalty[fresh] != 0 {
		t.Errorf("penalty[fresh] = %d, want 0 (superseder is never sunk)", penalty[fresh])
	}

	if _, err := SupersedePenalties(ctx, s.db, []string{fresh}); err != nil {
		t.Fatalf("single-id window: %v", err)
	}
}

func TestDemotionPenaltiesIgnoresBelowThreshold(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "topic A discussion")
	b := makeMemory(t, s, "topic A follow-up")

	// Related, but not redundant: clears linking.threshold (0.70) without
	// clearing linking.demotion_threshold (0.90).
	if err := s.CreateLink(ctx, a, b, "related", 0.75, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	penalty, err := DemotionPenalties(ctx, s.db, []string{a, b}, map[string]bool{}, 0.90)
	if err != nil {
		t.Fatalf("DemotionPenalties: %v", err)
	}
	if penalty[a] != 0 || penalty[b] != 0 {
		t.Errorf("expected no penalties below threshold, got a=%d b=%d", penalty[a], penalty[b])
	}
}

func TestDemotionPenaltiesNeverPenalizesPinnedOverUnpinned(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// a ranks higher than b (index 0 vs 1) but b is pinned and a is not.
	a := makeMemory(t, s, "unpinned near-duplicate, ranked higher")
	b := makeMemory(t, s, "pinned near-duplicate, ranked lower")

	if err := s.CreateLink(ctx, a, b, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	pinned := map[string]bool{b: true}
	penalty, err := DemotionPenalties(ctx, s.db, []string{a, b}, pinned, 0.90)
	if err != nil {
		t.Fatalf("DemotionPenalties: %v", err)
	}
	if penalty[a] != 1 {
		t.Errorf("penalty[a] = %d, want 1 (unpinned should absorb the penalty)", penalty[a])
	}
	if penalty[b] != 0 {
		t.Errorf("penalty[b] = %d, want 0 (pinned must survive)", penalty[b])
	}
}

func TestDemotionPenaltiesCollapsesMutualCluster(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "cluster fact v1")
	b := makeMemory(t, s, "cluster fact v2")
	c := makeMemory(t, s, "cluster fact v3")

	// All-pairwise above threshold: a is rank 0, b rank 1, c rank 2.
	if err := s.CreateLink(ctx, a, b, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink a-b: %v", err)
	}
	if err := s.CreateLink(ctx, a, c, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink a-c: %v", err)
	}
	if err := s.CreateLink(ctx, b, c, "related", 0.95, "auto"); err != nil {
		t.Fatalf("CreateLink b-c: %v", err)
	}

	penalty, err := DemotionPenalties(ctx, s.db, []string{a, b, c}, map[string]bool{}, 0.90)
	if err != nil {
		t.Fatalf("DemotionPenalties: %v", err)
	}
	if penalty[a] != 0 {
		t.Errorf("penalty[a] = %d, want 0 (top-ranked survivor)", penalty[a])
	}
	if penalty[b] != 1 {
		t.Errorf("penalty[b] = %d, want 1", penalty[b])
	}
	if penalty[c] != 2 {
		t.Errorf("penalty[c] = %d, want 2 (loses to both a and b)", penalty[c])
	}

	items := []string{a, b, c}
	ordered := StableDemote(items, func(id string) string { return id }, penalty)
	if ordered[0] != a {
		t.Errorf("expected a first after StableDemote, got %v", ordered)
	}
}

func TestDemotionPenaltiesQueryErrorPropagates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	_, err := DemotionPenalties(ctx, s.db, []string{"a", "b"}, map[string]bool{}, 0.90)
	if err == nil {
		t.Fatal("expected error from DemotionPenalties on closed db, got nil")
	}
}

// TestDemotionPenaltiesIncludesDuplicateEdges: Upsert's 'duplicate' edges are
// exempt from the cosine strength threshold (their strength is a Jaccard
// score), so a lexically near-identical pair is demoted even though the linker
// never wrote a >=0.90 'related' edge for it.
func TestDemotionPenaltiesIncludesDuplicateEdges(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "deploy uses postgres sixteen")
	b := makeMemory(t, s, "deploy uses postgres sixteen")

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_links (source_id, target_id, relation, strength, source)
		 VALUES (?, ?, 'duplicate', 0.5, 'auto')`, b, a); err != nil {
		t.Fatalf("insert duplicate link: %v", err)
	}

	// Rank order [b, a]: the lower-ranked member (a) must be penalized.
	penalty, err := DemotionPenalties(ctx, s.db, []string{b, a}, map[string]bool{}, DefaultDemotionThreshold)
	if err != nil {
		t.Fatalf("DemotionPenalties: %v", err)
	}
	if penalty[a] != 1 {
		t.Errorf("expected duplicate edge to penalize lower-ranked %s, got penalty=%v", a, penalty)
	}
}

// TestNearDuplicateDemoteReordersSearchWindow verifies the search path's
// demotion actually reorders the window: with [B, A, other] and a B-A
// duplicate edge, A (lower-ranked) sinks below the unrelated memory.
func TestNearDuplicateDemoteReordersSearchWindow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "alpha duplicate")
	b := makeMemory(t, s, "alpha duplicate")
	other := makeMemory(t, s, "unrelated fact")

	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memory_links (source_id, target_id, relation, strength, source)
		 VALUES (?, ?, 'duplicate', 0.5, 'auto')`, b, a); err != nil {
		t.Fatalf("insert duplicate link: %v", err)
	}

	in := []Memory{{ID: b}, {ID: a}, {ID: other}}
	out := s.demoteNearDuplicates(ctx, in)
	if len(out) != 3 {
		t.Fatalf("demotion changed membership: %v", out)
	}
	if out[0].ID != b {
		t.Errorf("duplicate winner should stay first, got %s", out[0].ID)
	}
	if out[2].ID != a {
		t.Errorf("duplicate loser should sink below the unrelated memory, got order %s,%s,%s",
			out[0].ID, out[1].ID, out[2].ID)
	}
}
