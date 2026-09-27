package memory

import (
	"context"
	"testing"
)

// makeMemory creates a memory and returns its ID.
func makeMemory(t *testing.T, s *Store, content string) string {
	t.Helper()
	id, err := s.Create(context.Background(), testProject, Memory{
		Category: "fact", Content: content, Source: "manual", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return id
}

func TestCreateAndGetLinks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "memory alpha about SQLite WAL mode")
	b := makeMemory(t, s, "memory beta about SQLite busy timeout")

	if err := s.CreateLink(ctx, a, b, "related", 0.85, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}

	// Links are visible from BOTH endpoints.
	for _, id := range []string{a, b} {
		links, err := s.GetLinks(ctx, id)
		if err != nil {
			t.Fatalf("GetLinks(%s): %v", id, err)
		}
		if len(links) != 1 {
			t.Fatalf("GetLinks(%s): got %d links, want 1", id, len(links))
		}
		if links[0].Relation != "related" || links[0].Strength != 0.85 {
			t.Errorf("link = %+v, want relation=related strength=0.85", links[0])
		}
	}
}

func TestCreateLinkNormalizesSymmetricPair(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "alpha content one")
	b := makeMemory(t, s, "beta content two")

	// Inserting A→B then B→A for symmetric 'related' must not duplicate.
	if err := s.CreateLink(ctx, a, b, "related", 0.80, "auto"); err != nil {
		t.Fatalf("CreateLink a->b: %v", err)
	}
	if err := s.CreateLink(ctx, b, a, "related", 0.90, "auto"); err != nil {
		t.Fatalf("CreateLink b->a: %v", err)
	}
	links, err := s.GetLinks(ctx, a)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("got %d links, want 1 (symmetric pair must normalize)", len(links))
	}
	if links[0].Strength != 0.90 {
		t.Errorf("strength = %f, want 0.90 (re-insert keeps higher strength)", links[0].Strength)
	}
}

func TestCreateLinkRejectsSelfLink(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "self referential memory")

	if err := s.CreateLink(ctx, a, a, "related", 0.9, "auto"); err == nil {
		t.Fatal("CreateLink(a, a) succeeded, want error")
	}
}

func TestLinksCascadeOnMemoryDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "alpha to be deleted")
	b := makeMemory(t, s, "beta survivor")

	if err := s.CreateLink(ctx, a, b, "related", 0.8, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if err := s.Delete(ctx, a); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	links, err := s.GetLinks(ctx, b)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("got %d links after cascade delete, want 0", len(links))
	}
}

func TestInvalidateLinkHidesFromGetLinks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "alpha invalidation test")
	b := makeMemory(t, s, "beta invalidation test")

	if err := s.CreateLink(ctx, a, b, "related", 0.8, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	// Invalidate using reversed order — normalization must still find it.
	if err := s.InvalidateLink(ctx, b, a, "related"); err != nil {
		t.Fatalf("InvalidateLink: %v", err)
	}
	links, err := s.GetLinks(ctx, a)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("got %d links after invalidation, want 0", len(links))
	}
	// Re-creating the link revives it.
	if err := s.CreateLink(ctx, a, b, "related", 0.9, "auto"); err != nil {
		t.Fatalf("CreateLink revive: %v", err)
	}
	links, _ = s.GetLinks(ctx, a)
	if len(links) != 1 {
		t.Fatalf("got %d links after revive, want 1", len(links))
	}
}

func TestUnscannedEmbeddedMemoryIDs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "embedded and unscanned")
	b := makeMemory(t, s, "embedded and scanned")
	_ = makeMemory(t, s, "not embedded")

	vec := []float32{1, 0, 0}
	if err := s.StoreEmbedding(ctx, a, vec, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if err := s.StoreEmbedding(ctx, b, vec, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if err := s.MarkLinkScanned(ctx, b); err != nil {
		t.Fatalf("MarkLinkScanned: %v", err)
	}

	ids, err := s.UnscannedEmbeddedMemoryIDs(ctx, testProject, 10)
	if err != nil {
		t.Fatalf("UnscannedEmbeddedMemoryIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != a {
		t.Fatalf("got %v, want [%s] (embedded, unscanned only)", ids, a)
	}
}

func TestGetEmbedding(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "embedding roundtrip")

	want := []float32{0.1, 0.2, 0.3}
	if err := s.StoreEmbedding(ctx, a, want, "test-model"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	got, err := s.GetEmbedding(ctx, a)
	if err != nil {
		t.Fatalf("GetEmbedding: %v", err)
	}
	if len(got) != 3 || got[0] != 0.1 || got[1] != 0.2 || got[2] != 0.3 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestLinksByRelationSource(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "decision: switched to Postgres LISTEN/NOTIFY")
	b := makeMemory(t, s, "gotcha: NATS can reorder messages under partition rebalance")
	c := makeMemory(t, s, "unrelated memory about grafana ports")

	if err := s.CreateLink(ctx, a, b, "causes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink causes: %v", err)
	}
	if err := s.CreateLink(ctx, c, a, "supersedes", 0.85, "manual"); err != nil {
		t.Fatalf("CreateLink supersedes: %v", err)
	}

	links, err := s.LinksByRelationSource(ctx, testProject, "causes", "llm")
	if err != nil {
		t.Fatalf("LinksByRelationSource: %v", err)
	}
	if len(links) != 1 || links[0].SourceID != a || links[0].TargetID != b {
		t.Fatalf("got %+v, want exactly one causes/llm link a->b", links)
	}

	// A different source ('manual') for the same relation must not match.
	none, err := s.LinksByRelationSource(ctx, testProject, "supersedes", "llm")
	if err != nil {
		t.Fatalf("LinksByRelationSource: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("got %d links, want 0 (source filter must exclude 'manual')", len(none))
	}
}

// TestSupersedesWithinIgnoresScopeConflictingEdge: this function has no
// production caller — SupersedePenalties is the reader ranking uses — but it is
// exported and documented as returning the 'supersedes' edges ranking consults,
// so a caller added later would inherit an answer that contradicts every other
// reader of the same edge. The guard is here so the function's contract cannot
// quietly disagree with the rest of the store.
func TestSupersedesWithinIgnoresScopeConflictingEdge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	dev := makeScopedMemory(t, s, "api listen port is 8443", "development")
	prod := makeScopedMemory(t, s, "api listen port is 8443", "production")
	sameProd := makeScopedMemory(t, s, "worker pool size is 12", "production")
	sameProdNewer := makeScopedMemory(t, s, "worker pool size is 12", "production")

	if err := s.CreateLink(ctx, dev, prod, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink conflicting: %v", err)
	}
	if err := s.CreateLink(ctx, sameProdNewer, sameProd, "supersedes", 0.95, "llm"); err != nil {
		t.Fatalf("CreateLink same scope: %v", err)
	}

	// The conflicting edge is not deleted, only withheld.
	links, err := s.GetLinks(ctx, prod)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("scope guard must not delete the existing edge, got %+v", links)
	}

	pairs, err := s.SupersedesWithin(ctx, []string{dev, prod, sameProdNewer, sameProd})
	if err != nil {
		t.Fatalf("SupersedesWithin: %v", err)
	}
	if len(pairs) != 1 || pairs[0] != [2]string{sameProdNewer, sameProd} {
		t.Errorf("SupersedesWithin = %v, want only the same-scope pair %v", pairs, [2]string{sameProdNewer, sameProd})
	}
}

func TestLinksByRelationSourceExcludesInvalidated(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "decision alpha")
	b := makeMemory(t, s, "evidence beta")

	if err := s.CreateLink(ctx, a, b, "causes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if err := s.InvalidateLink(ctx, a, b, "causes"); err != nil {
		t.Fatalf("InvalidateLink: %v", err)
	}

	links, err := s.LinksByRelationSource(ctx, testProject, "causes", "llm")
	if err != nil {
		t.Fatalf("LinksByRelationSource: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("got %d links, want 0 (invalidated link must be excluded)", len(links))
	}
}

func TestEmbeddingAndLinkStats(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	a := makeMemory(t, s, "stats memory one")
	b := makeMemory(t, s, "stats memory two")
	_ = makeMemory(t, s, "stats memory three unembedded")

	if err := s.StoreEmbedding(ctx, a, []float32{1, 0}, "test"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if err := s.StoreEmbedding(ctx, b, []float32{0, 1}, "test"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if err := s.CreateLink(ctx, a, b, "related", 0.9, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if err := s.MarkLinkScanned(ctx, a); err != nil {
		t.Fatalf("MarkLinkScanned: %v", err)
	}

	embedded, stale, total, err := s.EmbeddingStats(ctx)
	if err != nil {
		t.Fatalf("EmbeddingStats: %v", err)
	}
	if embedded != 2 || total != 3 {
		t.Errorf("EmbeddingStats = %d/%d, want 2/3", embedded, total)
	}
	if stale != 0 {
		t.Errorf("EmbeddingStats stale = %d, want 0: no identity is configured, so no row is retired", stale)
	}

	links, scans, err := s.LinkStats(ctx)
	if err != nil {
		t.Fatalf("LinkStats: %v", err)
	}
	if links != 1 || scans != 1 {
		t.Errorf("LinkStats = links %d scans %d, want 1 and 1", links, scans)
	}
}
