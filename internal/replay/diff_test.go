package replay

import (
	"strings"
	"testing"
)

func TestComputeDiffReportsOverlapShiftsAndAChangedTopHit(t *testing.T) {
	baseline := []string{"A", "B", "C", "D"}
	current := []string{"B", "A", "X", "C"}
	d := ComputeDiff(baseline, current, 4)

	if !d.TopHitChanged || d.BaselineTop != "A" || d.CurrentTop != "B" {
		t.Errorf("top hit = %q -> %q changed=%v, want A -> B changed", d.BaselineTop, d.CurrentTop, d.TopHitChanged)
	}
	if d.Overlap != 3 {
		t.Errorf("overlap = %d, want 3 (A, B, C)", d.Overlap)
	}
	if len(d.Added) != 1 || d.Added[0] != "X" {
		t.Errorf("added = %v, want [X]", d.Added)
	}
	if len(d.Removed) != 1 || d.Removed[0] != "D" {
		t.Errorf("removed = %v, want [D]", d.Removed)
	}
	shifts := map[string]int{}
	for _, s := range d.Shifts {
		shifts[s.ID] = s.Current - s.Baseline
	}
	if shifts["A"] != 1 || shifts["B"] != -1 || shifts["C"] != 1 {
		t.Errorf("rank shifts = %v, want A +1, B -1 and C +1", shifts)
	}
}

func TestComputeDiffUsesOnlyTheTopKWindow(t *testing.T) {
	baseline := []string{"A", "B", "C", "D", "E"}
	current := []string{"A", "B", "C", "D", "Z"}
	d := ComputeDiff(baseline, current, 3)
	if d.TopHitChanged {
		t.Error("top hit did not change in the top-3 window")
	}
	if d.Overlap != 3 {
		t.Errorf("overlap = %d, want 3", d.Overlap)
	}
	if len(d.Added) != 0 || len(d.Removed) != 0 {
		t.Errorf("added=%v removed=%v, want no membership change inside the top 3", d.Added, d.Removed)
	}
}

func TestComputeDiffIsStableForEmptyResults(t *testing.T) {
	d := ComputeDiff(nil, nil, 5)
	if d.TopHitChanged {
		t.Error("two empty result sets cannot have a changed top hit")
	}
	if d.BaselineTop != "" || d.CurrentTop != "" {
		t.Errorf("tops = %q/%q, want empty", d.BaselineTop, d.CurrentTop)
	}
	if d.Jaccard != 0 {
		t.Errorf("jaccard = %v, want 0", d.Jaccard)
	}
}

func TestFormatReportNamesChangedTopHitsAndTheSummary(t *testing.T) {
	report := FormatReport([]Result{
		{Query: queryRecord("first", "p1"), Diff: ComputeDiff([]string{"A", "B"}, []string{"B", "A"}, 2)},
		{Query: queryRecord("second", "p1"), Diff: ComputeDiff([]string{"A"}, []string{"A"}, 2)},
	})
	for _, want := range []string{"first", "changed top hit", "A", "B", "2 queries", "1 changed top hit"} {
		if !strings.Contains(report, want) {
			t.Errorf("report does not contain %q:\n%s", want, report)
		}
	}
}

func queryRecord(q, project string) Query {
	return Query{Query: q, ProjectID: project, Source: "search", Results: []string{"A"}}
}
