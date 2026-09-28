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

// TestReflectRepairNoteIsCountable pins the token the summary carries. A repair
// that is invisible is a repair nobody can count, and the count is the point: the
// defect this closes was measured at roughly one run in three, and the only way
// to know whether that still holds for a given corpus is a stable string in
// `ghost reflect`'s own output.
//
// Two rules make the token usable, which is why this is a formatter rather than a
// line dropped in where convenient. It prints on exactly one of this process's
// streams, because on the unattended path that stdout is the append-only
// lifecycle.log and a second mention would double every count. And the zero case
// prints nothing, so an absent token means "the first answer parsed" rather than
// "nobody measured".
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

// TestResultLineCarriesTheRepairToken pins where the token lands: the `Result:`
// line, the one a person reads and a benchmark greps. It renders the same
// function runReflect prints rather than a copy of its format string, so
// changing the command's line changes this test; and it pins the pre-existing
// shape byte for byte, so a run that needed no repair still prints exactly what it
// printed before this change.
func TestResultLineCarriesTheRepairToken(t *testing.T) {
	mems := []reflection.ReflectMemory{{Category: "gotcha", Content: "one fact", Importance: 0.8, Tags: []string{}}}

	clean := reflectResultLine(reflection.ReflectionResult{Memories: mems})
	if clean != "Result:       1 memories (1 gotcha)\n" {
		t.Errorf("clean result line = %q, want the pre-existing shape unchanged", clean)
	}

	repaired := reflectResultLine(reflection.ReflectionResult{Memories: mems, RepairTurns: 1})
	if !strings.Contains(repaired, "repair: 1") {
		t.Errorf("repaired result line = %q, want it to carry the repair token", repaired)
	}
	if strings.Count(repaired, "repair:") != 1 {
		t.Errorf("repaired result line = %q, want the token exactly once", repaired)
	}
}

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
	// secretInClip is the part of the value clipOpLine's 80-rune window keeps
	// whole, and it is what the log must not contain. Checking the full value
	// instead would pass whatever the log did, because the clip truncates the
	// tail of any token this long — a vacuous assertion.
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
	// The replacement text is the value itself, and it is a rewrite rather than a
	// merge, for one reason: clipOpLine keeps 80 runes of the line, so a fixture
	// has to put the WHOLE value inside that window or the assertion below passes
	// whatever the log did. "rewrite <26-char id> -> " is 38 runes and the value
	// is 40, so it fits with room to spare; two 26-character ids and a sentence of
	// prose would not.
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

// TestTieredFailureLogKeepsATransportFailure is the other half, and the reason
// safeTierError is not readerComplaintForLog: the common error on that line is a
// harness that died, and its message carries no model text. A log that said
// "<withheld: *errors.errorString>" for "opencode run: signal: killed" would be
// strictly worse than useless, and this is the line an operator reads to tell a
// dead harness from a refused answer.
func TestTieredFailureLogKeepsATransportFailure(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	tiers := []reflection.Consolidator{
		reflection.NewNamedConsolidator(&scriptedHarness{err: fmt.Errorf("opencode run: signal: killed")}, "opencode"),
		reflection.NewSQLiteConsolidator(),
	}
	input := reflection.ReflectionInput{ProjectName: "ghost", ExistingMemories: []memory.Memory{
		{ID: "D20E133860CC4AFE38B485AD5371BA59", Category: "fact", Content: "a note", Importance: 0.5, Source: "mcp"},
	}}

	if _, err := reflection.NewTieredConsolidator(tiers, logger).Consolidate(context.Background(), input); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	logged := buf.String()
	if !strings.Contains(logged, "signal: killed") {
		t.Errorf("a transport failure was withheld from the log, so an operator cannot tell it from a refusal:\n%s", logged)
	}
	if strings.Contains(logged, "withheld") {
		t.Errorf("a transport failure carries no model text and should not be redacted:\n%s", logged)
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
