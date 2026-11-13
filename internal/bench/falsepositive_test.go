package bench

import (
	"context"
	"testing"
)

// TestFalsePositiveReport runs the no-answer set through the production search
// path and logs the baseline. Report-only, for the same reason the maintenance
// suite is: the numbers are the starting point the abstention work (#580) moves,
// and a floor here would be a gate on that work rather than on this change.
//
// What IS enforced is that the set can measure something at all — every flavor
// present, and results actually returned for queries nothing answers. A suite
// where search returned nothing for any of them would report a beautiful 0.000
// and mean nothing. What the two distributions do with each other is reported,
// never asserted; see the comment inside for why that line must stay unset.
func TestFalsePositiveReport(t *testing.T) {
	// Parallel: this test seeds and searches the immutable headline corpus and only
	// reads it, and at 60-130s under -race it is one of the corpus-wide tests that decide whether
	// this package fits Go's 600s per-binary default — a budget it had already
	// spent down to ~10s when #677 added a sixth corpus-wide test, not a budget that
	// rows cost. There is no shared state to order against — the
	// package-level values are embedded bytes and one constant floor slice, and no
	// bench test sets an env var or the default logger — so the only thing running
	// these together buys is the overlap. Measured, in the commit that added this.
	t.Parallel()
	ds, vecs, err := BuiltinDataset()
	if err != nil {
		t.Fatalf("BuiltinDataset: %v", err)
	}
	negs := ds.Negatives
	if len(negs) < 20 {
		t.Fatalf("fixture has %d no-answer queries, want >= 20", len(negs))
	}
	flavors := map[string]int{}
	for _, n := range negs {
		flavors[n.Flavor]++
		if len(n.Rel) != 0 {
			t.Errorf("no-answer query %q grades %d memories", n.Name, len(n.Rel))
		}
	}
	for _, flavor := range []string{"off_domain", "near_miss"} {
		if flavors[flavor] == 0 {
			t.Errorf("no %s no-answer queries: one flavor alone cannot show what an abstain rule has to survive", flavor)
		}
	}

	store, db := newBenchStoreWithDB(t)
	ctx := context.Background()
	graded, err := Seed(ctx, store, db, ds, vecs)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	noAnswer, err := NegativeQueries(ds, vecs)
	if err != nil {
		t.Fatalf("NegativeQueries: %v", err)
	}
	// The no-answer set is measured by the RUNNER, per condition, and the
	// hybrid condition's measurement is what this report is built on: one
	// search per query rather than one per query per report.
	all := append(append([]Query{}, graded...), noAnswer...)
	results, err := Run(ctx, store, all)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	measured := NoAnswerFor(results, CondHybrid)
	if len(measured) != len(noAnswer) {
		t.Fatalf("runner measured %d no-answer queries, fixture has %d", len(measured), len(noAnswer))
	}
	rep, err := FalsePositives(measured, PerQueryFor(results, CondHybrid))
	if err != nil {
		t.Fatalf("FalsePositives: %v", err)
	}
	if rep.Queries != len(noAnswer) || rep.Answerable != len(graded) {
		t.Errorf("report counts %d/%d queries, want %d/%d", rep.Queries, rep.Answerable, len(noAnswer), len(graded))
	}
	if len(rep.Floors) != len(FalsePositiveFloors) {
		t.Errorf("report has %d floor rows, want %d", len(rep.Floors), len(FalsePositiveFloors))
	}
	// What follows asserts the shape of the measurement, never today's numbers.
	// In particular it does NOT assert that the no-answer and answerable
	// distributions are separated: that is a claim about the current ranking, and
	// an abstention fix that legitimately returns fewer, more similar rows for an
	// unanswered query would move the no-answer mean up and trip a test whose
	// whole job is to watch that fix land. The separation is reported instead
	// (mean top 0.584 vs 0.741), where a human reads it.
	//
	// The one outcome claim left is the flavor ordering below, and it is a
	// statement about the fixture: the near-miss set is supposed to be the hard
	// one, and if it is not, the set is mislabelled rather than the ranking wrong.
	if rep.MeanResults <= 0 {
		t.Errorf("mean results per no-answer query is %.1f: the search returned nothing, so the false-positive count is not being measured", rep.MeanResults)
	}
	if rep.Unseparable < 0 || rep.Unseparable > rep.Answerable {
		t.Errorf("unseparable count %d is outside 0..%d", rep.Unseparable, rep.Answerable)
	}
	// Every flavor must survive into the report with its own count: a pooled
	// mean would let the easy flavor flatter the hard one, which is the only
	// reason the two are kept apart.
	if len(rep.Flavors) != len(flavors) {
		t.Errorf("report has %d flavor rows, want %d", len(rep.Flavors), len(flavors))
	}
	byFlavor := map[string]FlavorStat{}
	for _, f := range rep.Flavors {
		if f.Queries != flavors[f.Flavor] {
			t.Errorf("flavor %s: report counts %d queries, fixture has %d", f.Flavor, f.Queries, flavors[f.Flavor])
		}
		byFlavor[f.Flavor] = f
	}
	// The near-miss set is the one an abstain rule has to survive. If a fixture
	// edit made it easier than the off-domain floor, the report would be
	// reporting the easy case and calling it the hard one.
	if byFlavor["near_miss"].MeanTop < byFlavor["off_domain"].MeanTop {
		t.Errorf("near-miss queries score %.3f on average against %.3f for off-domain ones: "+
			"the set meant to be the hard case is now the easy one",
			byFlavor["near_miss"].MeanTop, byFlavor["off_domain"].MeanTop)
	}
	// The maximum cannot be below the mean it is drawn from — that is arithmetic,
	// not a property of the ranking, so it stays asserted.
	if rep.NoAnswerMax < rep.MeanTop {
		t.Errorf("no-answer maximum %.3f is below the mean %.3f", rep.NoAnswerMax, rep.MeanTop)
	}

	// The deep report and the per-condition table describe the SAME run of the
	// shipped path, so their floor rows have to be the same numbers: Results is a
	// mean per query and Rate a fraction in both. A sum in one and a mean in the
	// other reads as "240 results per query" against a window of 10.
	sum := SummarizeNoAnswer(CondHybrid, measured)
	if len(sum.Floors) != len(rep.Floors) {
		t.Fatalf("the two reports have %d and %d floor rows", len(sum.Floors), len(rep.Floors))
	}
	for i := range sum.Floors {
		if sum.Floors[i] != rep.Floors[i] {
			t.Errorf("floor %.2f: per-condition table says %+v, deep report says %+v",
				sum.Floors[i].Floor, sum.Floors[i], rep.Floors[i])
		}
	}
	if rep.Floors[len(rep.Floors)-1].Results > scoreK {
		t.Errorf("floor %.2f reports %.2f results per query, more than the %d-row window",
			rep.Floors[len(rep.Floors)-1].Floor, rep.Floors[len(rep.Floors)-1].Results, scoreK)
	}
	t.Logf("no-answer queries against the graded corpus:\n%s", FormatFalsePositives(rep, CondHybrid))
}
