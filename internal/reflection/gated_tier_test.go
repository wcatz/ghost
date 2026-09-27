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

// TestNewGatedConsolidator_KeepsTheTierName: the operator named a backend on
// the command line and `ghost reflect` prints this back as
// `Consolidator: <name>`. Wrapping a tier is a change in how it is BOUNDED, not
// in which one runs, so the label must not gain the "tiered:" prefix the `auto`
// path uses — a script or a saved log-grep keyed on `Consolidator: cli` has to
// keep matching.
func TestNewGatedConsolidator_KeepsTheTierName(t *testing.T) {
	g := NewGatedConsolidator(&stubConsolidator{name: "opencode", available: true}, slog.Default())
	if got := g.Name(); got != "opencode" {
		t.Errorf("Name() = %q, want %q — the explicit selection must not be relabelled", got, "opencode")
	}
}

// TestNewTieredConsolidator_StillPrefixesTheAutoPath is the other side: the
// `auto` tier really does choose between several, so its label keeps saying so.
func TestNewTieredConsolidator_StillPrefixesTheAutoPath(t *testing.T) {
	g := NewTieredConsolidator([]Consolidator{&stubConsolidator{name: "opencode", available: true}}, slog.Default())
	if got := g.Name(); got != "tiered:opencode" {
		t.Errorf("Name() = %q, want %q", got, "tiered:opencode")
	}
}
