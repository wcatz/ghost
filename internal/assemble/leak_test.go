package assemble

import (
	"reflect"
	"testing"
	"time"
)

// TestResultLeaksReadsTheAssemblersOwnVerdicts pins the contamination classifier
// to the verdicts the trace recorded, one arm at a time.
func TestResultLeaksReadsTheAssemblersOwnVerdicts(t *testing.T) {
	resolved := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	open := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	closed := time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC)
	later := closed.AddDate(0, 1, 0)

	tests := []struct {
		name  string
		item  Item
		sig   Signals
		want  []string
		unset bool // no recorded signal for this item at all
	}{
		{
			name: "a resolved row is contaminated",
			item: Item{ID: "r1", ResolvedAt: &resolved},
			sig:  Signals{ValidityState: validityValid, ScopeMatched: true, ProjectMatch: true},
			want: []string{LeakResolved},
		},
		{
			name: "an expired row is contaminated",
			item: Item{ID: "r2"},
			sig:  Signals{ValidityState: validityExpired, ScopeMatched: true, ProjectMatch: true},
			want: []string{LeakExpired},
		},
		{
			name: "a not-yet-valid row is contaminated",
			item: Item{ID: "r3"},
			sig:  Signals{ValidityState: validityFuture, ScopeMatched: true, ProjectMatch: true},
			want: []string{LeakNotYetValid},
		},
		{
			name: "a scope contradiction is contaminated",
			item: Item{ID: "r4", Scope: map[string]string{"environment": "staging"}},
			sig:  Signals{ValidityState: validityValid, ScopeMatched: false, ProjectMatch: true},
			want: []string{LeakOutOfScope},
		},
		{
			name: "another project's row is contaminated",
			item: Item{ID: "r5", Bucket: "other-project"},
			sig:  Signals{ValidityState: validityValid, ScopeMatched: true, ProjectMatch: false},
			want: []string{LeakOtherProject},
		},
		{
			name: "an open window is not contamination",
			item: Item{ID: "r6", ValidFrom: &open, ValidUntil: &later},
			sig:  Signals{ValidityState: validityValid, ScopeMatched: true, ProjectMatch: true},
			want: nil,
		},
		{
			name: "a row that states no window at all is not contamination",
			item: Item{ID: "r7"},
			sig:  Signals{ValidityState: validityUnset, ScopeMatched: true, ProjectMatch: true},
			want: nil,
		},
		{
			name: "an unverified claim is not contamination",
			item: Item{ID: "r8"},
			sig:  Signals{ValidityState: validityUnverified, ScopeMatched: true, ProjectMatch: true},
			want: nil,
		},
		{
			// Two arms, and arms in report order rather than in the order this struct
			// happens to list its fields: a map walked for the report.
			name: "a row can raise more than one arm",
			item: Item{ID: "r9", ResolvedAt: &resolved, Bucket: "other-project"},
			sig:  Signals{ValidityState: validityValid, ScopeMatched: true, ProjectMatch: false},
			want: []string{LeakResolved, LeakOtherProject},
		},
		{
			// No recorded verdict is not a clean verdict.
			name:  "an item with no recorded signal raises no arm",
			item:  Item{ID: "r10"},
			unset: true,
			want:  nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := Result{Items: []Item{tc.item}, Trace: &Trace{Signals: map[string]Signals{}}}
			if !tc.unset {
				res.Trace.Signals[tc.item.ID] = tc.sig
			}
			got := res.Leaks()
			if len(got) != 1 {
				if tc.want == nil {
					if len(got) != 0 {
						t.Fatalf("Leaks() = %+v, want none", got)
					}
					return
				}
				t.Fatalf("Leaks() = %+v, want one entry for %q", got, tc.item.ID)
			}
			if got[0].ID != tc.item.ID {
				t.Errorf("Leak.ID = %q, want %q", got[0].ID, tc.item.ID)
			}
			if !reflect.DeepEqual(got[0].Arms, tc.want) {
				t.Errorf("Leak.Arms = %v, want %v", got[0].Arms, tc.want)
			}
		})
	}
}

// TestTraceTrimmedByBudgetSeparatesTheTwoTrims: the budget stage and the response-
// fit post-pass both remove rows and both can remove a row a query graded
// relevant, and their remedies differ — one is the caller's limit.
func TestTraceTrimmedByBudgetSeparatesTheTwoTrims(t *testing.T) {
	tr := &Trace{Stages: []StageTrace{
		{Stage: stageValidity, DroppedIDs: []string{"expired"}},
		{Stage: stageBudget, DroppedIDs: []string{"b1", "b2"}},
		{Stage: stageResponseFit, DroppedIDs: []string{"f1"}},
	}}
	stage, fit := tr.TrimmedByBudget()
	if !reflect.DeepEqual(stage, []string{"b1", "b2"}) {
		t.Errorf("stage-8 trims = %v, want [b1 b2]", stage)
	}
	if !reflect.DeepEqual(fit, []string{"f1"}) {
		t.Errorf("response_fit trims = %v, want [f1]", fit)
	}

	// An empty trace is a run that trimmed nothing, and nil rather than an empty
	// slice so a caller can print "0" without distinguishing the two.
	if stage, fit := (&Trace{}).TrimmedByBudget(); stage != nil || fit != nil {
		t.Errorf("a trace with no stages reported trims %v / %v, want nil / nil", stage, fit)
	}
}

func TestLeaksAndTagsLabelAreNilSafe(t *testing.T) {
	if got := (Result{}).Leaks(); got != nil {
		t.Errorf("Leaks over a result with no trace = %v, want nil", got)
	}
	if got := TagsLabel(nil); got != "" {
		t.Errorf("TagsLabel(nil) = %q, want empty", got)
	}
}
