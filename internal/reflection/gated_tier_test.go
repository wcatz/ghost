package reflection

import (
	"context"
	"log/slog"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestNewGatedConsolidator_ExplicitLLMTierIsStillGated is the #549 hole in a
// second shape. `ghost reflect --tier cli` and `--tier opencode` handed the bare
// LlmConsolidator straight to the apply path, so the quality gate's floor did
// not exist for those invocations at all: any answer the harness returned was
// applied, however small. The `auto` tier wraps its tiers in
// TieredConsolidator, which is where the gate lives, so the default and the
// lifecycle's unattended path were bounded while the two explicitly-selected
// LLM tiers were not — and eval/cycle measures with `--tier opencode --apply`,
// so the suite was grading an ungated consolidator while production is gated.
func TestNewGatedConsolidator_ExplicitLLMTierIsStillGated(t *testing.T) {
	inputs := make([]memory.Memory, 200)
	for i := range inputs {
		inputs[i] = memory.Memory{Category: "gotcha", Content: "incident note", Importance: 0.8}
	}
	outputs := func(n int) []ReflectMemory {
		out := make([]ReflectMemory, n)
		for i := range out {
			out[i] = ReflectMemory{Category: "gotcha", Content: "distinct fact", Importance: 0.8, Tags: []string{}}
		}
		return out
	}

	// One below the floor a 200-memory input demands, so it must be rejected.
	// There is no next tier to fall through to, so this is a failure rather than
	// a silent apply.
	floor := gateMinOutput(len(inputs))
	tooSmall := NewGatedConsolidator(&stubConsolidator{
		name: "cli", available: true,
		result: ReflectionResult{LearnedContext: "ctx", Memories: outputs(floor - 1)},
	}, slog.Default())
	if _, err := tooSmall.Consolidate(context.Background(), ReflectionInput{ExistingMemories: inputs}); err == nil {
		t.Errorf("an explicitly-selected LLM tier returned %d memories for %d inputs and was applied anyway",
			floor-1, len(inputs))
	}

	// And the accept side: a result at the floor still goes through, so wrapping
	// the tier in the gate does not make the explicit paths unusable.
	enough := NewGatedConsolidator(&stubConsolidator{
		name: "cli", available: true,
		result: ReflectionResult{LearnedContext: "ctx", Memories: outputs(floor)},
	}, slog.Default())
	if _, err := enough.Consolidate(context.Background(), ReflectionInput{ExistingMemories: inputs}); err != nil {
		t.Errorf("a result at the floor must pass the gate, got %v", err)
	}
}

// TestNewGatedConsolidator_KeepsTheTierName: the selected tier is what the
// operator asked for and what `Consolidator:` prints, so wrapping must not
// relabel it into something they did not select.
func TestNewGatedConsolidator_KeepsTheTierName(t *testing.T) {
	g := NewGatedConsolidator(&stubConsolidator{name: "opencode", available: true}, slog.Default())
	if got := g.Name(); got != "tiered:opencode" {
		t.Errorf("Name() = %q, want %q", got, "tiered:opencode")
	}
}
