package reflection

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/project"
)

// mockConsolidator implements the Consolidator interface for testing.
type mockConsolidator struct {
	response string
}

func (m *mockConsolidator) Name() string                          { return "mock" }
func (m *mockConsolidator) Available(_ context.Context) bool      { return true }
func (m *mockConsolidator) Consolidate(_ context.Context, _ ReflectionInput) (ReflectionResult, error) {
	return parseReflectionResponse(m.response), nil
}

// mockMemStore implements the memoryStore interface for testing.
type mockMemStore struct {
	interactionCount int
	existingMemories []memory.Memory
	replacedWith     []memory.Memory // captures what was passed to ReplaceNonManual
	replaceCalled    bool
}

func (m *mockMemStore) IncrementInteraction(_ context.Context, _ string) (int, error) {
	m.interactionCount++
	return m.interactionCount, nil
}

func (m *mockMemStore) GetRecentExchanges(_ context.Context, _ string, _ int) ([][2]string, error) {
	return nil, nil
}

func (m *mockMemStore) GetAll(_ context.Context, _ string, _ int) ([]memory.Memory, error) {
	return m.existingMemories, nil
}

func (m *mockMemStore) GetLearnedContext(_ context.Context, _ string) (string, error) {
	return "", nil
}

func (m *mockMemStore) ReplaceNonManual(_ context.Context, _ string, memories []memory.Memory) error {
	m.replaceCalled = true
	m.replacedWith = memories
	return nil
}

func (m *mockMemStore) UpdateLearnedContext(_ context.Context, _, _, _ string) error {
	return nil
}

func (m *mockMemStore) Upsert(_ context.Context, _, _, _, _ string, _ float32, _ []string) (string, bool, error) {
	return "id", false, nil
}

func TestParseReflectionResponse_FiltersEmptyContent(t *testing.T) {
	input := `{"learned_context":"test","memories":[
		{"category":"fact","content":"valid memory","importance":0.8,"tags":["go"]},
		{"category":"fact","content":"","importance":0.5,"tags":[]},
		{"category":"fact","content":"   ","importance":0.5,"tags":[]},
		{"category":"pattern","content":"also valid","importance":0.7,"tags":["test"]}
	]}`

	result := parseReflectionResponse(input)

	// parseReflectionResponse doesn't filter — the engine does.
	// But we can verify it parses correctly.
	if len(result.Memories) != 4 {
		t.Fatalf("expected 4 parsed memories, got %d", len(result.Memories))
	}
}

func TestEngineFiltersEmptyContent(t *testing.T) {
	// Haiku returns 3 memories, 1 with empty content.
	store := &mockMemStore{
		interactionCount: 9, // next increment = 10, triggers reflection
		existingMemories: []memory.Memory{
			{Source: "reflection", Content: "existing1"},
			{Source: "reflection", Content: "existing2"},
		},
	}

	client := &mockConsolidator{
		response: `{"learned_context":"ctx","memories":[
			{"category":"fact","content":"valid","importance":0.8,"tags":["go"]},
			{"category":"fact","content":"","importance":0.5,"tags":[]},
			{"category":"pattern","content":"also valid","importance":0.7,"tags":[]}
		]}`,
	}

	e := NewEngine(client, store, testLogger(), 10)
	e.MaybeReflect(context.Background(), "proj1", testProjectCtx())

	if !store.replaceCalled {
		t.Fatal("expected ReplaceNonManual to be called")
	}
	if len(store.replacedWith) != 2 {
		t.Fatalf("expected 2 memories after filtering empty content, got %d", len(store.replacedWith))
	}
	for _, m := range store.replacedWith {
		if m.Content == "" {
			t.Error("empty-content memory should have been filtered")
		}
	}
}

func TestEnginePreventsDataLoss(t *testing.T) {
	// 10 existing non-manual memories, Haiku returns only 2 → should be blocked.
	existing := make([]memory.Memory, 10)
	for i := range existing {
		existing[i] = memory.Memory{Source: "reflection", Content: "memory"}
	}

	store := &mockMemStore{
		interactionCount: 9,
		existingMemories: existing,
	}

	client := &mockConsolidator{
		response: `{"learned_context":"ctx","memories":[
			{"category":"fact","content":"only one","importance":0.8,"tags":[]},
			{"category":"fact","content":"only two","importance":0.7,"tags":[]}
		]}`,
	}

	e := NewEngine(client, store, testLogger(), 10)
	e.MaybeReflect(context.Background(), "proj1", testProjectCtx())

	if store.replaceCalled {
		t.Fatal("ReplaceNonManual should NOT have been called — dramatic reduction guard should block")
	}
}

func TestEngineAllowsReasonableConsolidation(t *testing.T) {
	// 10 existing, Haiku returns 7 → more than 50%, should be allowed.
	existing := make([]memory.Memory, 10)
	for i := range existing {
		existing[i] = memory.Memory{Source: "reflection", Content: "memory"}
	}

	store := &mockMemStore{
		interactionCount: 9,
		existingMemories: existing,
	}

	memories := make([]ReflectMemory, 7)
	for i := range memories {
		memories[i] = ReflectMemory{Category: "fact", Content: "consolidated", Importance: 0.8, Tags: []string{}}
	}
	response := mustJSON(t, ReflectionResult{LearnedContext: "ctx", Memories: memories})

	client := &mockConsolidator{response: response}

	e := NewEngine(client, store, testLogger(), 10)
	e.MaybeReflect(context.Background(), "proj1", testProjectCtx())

	if !store.replaceCalled {
		t.Fatal("ReplaceNonManual should have been called — 7/10 is reasonable consolidation")
	}
	if len(store.replacedWith) != 7 {
		t.Fatalf("expected 7 memories, got %d", len(store.replacedWith))
	}
}

func TestEngineSkipsGuardForSmallSets(t *testing.T) {
	// 4 existing (< 6 threshold), Haiku returns 1 → should still be allowed.
	existing := make([]memory.Memory, 4)
	for i := range existing {
		existing[i] = memory.Memory{Source: "reflection", Content: "memory"}
	}

	store := &mockMemStore{
		interactionCount: 9,
		existingMemories: existing,
	}

	client := &mockConsolidator{
		response: `{"learned_context":"ctx","memories":[
			{"category":"fact","content":"the only one","importance":0.9,"tags":[]}
		]}`,
	}

	e := NewEngine(client, store, testLogger(), 10)
	e.MaybeReflect(context.Background(), "proj1", testProjectCtx())

	if !store.replaceCalled {
		t.Fatal("ReplaceNonManual should be called — small existing set, guard threshold not met")
	}
}

func TestEngineCountsOnlyNonManual(t *testing.T) {
	// 8 manual + 2 reflection = 10 total, but only 2 non-manual.
	// Haiku returns 1 → 1 < 2/2=1 is false, should be allowed.
	existing := make([]memory.Memory, 10)
	for i := range 8 {
		existing[i] = memory.Memory{Source: "manual", Content: "manual memory"}
	}
	existing[8] = memory.Memory{Source: "reflection", Content: "reflection1"}
	existing[9] = memory.Memory{Source: "reflection", Content: "reflection2"}

	store := &mockMemStore{
		interactionCount: 9,
		existingMemories: existing,
	}

	client := &mockConsolidator{
		response: `{"learned_context":"ctx","memories":[
			{"category":"fact","content":"consolidated","importance":0.9,"tags":[]}
		]}`,
	}

	e := NewEngine(client, store, testLogger(), 10)
	e.MaybeReflect(context.Background(), "proj1", testProjectCtx())

	if !store.replaceCalled {
		t.Fatal("ReplaceNonManual should be called — only 2 non-manual, threshold not met")
	}
}

// --- helpers ---

func testLogger() *slog.Logger {
	return slog.Default()
}

func testProjectCtx() *project.Context {
	return &project.Context{
		ID:   "test-project",
		Name: "test",
		Path: "/tmp/test",
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
