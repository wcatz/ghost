package reflection

// Package reflection performs memory consolidation: HaikuConsolidator (Anthropic API)
// for intelligent merge, tier_sqlite.go (Jaccard similarity) for deterministic
// fallback, and TieredConsolidator that tries tiers in priority order with a quality
// gate rejecting LLM tiers that fall below the 30% threshold. Mechanical tiers
// are exempt from the quality gate.

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
)

// Consolidator performs memory consolidation. Each tier implements this
// differently: Haiku uses the Anthropic API for intelligent consolidation,
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
// Tiers should be ordered from highest quality to lowest (e.g. haiku, sqlite).
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

		// Quality gate: if there were enough input memories and the tier returned
		// less than 30% of them, treat the result as garbage and fall through.
		// The mechanical fallback (SQLite Jaccard dedup) is always accepted — it
		// is deterministic and cannot truncate or hallucinate. LLM tiers are
		// never exempt by position: with --require-llm the sqlite tier is omitted
		// from the list, so the last tier may be a real LLM whose garbage output
		// must still be rejected.
		inputCount := len(input.ExistingMemories)
		if inputCount >= 6 && len(result.Memories) < inputCount*3/10 && !tier.Mechanical() {
			t.logger.Warn("consolidator returned too few memories, trying next tier",
				"tier", tier.Name(),
				"input", inputCount,
				"output", len(result.Memories),
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
