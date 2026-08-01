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
