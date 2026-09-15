package reflection

// Package reflection performs memory consolidation: LlmConsolidator (a
// subscription-billed CLI harness — claude, opencode, codex, or goose) for
// intelligent merge, tier_sqlite.go (Jaccard similarity) for deterministic
// fallback, and TieredConsolidator that tries tiers in priority order with a
// scale-aware quality gate rejecting LLM tiers whose output is implausibly
// small for the input size. Mechanical tiers are exempt from the quality gate.

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"
)

// Consolidator performs memory consolidation. Each tier implements this
// differently: LLM uses a CLI harness for intelligent consolidation,
// and SQLite uses Jaccard similarity for mechanical deduplication.
type Consolidator interface {
	Name() string
	Available(ctx context.Context) bool
	Consolidate(ctx context.Context, input ReflectionInput) (ReflectionResult, error)
	// Mechanical reports whether this tier is the deterministic fallback
	// (Jaccard dedup) rather than an LLM. Mechanical tiers are exempt from the
	// quality gate; LLM tiers are not — a truncated/hallucinated LLM result
	// must never be accepted merely because it happens to be the last tier.
	Mechanical() bool
}

// TieredConsolidator tries consolidators in priority order (highest tier first),
// falling back to the next tier on failure or unavailability.
type TieredConsolidator struct {
	tiers  []Consolidator
	active atomic.Int32
	logger *slog.Logger
}

// NewTieredConsolidator creates a consolidator that tries each tier in order.
// Tiers should be ordered from highest quality to lowest (e.g. llm, sqlite).
func NewTieredConsolidator(tiers []Consolidator, logger *slog.Logger) *TieredConsolidator {
	// Hand the configured logger to tiers that can accept one, so their
	// diagnostics reach the same sink the tiered consolidator logs to
	// (GHOST_LOG_FILE / level filtering) instead of the global default.
	for _, tier := range tiers {
		if ls, ok := tier.(interface{ SetLogger(*slog.Logger) }); ok {
			ls.SetLogger(logger)
		}
	}
	return &TieredConsolidator{
		tiers:  tiers,
		logger: logger,
	}
}

func (t *TieredConsolidator) Name() string {
	idx := int(t.active.Load())
	if idx >= 0 && idx < len(t.tiers) {
		return "tiered:" + t.tiers[idx].Name()
	}
	return "tiered:none"
}

// Mechanical is false: a tiered consolidator may contain LLM tiers, so its
// output is never exempt from the quality gate.
func (t *TieredConsolidator) Mechanical() bool { return false }

// Quality-gate shape. The gate's purpose is to reject an LLM response that was
// truncated or hallucinated, not to enforce a fixed compression ratio — so it
// is scale-aware. On a small input, "consolidated" down to a sliver is almost
// certainly a truncation; on a large, redundant backlog the correct result
// legitimately retains far less, and a fixed ratio would reject the right
// answer (measured: a 172-memory incident log consolidating to ~24, which the
// old flat 30% floor rejected on every run, so that project never
// auto-consolidated — see docs/superpowers/reports/2026-09-15-memory-and-publish-audit.md).
const (
	// gateMinInput is the smallest input the gate applies to at all; below it
	// the store's empty-set guard is the only protection needed.
	gateMinInput = 6
	// gateStrictInputMax is the largest input that gets the strict 30% floor.
	// Up to here, a result keeping under the floor is treated as truncated.
	gateStrictInputMax = 60
	// gateBacklogInputMax is the input size at which the floor reaches its
	// lowest value. Beyond it the minimum stops relaxing.
	gateBacklogInputMax = 200
	// gateStrictRetentionFloor is the fraction a small input must retain.
	gateStrictRetentionFloor = 0.30
	// gateBacklogMinOutput is the absolute minimum number of memories a large
	// backlog consolidation must return. It is absolute, not a fraction: the
	// prompt targets 10-25 memories regardless of input size, so a percentage
	// floor would demand more output than the prompt ever asks for once the
	// backlog is large (5% of 1000 = 50 > 25) and reject every valid run,
	// stranding it in the O(n^2) SQLite tier. The anti-hallucination work at
	// this scale is done by dropFabricatedMemories (which runs on every LLM
	// result, tier_llm.go) and the non-empty guarantee.
	gateBacklogMinOutput = 5
	// gateStrictInputMinOutput is the output the strict floor demands at
	// gateStrictInputMax (0.30 x 60 = 18) — the anchor for interpolation.
	gateStrictInputMinOutput = 18
)

// gateMinOutput returns the minimum number of memories an LLM consolidation
// must return to pass the quality gate for a given input size. It is strict
// (30% of input) on small inputs, where a sliver means a truncated response,
// and relaxes to a small absolute minimum on a large backlog, where the prompt
// itself aims for 10-25 memories no matter how many went in.
func gateMinOutput(inputCount int) int {
	if inputCount <= gateStrictInputMax {
		return int(math.Ceil(float64(inputCount) * gateStrictRetentionFloor))
	}
	if inputCount >= gateBacklogInputMax {
		return gateBacklogMinOutput
	}
	frac := float64(inputCount-gateStrictInputMax) / float64(gateBacklogInputMax-gateStrictInputMax)
	return gateStrictInputMinOutput + int(math.Round(frac*float64(gateBacklogMinOutput-gateStrictInputMinOutput)))
}

func (t *TieredConsolidator) Available(ctx context.Context) bool {
	for _, tier := range t.tiers {
		if tier.Available(ctx) {
			return true
		}
	}
	return false
}

func (t *TieredConsolidator) Consolidate(ctx context.Context, input ReflectionInput) (ReflectionResult, error) {
	var lastErr error
	for i, tier := range t.tiers {
		if !tier.Available(ctx) {
			t.logger.Debug("consolidator unavailable, skipping", "tier", tier.Name())
			continue
		}

		result, err := tier.Consolidate(ctx, input)
		if err != nil {
			t.logger.Warn("consolidator failed, trying next tier", "tier", tier.Name(), "error", err)
			lastErr = err
			continue
		}

		// Quality gate: if the input was large enough to expect a real
		// consolidation and the tier returned fewer memories than the
		// scale-aware minimum, treat the result as truncated and fall through.
		// The minimum is strict on small inputs and a small absolute count on a
		// large backlog (see gateMinOutput). The mechanical fallback (SQLite
		// Jaccard dedup) is always accepted — it is deterministic and cannot
		// truncate or hallucinate. LLM tiers are never exempt by position: with
		// --require-llm the sqlite tier is omitted from the list, so the last
		// tier may be a real LLM whose truncated output must still be rejected.
		inputCount := len(input.ExistingMemories)
		if inputCount >= gateMinInput && len(result.Memories) < gateMinOutput(inputCount) && !tier.Mechanical() {
			t.logger.Warn("consolidator returned too few memories, trying next tier",
				"tier", tier.Name(),
				"input", inputCount,
				"output", len(result.Memories),
				"min_output", gateMinOutput(inputCount),
			)
			lastErr = fmt.Errorf("%s: quality gate failed (%d/%d memories)", tier.Name(), len(result.Memories), inputCount)
			continue
		}

		t.active.Store(int32(i))
		return result, nil
	}

	if lastErr != nil {
		return ReflectionResult{}, fmt.Errorf("all consolidation tiers failed (last: %w)", lastErr)
	}
	return ReflectionResult{}, fmt.Errorf("no consolidation tiers available")
}

// ActiveTier returns the name of the currently active consolidation tier.
func (t *TieredConsolidator) ActiveTier() string {
	return t.Name()
}
