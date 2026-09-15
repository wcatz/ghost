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
	// gateStrictInputMax is the largest input that gets the strict retention
	// floor. Up to here, a result keeping under the floor is treated as
	// truncated.
	gateStrictInputMax = 60
	// gateBacklogInputMax is the input size at which the floor reaches its
	// lowest value. Beyond it the floor no longer decays.
	gateBacklogInputMax = 200
	// gateStrictRetentionFloor is the fraction a small input must retain.
	gateStrictRetentionFloor = 0.30
	// gateBacklogRetentionFloor is the fraction a large backlog must retain.
	// It is a gross-truncation backstop, not a quality bar: at this scale the
	// anti-hallucination work is done by dropFabricatedMemories (which runs on
	// every LLM result, tier_llm.go) and the non-empty guarantee, because
	// retention alone cannot distinguish a good large-backlog consolidation
	// from a bad one.
	gateBacklogRetentionFloor = 0.05
)

// retentionFloor returns the minimum fraction of input memories an LLM
// consolidation must retain to pass the quality gate, decaying linearly from
// the strict small-input floor to the backlog floor as the input grows.
func retentionFloor(inputCount int) float64 {
	if inputCount <= gateStrictInputMax {
		return gateStrictRetentionFloor
	}
	if inputCount >= gateBacklogInputMax {
		return gateBacklogRetentionFloor
	}
	frac := float64(inputCount-gateStrictInputMax) / float64(gateBacklogInputMax-gateStrictInputMax)
	return gateStrictRetentionFloor + frac*(gateBacklogRetentionFloor-gateStrictRetentionFloor)
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
		// consolidation and the tier returned an implausibly small fraction of
		// it, treat the result as truncated and fall through. The floor is
		// scale-aware (see retentionFloor): strict on small inputs, low on
		// large backlogs where heavy compression is the correct answer. The
		// mechanical fallback (SQLite Jaccard dedup) is always accepted — it is
		// deterministic and cannot truncate or hallucinate. LLM tiers are never
		// exempt by position: with --require-llm the sqlite tier is omitted
		// from the list, so the last tier may be a real LLM whose truncated
		// output must still be rejected.
		inputCount := len(input.ExistingMemories)
		if inputCount >= gateMinInput && float64(len(result.Memories)) < float64(inputCount)*retentionFloor(inputCount) && !tier.Mechanical() {
			t.logger.Warn("consolidator returned too few memories, trying next tier",
				"tier", tier.Name(),
				"input", inputCount,
				"output", len(result.Memories),
				"floor", retentionFloor(inputCount),
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
