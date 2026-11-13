package bench

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestContextInstantSitsInsideEveryGradedWindow guards the one thing that makes a
// fixed clock safe: it must not sit NEAR a boundary any corpus row states.
//
// Near is the operative word. The corpus deliberately holds a row whose window
// closed in 2020 and one whose window opens in 2099 — those two are what stage 2
// exercises, and a clock outside either is correct rather than wrong. What would be
// wrong is a clock days from a boundary, because then a small corpus edit moves a
// row in or out of the block and the published table changes for a reason that has
// nothing to do with retrieval. So the rule is a margin rather than a
// containment: every boundary in the corpus must be at least contextClockMargin
// away from the instant the report is measured at.
//
// A corpus edit that added a window closing next month fails here. It would not
// fail anywhere else: stage 2 would retire the row, the table would lose a row,
// and the numbers in docs/benchmarks.md would be quietly wrong.
func TestContextInstantSitsInsideEveryGradedWindow(t *testing.T) {
	// 90 days. Comfortably inside the corpus's own layout — its nearest open
	// boundary is 2024-01-01 and its nearest closed one 2020-06-01 — and far
	// enough out that a corpus edit cannot move a row across it by accident.
	const margin = 90 * 24 * time.Hour

	ds, _, err := BuiltinDataset()
	if err != nil {
		t.Fatalf("load the built-in dataset: %v", err)
	}
	now := ContextInstant()
	for _, m := range ds.Memories {
		for _, bound := range []struct {
			name string
			text *string
		}{{"valid_from", m.ValidFrom}, {"valid_until", m.ValidUntil}} {
			if bound.text == nil {
				continue
			}
			at, ok := memory.ParseStamp(*bound.text)
			if !ok {
				t.Errorf("memory %q states an unreadable %s %q, which stage 2 cannot read either", m.Key, bound.name, *bound.text)
				continue
			}
			if d := at.Sub(now); d > -margin && d < margin {
				t.Errorf("memory %q has %s = %s, only %v from the pinned context clock %s: move the clock or the boundary, not the margin",
					m.Key, bound.name, at.Format(time.RFC3339), absDuration(d), now.Format(time.RFC3339))
			}
		}
	}
	// The clock is UTC and second-resolution on purpose: the stamp it is paired
	// with is written as SQLite datetime text, and a caller that got a local-zone
	// instant would be measuring against a different day than the corpus was
	// stamped on.
	if got := ContextInstant(); got.Location() != time.UTC || got.Nanosecond() != 0 {
		t.Errorf("ContextInstant = %v, want a UTC instant with no sub-second part", got)
	}
}

// absDuration is |d|, so a margin test reads the same whichever side of the clock
// the boundary fell on.
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// TestReportsRenderOverASliceOfTheBuiltinCorpus drives the context, cutoff-sweep,
// no-answer-sweep, runner and trap renderers over a small slice of the shipped
// corpus: each has to return a measurement of the real block and print its own
// section header, which is all the corpus-wide report tests leave unchecked.
func TestReportsRenderOverASliceOfTheBuiltinCorpus(t *testing.T) {
	t.Parallel()
	ds, vecs, err := BuiltinDataset()
	if err != nil {
		t.Fatalf("BuiltinDataset: %v", err)
	}
	store, db := newBenchStoreWithDB(t)
	ctx := context.Background()
	graded, at, err := SeedAt(ctx, store, db, ds, vecs, time.Now().UTC())
	if err != nil {
		t.Fatalf("SeedAt: %v", err)
	}
	negs, err := NegativeQueries(ds, vecs)
	if err != nil {
		t.Fatalf("NegativeQueries: %v", err)
	}
	graded, negs = graded[:20], negs[:8]

	rep, err := RunContext(ctx, store, graded, at)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}
	if rep.Queries != len(graded) {
		t.Errorf("context report measured %d queries, want %d", rep.Queries, len(graded))
	}
	points, err := ContextCutoffSweep(ctx, store, graded, at, ContextCutoffGrid())
	if err != nil {
		t.Fatalf("ContextCutoffSweep: %v", err)
	}
	noAnswer, err := NoAnswerSweep(ctx, store, graded, negs, at)
	if err != nil {
		t.Fatalf("NoAnswerSweep: %v", err)
	}
	results, err := Run(ctx, store, append(append([]Query{}, graded...), negs...))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var sums []NoAnswerSummary
	for _, r := range results {
		sums = append(sums, SummarizeNoAnswer(r.Condition, r.NoAnswer))
	}
	for _, c := range []struct{ name, out, want string }{
		{"context", FormatContext(rep), "context"},
		{"cutoff sweep", FormatContextCutoffSweep(points), "shipped default"},
		{"no-answer sweep", FormatNoAnswerSweep(noAnswer), "cosine"},
		{"results", FormatResults(results), "NDCG"},
		{"top-row shares", FormatTopRowShares(results), "top"},
		{"no-answer", FormatNoAnswer(sums), "no-answer"},
		{"fusion gaps", FormatFusionGaps(results), "Fused vs one leg"},
	} {
		if !strings.Contains(strings.ToLower(c.out), strings.ToLower(c.want)) {
			t.Errorf("%s render is missing %q:\n%s", c.name, c.want, c.out)
		}
	}

	scenarios := loadTrapTestdata(t)
	p := memory.DefaultSearchParams()
	on, err := RunRecencyTrap(ctx, scenarios, p)
	if err != nil {
		t.Fatalf("RunRecencyTrap: %v", err)
	}
	if out := FormatTrap(on, on); out == "" {
		t.Error("FormatTrap rendered nothing")
	}
}

// TestSmallPredicatesAndRenderersTakeTheirEdges covers the branches the corpus
// run never reaches: an undefined ratio, a gate with no denominator or too many
// refusals, a sweep grid with and without its default, a supersedes cycle, and a
// fusion table with a leg missing.
func TestSmallPredicatesAndRenderersTakeTheirEdges(t *testing.T) {
	if got := (Ratio{}).String(); got != "n/a" {
		t.Errorf("undefined ratio renders %q, want n/a", got)
	}
	if (NoAnswerSweepPoint{}).Gate() {
		t.Error("a point with no no-answer queries passed the gate")
	}
	if (NoAnswerSweepPoint{NoAnswer: Ratio{0, 4}, Refused: Ratio{NoAnswerGateRefused + 1, 20}}).Gate() {
		t.Error("a point refusing too many answerable queries passed the gate")
	}
	if !(NoAnswerSweepPoint{NoAnswer: Ratio{0, 4}, Refused: Ratio{0, 20}}).Gate() {
		t.Error("a clean point failed the gate")
	}

	def := memory.DefaultSearchParams()
	other := def
	other.VecWeight = 0.31
	paired := &PairDiff{Mean: 0.01, Lo: -0.01, Hi: 0.02}
	withDefault := FormatSweep([]SweepPoint{{Params: def}, {Params: other, VsDefault: paired}, {Params: other}})
	if !strings.Contains(withDefault, "this is the default") || !strings.Contains(withDefault, "+0.0100") {
		t.Errorf("sweep with a default is missing its markers:\n%s", withDefault)
	}
	if out := FormatSweep([]SweepPoint{{Params: other}}); !strings.Contains(out, "no default in grid") {
		t.Errorf("sweep without a default does not say so:\n%s", out)
	}

	if got := supersedesCycle([]MemorySpec{{Key: "a", Supersedes: []string{"b"}}, {Key: "b", Supersedes: []string{"a"}}}); got == "" {
		t.Error("a two-memory supersedes cycle went undetected")
	}
	if got := supersedesCycle([]MemorySpec{{Key: "a", Supersedes: []string{"b"}}, {Key: "b"}}); got != "" {
		t.Errorf("an acyclic graph reported the cycle %q", got)
	}
	if got := FormatFusionGaps([]Result{{Condition: CondHybrid, Queries: 1}}); strings.Contains(got, "\n"+CondVector) {
		t.Errorf("fusion table printed a leg that was never run:\n%s", got)
	}
}
