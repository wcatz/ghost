package memory

import (
	"context"
	"fmt"
	"strings"
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

	if err := s.CreateLink(ctx, a, a, "related", 0.9, "auto"); err == nil {
		t.Fatal("CreateLink(a, a) succeeded, want error")
	}
	// A symmetric pair inserted both ways is one link that keeps the higher strength.
	if err := s.CreateLink(ctx, a, b, "related", 0.80, "auto"); err != nil {
		t.Fatalf("CreateLink a->b: %v", err)
	}
	if err := s.CreateLink(ctx, b, a, "related", 0.90, "auto"); err != nil {
		t.Fatalf("CreateLink b->a: %v", err)
	}
	// Links are visible from BOTH endpoints.
	for _, id := range []string{a, b} {
		links, err := s.GetLinks(ctx, id)
		if err != nil || len(links) != 1 {
			t.Fatalf("GetLinks(%s) = %d links, %v, want 1", id, len(links), err)
		}
		if links[0].Relation != "related" || links[0].Strength != 0.90 {
			t.Errorf("link = %+v, want relation=related strength=0.90", links[0])
		}
	}

	c := makeMemory(t, s, "alpha to be deleted")
	if err := s.CreateLink(ctx, c, b, "related", 0.8, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	if err := s.Delete(ctx, c); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if links, err := s.GetLinks(ctx, b); err != nil || len(links) != 1 {
		t.Fatalf("after cascade delete GetLinks(b) = %d, %v, want only the a-b link", len(links), err)
	}
}

// TestInvalidateLinkCountsOnlyLiveLinks pins the count's meaning: links that LEFT
// the live set, not rows the UPDATE matched, so re-invalidating a dead link or
// an absent relation reports 0. A symmetric link is found in either order,
// hides it from GetLinks and LinksByRelationSource, and re-creating revives it.
func TestInvalidateLinkCountsOnlyLiveLinks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "alpha count test")
	b := makeMemory(t, s, "beta count test")

	if err := s.CreateLink(ctx, a, b, "supersedes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	for _, tc := range []struct {
		x, y, rel string
		want      int64
	}{
		{a, b, "supersedes", 1},
		{a, b, "supersedes", 0},
		{a, b, "causes", 0},
	} {
		n, err := s.InvalidateLink(ctx, tc.x, tc.y, tc.rel)
		if err != nil || n != tc.want {
			t.Errorf("InvalidateLink(%s) = %d, %v, want %d", tc.rel, n, err, tc.want)
		}
	}
	if links, _ := s.GetLinks(ctx, a); len(links) != 0 {
		t.Fatalf("got %d links after invalidation, want 0", len(links))
	}
	if links, err := s.LinksByRelationSource(ctx, testProject, "supersedes", "llm"); err != nil || len(links) != 0 {
		t.Fatalf("LinksByRelationSource kept an invalidated link: %d, %v", len(links), err)
	}
	if err := s.CreateLink(ctx, a, b, "supersedes", 0.9, "llm"); err != nil {
		t.Fatalf("CreateLink revive: %v", err)
	}
	if links, _ := s.GetLinks(ctx, a); len(links) != 1 {
		t.Fatalf("got %d links after revive, want 1", len(links))
	}
	// A symmetric link is found whichever order it is invalidated in.
	if err := s.CreateLink(ctx, a, b, "related", 0.8, "auto"); err != nil {
		t.Fatalf("CreateLink related: %v", err)
	}
	if n, err := s.InvalidateLink(ctx, b, a, "related"); err != nil || n != 1 {
		t.Errorf("InvalidateLink(reversed related) = %d, %v, want 1", n, err)
	}
}

func TestUnscannedEmbeddedMemoryIDs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := makeMemory(t, s, "embedded and unscanned")
	b := makeMemory(t, s, "embedded and scanned")
	_ = makeMemory(t, s, "not embedded")

	vec := []float32{1, 0, 0}
	for _, id := range []string{a, b} {
		if err := s.StoreEmbedding(ctx, id, vec, "test-model"); err != nil {
			t.Fatalf("StoreEmbedding: %v", err)
		}
	}
	got, err := s.GetEmbedding(ctx, a)
	if err != nil || len(got) != 3 || got[0] != 1 {
		t.Fatalf("GetEmbedding = %v, %v, want %v", got, err, vec)
	}
	if err := s.MarkLinkScanned(ctx, b, vec); err != nil {
		t.Fatalf("MarkLinkScanned: %v", err)
	}
	ids, err := s.UnscannedEmbeddedMemoryIDs(ctx, testProject, 10)
	if err != nil || len(ids) != 1 || ids[0] != a {
		t.Fatalf("got %v, %v, want [%s] (embedded, unscanned only)", ids, err, a)
	}
	if err := s.CreateLink(ctx, a, b, "related", 0.9, "auto"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	embedded, stale, total, err := s.EmbeddingStats(ctx)
	if err != nil || embedded != 2 || total != 3 || stale != 0 {
		t.Errorf("EmbeddingStats = %d embedded, %d stale, %d total, %v, want 2, 0, 3", embedded, stale, total, err)
	}
	if links, scans, err := s.LinkStats(ctx); err != nil || links != 1 || scans != 1 {
		t.Errorf("LinkStats = links %d scans %d, %v, want 1 and 1", links, scans, err)
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
	if err != nil || len(links) != 1 || links[0].SourceID != a || links[0].TargetID != b {
		t.Fatalf("got %+v, %v, want exactly one causes/llm link a->b", links, err)
	}
	// A different source for the same relation must not match.
	if none, err := s.LinksByRelationSource(ctx, testProject, "supersedes", "llm"); err != nil || len(none) != 0 {
		t.Fatalf("got %d links, %v, want 0 (source filter must exclude 'manual')", len(none), err)
	}
}

// TestSupersedesWithinIgnoresScopeConflictingEdge: SupersedesWithin has no
// production caller (SupersedePenalties is the reader ranking uses) but is
// exported, so its answer must not contradict every other reader of the edge.
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
	if links, err := s.GetLinks(ctx, prod); err != nil || len(links) != 1 {
		t.Fatalf("scope guard must not delete the existing edge, got %+v, %v", links, err)
	}
	pairs, err := s.SupersedesWithin(ctx, []string{dev, prod, sameProdNewer, sameProd})
	if err != nil {
		t.Fatalf("SupersedesWithin: %v", err)
	}
	if len(pairs) != 1 || pairs[0] != [2]string{sameProdNewer, sameProd} {
		t.Errorf("SupersedesWithin = %v, want only the same-scope pair", pairs)
	}
}

func liveRelatedDegree(t *testing.T, s *Store, id string) int {
	t.Helper()
	links, err := s.GetLinks(context.Background(), id)
	if err != nil {
		t.Fatalf("GetLinks(%s): %v", id, err)
	}
	n := 0
	for _, l := range links {
		if l.Relation == "related" {
			n++
		}
	}
	return n
}

// keptNeighbors returns the set of memories still related-linked to hub.
func keptNeighbors(t *testing.T, s *Store, hub string) map[string]bool {
	t.Helper()
	links, err := s.GetLinks(context.Background(), hub)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	kept := map[string]bool{}
	for _, l := range links {
		if l.Relation != "related" {
			continue
		}
		other := l.SourceID
		if other == hub {
			other = l.TargetID
		}
		kept[other] = true
	}
	return kept
}

// A hub's related degree is capped at MaxRelatedDegree, the weakest links go,
// and the cap holds on both endpoints of a new link. TrimOverCapRelatedDegrees
// repairs a store seeded over the cap the same way.
func TestRelatedDegreeCap(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, s *Store, hub string, others []string) {
		t.Helper()
		if got := liveRelatedDegree(t, s, hub); got != MaxRelatedDegree {
			t.Fatalf("hub related degree = %d, want %d", got, MaxRelatedDegree)
		}
		kept := keptNeighbors(t, s, hub)
		for i, o := range others {
			if want := i < MaxRelatedDegree; kept[o] != want {
				t.Errorf("neighbor %d kept = %v, want %v (strongest survive)", i, kept[o], want)
			}
		}
	}
	t.Run("create", func(t *testing.T) {
		s := testStore(t)
		hub := makeMemory(t, s, "hub memory")
		others := make([]string, MaxRelatedDegree+5)
		for i := range others {
			others[i] = makeMemory(t, s, fmt.Sprintf("neighbor %d", i))
			if err := s.CreateLink(ctx, hub, others[i], "related", 0.99-float32(i)*0.001, "auto"); err != nil {
				t.Fatalf("CreateLink %d: %v", i, err)
			}
		}
		check(t, s, hub, others)
	})
	t.Run("both endpoints", func(t *testing.T) {
		s := testStore(t)
		a := makeMemory(t, s, "endpoint a")
		b := makeMemory(t, s, "endpoint b")
		for i := 0; i < MaxRelatedDegree; i++ {
			peer := makeMemory(t, s, fmt.Sprintf("a peer %d", i))
			if err := s.CreateLink(ctx, a, peer, "related", 0.95, "auto"); err != nil {
				t.Fatalf("fill a: %v", err)
			}
		}
		if err := s.CreateLink(ctx, a, b, "related", 0.70, "auto"); err != nil {
			t.Fatalf("CreateLink a-b: %v", err)
		}
		if keptNeighbors(t, s, a)[b] {
			t.Fatal("weak a-b link survived on endpoint a despite degree cap")
		}
		if got := liveRelatedDegree(t, s, a); got != MaxRelatedDegree {
			t.Fatalf("a degree = %d, want %d", got, MaxRelatedDegree)
		}
	})
	t.Run("trim", func(t *testing.T) {
		s := testStore(t)
		hub := makeMemory(t, s, "over-cap hub")
		others := make([]string, MaxRelatedDegree+10)
		for i := range others {
			others[i] = makeMemory(t, s, fmt.Sprintf("trim neighbor %d", i))
			src, tgt := hub, others[i]
			if src > tgt {
				src, tgt = tgt, src
			}
			if _, err := s.db.ExecContext(ctx, `INSERT INTO memory_links (source_id, target_id, relation, strength, source)
				VALUES (?, ?, 'related', ?, 'auto')`, src, tgt, 0.99-float32(i)*0.001); err != nil {
				t.Fatalf("seed link %d: %v", i, err)
			}
		}
		if err := s.TrimOverCapRelatedDegrees(ctx); err != nil {
			t.Fatalf("TrimOverCapRelatedDegrees: %v", err)
		}
		check(t, s, hub, others)
	})
}

func TestCapRelatedDegreePlanUsesIndexes(t *testing.T) {
	s := testStore(t)
	rows, err := s.db.QueryContext(context.Background(),
		"EXPLAIN QUERY PLAN "+capRelatedDegreeSQL,
		"00000000000000000000000000000000", MaxRelatedDegree)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil || len(plan) == 0 {
		t.Fatalf("plan rows = %d, %v", len(plan), err)
	}
	joined := strings.Join(plan, "\n")
	if strings.Contains(joined, "SCAN memory_links") {
		t.Errorf("cap plan scans memory_links:\n%s", joined)
	}
	if !strings.Contains(joined, "idx_links_source") || !strings.Contains(joined, "idx_links_target") {
		t.Errorf("cap plan does not use both link indexes:\n%s", joined)
	}
}
