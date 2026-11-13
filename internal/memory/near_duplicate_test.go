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
