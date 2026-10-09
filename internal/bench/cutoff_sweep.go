package bench

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// This file sweeps the relevance-cutoff parameter (#954) over the SAME graded
// corpus and the SAME real assemble.Run path that `ghost bench --context`
// measures, so the default the assembler ships is chosen from the block's own
// numbers rather than from a hand-set constant.
//
// What it measures per share is the four things the ship gate is about: how many
// graded-relevant rows the block still admits, the context precision (relevant of
// the admitted rows), the result rate on answerable queries (which must stay
// 1.000 — a row that answered the question must never be the one a cutoff drops),
// and the estimated token cost per answer. The 0.000 share is the pre-cutoff
// baseline, measured on the same run, so every other row reads as a change from
// it rather than as an absolute.
//
// It shares RunContext's pool and its measureQuery rather than re-deriving
// anything: the figures have to be the block's figures, computed the one way the
// report computes them, or a sweep row would be a second implementation of the
// same rules free to disagree with the report it is choosing a default for.

// CutoffSweepPoint is the context block measured at one relevance-cutoff share.
type CutoffSweepPoint struct {
	Cutoff float64
	Report ContextReport
}

// ContextCutoffGrid is the set of shares the sweep measures. 0.0 is off (the
// baseline a reader compares against) and the rest step from a short answer to a
// long one. The shipped default is drawn from config.DefaultRelevanceCutoff and
// the grid spans it on both sides, so the choice is visible as a gradient rather
// than as a single number.
func ContextCutoffGrid() []float64 {
	return []float64{0.0, 0.50, 0.60, 0.63, 0.64, 0.65, 0.70}
}

// ContextCutoffSweep runs the context block at each share in the grid over an
// already-seeded store. One store serves every point, the pool is scored once per
// point, and an error at any point is refused rather than skipped: a sweep with a
// hole in it is a table whose gradient nobody can trust.
func ContextCutoffSweep(ctx context.Context, store *memory.Store, queries []Query, at time.Time, grid []float64) ([]CutoffSweepPoint, error) {
	points := make([]CutoffSweepPoint, 0, len(grid))
	for _, cutoff := range grid {
		rep, err := runContextAt(ctx, store, queries, at, cutoff)
		if err != nil {
			return nil, fmt.Errorf("cutoff %.3f: %w", cutoff, err)
		}
		points = append(points, CutoffSweepPoint{Cutoff: cutoff, Report: rep})
	}
	return points, nil
}

// FormatContextCutoffSweep renders the sweep as an aligned table: the share, how
// many graded-relevant rows survived, the pooled admitted rows, context
// precision, result rate and estimated tokens per answer — each figure with its
// denominator where the report carries one. The shipped default row is marked.
//
// The footer restates the ship gate the table is being read against, because a
// gradient of numbers with no target beside it invites reading "fewest tokens" as
// "best", and the gate is a four-way trade (keep the relevant rows, keep the
// result rate at 1.000, raise precision, lower tokens), not a single axis.
func FormatContextCutoffSweep(points []CutoffSweepPoint) string {
	var b bytes.Buffer
	queries := 0
	for _, pt := range points {
		queries = pt.Report.Queries
		break
	}
	fmt.Fprintf(&b, "relevance cutoff sweep (%d graded queries at the ghost_memory_search budget)\n", queries)
	b.WriteString("a row is cut once its fused score falls below the share of the TOP row's; 0.000 is off (the baseline)\n\n")
	fmt.Fprintf(&b, "  %-8s %10s %9s %-22s %-22s %14s\n",
		"cutoff", "relevant", "items", "context precision", "result rate", "tokens/ans")
	for _, pt := range points {
		mark := ""
		if pt.Cutoff == config.DefaultRelevanceCutoff {
			mark = "  <- shipped default"
		}
		fmt.Fprintf(&b, "  %-8s %10d %9d %-22s %-22s %14s%s\n",
			cutoffLabel(pt.Cutoff), pt.Report.Relevant, pt.Report.Items,
			pt.Report.Precision.String(), pt.Report.ResultRate.String(),
			pt.Report.Cost.TokensPerQuery.String(), mark)
	}
	base := points[0].Report
	fmt.Fprintf(&b, "\nbaseline (cutoff off): %d relevant, precision %s, %s tokens/answer, result rate %s\n",
		base.Relevant, base.Precision.String(), base.Cost.TokensPerQuery.String(), base.ResultRate.String())
	b.WriteString("ship gate: relevant within 2% of the baseline (>= its 0.98), result rate 1.000,\n")
	b.WriteString("precision above the baseline, tokens below it. Pick the default from this table.\n")
	return b.String()
}

// cutoffLabel renders a share as three decimals so 0 and the grid's tenths and the
// default all line up in the column.
func cutoffLabel(cutoff float64) string {
	return fmt.Sprintf("%.3f", cutoff)
}
