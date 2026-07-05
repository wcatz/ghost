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
