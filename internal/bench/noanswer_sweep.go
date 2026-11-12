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

const (
	// NoAnswerGateFalsePositive is the largest false-positive rate the gate allows.
	NoAnswerGateFalsePositive = 0.5
	// NoAnswerGateRefused is the most answerable queries the gate allows refused.
	NoAnswerGateRefused = 7
)

const noAnswerKeywordRank = 3

// Rule names.
const (
	RuleCosine   = "cosine"
	RuleFused    = "fused"
	RuleCombined = "combined"
)

// NoAnswerSweepPoint is one rule at one setting.
type NoAnswerSweepPoint struct {
	Rule     string
	Setting  float64
	Built    bool
	NoAnswer Ratio
	Refused  Ratio
	Report   ContextReport
}

// Gate reports whether this point meets the ship gate.
func (p NoAnswerSweepPoint) Gate() bool {
	if !p.NoAnswer.Defined() {
		return false
	}
	return float64(p.NoAnswer.Num)/float64(p.NoAnswer.Den) <= NoAnswerGateFalsePositive &&
		p.Refused.Num <= NoAnswerGateRefused
}

// NoAnswerCosineGrid is the bars the built rule is measured at.
func NoAnswerCosineGrid() []float64 {
	return []float64{0.0, 0.50, 0.55, 0.58, 0.60, 0.62, 0.64, 0.66, 0.70}
}

// NoAnswerFusedGrid is the fused rule settings.
func NoAnswerFusedGrid() []float64 {
	return []float64{0.0150, 0.0155, 0.0160, 0.0162}
}

// NoAnswerCombinedGrid is the combined rule settings.
func NoAnswerCombinedGrid() []float64 {
	return []float64{0.60, 0.62, 0.64, 0.70}
}

func bestCosine(res assemble.Result) float64 {
	best := -1.0
	for _, it := range res.Items {
		if sig, ok := res.Trace.Signals[it.ID]; ok && sig.VectorScore > best {
			best = sig.VectorScore
		}
	}
	return best
}

func fusedRefuses(floor float64) func(assemble.Result) bool {
	return func(res assemble.Result) bool {
		if len(res.Items) == 0 || res.Trace == nil {
			return false
		}
		sig, ok := res.Trace.Signals[res.Items[0].ID]
		return ok && sig.Base < floor
	}
}

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

// NoAnswerSweep measures every rule at every setting.
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

// FormatNoAnswerSweep renders the sweep table.
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
