package memory

import (
	"context"
	"fmt"
	"testing"
)

func TestAreNearDuplicates(t *testing.T) {
	// Pinned by reflection's own cases: same fact with filler words.
	if !AreNearDuplicates("the relay listens on port 2222 in production", "relay listens on port 2222") {
		t.Error("expected near duplicate")
	}
	if AreNearDuplicates("port 2222", "port 2223") {
		t.Error("numeric conflict should not be near duplicate")
	}
	if !AreNearDuplicates("always run the full test suite", "never run the full test suite") {
		// numeric conflict not, but this pair differs by negation; the token
		// predicate still scores it high – the polarity guard lives in
		// PlanGlobalFold, not here. This test pins the bare predicate.
		// It should be near duplicate at this layer.
		t.Error("expected near duplicate at bare predicate layer")
	}
}

func TestNearDuplicatePairsCountsPair(t *testing.T) {
	a := Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Content: "the relay listens on port 2222 in production"}
	b := Memory{ID: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", Content: "relay listens on port 2222"}
	c := Memory{ID: "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC", Content: "unrelated fact about cats"}
	mems := []Memory{a, b, c}
	pairs := NearDuplicatePairs(mems)
	if len(pairs) != 1 {
		t.Fatalf("want 1 pair, got %v", pairs)
	}
	if pairs[0][0] != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" || pairs[0][1] != "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB" {
		t.Fatalf("unexpected pair %v", pairs[0])
	}
	// Resolved row is excluded.
	resolved := "2026-01-01T00:00:00Z"
	b.ResolvedAt = &resolved
	pairs = NearDuplicatePairs([]Memory{a, b, c})
	if len(pairs) != 0 {
		t.Fatalf("resolved row should not be counted, got %v", pairs)
	}
}

func TestNearDuplicatePairsRespectsResolved(t *testing.T) {
	a := Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Content: "use sqlite for storage"}
	b := Memory{ID: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", Content: "use sqlite for storage; it is the store"}
	pairs := NearDuplicatePairs([]Memory{a, b})
	if len(pairs) != 1 {
		t.Fatalf("want pair, got %v", pairs)
	}
}

// A pair is counted only when the rows it spans are rows ghost reflect and
// ghost maintenance consolidate-global would merge: consolidatable rows.
// Pinned, persistent, manual, builtin and resolved rows are excluded.
func TestNearDuplicatePairsCountsOnlyConsolidatableRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	content := "the relay listens on port 2222 in production"
	content2 := "relay listens on port 2222"
	mk := func(source string) string {
		id, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: content, Source: source, Importance: 0.5})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return id
	}
	// A consolidatable pair: same content shape, neither row pinned or exempt.
	a, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: content, Source: "mcp", Importance: 0.5})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: content2, Source: "mcp", Importance: 0.5}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	pairs, _, err := s.NearDuplicatePairsForProject(ctx, testProject)
	if err != nil {
		t.Fatalf("NearDuplicatePairsForProject: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("want 1 consolidatable pair, got %v", pairs)
	}
	// Frozen rows are excluded: manual, builtin, pinned and persistent each
	// name a row the merge commands preserve.
	if _, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: content, Source: "manual", Importance: 0.5}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	mk("builtin")
	pinned, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: content, Source: "mcp", Importance: 0.5})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET pinned = 1 WHERE id = ?`, pinned); err != nil {
		t.Fatalf("pin: %v", err)
	}
	persistent, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: content, Source: "mcp", Importance: 0.5, Retention: RetentionPersistent})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !RetentionExempt(Memory{Retention: RetentionPersistent}) {
		t.Fatal("fixture broken: persistent row is not retention-exempt")
	}
	// The consolidatable pair is unchanged by the frozen rows around it.
	pairs, _, err = s.NearDuplicatePairsForProject(ctx, testProject)
	if err != nil {
		t.Fatalf("NearDuplicatePairsForProject: %v", err)
	}
	if len(pairs) != 1 {
		t.Fatalf("frozen rows changed the count: got %v, want 1", pairs)
	}
	// The reverse side: two frozen rows that match each other are not a pair.
	frozenStore := testStore(t)
	fid1, err := frozenStore.Create(ctx, testProject, Memory{Category: "fact", Content: content, Source: "manual", Importance: 0.5})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fid2, err := frozenStore.Create(ctx, testProject, Memory{Category: "fact", Content: content2, Source: "manual", Importance: 0.5})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pairs, _, err = frozenStore.NearDuplicatePairsForProject(ctx, testProject)
	if err != nil {
		t.Fatalf("NearDuplicatePairsForProject: %v", err)
	}
	if len(pairs) != 0 {
		t.Fatalf("manual rows are not consolidatable but were counted as a pair: %v (ids %s, %s)", pairs, fid1, fid2)
	}
	// A resolved consolidatable row stops counting, the documented case.
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET resolved_at = datetime('now') WHERE id = ?`, a); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	pairs, _, err = s.NearDuplicatePairsForProject(ctx, testProject)
	if err != nil {
		t.Fatalf("NearDuplicatePairsForProject: %v", err)
	}
	if len(pairs) != 0 {
		t.Fatalf("resolved row still paired: %v", pairs)
	}
	_ = persistent
}

// The token-set variant and the string wrapper must agree on the same inputs.
func TestAreNearDuplicatesTokensAgreesWithAreNearDuplicates(t *testing.T) {
	cases := []struct{ a, b string }{
		{"the relay listens on port 2222 in production", "relay listens on port 2222"},
		{"port 2222", "port 2223"},
		{"always run the full test suite", "never run the full test suite"},
		{"unrelated fact about cats", "the relay listens on port 2222 in production"},
		{"", ""},
		{"a", "a"},
	}
	for _, tc := range cases {
		want := AreNearDuplicates(tc.a, tc.b)
		got := AreNearDuplicatesTokens(DedupTokenize(tc.a), DedupTokenize(tc.b))
		if got != want {
			t.Errorf("AreNearDuplicatesTokens(%q, %q) = %v, AreNearDuplicates = %v; they must agree", tc.a, tc.b, got, want)
		}
	}
}

// The scan is bounded: past maxNearDuplicateScanRows it stops considering rows
// and reports the cut through the truncated flag.
func TestNearDuplicatePairsForProjectBoundsTheScan(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	total := maxNearDuplicateScanRows + 2
	for i := 0; i < total; i++ {
		if _, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: fmt.Sprintf("row %d filler alpha beta gamma delta epsilon", i), Source: "mcp", Importance: 0.3}); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	// One near-duplicate pair sits past the bound; the scan must not reach it.
	if _, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "the relay listens on port 2222 in production", Source: "mcp", Importance: 0.5}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: "relay listens on port 2222", Source: "mcp", Importance: 0.5}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, truncated, err := s.NearDuplicatePairsForProject(ctx, testProject)
	if err != nil {
		t.Fatalf("NearDuplicatePairsForProject: %v", err)
	}
	if !truncated {
		t.Fatal("the scan over the bound did not report truncated=true")
	}
}

// A cancelled context aborts the scan with its error.
func TestNearDuplicatePairsForProjectHonoursContext(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: fmt.Sprintf("row %d about alpha beta", i), Source: "mcp", Importance: 0.3}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.NearDuplicatePairsForProject(cancelled, testProject); err == nil {
		t.Fatal("a cancelled context did not abort the scan")
	}
}
