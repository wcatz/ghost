package main

import (
	"bytes"
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

// TestTieredFailureLogWithholdsTheRejectedLine pins the SECOND sink on the same
// failure path, which the review bot caught as unredacted: the tier redacts its
// own WARN, but TieredConsolidator is what every caller actually runs and the
// only thing between the tier and the log, and it logs the error itself — which
// for a reader refusal is the full rendering, quoting the rejected operation line
// and, for a merge or a rewrite, the model's own replacement prose inside it. On
// the `auto` path this line fires on a run that then SUCCEEDS on SQLite, so the
// prose would land in the append-only lifecycle.log with no failure anywhere to
// explain it.
//
// The credential-shaped value is the case that matters: a pre-guard database can
// hold one, a rewrite can restate it, and the whole reason the other in-tier log
// lines go through previewContent is that this file is read by the next session.
func TestTieredFailureLogWithholdsTheRejectedLine(t *testing.T) {
	const secret = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	// secretInClip is the part of the value that survives clipOpLine's 80-rune
	// window on the fixture below, and it is what the log must not contain.
	// Checking the full value instead would be a vacuous assertion: the clip cuts
	// the tail of any token this long, so the full value could not be there
	// whatever the log did.
	secretInClip := secret[:32]
	badID := "D20E133860CC4AFE38B485AD5371BA599"
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	// Every answer refused, so the LLM tier spends its repair turn and then fails
	// outright. Each answer is a rewrite of an id this run was not given, whose
	// replacement text restates the stored credential; the reader refuses the
	// unknown id, so the tier fails and the tiered wrapper logs it. Two tiers, so
	// the wrapper takes its failure path — the mechanical one exists only to be
	// the next thing tried.
	//
	// A rewrite rather than a merge, with the arithmetic that makes it a real
	// probe: clipOpLine keeps 80 runes of the line, "rewrite " + the 33-rune
	// badID + " -> " is 45 of them, so 35 of the value's 40 runes survive — and
	// secretInClip below is the first 32. Three runes of margin, which is why the
	// probe is a stated prefix rather than the whole value: two 33-rune ids in a
	// merge, or any prose in front of the value, would truncate it and the
	// assertion would pass whatever the log did.
	refused := `{"ops":["rewrite ` + badID + ` -> ` + secret + `"]}`
	llm := reflection.NewNamedConsolidator(&scriptedHarness{replies: []string{refused, refused}}, "opencode")
	input := reflection.ReflectionInput{ProjectName: "ghost", ExistingMemories: []memory.Memory{
		{ID: "D20E133860CC4AFE38B485AD5371BA59", Category: "fact",
			Content: "the deploy token is " + secret, Importance: 0.5, Source: "mcp"},
	}}
	tiers := []reflection.Consolidator{llm, reflection.NewSQLiteConsolidator()}

	if _, err := reflection.NewTieredConsolidator(tiers, logger).Consolidate(context.Background(), input); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "consolidator failed") {
		t.Fatalf("the tiered failure path logged nothing, so this test proves nothing:\n%s", logged)
	}
	if strings.Contains(logged, secretInClip) {
		t.Errorf("the tiered WARN carries the value the refused operation quoted:\n%s", logged)
	}
	if !strings.Contains(logged, "withheld") {
		t.Errorf("the tiered WARN does not say the operation line was withheld:\n%s", logged)
	}
	// The reason and the id still make it: a log line with neither is not a
	// diagnostic, and withholding the line is not the same as withholding the
	// failure.
	if !strings.Contains(logged, "is not one of the memories this run was given") {
		t.Errorf("the tiered WARN dropped the reader's reason:\n%s", logged)
	}
	if !strings.Contains(logged, badID) {
		t.Errorf("the tiered WARN dropped the id that was refused:\n%s", logged)
	}
}

// scriptedHarness is a fake reflector with a queued reply, a single reply
// repeated, or a transport failure. No real CLI harness is ever spawned.
type scriptedHarness struct {
	reply   string
	replies []string
	err     error
	calls   int
}

func (h *scriptedHarness) Reflect(_ context.Context, _ string) (string, ai.TokenUsage, error) {
	h.calls++
	if h.err != nil {
		return "", ai.TokenUsage{}, h.err
	}
	if len(h.replies) > 0 {
		i := h.calls - 1
		if i >= len(h.replies) {
			return "", ai.TokenUsage{}, fmt.Errorf("scriptedHarness: call %d was not scripted", h.calls)
		}
		return h.replies[i], ai.TokenUsage{}, nil
	}
	return h.reply, ai.TokenUsage{}, nil
}

// TestRepairTurnsSurvivesEveryWrapper: the count is set by the LLM tier, and the
// wrappers are what every caller actually runs — the `auto` tiered list, the
// `--require-llm` one-tier list, and the gated single-backend wrapper behind
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

// TestRepairTurnsSurvivesAFallthroughToTheMechanicalTier is the case that makes
// the count per RUN rather than per tier. On the `auto` path the LLM tier sits
// above the Jaccard-only SQLite tier: an LLM answer that is refused even after the
// repair turn is discarded and the mechanical result is returned instead. If the
// count died with the discarded result, the summary would print the Jaccard-only
// outcome with no repair token — the exact degradation #689 makes visible, now
// arriving silently after two billed calls.
func TestRepairTurnsSurvivesAFallthroughToTheMechanicalTier(t *testing.T) {
	harness := &alwaysRefusedHarness{}
	// Nine near-identical inputs: above the gate's minimum, so a mechanical
	// result is accepted rather than itself refused.
	input := reflection.ReflectionInput{ProjectName: "ghost"}
	for i := 0; i < 9; i++ {
		input.ExistingMemories = append(input.ExistingMemories, memory.Memory{
			ID: fmt.Sprintf("D20E133860CC4AFE38B485AD5371BA5%d", i), Category: "fact",
			Content: fmt.Sprintf("deployment note %d", i), Importance: 0.5, Source: "mcp",
		})
	}
	tiers := []reflection.Consolidator{
		reflection.NewNamedConsolidator(harness, "opencode"),
		reflection.NewSQLiteConsolidator(),
	}
	tiered := reflection.NewTieredConsolidator(tiers, slog.New(slog.NewTextHandler(io.Discard, nil)))

	result, err := tiered.Consolidate(context.Background(), input)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if !strings.Contains(tiered.Name(), "sqlite") {
		t.Fatalf("active tier = %q, want the mechanical fallback: the fixture has to fail the LLM tier for this to be the fallthrough case", tiered.Name())
	}
	if result.RepairTurns != 1 {
		t.Errorf("RepairTurns = %d, want 1 on a result the mechanical tier produced", result.RepairTurns)
	}
	if harness.calls != 2 {
		t.Errorf("harness calls = %d, want 2 — the discarded tier's repair turns are what the count reports", harness.calls)
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

// alwaysRefusedHarness refuses every answer it is given, differently each time,
// so the tier spends its repair turn and then fails for good. The FIRST refusal
// is the measured defect's own shape — a merge naming one extra hex character on
// a real id, which the reader refuses because that id is not one this run was
// given — so this fixture exercises the path #689 is about rather than some other
// refusal that happens to cost the same.
type alwaysRefusedHarness struct{ calls int }

func (h *alwaysRefusedHarness) Reflect(_ context.Context, _ string) (string, ai.TokenUsage, error) {
	h.calls++
	if h.calls == 1 {
		return `{"ops":["merge D20E133860CC4AFE38B485AD5371BA599,D20E133860CC4AFE38B485AD5371BA5%d -> the bastion and the mesh"]}`,
			ai.TokenUsage{}, nil
	}
	return `{"ops":["fold D20E133860CC4AFE38B485AD5371BA5%d -> x"]}`, ai.TokenUsage{}, nil
}
