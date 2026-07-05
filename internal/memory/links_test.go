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

func TestGraphNeighborsTwoHops(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Chain: a — b — c, plus d unlinked.
	a := makeMemory(t, s, "graph node a")
	b := makeMemory(t, s, "graph node b")
	c := makeMemory(t, s, "graph node c")
	_ = makeMemory(t, s, "graph node d unlinked")

	if err := s.CreateLink(ctx, a, b, "related", 0.9, "auto"); err != nil {
		t.Fatalf("CreateLink a-b: %v", err)
	}
	if err := s.CreateLink(ctx, b, c, "related", 0.8, "auto"); err != nil {
		t.Fatalf("CreateLink b-c: %v", err)
	}

	// From seed a with 2 hops: b at depth 1 (strength 0.9), c at depth 2 (0.9*0.8=0.72).
	neighbors, err := s.GraphNeighbors(ctx, testProject, []string{a}, 2, 10)
	if err != nil {
		t.Fatalf("GraphNeighbors: %v", err)
	}
	if len(neighbors) != 2 {
		t.Fatalf("got %d neighbors, want 2: %+v", len(neighbors), neighbors)
	}
	if neighbors[0].MemoryID != b || neighbors[0].Depth != 1 {
		t.Errorf("first neighbor = %+v, want id=%s depth=1", neighbors[0], b)
	}
	if neighbors[1].MemoryID != c || neighbors[1].Depth != 2 {
		t.Errorf("second neighbor = %+v, want id=%s depth=2", neighbors[1], c)
	}
	if diff := neighbors[1].Strength - 0.72; diff > 0.001 || diff < -0.001 {
		t.Errorf("c strength = %f, want ~0.72", neighbors[1].Strength)
	}

	// With 1 hop, only b is reachable.
	neighbors, err = s.GraphNeighbors(ctx, testProject, []string{a}, 1, 10)
	if err != nil {
		t.Fatalf("GraphNeighbors 1hop: %v", err)
	}
	if len(neighbors) != 1 || neighbors[0].MemoryID != b {
		t.Fatalf("1-hop got %+v, want only %s", neighbors, b)
	}
}

func TestGraphNeighborsExcludesSeedsAndCycles(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Triangle: a — b — c — a. Seeds {a, b}: only c should be returned.
	a := makeMemory(t, s, "cycle node a")
	b := makeMemory(t, s, "cycle node b")
	c := makeMemory(t, s, "cycle node c")

	for _, pair := range [][2]string{{a, b}, {b, c}, {c, a}} {
		if err := s.CreateLink(ctx, pair[0], pair[1], "related", 0.9, "auto"); err != nil {
			t.Fatalf("CreateLink: %v", err)
		}
	}
	neighbors, err := s.GraphNeighbors(ctx, testProject, []string{a, b}, 2, 10)
	if err != nil {
		t.Fatalf("GraphNeighbors: %v", err)
	}
	if len(neighbors) != 1 || neighbors[0].MemoryID != c {
		t.Fatalf("got %+v, want only %s (seeds excluded, no cycle blowup)", neighbors, c)
	}
}

func TestGraphNeighborsEmptySeeds(t *testing.T) {
	s := testStore(t)
	neighbors, err := s.GraphNeighbors(context.Background(), testProject, nil, 2, 10)
	if err != nil {
		t.Fatalf("GraphNeighbors(nil): %v", err)
	}
	if len(neighbors) != 0 {
		t.Fatalf("got %d neighbors for empty seeds, want 0", len(neighbors))
	}
}

func TestGraphNeighborsDoesNotRouteThroughOtherProjects(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "other-project", "/tmp/other", "other"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// x and z in testProject; y in other-project bridges them: x — y — z.
	x := makeMemory(t, s, "in-project node x")
	y, err := s.Create(ctx, "other-project", Memory{Category: "fact", Content: "foreign bridge node", Source: "manual", Importance: 0.7})
	if err != nil {
		t.Fatalf("Create y: %v", err)
	}
	z := makeMemory(t, s, "in-project node z")

	if err := s.CreateLink(ctx, x, y, "related", 0.9, "manual"); err != nil {
		t.Fatalf("CreateLink x-y: %v", err)
	}
	if err := s.CreateLink(ctx, y, z, "related", 0.9, "manual"); err != nil {
		t.Fatalf("CreateLink y-z: %v", err)
	}

	// Project-scoped walk from x must not reach z via the foreign bridge y.
	neighbors, err := s.GraphNeighbors(ctx, testProject, []string{x}, 2, 10)
	if err != nil {
		t.Fatalf("GraphNeighbors: %v", err)
	}
	if len(neighbors) != 0 {
		t.Fatalf("got %+v, want none (foreign intermediate must block the path)", neighbors)
	}

	// Unscoped walk (projectID "") still traverses the full graph.
	neighbors, err = s.GraphNeighbors(ctx, "", []string{x}, 2, 10)
	if err != nil {
		t.Fatalf("GraphNeighbors all: %v", err)
	}
	if len(neighbors) != 2 {
		t.Fatalf("unscoped got %+v, want y and z", neighbors)
	}
}

func TestGraphNeighborsTraversesGlobalBridge(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// _global memories are shared context: x — g(_global) — z must work.
	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}
	x := makeMemory(t, s, "project node x")
	g, err := s.Create(ctx, "_global", Memory{Category: "fact", Content: "global bridge", Source: "manual", Importance: 0.7})
	if err != nil {
		t.Fatalf("Create global: %v", err)
	}
	z := makeMemory(t, s, "project node z")

	if err := s.CreateLink(ctx, x, g, "related", 0.9, "manual"); err != nil {
		t.Fatalf("CreateLink x-g: %v", err)
	}
	if err := s.CreateLink(ctx, g, z, "related", 0.9, "manual"); err != nil {
		t.Fatalf("CreateLink g-z: %v", err)
	}

	neighbors, err := s.GraphNeighbors(ctx, testProject, []string{x}, 2, 10)
	if err != nil {
		t.Fatalf("GraphNeighbors: %v", err)
	}
	if len(neighbors) != 2 {
		t.Fatalf("got %+v, want g and z (global bridge traversable)", neighbors)
	}
}
