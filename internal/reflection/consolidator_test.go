package reflection

import (
	"context"
	"fmt"
	"log/slog"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestTieredConsolidator_UsesFirstAvailable(t *testing.T) {
	unavailable := &stubConsolidator{name: "unavail", available: false}
	available := &stubConsolidator{
		name:      "avail",
		available: true,
		result:    ReflectionResult{LearnedContext: "from-avail"},
	}

	tc := NewTieredConsolidator([]Consolidator{unavailable, available}, slog.Default())
	result, err := tc.Consolidate(context.Background(), ReflectionInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.LearnedContext != "from-avail" {
		t.Errorf("expected result from avail tier, got %q", result.LearnedContext)
	}
	if tc.ActiveTier() != "tiered:avail" {
		t.Errorf("expected active tier 'tiered:avail', got %q", tc.ActiveTier())
	}
}

func TestTieredConsolidator_FallsBackOnError(t *testing.T) {
	failing := &stubConsolidator{name: "fail", available: true, err: fmt.Errorf("boom")}
	fallback := &stubConsolidator{
		name:      "fallback",
		available: true,
		result:    ReflectionResult{LearnedContext: "from-fallback"},
	}

	tc := NewTieredConsolidator([]Consolidator{failing, fallback}, slog.Default())
	result, err := tc.Consolidate(context.Background(), ReflectionInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.LearnedContext != "from-fallback" {
		t.Errorf("expected fallback result, got %q", result.LearnedContext)
	}
}

func TestTieredConsolidator_AllFail(t *testing.T) {
	failing := &stubConsolidator{name: "fail", available: true, err: fmt.Errorf("boom")}

	tc := NewTieredConsolidator([]Consolidator{failing}, slog.Default())
	_, err := tc.Consolidate(context.Background(), ReflectionInput{})
	if err == nil {
		t.Fatal("expected error when all tiers fail")
	}
}

func TestTieredConsolidator_NoneAvailable(t *testing.T) {
	tc := NewTieredConsolidator([]Consolidator{
		&stubConsolidator{name: "off", available: false},
	}, slog.Default())

	if tc.Available(context.Background()) {
		t.Error("should not be available when no tiers are")
	}
	_, err := tc.Consolidate(context.Background(), ReflectionInput{})
	if err == nil {
		t.Fatal("expected error when no tiers available")
	}
}

func TestSQLiteConsolidator_MergesDuplicates(t *testing.T) {
	sc := NewSQLiteConsolidator()

	input := ReflectionInput{
		CurrentContext: "existing context",
		ExistingMemories: []memory.Memory{
			{Category: "fact", Content: "Ghost uses SQLite for storage with FTS5", Importance: 0.7, Tags: []string{"db"}},
			{Category: "fact", Content: "Ghost uses SQLite for persistent storage with FTS5 search", Importance: 0.8, Tags: []string{"storage"}},
			{Category: "pattern", Content: "completely different memory about patterns", Importance: 0.6, Tags: []string{}},
		},
	}

	result, err := sc.Consolidate(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The two similar "fact" memories should be merged into one.
	if len(result.Memories) != 2 {
		t.Fatalf("expected 2 memories after dedup, got %d", len(result.Memories))
	}
	if result.LearnedContext != "existing context" {
		t.Error("should preserve existing learned context")
	}

	// The merged memory should have the higher importance.
	for _, m := range result.Memories {
		if m.Category == "fact" && m.Importance != 0.8 {
			t.Errorf("merged fact should have importance 0.8, got %v", m.Importance)
		}
	}
}

func TestSQLiteConsolidator_DifferentCategoriesNotMerged(t *testing.T) {
	sc := NewSQLiteConsolidator()

	input := ReflectionInput{
		ExistingMemories: []memory.Memory{
			{Category: "fact", Content: "Ghost uses SQLite for storage", Importance: 0.7},
			{Category: "architecture", Content: "Ghost uses SQLite for storage", Importance: 0.8},
		},
	}

	result, err := sc.Consolidate(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Same content but different categories — should NOT be merged.
	if len(result.Memories) != 2 {
		t.Fatalf("expected 2 memories (different categories), got %d", len(result.Memories))
	}
}

func TestSQLiteConsolidator_EmptyInput(t *testing.T) {
	sc := NewSQLiteConsolidator()
	result, err := sc.Consolidate(context.Background(), ReflectionInput{CurrentContext: "ctx"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Memories) != 0 {
		t.Errorf("expected 0 memories for empty input, got %d", len(result.Memories))
	}
	if result.LearnedContext != "ctx" {
		t.Error("should preserve context on empty input")
	}
}

func TestJaccard(t *testing.T) {
	tests := []struct {
		name string
		a, b map[string]bool
		want float64
	}{
		{"identical", map[string]bool{"go": true, "sqlite": true}, map[string]bool{"go": true, "sqlite": true}, 1.0},
		{"disjoint", map[string]bool{"go": true}, map[string]bool{"rust": true}, 0.0},
		{"partial", map[string]bool{"go": true, "sqlite": true, "fts5": true}, map[string]bool{"go": true, "sqlite": true, "vector": true}, 0.5},
		{"both empty", map[string]bool{}, map[string]bool{}, 1.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jaccard(tt.a, tt.b)
			if got != tt.want {
				t.Errorf("jaccard = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTokenize(t *testing.T) {
	tokens := tokenize("Ghost uses SQLite for FTS5 search!")
	if !tokens["ghost"] || !tokens["sqlite"] || !tokens["fts5"] {
		t.Errorf("expected key tokens, got %v", tokens)
	}
	// Single-char words should be excluded.
	if tokens["a"] {
		t.Error("single-char tokens should be excluded")
	}
}

func TestInferGlobalScope(t *testing.T) {
	tests := []struct {
		name     string
		category string
		content  string
		want     string
	}{
		{"preference always global", "preference", "use tabs not spaces", "global"},
		{"architecture is project", "architecture", "ghost uses 3-block prompt caching", "project"},
		{"cross-repo workflow", "fact", "deploy to infra cluster from any repo", "global"},
		{"ssh host", "fact", "SSH into node3 via bastion", "global"},
		{"always use pattern", "convention", "always use nerdctl not docker", "global"},
		{"project convention", "convention", "commit messages use feat/fix prefix", "project"},
		{"all repos", "pattern", "run go vet across all repos before release", "global"},
		{"dev machine", "fact", "dev machine runs Asahi Fedora on Apple Silicon", "global"},
		{"plain project fact", "fact", "FTS5 uses porter stemmer tokenizer", "project"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferGlobalScope(tt.category, tt.content)
			if got != tt.want {
				t.Errorf("inferGlobalScope(%q, %q) = %q, want %q", tt.category, tt.content, got, tt.want)
			}
		})
	}
}

func TestSQLiteConsolidator_SetsScope(t *testing.T) {
	sc := NewSQLiteConsolidator()

	input := ReflectionInput{
		ExistingMemories: []memory.Memory{
			{Category: "preference", Content: "use nerdctl not docker", Importance: 0.9, Tags: []string{}},
			{Category: "architecture", Content: "ghost uses SQLite with FTS5", Importance: 0.8, Tags: []string{}},
		},
	}

	result, err := sc.Consolidate(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, m := range result.Memories {
		if m.Category == "preference" && m.Scope != "global" {
			t.Errorf("preference should be global, got %q", m.Scope)
		}
		if m.Category == "architecture" && m.Scope != "project" {
			t.Errorf("architecture should be project, got %q", m.Scope)
		}
	}
}

func TestParseReflectionResponse_NormalizesScope(t *testing.T) {
	input := `{"learned_context":"ctx","memories":[
		{"category":"fact","content":"with scope","importance":0.8,"tags":[],"scope":"global"},
		{"category":"fact","content":"no scope","importance":0.7,"tags":[]},
		{"category":"fact","content":"bad scope","importance":0.6,"tags":[],"scope":"invalid"}
	]}`

	result := parseReflectionResponse(input)
	if result.Memories[0].Scope != "global" {
		t.Errorf("expected global scope preserved, got %q", result.Memories[0].Scope)
	}
	if result.Memories[1].Scope != "project" {
		t.Errorf("expected missing scope normalized to project, got %q", result.Memories[1].Scope)
	}
	if result.Memories[2].Scope != "project" {
		t.Errorf("expected invalid scope normalized to project, got %q", result.Memories[2].Scope)
	}
}

func TestHaikuConsolidator_NilClient(t *testing.T) {
	h := NewHaikuConsolidator(nil)
	if h.Available(context.Background()) {
		t.Error("should not be available with nil client")
	}
}

// --- stubs ---

type stubConsolidator struct {
	name      string
	available bool
	result    ReflectionResult
	err       error
}

func (s *stubConsolidator) Name() string                     { return s.name }
func (s *stubConsolidator) Available(_ context.Context) bool { return s.available }
func (s *stubConsolidator) Consolidate(_ context.Context, _ ReflectionInput) (ReflectionResult, error) {
	return s.result, s.err
}
