package bench

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// This file measures the no-answer rule (#955): should ghost_memory_search say
// "nothing here answers this" when the best match is weak, and at what bar.
//
// Three rule families are measured, because the choice between them is the
// finding and not a premise:
//
//   - cosine: an absolute floor on the BEST vector cosine in the block. This is
//     the rule the assembler ships (context.no_answer_cosine), so each of its
//     rows is the real Store.Candidates -> assemble.Run path with the bar set.
//   - fused: a floor on the top row's fused Base. Measured, not built.
//   - combined: refuse only when the best cosine is below the bar AND no row in
//     the block is a strong keyword match. Measured, not built.
//
// The last two are judged from the block the assembler produced at the same
// shipped cutoff, with the rule applied afterwards, and every row says which
// rows are real and which are measured that way. They exist so the table shows
// why the shipped rule is the cosine one, not so a second implementation of a
// rule can drift from the first.
//
// Every row reports the five numbers the ship gate is about: the no-answer
// false-positive rate (no-answer queries that still got rows), the answerable
// queries refused, the graded-relevant rows still admitted, context precision
// and the estimated tokens per answer. The off row (bar 0) is the baseline every
// other row reads as a change from.

// The no-answer ship gate, from the issue that introduced the rule: at most half
// the no-answer queries may still get rows, and at most 3% of the answerable
// ones (7 of 220) may be refused.
const (
	// NoAnswerGateFalsePositive is the largest false-positive rate the gate allows.
	NoAnswerGateFalsePositive = 0.5
	// NoAnswerGateRefused is the most answerable queries the gate allows refused.
	NoAnswerGateRefused = 7
)

// noAnswerKeywordRank is the keyword rank within which a row counts as a strong
// keyword match for the combined rule. It is the same value as the assembler's
// keyword arm of the verdict floor (rank 0..3).
const noAnswerKeywordRank = 3

// Rule names, as the table prints them.
const (
	RuleCosine   = "cosine"
	RuleFused    = "fused"
	RuleCombined = "combined"
)

// NoAnswerSweepPoint is one rule at one setting.
type NoAnswerSweepPoint struct {
	Rule    string
	Setting float64
	// Built says the rule is the one the assembler ships (real path) as opposed
	// to one judged from the block afterwards.
	Built bool
	// NoAnswer is the false-positive rate: no-answer queries that still came back
	// with at least one row, out of the no-answer queries measured.
	NoAnswer Ratio
	// Refused is the answerable queries that came back with no row.
	Refused Ratio
	// Report is the block over the answerable queries at this setting.
	Report ContextReport
}

// Gate reports whether this point meets the ship gate.
func (p NoAnswerSweepPoint) Gate() bool {
	if !p.NoAnswer.Defined() {
		return false
	}
	return float64(p.NoAnswer.Num)/float64(p.NoAnswer.Den) <= NoAnswerGateFalsePositive &&
		p.Refused.Num <= NoAnswerGateRefused
}

// NoAnswerCosineGrid is the bars the built rule is measured at. 0 is off, the
// baseline; the rest span the band between the no-answer and answerable cosine
// distributions, and the shipped default is in it.
func NoAnswerCosineGrid() []float64 {
	return []float64{0.0, 0.50, 0.55, 0.58, 0.60, 0.62, 0.64, 0.66, 0.70}
}

// NoAnswerFusedGrid is the top-row fused Base floors the fused rule is measured
// at. The fused score is a reciprocal-rank sum, so its scale is a function of the
// retrieval depth and not of how close a match is; the grid sits where it
// actually varies.
func NoAnswerFusedGrid() []float64 {
	return []float64{0.0150, 0.0155, 0.0160, 0.0162}
}

// NoAnswerCombinedGrid is the cosine bars the combined rule is measured at.
func NoAnswerCombinedGrid() []float64 {
	return []float64{0.60, 0.62, 0.64, 0.70}
}

// bestCosine is the best vector cosine among the admitted rows, or -1 when no
// row carries one.
func bestCosine(res assemble.Result) float64 {
	best := -1.0
	for _, it := range res.Items {
		if sig, ok := res.Trace.Signals[it.ID]; ok && sig.VectorScore > best {
			best = sig.VectorScore
		}
	}
	return best
}

// fusedRefuses is the fused rule: the top admitted row's fused Base is below the
// floor.
func fusedRefuses(floor float64) func(assemble.Result) bool {
	return func(res assemble.Result) bool {
		if len(res.Items) == 0 || res.Trace == nil {
			return false
		}
		sig, ok := res.Trace.Signals[res.Items[0].ID]
		return ok && sig.Base < floor
	}
}

// combinedRefuses is the combined rule: the best cosine is below the bar AND no
// admitted row is a strong keyword match.
func combinedRefuses(bar float64) func(assemble.Result) bool {
	return func(res assemble.Result) bool {
		if len(res.Items) == 0 || res.Trace == nil {
			return false
		}
		best := bestCosine(res)
		if best < 0 || best >= bar {
			return false
		}
		for _, it := range res.Items {
			if sig, ok := res.Trace.Signals[it.ID]; ok && sig.FTSRank >= 0 && sig.FTSRank <= noAnswerKeywordRank {
				return false
			}
		}
		return true
	}
}

// NoAnswerSweep measures every rule at every setting over an already-seeded
// store: the graded queries for the answerable half and the no-answer queries for
// the false-positive half. All rows run at the shipped relevance cutoff, so the
// numbers are the block the tool would return. An error at any point is refused.
func NoAnswerSweep(ctx context.Context, store *memory.Store, graded, noAnswer []Query, at time.Time) ([]NoAnswerSweepPoint, error) {
	cutoff := config.DefaultRelevanceCutoff
	var points []NoAnswerSweepPoint

	measure := func(rule string, setting float64, built bool, bar float64, refuse func(assemble.Result) bool) error {
		rep, err := runContextWith(ctx, store, graded, at, cutoff, bar, refuse)
		if err != nil {
			return fmt.Errorf("%s %.4f: %w", rule, setting, err)
		}
		fp := Ratio{Den: len(noAnswer)}
		for _, q := range noAnswer {
			res, err := assemble.Run(ctx, store, contextRequestAt(q, at, cutoff, bar))
			if err != nil {
				return fmt.Errorf("%s %.4f: assemble %q: %w", rule, setting, q.Name, err)
			}
			if refuse != nil && refuse(res) {
				res.Items = nil
			}
			if len(res.Items) > 0 {
				fp.Num++
			}
		}
		points = append(points, NoAnswerSweepPoint{
			Rule: rule, Setting: setting, Built: built, NoAnswer: fp,
			Refused: Ratio{Num: rep.Queries - rep.Answered, Den: rep.Queries},
			Report:  rep,
		})
		return nil
	}

	for _, bar := range NoAnswerCosineGrid() {
		if err := measure(RuleCosine, bar, true, bar, nil); err != nil {
			return nil, err
		}
	}
	for _, floor := range NoAnswerFusedGrid() {
		if err := measure(RuleFused, floor, false, 0, fusedRefuses(floor)); err != nil {
			return nil, err
		}
	}
	for _, bar := range NoAnswerCombinedGrid() {
		if err := measure(RuleCombined, bar, false, 0, combinedRefuses(bar)); err != nil {
			return nil, err
		}
	}
	return points, nil
}

// FormatNoAnswerSweep renders the sweep as an aligned table, the shipped default
// marked and the gate verdict beside every row.
func FormatNoAnswerSweep(points []NoAnswerSweepPoint) string {
	var b bytes.Buffer
	graded, noAnswer := 0, 0
	if len(points) > 0 {
		graded, noAnswer = points[0].Report.Queries, points[0].NoAnswer.Den
	}
	fmt.Fprintf(&b, "no-answer sweep (%d graded queries, %d no-answer queries, at the ghost_memory_search budget and the shipped relevance cutoff)\n", graded, noAnswer)
	b.WriteString("cosine  = withhold the block when the BEST vector cosine in it is below the bar (built: context.no_answer_cosine; 0.0000 is off, the baseline)\n")
	b.WriteString("fused   = withhold when the top row's fused Base is below the floor (measured from the block, not built)\n")
	b.WriteString("combined= withhold when the best cosine is below the bar AND no row ranks in the top four keyword hits (measured from the block, not built)\n\n")
	fmt.Fprintf(&b, "  %-9s %-7s %-20s %-18s %9s %-20s %12s  %s\n",
		"rule", "setting", "no-answer FP rate", "answerable refused", "relevant", "context precision", "tokens/ans", "gate")
	for _, pt := range points {
		verdict := "fails"
		if pt.Gate() {
			verdict = "meets"
		}
		if pt.Setting == 0 && pt.Rule == RuleCosine {
			verdict = "off"
		}
		mark := ""
		if pt.Rule == RuleCosine && pt.Setting == config.DefaultNoAnswerCosine {
			mark = "  <- shipped default"
		}
		fmt.Fprintf(&b, "  %-9s %-7.4f %-20s %-18s %9d %-20s %12s  %s%s\n",
			pt.Rule, pt.Setting, pt.NoAnswer.String(),
			fmt.Sprintf("%d/%d", pt.Refused.Num, pt.Refused.Den),
			pt.Report.Relevant, pt.Report.Precision.String(),
			pt.Report.Cost.TokensPerQuery.String(), verdict, mark)
	}
	fmt.Fprintf(&b, "\nship gate: no-answer false-positive rate at most %.3f, answerable queries refused at most %d,\n", NoAnswerGateFalsePositive, NoAnswerGateRefused)
	b.WriteString("graded-relevant admitted not lower than the refused queries' own rows. Pick the default from this table.\n")
	return b.String()
}
