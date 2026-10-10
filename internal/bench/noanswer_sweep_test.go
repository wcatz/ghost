package bench

import (
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/config"
)

// The no-answer sweep (#955) is the measurement the shipped bar is chosen from, so
// it has to drive the real assembler path at every setting. It is scoped to the
// small context fixture rather than the 220-query corpus for the reason the cutoff
// sweep's test gives: a corpus-wide sweep is a corpus-wide run per grid point and
// the bench package is close to CI's per-binary ceiling. The graded figures live in
// docs/benchmarks.md and `ghost bench --no-answer-sweep`.
func TestNoAnswerSweepDrivesTheRealRuleAtEverySetting(t *testing.T) {
	store, queries, at := contextFixtureStore(t)
	// One no-answer query: no relevant row, and the all-components vector that has
	// a positive but small cosine with every embedded row.
	var every []float32
	for _, q := range queries {
		if q.Name == "q_pool" {
			every = q.Vector
		}
	}
	negatives := []Query{{Name: "n_none", ProjectID: queries[0].ProjectID, Text: "zzzz qqqq", Vector: every, Flavor: "off_domain"}}

	points, err := NoAnswerSweep(context.Background(), store, queries, negatives, at)
	if err != nil {
		t.Fatalf("NoAnswerSweep: %v", err)
	}
	want := len(NoAnswerCosineGrid()) + len(NoAnswerFusedGrid()) + len(NoAnswerCombinedGrid())
	if len(points) != want {
		t.Fatalf("sweep returned %d points, want one per setting (%d)", len(points), want)
	}
	off := points[0]
	if off.Rule != RuleCosine || off.Setting != 0 || !off.Built {
		t.Fatalf("first point = %+v, want the built rule off (the baseline)", off)
	}
	if off.NoAnswer.Num != 1 || off.NoAnswer.Den != 1 {
		t.Errorf("baseline false positives = %v, want 1/1: with the bar off the no-answer query gets rows", off.NoAnswer)
	}
	if off.Refused.Num != 0 {
		t.Errorf("baseline refused %d answerable queries, want 0", off.Refused.Num)
	}
	// A bar above every cosine in the fixture refuses everything: the real rule
	// ran, because only the assembler's own step can empty a block here.
	top := points[len(NoAnswerCosineGrid())-1]
	if top.Rule != RuleCosine || top.NoAnswer.Num != 0 {
		t.Errorf("highest bar point = %+v, want the no-answer query refused (0 false positives)", top)
	}
	if top.Refused.Num < off.Refused.Num {
		t.Errorf("refused fell from %d to %d as the bar rose", off.Refused.Num, top.Refused.Num)
	}
	foundDefault := false
	for _, pt := range points {
		if pt.Rule == RuleCosine && pt.Setting == config.DefaultNoAnswerCosine {
			foundDefault = true
		}
	}
	if !foundDefault {
		t.Errorf("the cosine grid %v does not hold the shipped default %.3f", NoAnswerCosineGrid(), config.DefaultNoAnswerCosine)
	}
	out := FormatNoAnswerSweep(points)
	for _, w := range []string{"no-answer FP rate", "answerable refused", "context precision", "tokens/ans", "ship gate", "<- shipped default", "not built"} {
		if !strings.Contains(out, w) {
			t.Errorf("the rendered sweep is missing %q:\n%s", w, out)
		}
	}
}

func TestNoAnswerSweepGate(t *testing.T) {
	pt := func(fp, den, refused int) NoAnswerSweepPoint {
		return NoAnswerSweepPoint{NoAnswer: Ratio{Num: fp, Den: den}, Refused: Ratio{Num: refused, Den: 220}}
	}
	for _, tc := range []struct {
		name string
		p    NoAnswerSweepPoint
		want bool
	}{
		{"meets", pt(5, 24, 2), true},
		{"at both limits", pt(12, 24, 7), true},
		{"too many false positives", pt(13, 24, 0), false},
		{"too many refused", pt(0, 24, 8), false},
		{"no no-answer population", pt(0, 0, 0), false},
	} {
		if got := tc.p.Gate(); got != tc.want {
			t.Errorf("%s: Gate() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestNoAnswerRulesJudgeTheBlock pins the two measured-not-built rules on a
// hand-built block, so their predicates are what the table says they are.
func TestNoAnswerRulesJudgeTheBlock(t *testing.T) {
	res := assemble.Result{
		Items: []assemble.Item{{ID: "a"}, {ID: "b"}},
		Trace: &assemble.Trace{Signals: map[string]assemble.Signals{
			"a": {Base: 0.0150, VectorScore: 0.55, FTSRank: 9},
			"b": {Base: 0.0100, VectorScore: 0.40, FTSRank: -1},
		}},
	}
	if !fusedRefuses(0.016)(res) || fusedRefuses(0.014)(res) {
		t.Error("the fused rule must refuse a top Base below the floor and keep one above it")
	}
	if !combinedRefuses(0.6)(res) {
		t.Error("the combined rule must refuse: best cosine 0.55 is below 0.6 and no row is a top-four keyword hit")
	}
	if combinedRefuses(0.5)(res) {
		t.Error("the combined rule must keep a block whose best cosine clears the bar")
	}
	res.Trace.Signals["b"] = assemble.Signals{Base: 0.01, VectorScore: 0.40, FTSRank: 1}
	if combinedRefuses(0.6)(res) {
		t.Error("the combined rule must keep a block with a strong keyword match")
	}
}

func TestContextRequestCarriesTheShippedBarButTheCutoffSweepDoesNot(t *testing.T) {
	q := Query{ProjectID: "p", Text: "x", Vector: []float32{1, 0}}
	if got := ContextRequest(q, contextFixtureInstant).NoAnswerCosine; got != config.DefaultNoAnswerCosine {
		t.Errorf("ContextRequest bar = %v, want the shipped default %v", got, config.DefaultNoAnswerCosine)
	}
	if got := contextRequestWithCutoff(q, contextFixtureInstant, 0.5).NoAnswerCosine; got != 0 {
		t.Errorf("the cutoff sweep's request carries bar %v, want 0 so it measures the cutoff alone", got)
	}
}
