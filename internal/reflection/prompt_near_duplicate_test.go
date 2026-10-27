package reflection

import (
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
)

func TestReflectInputNamesNearDuplicatePair(t *testing.T) {
	input := ReflectionInput{
		ProjectName:      "proj",
		ExistingMemories: nil,
		NearDuplicatePairs: [][2]string{
			{"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"},
		},
	}
	prompt := BuildReflectionPrompt(input)
	if !strings.Contains(strings.ToLower(prompt), "these pairs read as duplicates") {
		t.Fatalf("prompt does not name pairs as duplicates:\n%s", prompt)
	}
	if !strings.Contains(prompt, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA") || !strings.Contains(prompt, "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB") {
		t.Fatalf("prompt does not contain pair ids:\n%s", prompt)
	}
}

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

func TestNearDuplicatePairMergesWithFakeHarness(t *testing.T) {
	a := memory.Memory{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", Category: "fact", Content: "the relay listens on port 2222 in production"}
	b := memory.Memory{ID: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB", Category: "fact", Content: "relay listens on port 2222"}
	pairs := memory.NearDuplicatePairs([]memory.Memory{a, b})
	if len(pairs) != 1 {
		t.Fatalf("want 1 pair, got %v", pairs)
	}
	input := ReflectionInput{
		ProjectName:        "proj",
		ExistingMemories:   []memory.Memory{a, b},
		NearDuplicatePairs: pairs,
	}
	// Fake harness that merges the named pair.
	fake := &fakeNearDupReflector{
		respond: func(prompt string) string {
			if !strings.Contains(strings.ToLower(prompt), "these pairs read as duplicates") {
				t.Fatalf("prompt missing pair hint")
			}
			// Return ops that merge the pair.
			return `{"learned_context":"ctx","ops":["merge AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA,BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB -> the relay listens on port 2222"]}`
		},
	}
	cons := NewLlmConsolidator(fake)
	res, err := cons.Consolidate(context.TODO(), input)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if len(res.Memories) != 1 {
		t.Fatalf("want 1 merged memory, got %d: %+v", len(res.Memories), res.Memories)
	}
	if len(res.Merges) != 1 || len(res.Merges[0].IDs) != 2 {
		t.Fatalf("want 1 merge of 2 ids, got %+v", res.Merges)
	}
}

type fakeNearDupReflector struct {
	respond func(string) string
}

func (f *fakeNearDupReflector) Reflect(_ context.Context, prompt string) (string, ai.TokenUsage, error) {
	return f.respond(prompt), ai.TokenUsage{}, nil
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
