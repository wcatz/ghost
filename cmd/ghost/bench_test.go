package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/bench"
)

// TestBenchModeOfReadsTheReports pins the flag table `ghost bench` has: the
// default metric table, --sweep's ranked grid, --context's context-assembly
// table, --passive's passive-block table and the two sweeps. runBench is not
// reachable from a test (it prints to stdout and exits), so the parsing is a
// named function with a test of its own.
func TestBenchModeOfReadsTheThreeReports(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no flags is the metric table", nil, benchModeResults},
		{"sweep", []string{"--sweep"}, benchModeSweep},
		{"context", []string{"--context"}, benchModeContext},
		{"passive", []string{"--passive"}, benchModePassive},
		{"cutoff sweep", []string{"--cutoff-sweep"}, benchModeCutoff},
		{"no-answer sweep", []string{"--no-answer-sweep"}, benchModeNoAnswer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := benchModeOf(tc.args)
			if err != nil {
				t.Fatalf("benchModeOf(%v): %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("benchModeOf(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}

	// An unknown flag is refused rather than ignored: a typo that fell through to
	// the default table would print a complete, plausible, wrong report.
	if _, err := benchModeOf([]string{"--contex"}); err == nil {
		t.Error("an unknown flag was accepted, so a typo prints the default table while looking like the context one")
	}
	// Two modes are two reports over two questions, and honouring both would have
	// to drop one of them silently.
	for _, args := range [][]string{{"--sweep", "--context"}, {"--context", "--sweep"},
		{"--passive", "--context"}, {"--sweep", "--passive"}, {"--passive", "--sweep"},
		{"--no-answer-sweep", "--cutoff-sweep"}, {"--no-answer-sweep", "--context"}, {"--passive", "--no-answer-sweep"}} {
		if _, err := benchModeOf(args); err == nil {
			t.Errorf("benchModeOf(%v) was accepted, so one of the two reports is dropped without saying so", args)
		}
	}

	for _, flag := range []string{"--sweep", "--context", "--passive", "--cutoff-sweep", "--no-answer-sweep"} {
		if !strings.Contains(benchUsage, flag) {
			t.Errorf("benchUsage does not mention %s, so the mode is undiscoverable", flag)
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
