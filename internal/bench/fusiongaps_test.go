package bench

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
)

// TestFormatFusionGapsLabelsTheCaseBehindTheLeg: the verdict has three cases, not
// two. An interval entirely below zero is separable, and it is the one
// fusionGate fails the build on, so a two-case verdict has the command and the
// gate describing one measurement two opposite ways — and contradicts the
// footnote this same table prints.
func TestFormatFusionGapsLabelsTheCaseBehindTheLeg(t *testing.T) {
	per := func(vals ...float64) []QueryScore {
		out := make([]QueryScore, len(vals))
		for i, v := range vals {
			out[i] = QueryScore{Name: fmt.Sprintf("q%d", i), NDCG: v}
		}
		return out
	}
	results := []Result{
		// The fused condition LOSES to the vector leg on every query, so the whole
		// interval sits below zero: separable, and the case that must not be
		// reported as a tie.
		{Condition: CondHybrid, NDCG10: 0.4, Queries: 3, PerQuery: per(0.2, 0.3, 0.4)},
		{Condition: CondVector, NDCG10: 0.6, Queries: 3, PerQuery: per(0.7, 0.8, 0.9)},
		// And a leg it is exactly level with, which IS a tie.
		{Condition: CondFTS, NDCG10: 0.4, Queries: 3, PerQuery: per(0.2, 0.3, 0.4)},
	}
	out := FormatFusionGaps(results)
	if !strings.Contains(out, "behind the leg") {
		t.Errorf("a fused condition below the leg on every query is not labelled behind it:\n%s", out)
	}
	if !strings.Contains(out, "not separable from the leg") {
		t.Errorf("a leg the fused condition is level with is not labelled not separable:\n%s", out)
	}
	// The footnote's rule and the cells must agree: no row may carry a label
	// that contradicts its own interval. The numbers are located by POSITION from
	// the start of the numeric block, and a row that should be numeric but is not
	// FAILS — the earlier version of this loop indexed cells[2]/cells[3], which
	// are the condition name and the mean, and `continue`d on the parse error, so
	// every row was skipped and the two assertions below were unreachable. A check
	// that can be disabled by a format change is not a check.
	rows, checked := 0, 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "fused - ") {
			continue
		}
		rows++
		if strings.Contains(line, "not comparable") {
			// A refusal carries no interval, so it must not also carry a verdict.
			if strings.Contains(line, "ahead of") || strings.Contains(line, "behind the") ||
				strings.Contains(line, "not separable") {
				t.Errorf("a refused row also carries a verdict: %q", line)
			}
			continue
		}
		// fused | - | <condition> | mean | lo | hi | n | verdict...
		cells := strings.Fields(line)
		if len(cells) < 7 {
			t.Errorf("row has %d fields, too few for mean, lo, hi, a count and a verdict: %q", len(cells), line)
			continue
		}
		// The mean is parsed to hold the three-number block in place (a row whose
		// mean cell is not a number is a broken row) and then checked against the
		// verdict's direction, which is the claim the label makes.
		mean, err1 := strconv.ParseFloat(cells[3], 64)
		lo, err2 := strconv.ParseFloat(cells[4], 64)
		hi, err3 := strconv.ParseFloat(cells[5], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			t.Errorf("row does not carry three numbers where they belong (%v %v %v): %q", err1, err2, err3, line)
			continue
		}
		if n, err := strconv.Atoi(cells[6]); err != nil || n != 3 {
			t.Errorf("row reports %q queries, want the 3 it was given: %q", cells[6], line)
		}
		if lo > hi {
			t.Errorf("row prints an inverted interval: %q", line)
		}
		checked++
		separable := lo > 0 || hi < 0
		isTie := strings.Contains(line, "not separable from the leg")
		isAhead := strings.Contains(line, "ahead of the leg")
		isBehind := strings.Contains(line, "behind the leg")
		switch {
		case separable && isTie:
			t.Errorf("row is separable ([%s, %s]) and labelled a tie: %q", cells[4], cells[5], line)
		case !separable && !isTie:
			t.Errorf("row is not separable ([%s, %s]) and not labelled a tie: %q", cells[4], cells[5], line)
		case isAhead && !separable:
			t.Errorf("row is labelled ahead of the leg and its interval does not exclude zero: %q", line)
		case isBehind && !separable:
			t.Errorf("row is labelled behind the leg and its interval does not exclude zero: %q", line)
		case isAhead && isBehind:
			t.Errorf("row carries two opposing verdicts: %q", line)
		case isAhead && mean <= 0:
			t.Errorf("row is labelled ahead of the leg with a non-positive mean %+.4f: %q", mean, line)
		case isBehind && mean >= 0:
			t.Errorf("row is labelled behind the leg with a non-negative mean %+.4f: %q", mean, line)
		}
	}
	if rows != 2 {
		t.Errorf("parsed %d comparison rows, want 2", rows)
	}
	if checked != 2 {
		t.Errorf("verified the label against the interval on %d rows, want 2 — a row was skipped, so the check is not running", checked)
	}
}

// TestFormatFusionGapsRefusesAnEmptyPairing: pairedNDiff sees two empty PerQuery
// slices as a valid pairing (the names all match, because there are none) and
// bootstrapMeanCI returns (0, 0, 0) for an empty sample. The result is a
// confident-looking all-zero row that reads as "identical" and is in fact "not
// measured", printed under a header that says how many queries were graded — and
// not hypothetical: FormatResults is called that way by an existing test, so
// that report carries two such rows today.
func TestFormatFusionGapsRefusesAnEmptyPairing(t *testing.T) {
	results := []Result{
		{Condition: CondHybrid, NDCG10: 0.8, Queries: 2},
		{Condition: CondVector, NDCG10: 0.7, Queries: 2},
		{Condition: CondFTS, NDCG10: 0.6, Queries: 2},
	}
	out := FormatFusionGaps(results)
	if strings.Contains(out, "+0.0000") {
		t.Errorf("an empty pairing printed a confident zero row:\n%s", out)
	}
	if n := strings.Count(out, "not comparable: no paired queries"); n != 2 {
		t.Errorf("%d rows refused an empty pairing, want 2:\n%s", n, out)
	}
	// And through the report the command actually prints.
	full := FormatResults(results)
	if strings.Contains(full, "+0.0000") {
		t.Errorf("`ghost bench` output carries a confident zero row for results carrying no per-query scores:\n%s", full)
	}
}

// TestFormatFusionGapsPrintsTheHeadlineInterval: the fusion margin is the
// headline number in docs/benchmarks.md and README.md, and it used to exist only
// as a t.Logf in the regression test — a published interval no command prints,
// which is the defect #561 is about. `ghost bench` has to print it, from the same
// function the gate reads, so the doc and the build cannot disagree.
//
// Every field is checked rather than the string: a row can carry a plausible mean
// and the wrong interval, and the doc quotes both.
func TestFormatFusionGapsPrintsTheHeadlineInterval(t *testing.T) {
	// Four queries, the fused condition ahead on two of them and level on two, so
	// the paired mean is positive and the interval straddles zero — the shape the
	// committed dataset actually produces, and the one the doc's claim rests on.
	per := func(name string, vals ...float64) []QueryScore {
		out := make([]QueryScore, len(vals))
		for i, v := range vals {
			out[i] = QueryScore{Name: name + string(rune('a'+i)), NDCG: v}
		}
		return out
	}
	results := []Result{
		{Condition: CondHybrid, NDCG10: 0.8, Queries: 4, PerQuery: per("q", 1.0, 1.0, 0.5, 0.5)},
		{Condition: CondVector, NDCG10: 0.7, Queries: 4, PerQuery: per("q", 0.5, 0.5, 0.5, 0.5)},
		{Condition: CondFTS, NDCG10: 0.6, Queries: 4, PerQuery: per("q", 0.2, 0.2, 0.2, 0.2)},
	}
	out := FormatFusionGaps(results)

	for _, want := range []string{"fused - " + CondVector, "fused - " + CondFTS, "paired per query", "95% percentile bootstrap"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not mention %q:\n%s", want, out)
		}
	}
	// Both legs get a row; a leg that happened to tie is still a row, because
	// "not separable" is a finding and a missing row is not.
	if n := strings.Count(out, "fused - "); n != 2 {
		t.Errorf("%d comparison rows, want 2:\n%s", n, out)
	}

	// The vector row's numbers, recomputed from the same Results rather than
	// matched as text: (0.5, 0.5, 0, 0) has mean +0.25.
	hybrid, _ := resultFor(results, CondHybrid)
	vector, _ := resultFor(results, CondVector)
	want, err := CompareFusion(hybrid, vector)
	if err != nil {
		t.Fatalf("CompareFusion: %v", err)
	}
	if math.Abs(want.Mean-0.25) > 1e-12 {
		t.Fatalf("fixture mean = %+.6f, want +0.25; the test is not measuring what it thinks", want.Mean)
	}
	if !strings.Contains(out, sprintf4(want.Mean, want.Lo, want.Hi)) {
		t.Errorf("the printed report does not carry the recomputed interval %s:\n%s", sprintf4(want.Mean, want.Lo, want.Hi), out)
	}
	// The verdict has to follow the interval, not the sign of the mean: a positive
	// mean with an interval reaching zero is NOT "ahead of the leg".
	if want.Lo <= 0 && !strings.Contains(out, "not separable from the leg") {
		t.Errorf("an interval containing zero is called something other than not separable:\n%s", out)
	}
	// And the fused-vs-FTS row here IS separable (constant +0.4), so both
	// verdicts have to be reachable from the same formatter.
	if !strings.Contains(out, "ahead of the leg") {
		t.Errorf("a constant positive difference was not called ahead of the leg:\n%s", out)
	}
}

// sprintf4 renders an interval the way the report does, so the test compares the
// printed numbers against a recomputation rather than against another copy of the
// format string.
func sprintf4(mean, lo, hi float64) string {
	return fmt.Sprintf("%+9.4f %+9.4f %+9.4f", mean, lo, hi)
}

// TestFormatFusionGapsSaysWhenThereIsNothingToCompare: a results slice with no
// fused condition, or with no legs, has no interval. Printing an empty header
// would be a table of nothing; printing a zero row would read as "identical".
func TestFormatFusionGapsSaysWhenThereIsNothingToCompare(t *testing.T) {
	if got := FormatFusionGaps(nil); got != "" {
		t.Errorf("no results printed a section anyway:\n%s", got)
	}
	if got := FormatFusionGaps([]Result{{Condition: CondVector}}); got != "" {
		t.Errorf("a results slice with no fused condition printed a section:\n%s", got)
	}
	if got := FormatFusionGaps([]Result{{Condition: CondHybrid}}); got != "" {
		t.Errorf("a results slice with no leg to compare against printed a section:\n%s", got)
	}
}

// TestFormatFusionGapsRefusesAnUnpairableComparison: two conditions over
// different query sets produce a confident, meaningless interval if the names are
// not checked. The row has to say it is not comparable rather than print zeros.
func TestFormatFusionGapsRefusesAnUnpairableComparison(t *testing.T) {
	results := []Result{
		{Condition: CondHybrid, NDCG10: 0.8, PerQuery: []QueryScore{{Name: "q1", NDCG: 1}}},
		{Condition: CondVector, NDCG10: 0.7, PerQuery: []QueryScore{{Name: "other", NDCG: 0.5}}},
		{Condition: CondFTS, NDCG10: 0.6, PerQuery: []QueryScore{{Name: "q1", NDCG: 0.2}}},
	}
	out := FormatFusionGaps(results)
	if !strings.Contains(out, "not comparable") {
		t.Errorf("an unpairable comparison printed a row instead of refusing it:\n%s", out)
	}
	if strings.Contains(out, "+0.0000") {
		t.Errorf("an unpairable comparison printed zeros, which read as identical:\n%s", out)
	}
	// The pairable leg beside it still gets its row: one bad comparison does not
	// cost the good one.
	if !strings.Contains(out, "fused - "+CondFTS) {
		t.Errorf("the pairable leg's row was lost:\n%s", out)
	}
}

// TestFormatResultsCarriesTheFusionGaps: the reason for putting the table in
// FormatResults rather than only in a test log is that `ghost bench` prints
// FormatResults. If the wiring is dropped, the headline number stops being
// runnable again, and nothing else fails.
func TestFormatResultsCarriesTheFusionGaps(t *testing.T) {
	results := []Result{
		{Condition: CondHybrid, NDCG10: 0.8, Queries: 2, PerQuery: []QueryScore{{Name: "q1", NDCG: 1}, {Name: "q2", NDCG: 0.5}}},
		{Condition: CondVector, NDCG10: 0.7, Queries: 2, PerQuery: []QueryScore{{Name: "q1", NDCG: 0.5}, {Name: "q2", NDCG: 0.5}}},
		{Condition: CondFTS, NDCG10: 0.6, Queries: 2, PerQuery: []QueryScore{{Name: "q1", NDCG: 0.2}, {Name: "q2", NDCG: 0.2}}},
	}
	out := FormatResults(results)
	if !strings.Contains(out, "Fused vs one leg at a time") {
		t.Errorf("`ghost bench` output carries no fusion-interval table, so the doc's headline number is not runnable:\n%s", out)
	}
	if !strings.Contains(out, "fused - "+CondVector) {
		t.Errorf("the NDCG table is present but the comparison rows are not:\n%s", out)
	}
}
