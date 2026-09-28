package reflection

// Package reflection performs memory consolidation: LlmConsolidator (a
// configured CLI harness — claude, opencode, codex, or goose) for intelligent
// merge, tier_sqlite.go (Jaccard similarity) for deterministic
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
	// gated marks the single-tier form built by NewGatedConsolidator. It changes
	// only the reported name (see Name), never the gate: the gate is keyed on
	// tier.Mechanical() and the tier list, both of which are the same here.
	gated bool
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

// Name reports the active tier, prefixed with "tiered:" to mark that this came
// from the tiered constructor the `auto` path uses. The prefix is a marker of
// the constructor, NOT of how many tiers were in the list: `auto --require-llm`
// builds a one-tier list too and still prints the prefix.
//
// A consolidator built by NewGatedConsolidator reports its one tier UNPREFIXED.
// The operator named that backend on the command line, and `ghost reflect`
// prints this as `Consolidator: <name>` — wrapping it changes how the tier is
// bounded, not which tier runs, so a script or a saved log-grep keyed on
// `Consolidator: cli` must keep matching. Nothing parses the label either way.
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

// Quality-gate shape. The gate's purpose is to reject an LLM response that was
// truncated or hallucinated, not to enforce a fixed compression ratio — so it
// is scale-aware. On a small input, "consolidated" down to a sliver is almost
// certainly a truncation; on a large, redundant backlog the correct result
// legitimately retains far less, and a fixed ratio would reject the right
// answer (measured: a 172-memory incident log consolidating to ~24, which the
// old flat 30% floor rejected on every run, so that project never
// auto-consolidated — see docs/superpowers/reports/2026-09-15-memory-and-publish-audit.md).
//
// The floor is PER TIER, so it only bounds a run with nowhere else to go. The
// `auto` default drops to the exempt mechanical tier (Jaccard dedup cannot
// truncate or hallucinate); `--require-llm`, which omits it, and the
// explicitly-selected LLM tiers (NewGatedConsolidator) have exactly one tier and
// report the rejection as a failed run. Anything the harness declines to name
// is carried through by the pass-through rather than counted as an output, so a
// response that is a list of operations — not a rewritten corpus — is measured
// by how many rows it would change, not by how short it looks.
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
	// backlog consolidation must return. It is absolute, not a fraction, and
	// deliberately says nothing about what the prompt asks for: a large
	// redundant backlog legitimately compresses by an order of magnitude, so a
	// percentage has no stable meaning at that scale — 30% of 1000 demands 300
	// memories, and every genuinely consolidated backlog fails that, so the floor
	// would reject every valid run and strand the project in the O(n^2) SQLite
	// tier forever. The measured case is 172 memories consolidating to ~24, which
	// is the shape the absolute floor admits. What remains is a backstop against
	// an answer that has collapsed the corpus to nothing; the anti-hallucination
	// work at this scale is done by dropFabricatedMemories (which runs on every
	// LLM result, tier_llm.go) and the non-empty guarantee.
	gateBacklogMinOutput = 5
	// gateStrictInputMinOutput is the output the strict floor demands at
	// gateStrictInputMax (0.30 x 60 = 18) — the anchor for interpolation.
	gateStrictInputMinOutput = 18
)

// gateMinOutput returns the minimum number of memories an LLM consolidation
// must return to pass the quality gate for a given input size. It is strict
// (30% of input) on small inputs, where a sliver means a truncated response,
// and relaxes to a small absolute minimum on a large backlog, where a genuine
// consolidation compresses by an order of magnitude and a fraction would reject
// the right answer.
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
// backend outright.
//
// `ghost reflect --tier cli` and `--tier opencode` used to hand the bare
// LlmConsolidator straight to the apply path, where the gate does not live —
// so the floor did not exist for those invocations at all and any answer the
// harness returned was applied, however small. The default `auto` tier has
// always wrapped its tiers, so the default and the unattended lifecycle path
// were bounded while the two explicit LLM tiers were not, and eval/cycle
// measures with `--tier opencode --apply`, meaning the suite graded an ungated
// consolidator while production ran a gated one (issue #549).
//
// One tier and no mechanical fallback is what makes this a bound rather than a
// rewrite: a result below the floor has nowhere to fall through to, so the run
// fails — which is the honest reading of "use exactly this tier" when the
// answer came back truncated. A mechanical tier is still exempt inside the
// wrapper, so a caller that gates a Jaccard tier keeps today's behaviour.
//
// It reports the tier's own name rather than the "tiered:" prefix, so
// `Consolidator: cli` still prints `cli` (see Name — the prefix marks the
// constructor, and the gate is not the `auto` path's constructor).
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
	// Repairs are counted per RUN, not per tier: a tier whose result is
	// discarded here may have spent a repair turn to produce it, and the count
	// has to reach whichever result does survive. Without that, a run that
	// repaired and then fell through to SQLite would report the Jaccard-only
	// outcome as one that never had a repair — the degradation #689 exists to
	// make visible, arriving silently after two billed calls.
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
		if err != nil {
			// safeTierError, not err: a reader refusal quotes the rejected
			// operation line back, and for a merge or a rewrite that line ends in
			// the model's own replacement prose over stored memory — which is why
			// the tier redacts its own WARN above and why the three proposal log
			// lines go through previewContent. This is the last thing between the
			// tier and the log, and on the unattended path the logger IS the
			// stderr the stop hook redirects into the append-only lifecycle.log.
			// A transport failure keeps its own message: it carries no model text,
			// and "signal: killed" is worth telling apart from a refusal.
			t.logger.Warn("consolidator failed, trying next tier", "tier", tier.Name(), "error", safeTierError(err))
			withRepairs(result)
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
