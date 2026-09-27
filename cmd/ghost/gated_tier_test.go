package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
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

	// Deliberately independent of the floor's VALUE: this test is about the
	// seam applying the gate at all, and gateMinOutput's own value is covered
	// inside the reflection package. An empty answer is below every floor the
	// gate can compute, and one memory per input is above every one.
	if err := run(&gatedTierStub{name: "cli", mems: nil}); err == nil {
		t.Errorf("an explicitly-selected LLM tier returned no memories for %d inputs and was applied anyway", len(inputs))
	}
	if err := run(&gatedTierStub{name: "opencode", mems: mems(len(inputs))}); err != nil {
		t.Errorf("a full-size result must go through, got %v", err)
	}
}

// TestGatedLLMTierAddsNoFallbackTier: "exactly this backend" has to keep meaning
// that. A gate failure must surface as a failure, not as a quiet drop to the
// Jaccard tier -- the wrapper is a bound, not a second opinion. And it has to
// SAY so: the operator asked for a specific backend, so "the gate rejected its
// answer" and "the harness crashed" are different problems with different fixes.
func TestGatedLLMTierAddsNoFallbackTier(t *testing.T) {
	inputs := make([]memory.Memory, 200)
	for i := range inputs {
		inputs[i] = memory.Memory{Category: "gotcha", Content: "incident note", Importance: 0.8}
	}
	one := []reflection.ReflectMemory{{Category: "gotcha", Content: "one", Importance: 0.8}}
	_, err := gatedLLMTier(&gatedTierStub{name: "cli", mems: one}, slog.Default()).
		Consolidate(context.Background(), reflection.ReflectionInput{ExistingMemories: inputs})
	if err == nil {
		t.Fatal("expected a failure: the explicit tier list has exactly one member, so a gate failure cannot be absorbed")
	}
	for _, want := range []string{"cli", "quality gate failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q, so the operator cannot tell a gate rejection from a crash", err, want)
		}
	}
}

// TestGatedLLMTierKeepsTheTierName: the operator selected a backend and the
// `Consolidator:` line reports it back, so wrapping it in the gate must not
// relabel it into `tiered:cli` — that would silently break any script or saved
// log-grep keyed on the old label. The prefix marks which constructor ran (see
// TieredConsolidator.Name), and only the gated one drops it.
func TestGatedLLMTierKeepsTheTierName(t *testing.T) {
	for _, name := range []string{"cli", "opencode"} {
		if got := gatedLLMTier(&gatedTierStub{name: name}, slog.Default()).Name(); got != name {
			t.Errorf("Name() = %q, want %q — the explicit selection must not be relabelled", got, name)
		}
	}
}

// TestGatedLLMTierPropagatesTierFailure: wrapping must not swallow the error the
// selected tier already reported, or a broken harness would read as success.
func TestGatedLLMTierPropagatesTierFailure(t *testing.T) {
	boom := errors.New("harness exited 1")
	stub := &gatedTierStub{name: "cli", tierErr: boom}
	_, err := gatedLLMTier(stub, slog.Default()).
		Consolidate(context.Background(), reflection.ReflectionInput{})
	if err == nil {
		t.Fatal("expected the tier's own error")
	}
	if stub.callSeen != 1 {
		t.Errorf("tier called %d times, want 1", stub.callSeen)
	}
}
