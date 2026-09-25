package reflection

import (
	"testing"
)

// TestNormalizeReflectMemoriesScopeRules pins the rules that decide what an
// LLM emission may claim about itself.
//
// The scope rule is the one with teeth. The SQLite tier refuses to promote
// secret-looking content and justified the LLM tier's gap by pointing at its
// extraction prompt — but a prompt is a request, not a guarantee, so the check
// has to be on the value the model actually returned. A global memory is
// replayed into every future session in every project: promoting a credential
// does not contain a leak, it takes one confined to a single project and widens
// it to all of them (issue #545).
//
// These are unit-level on purpose. Driving them through a harness call would
// make a billable spawn part of ordinary `go test ./...` — the problem issue
// #548 exists to remove — and would test the model's cooperation rather than
// Ghost's own rule.
func TestNormalizeReflectMemoriesScopeRules(t *testing.T) {
	cases := []struct {
		name      string
		memories  []ReflectMemory
		wantScope []string
	}{
		{
			name: "secret-looking content is never promoted",
			memories: []ReflectMemory{
				{Category: "fact", Content: "The api key for this service is sk-live-123.", Scope: "global"},
				{Category: "fact", Content: "Rotate the deployment password every quarter.", Scope: "global"},
				{Category: "fact", Content: "The CI access_token lives in the runner env.", Scope: "global"},
			},
			wantScope: []string{"project", "project", "project"},
		},
		{
			name: "ordinary cross-repo knowledge is still promoted",
			memories: []ReflectMemory{
				{Category: "convention", Content: "Conventional Commits are used across all repos.", Scope: "global"},
			},
			wantScope: []string{"global"},
		},
		{
			name: "an unknown scope collapses to project",
			memories: []ReflectMemory{
				{Category: "fact", Content: "Something neutral.", Scope: "everywhere"},
				{Category: "fact", Content: "Something neutral."},
			},
			wantScope: []string{"project", "project"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := ReflectionResult{Memories: tc.memories}
			normalizeReflectMemories(&result)
			if len(result.Memories) != len(tc.wantScope) {
				t.Fatalf("got %d memories, want %d", len(result.Memories), len(tc.wantScope))
			}
			for i, want := range tc.wantScope {
				if result.Memories[i].Scope != want {
					t.Errorf("memory %d (%q) scope = %q, want %q",
						i, result.Memories[i].Content, result.Memories[i].Scope, want)
				}
			}
		})
	}
}

// TestNormalizeReflectMemoriesFieldRules covers the rest of the normalization:
// these guard the apply transaction (an out-of-range importance or unknown
// category fails a schema CHECK mid-transaction and sinks the whole round).
func TestNormalizeReflectMemoriesFieldRules(t *testing.T) {
	result := ReflectionResult{Memories: []ReflectMemory{
		{Category: "not-a-real-category", Content: "a", Importance: 1.7},
		{Category: "fact", Content: "b", Importance: -3},
		{Category: "fact", Content: "c", Tags: nil},
	}}
	normalizeReflectMemories(&result)

	if got := result.Memories[0].Importance; got != 1 {
		t.Errorf("importance 1.7 clamped to %v, want 1", got)
	}
	if got := result.Memories[1].Importance; got != 0 {
		t.Errorf("importance -3 clamped to %v, want 0", got)
	}
	if got := result.Memories[0].Category; got != "fact" {
		t.Errorf("invalid category %q fell back to %q, want fact", "not-a-real-category", got)
	}
	if result.Memories[2].Tags == nil {
		t.Error("nil tags left nil — the schema expects a list, not a NULL")
	}
}
