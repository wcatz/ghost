package reflection

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestNearDuplicatePredicateIsShared(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"the relay listens on port 2222 in production", "relay listens on port 2222", true},
		{"port 2222", "port 2223", false},
		{"use sqlite for storage", "use sqlite for storage; it is the store", true},
	}
	for _, tc := range cases {
		local := areNearDuplicatesLocal(tc.a, tc.b)
		if local != tc.want {
			t.Errorf("local predicate for %q vs %q = %v want %v", tc.a, tc.b, local, tc.want)
		}
		shared := memory.AreNearDuplicates(tc.a, tc.b)
		if shared != local {
			t.Errorf("shared predicate drift for %q vs %q: local %v shared %v", tc.a, tc.b, local, shared)
		}
		if shared != tc.want {
			t.Errorf("shared predicate for %q vs %q = %v want %v", tc.a, tc.b, shared, tc.want)
		}
		// duplicateClusters must agree: two rows cluster together iff near duplicate.
		memA := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Content: tc.a}
		memB := memory.Memory{ID: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", Content: tc.b}
		clusters := duplicateClusters([]memory.Memory{memA, memB})
		clustered := len(clusters) == 1 && len(clusters[0]) == 2
		if clustered != shared {
			t.Errorf("cluster vs shared drift for %q vs %q: clustered %v shared %v", tc.a, tc.b, clustered, shared)
		}
	}
}

func areNearDuplicatesLocal(a, b string) bool {
	ta := tokenize(a)
	tb := tokenize(b)
	sim := jaccard(ta, tb)
	if c := containment(ta, tb); c == 1.0 {
		sim = 1.0
	}
	if sim < 0.5 {
		return false
	}
	if numericConflict(ta, tb) {
		return false
	}
	return true
}

func TestNearDuplicateHintIsBounded(t *testing.T) {
	mkPairs := func(n int) [][2]string {
		out := make([][2]string, n)
		for i := 0; i < n; i++ {
			out[i] = [2]string{fmt.Sprintf("ID%032d", i), fmt.Sprintf("ID%032d", i+1000)}
		}
		return out
	}
	// Fewer than the cap: every pair is listed, no "more" line.
	input := ReflectionInput{ProjectName: "proj", NearDuplicatePairs: mkPairs(3)}
	prompt := BuildReflectionPrompt(input)
	if strings.Count(prompt, "read as duplicates\n") != 4 {
		t.Fatalf("want 3 listed pairs, got %d", strings.Count(prompt, "read as duplicates\n")-1)
	}
	if strings.Contains(prompt, "more near-duplicate pair") {
		t.Fatalf("no overflow line expected for fewer-than-cap input:\n%s", prompt)
	}
	// More than the cap: exactly the cap is listed, plus one "N more" line.
	over := maxNearDuplicateHintPairs + 4
	input = ReflectionInput{ProjectName: "proj", NearDuplicatePairs: mkPairs(over)}
	prompt = BuildReflectionPrompt(input)
	if got := strings.Count(prompt, "read as duplicates\n") - 1; got != maxNearDuplicateHintPairs {
		t.Fatalf("want %d listed pairs, got %d", maxNearDuplicateHintPairs, got)
	}
	wantMore := fmt.Sprintf("... and %d more near-duplicate pair(s)", over-maxNearDuplicateHintPairs)
	if !strings.Contains(prompt, wantMore) {
		t.Fatalf("overflow line %q missing:\n%s", wantMore, prompt)
	}
}
