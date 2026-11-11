package reflection

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"
)

// Consolidator performs memory consolidation. Each tier implements this
// differently: LLM uses a CLI harness for intelligent consolidation,
type Consolidator interface {
	Name() string
	Available(ctx context.Context) bool
	Consolidate(ctx context.Context, input ReflectionInput) (ReflectionResult, error)
	Mechanical() bool
}

// TieredConsolidator tries consolidators in priority order (highest tier first),
// falling back to the next tier on failure or unavailability.
type TieredConsolidator struct {
	tiers  []Consolidator
	active atomic.Int32
	logger *slog.Logger
	gated  bool
}

// NewTieredConsolidator creates a consolidator that tries each tier in order.
// Tiers should be ordered from highest quality to lowest (e.g. llm, sqlite).
func NewTieredConsolidator(tiers []Consolidator, logger *slog.Logger) *TieredConsolidator {
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

// Name reports the active tier, prefixed with "tiered:" to mark that this came
// from the tiered constructor the `auto` path uses. The prefix is a marker of
func (t *TieredConsolidator) Name() string {
	idx := int(t.active.Load())
	prefix := "tiered:"
	if t.gated {
		prefix = ""
	}
	if idx >= 0 && idx < len(t.tiers) {
		return prefix + t.tiers[idx].Name()
	}
	return prefix + "none"
}

// Mechanical is false: a tiered consolidator may contain LLM tiers, so its
// output is never exempt from the quality gate.
func (t *TieredConsolidator) Mechanical() bool { return false }

const (
	gateMinInput             = 6
	gateStrictInputMax       = 60
	gateBacklogInputMax      = 200
	gateStrictRetentionFloor = 0.30
	gateBacklogMinOutput     = 5
	gateStrictInputMinOutput = 18
)

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

// NewGatedConsolidator wraps ONE explicitly-selected tier in the tiered
// consolidator, so the quality gate applies to a selection that named its
func NewGatedConsolidator(c Consolidator, logger *slog.Logger) *TieredConsolidator {
	t := NewTieredConsolidator([]Consolidator{c}, logger)
	t.gated = true
	return t
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
	spent := 0
	withRepairs := func(result ReflectionResult) ReflectionResult {
		if result.RepairTurns > spent {
			spent = result.RepairTurns
		}
		if spent > result.RepairTurns {
			result.RepairTurns = spent
		}
		return result
	}
	for i, tier := range t.tiers {
		if !tier.Available(ctx) {
			t.logger.Debug("consolidator unavailable, skipping", "tier", tier.Name())
			continue
		}

		result, err := tier.Consolidate(ctx, input)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ReflectionResult{}, ctxErr
		}
		if err != nil {
			t.logger.Warn("consolidator failed, trying next tier", "tier", tier.Name(), "error", safeTierError(err))
			withRepairs(result)
			lastErr = err
			continue
		}

		inputCount := len(input.ExistingMemories)
		if inputCount >= gateMinInput && len(result.Memories) < gateMinOutput(inputCount) && !tier.Mechanical() {
			t.logger.Warn("consolidator returned too few memories, trying next tier",
				"tier", tier.Name(),
				"input", inputCount,
				"output", len(result.Memories),
				"min_output", gateMinOutput(inputCount),
			)
			withRepairs(result)
			lastErr = fmt.Errorf("%s: quality gate failed (%d/%d memories)", tier.Name(), len(result.Memories), inputCount)
			continue
		}

		t.active.Store(int32(i))
		return withRepairs(result), nil
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
