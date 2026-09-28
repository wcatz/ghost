package bench

import (
	"context"
	"strings"
	"testing"
)

// TestRunMeasuresNoAnswerQueries is the runner half of #561: a query with no
// relevant items is not skipped. It is undefined for every graded ratio, so it
// cannot be averaged into one — but a plausible wrong memory returned for it
// counts as a hit for whatever it displaced, which means a recall-only suite
// cannot see this class of failure at all. The measurement is the no-answer
// false-positive rate, and it is per condition: the two legs fail differently,
// so a single number taken from the shipped hybrid path hides whichever leg is
// confidently wrong.
func TestRunMeasuresNoAnswerQueries(t *testing.T) {
	store, kubernetesID := seedRunnerStore(t)
	ctx := context.Background()

	queries := []Query{
		{
			Name: "k8s", ProjectID: "p", Text: "kubernetes",
			Vector: []float32{0.9, 0.1, 0},
			Rel:    Relevance{kubernetesID: 1},
		},
		// Same text as the graded query, so every condition has an exact match
		// to return and the "no answer" is the LABEL, not the retrieval.
		{Name: "no-relevant", ProjectID: "p", Text: "kubernetes", Vector: []float32{0.9, 0.1, 0}, Flavor: "near_miss"},
	}

	results, err := Run(ctx, store, queries)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, r := range results {
		if r.Queries != 1 {
			t.Errorf("%s: scored %d queries, want 1 (the no-answer query is measured, not scored)", r.Condition, r.Queries)
		}
		if len(r.NoAnswer) != 1 {
			t.Fatalf("%s: measured %d no-answer queries, want 1: the runner skipped it", r.Condition, len(r.NoAnswer))
		}
		got := r.NoAnswer[0]
		if got.Name != "no-relevant" || got.Flavor != "near_miss" {
			t.Errorf("%s: no-answer row = %+v, want name/flavor carried through", r.Condition, got)
		}
		if got.Results == 0 {
			t.Errorf("%s: measured 0 results for a no-answer query, so the rate is vacuous", r.Condition)
		}
		// The corpus holds an exact match for this query's text, so the best
		// returned row is that memory and its cosine is 1.0. A row with no
		// comparable score reads 0, which the report has to be able to tell
		// apart from a real weak match.
		if got.Top < 0.99 {
			t.Errorf("%s: best returned cosine = %.3f, want ~1.0 (the seeded kubernetes memory matches this text)", r.Condition, got.Top)
		}
		if len(got.Cosines) != got.Results {
			t.Errorf("%s: %d cosines for %d results", r.Condition, len(got.Cosines), got.Results)
		}

		sum := SummarizeNoAnswer(r.Condition, r.NoAnswer)
		if sum.Queries != 1 {
			t.Errorf("%s: summary counted %d queries, want 1", r.Condition, sum.Queries)
		}
		if sum.MeanTop < 0.99 {
			t.Errorf("%s: mean top cosine = %.3f, want ~1.0", r.Condition, sum.MeanTop)
		}
		if len(sum.Floors) != len(FalsePositiveFloors) {
			t.Errorf("%s: %d floor rows, want one per configured floor (%d)", r.Condition, len(sum.Floors), len(FalsePositiveFloors))
		}
		for _, f := range sum.Floors {
			if f.Rate != 1 {
				t.Errorf("%s: floor %.2f rate = %.3f, want 1.000 (one query, one result above every floor here)", r.Condition, f.Floor, f.Rate)
			}
		}
	}
}

// TestFormatResultsReportsFalsePositivesBesideNDCG pins the placement and the
// arithmetic of the printed rate: the no-answer rate has to be readable next to
// the graded table, or the headline reads as if recall were the whole of
// retrieval quality. Hand-built results so the formatter is what is under test.
func TestFormatResultsReportsFalsePositivesBesideNDCG(t *testing.T) {
	measured := []NoAnswerQuery{
		{Name: "n1", Flavor: "off_domain", Results: 10, Top: 0.62, Cosines: map[string]float32{"a": 0.62, "b": 0.10}},
		{Name: "n2", Flavor: "off_domain", Results: 10, Top: 0.41, Cosines: map[string]float32{"c": 0.41, "d": 0.05}},
		{Name: "n3", Flavor: "near_miss", Results: 10, Top: 0.22, Cosines: map[string]float32{"e": 0.22}},
	}
	results := []Result{
		{Condition: CondFTS, Queries: 2, NDCG10: 0.749, NoAnswer: measured},
		{Condition: CondVector, Queries: 2, NDCG10: 0.801, NoAnswer: measured},
		{Condition: CondHybrid, Queries: 2, NDCG10: 0.818, NoAnswer: measured},
	}
	out := FormatResults(results)
	for _, want := range []string{"NDCG@10", "no-answer", "false-positive rate", CondFTS, CondVector, CondHybrid} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q:\n%s", want, out)
		}
	}
	// Two of the three no-answer queries cleared 0.30 and 0.40, one cleared
	// 0.50: the printed rates have to be the measured ones, not a constant.
	for _, want := range []string{"0.667", "0.333"} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not carry the measured per-floor rate %s:\n%s", want, out)
		}
	}
	// A no-answer query must never have leaked into the graded denominator.
	if strings.Contains(out, "3 graded queries") {
		t.Errorf("report counted the no-answer queries in the graded set:\n%s", out)
	}
}
