package memory

import (
	"context"
	"testing"
)

func makeScopedMemory(t *testing.T, s *Store, content, environment string) string {
	t.Helper()
	id, err := s.Create(context.Background(), testProject, Memory{
		Category: "fact", Content: content, Source: "manual", Importance: 0.7,
		Scope: map[string]string{"environment": environment},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

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

func TestDemotionPenaltiesIgnoresScopeConflictingRelatedEdge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	dev := makeScopedMemory(t, s, "development database uses SQLite", "development")
	prod := makeScopedMemory(t, s, "production database uses PostgreSQL", "production")
	if err := s.CreateLink(ctx, dev, prod, "related", 0.95, "manual"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	links, err := s.GetLinks(ctx, dev)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("scope guard must not delete the existing edge, got %+v", links)
	}
	penalty, err := DemotionPenalties(ctx, s.db, []string{dev, prod}, map[string]bool{}, 0.90)
	if err != nil {
		t.Fatalf("DemotionPenalties: %v", err)
	}
	if len(penalty) != 0 {
		t.Fatalf("scope-conflicting related edge produced demotion penalties: %v", penalty)
	}
}

func TestDemotionPenaltiesKeepsOneSidedScopeRelatedEdge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scoped := makeScopedMemory(t, s, "production database uses PostgreSQL", "production")
	unscoped := makeMemory(t, s, "database guidance applies generally")
	if err := s.CreateLink(ctx, scoped, unscoped, "related", 0.95, "manual"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	penalty, err := DemotionPenalties(ctx, s.db, []string{scoped, unscoped}, map[string]bool{}, 0.90)
	if err != nil {
		t.Fatalf("DemotionPenalties: %v", err)
	}
	if penalty[unscoped] != 1 {
		t.Errorf("one-sided scope should remain compatible, got penalties %v", penalty)
	}
}

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

	// nil protection: nothing here is pinned or keep-forever, and the two tests
	// below are about the EDGE and about scope. The map is the caller's, not this
	// function's — see SupersedePenalties and
	// TestTheDemotionLookupsDoNotNameTheTierColumn.
	penalty, err := SupersedePenalties(ctx, s.db, []string{fresh, stale}, nil)
	if err != nil {
		t.Fatalf("SupersedePenalties: %v", err)
	}
	if penalty[stale] != 1 {
		t.Errorf("penalty[stale] = %d, want 1 (only the in-window edge)", penalty[stale])
	}
	if penalty[fresh] != 0 {
		t.Errorf("penalty[fresh] = %d, want 0 (superseder is never sunk)", penalty[fresh])
	}

	if _, err := SupersedePenalties(ctx, s.db, []string{fresh}, nil); err != nil {
		t.Fatalf("single-id window: %v", err)
	}
}

func TestSupersedePenaltiesIgnoresScopeConflictingEdge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	dev := makeScopedMemory(t, s, "development pool timeout is 5s", "development")
	prod := makeScopedMemory(t, s, "production pool timeout is 30s", "production")
	if err := s.CreateLink(ctx, dev, prod, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	links, err := s.GetLinks(ctx, dev)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("scope guard must not delete the existing edge, got %+v", links)
	}
	penalty, err := SupersedePenalties(ctx, s.db, []string{dev, prod}, nil)
	if err != nil {
		t.Fatalf("SupersedePenalties: %v", err)
	}
	if len(penalty) != 0 {
		t.Fatalf("scope-conflicting supersedes edge produced demotion penalties: %v", penalty)
	}
}

func TestSupersedePenaltiesKeepsOneSidedScopeEdge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	scoped := makeScopedMemory(t, s, "production pool timeout is 30s", "production")
	unscoped := makeMemory(t, s, "pool timeout is 5s everywhere")
	if err := s.CreateLink(ctx, scoped, unscoped, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	penalty, err := SupersedePenalties(ctx, s.db, []string{scoped, unscoped}, nil)
	if err != nil {
		t.Fatalf("SupersedePenalties: %v", err)
	}
	if penalty[unscoped] != 1 {
		t.Errorf("one-sided scope should remain compatible, got penalties %v", penalty)
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
	out := s.demoteNearDuplicates(ctx, in, DefaultSearchParams())
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
