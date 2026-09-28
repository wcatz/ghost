package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/bench"
)

// TestBenchQuerySetCarriesTheNoAnswerHalf pins what `ghost bench` hands the
// runner: the graded queries AND the no-answer ones, in one set. The no-answer
// queries are what the false-positive rate is measured from, and they are
// distinguished by an empty relevance map — so dropping them here is not a
// smaller table, it is a table with no false-positive rate in it and no error to
// notice. runBench is not reachable from a test (it prints to stdout and exits),
// so the composition is a named function with a test of its own.
func TestBenchQuerySetCarriesTheNoAnswerHalf(t *testing.T) {
	graded := []bench.Query{
		{Name: "q_graded", Rel: bench.Relevance{"id-1": 1}},
		{Name: "q_also_graded", Rel: bench.Relevance{"id-2": 3}},
	}
	noAnswer := []bench.Query{
		{Name: "n_off_domain", Flavor: "off_domain"},
		{Name: "n_near_miss", Flavor: "near_miss"},
	}

	all := benchQuerySet(graded, noAnswer)
	if len(all) != len(graded)+len(noAnswer) {
		t.Fatalf("query set has %d queries, want %d (%d graded + %d no-answer)", len(all), len(graded)+len(noAnswer), len(graded), len(noAnswer))
	}
	seen := map[string]bool{}
	for _, q := range all {
		if seen[q.Name] {
			t.Errorf("query %q appears twice", q.Name)
		}
		seen[q.Name] = true
	}
	for _, q := range noAnswer {
		if !seen[q.Name] {
			t.Errorf("no-answer query %q is not in the run, so it cannot be measured", q.Name)
		}
	}
	// The flavor has to survive the trip: it is what keeps the near-miss set
	// apart from the off-domain one in the report.
	for _, q := range all {
		if q.Name == "n_near_miss" && !strings.HasPrefix(q.Flavor, "near") {
			t.Errorf("query %q lost its flavor (%q)", q.Name, q.Flavor)
		}
	}
}

// TestBenchQuerySetDoesNotAliasItsInput: the graded slice belongs to the caller
// and the no-answer slice belongs to the dataset loader, and appending one to
// the other in place would write into whichever had spare capacity — a change
// that shows up as a duplicated or reordered query rather than as a wrong count.
func TestBenchQuerySetDoesNotAliasItsInput(t *testing.T) {
	graded := make([]bench.Query, 2, 8)
	graded[0] = bench.Query{Name: "q1", Rel: bench.Relevance{"id-1": 1}}
	graded[1] = bench.Query{Name: "q2", Rel: bench.Relevance{"id-2": 1}}
	noAnswer := []bench.Query{{Name: "n1", Flavor: "off_domain"}}

	all := benchQuerySet(graded, noAnswer)
	if len(graded) != 2 {
		t.Errorf("graded slice grew to %d entries: the composed set aliased it", len(graded))
	}
	if all[0].Name != "q1" || all[1].Name != "q2" || all[2].Name != "n1" {
		t.Errorf("composed set is out of order: %q, %q, %q", all[0].Name, all[1].Name, all[2].Name)
	}
}
