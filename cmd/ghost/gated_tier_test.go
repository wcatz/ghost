package main

import (
	"context"
	"log/slog"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// gatedTierStub is a minimal Consolidator: enough to drive the gate through
// gatedLLMTier without an LLM client.
type gatedTierStub struct {
	name     string
	mech     bool
	mems     []reflection.ReflectMemory
	unavail  bool
	tierErr  error
	callSeen int
}

func (s *gatedTierStub) Name() string                   { return s.name }
func (s *gatedTierStub) Available(context.Context) bool { return !s.unavail }
func (s *gatedTierStub) Mechanical() bool               { return s.mech }
func (s *gatedTierStub) Consolidate(context.Context, reflection.ReflectionInput) (reflection.ReflectionResult, error) {
	s.callSeen++
	if s.tierErr != nil {
		return reflection.ReflectionResult{}, s.tierErr
	}
	return reflection.ReflectionResult{LearnedContext: "ctx", Memories: s.mems}, nil
}

// TestGatedLLMTierRejectsATooSmallExplicitTier is review-sweeper's should-fix on
// PR #663, pinned at the seam. `ghost reflect --tier cli` and `--tier opencode`
// handed the bare LlmConsolidator to the apply path, and the quality gate lives
// in reflection.TieredConsolidator, not in the LLM tier — so selecting a
// backend by name silently switched the gate OFF. `ghost reflect --tier cli
// --apply` would apply a harness answer of any size, and eval/cycle, which
// measures with `--tier opencode --apply`, was grading an ungated consolidator
// while production ran a gated one.
func TestGatedLLMTierRejectsATooSmallExplicitTier(t *testing.T) {
	inputs := make([]memory.Memory, 200)
	for i := range inputs {
		inputs[i] = memory.Memory{Category: "gotcha", Content: "incident note", Importance: 0.8}
	}
	mems := func(n int) []reflection.ReflectMemory {
		out := make([]reflection.ReflectMemory, n)
		for i := range out {
			out[i] = reflection.ReflectMemory{Category: "gotcha", Content: "distinct fact", Importance: 0.8, Tags: []string{}}
		}
		return out
	}
	run := func(s *gatedTierStub) error {
		_, err := gatedLLMTier(s, slog.Default()).Consolidate(
			context.Background(), reflection.ReflectionInput{ExistingMemories: inputs})
		return err
	}

	if err := run(&gatedTierStub{name: "cli", mems: nil}); err == nil {
		t.Errorf("an explicitly-selected LLM tier returned no memories for %d inputs and was applied anyway", len(inputs))
	}
	if err := run(&gatedTierStub{name: "opencode", mems: mems(len(inputs))}); err != nil {
		t.Errorf("a full-size result must go through, got %v", err)
	}
}
