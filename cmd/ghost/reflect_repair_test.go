package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// The repair turn's reporting half (#689). The tier's behaviour is pinned in
// internal/reflection; what is pinned here is the part a reader and a benchmark
// see, and the part the unattended lifecycle leaves behind.

// TestReflectRepairNoteIsCountable pins the token the summary carries. A repair
// that is invisible is a repair nobody can count, and the count is the point: the
// defect this closes was measured at roughly one run in three, and the only way
// to know whether that still holds for a given corpus is a stable string in
// `ghost reflect`'s own output.
//
// Two rules make the token usable, which is why this is a formatter rather than a
// line dropped in where convenient. It prints on exactly one surface — on the
// unattended path this stdout is the append-only lifecycle.log, so a second
// mention on the other stream would double every count. And the zero case prints
// nothing, so an absent token means "the first answer parsed" rather than "nobody
// measured".
func TestReflectRepairNoteIsCountable(t *testing.T) {
	if got := reflectRepairNote(0); got != "" {
		t.Errorf("reflectRepairNote(0) = %q, want empty: a run that never repaired must not report a repair", got)
	}
	note := reflectRepairNote(1)
	if !strings.Contains(note, "repair: 1") {
		t.Errorf("reflectRepairNote(1) = %q, want the stable token %q", note, "repair: 1")
	}
	if !strings.HasPrefix(note, "  ") || !strings.HasSuffix(note, ")") {
		t.Errorf("reflectRepairNote(1) = %q, want an indented trailing clause so the category parentheses stay parseable", note)
	}
	// The tier only ever sets 0 or 1, but the formatter must not print a repair
	// for a count it cannot vouch for either.
	if got := reflectRepairNote(-1); got != "" {
		t.Errorf("reflectRepairNote(-1) = %q, want empty", got)
	}
}

// TestResultLineCarriesTheRepairToken renders the summary line itself, because
// the claim under test is where the token lands: the `Result:` line, which is the
// one a person reads and the one a benchmark greps. The existing shape is
// asserted alongside it — a run that never repaired must print byte for byte what
// it printed before this change.
func TestResultLineCarriesTheRepairToken(t *testing.T) {
	mems := []reflection.ReflectMemory{{Category: "gotcha", Content: "one fact", Importance: 0.8, Tags: []string{}}}
	line := func(r reflection.ReflectionResult) string {
		return fmt.Sprintf("Result:       %d memories (%s)%s\n", len(r.Memories), reflectCategoryParts(r.Memories), reflectRepairNote(r.RepairTurns))
	}

	clean := line(reflection.ReflectionResult{Memories: mems})
	if clean != "Result:       1 memories (1 gotcha)\n" {
		t.Errorf("clean result line = %q, want the pre-existing shape unchanged", clean)
	}

	repaired := line(reflection.ReflectionResult{Memories: mems, RepairTurns: 1})
	if !strings.Contains(repaired, "repair: 1") {
		t.Errorf("repaired result line = %q, want it to carry the repair token", repaired)
	}
	if strings.Count(repaired, "repair:") != 1 {
		t.Errorf("repaired result line = %q, want the token exactly once", repaired)
	}
}

// TestRepairTurnsSurvivesEveryWrapper: the count is set on the LLM tier's result,
// and the wrappers are what every caller actually runs — the `auto` tiered list,
// the `--require-llm` one-tier list, and the gated single-backend wrapper behind
// `--tier cli` / `--tier opencode`. If any of them rebuilt the result instead of
// returning the tier's, `repair: 1` would be a number no run could ever print.
//
// One input, so the quality gate judges nothing (gateMinInput is 6) and this is
// about the pass-through alone.
func TestRepairTurnsSurvivesEveryWrapper(t *testing.T) {
	input := reflection.ReflectionInput{ExistingMemories: []memory.Memory{
		{ID: "D20E133860CC4AFE38B485AD5371BA59", Category: "gotcha",
			Content: "SSH to the Hetzner bastion goes through port 2222, not 22", Importance: 0.9, Source: "mcp"},
	}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	wrappers := map[string]func(reflection.Consolidator) reflection.Consolidator{
		"tiered": func(c reflection.Consolidator) reflection.Consolidator {
			return reflection.NewTieredConsolidator([]reflection.Consolidator{c}, logger)
		},
		"gated": func(c reflection.Consolidator) reflection.Consolidator {
			return reflection.NewGatedConsolidator(c, logger)
		},
	}
	for name, wrap := range wrappers {
		t.Run(name, func(t *testing.T) {
			result, err := wrap(reflection.NewNamedConsolidator(&repairedHarness{}, "opencode")).
				Consolidate(context.Background(), input)
			if err != nil {
				t.Fatalf("Consolidate: %v", err)
			}
			if result.RepairTurns != 1 {
				t.Errorf("RepairTurns = %d, want 1 to survive the %s wrapper", result.RepairTurns, name)
			}
		})
	}
}

// repairedHarness is a fake reflector: its first answer names an id this run was
// never given, its second is well formed, so the tier spends its one repair turn
// and the result is the second answer's. No real CLI harness is spawned.
type repairedHarness struct{ calls int }

func (h *repairedHarness) Reflect(_ context.Context, _ string) (string, ai.TokenUsage, error) {
	h.calls++
	if h.calls == 1 {
		// One extra hex character on a real id: near enough to look right and
		// still not one of the ids this run was given.
		return `{"ops":["merge D20E133860CC4AFE38B485AD5371BA599,ZZ -> the bastion and the mesh"]}`,
			ai.TokenUsage{}, nil
	}
	return `{"ops":["keep D20E133860CC4AFE38B485AD5371BA59"]}`, ai.TokenUsage{}, nil
}
