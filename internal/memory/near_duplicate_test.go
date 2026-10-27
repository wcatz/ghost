package memory

import "testing"

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
