package main

import (
	"reflect"
	"testing"
)

func TestSessionsInRankOrder(t *testing.T) {
	memToSession := map[string]string{
		"m1": "sA", "m2": "sB", "m3": "sA", "m4": "sC",
	}
	got := sessionsInRankOrder([]string{"m3", "m1", "m4", "m2", "unknown"}, memToSession)
	want := []string{"sA", "sC", "sB"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sessionsInRankOrder = %v, want %v", got, want)
	}
}

func TestIsAbstention(t *testing.T) {
	if !isAbstention("gpt4_deadbeef_abs") {
		t.Error("suffix _abs must be abstention")
	}
	if isAbstention("gpt4_deadbeef") {
		t.Error("plain id must not be abstention")
	}
}

func TestParseFloors(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		want    map[string]float64
		wantErr bool
	}{
		{"empty spec", "", map[string]float64{}, false},
		{"single pair", "r5=0.74", map[string]float64{"r5": 0.74}, false},
		{"multiple pairs", "r5=0.74,ndcg10=0.72", map[string]float64{"r5": 0.74, "ndcg10": 0.72}, false},
		{"all known keys", "r1=0.1,r5=0.2,r10=0.3,mrr10=0.4,ndcg10=0.5",
			map[string]float64{"r1": 0.1, "r5": 0.2, "r10": 0.3, "mrr10": 0.4, "ndcg10": 0.5}, false},
		{"case-insensitive keys", "R5=0.74,NDCG10=0.72", map[string]float64{"r5": 0.74, "ndcg10": 0.72}, false},
		{"tolerates whitespace", " r5 = 0.74 , mrr10 = 0.9 ", map[string]float64{"r5": 0.74, "mrr10": 0.9}, false},
		{"unknown metric key", "bogus=1", nil, true},
		{"unknown key among valid ones", "r5=0.7,bogus=1", nil, true},
		{"missing equals sign", "r5", nil, true},
		{"non-numeric value", "r5=notanumber", nil, true},
		{"empty value", "r5=", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFloors(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseFloors(%q) = %v, nil; want error", tt.spec, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFloors(%q): unexpected error: %v", tt.spec, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseFloors(%q) = %v, want %v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestOverallMetrics(t *testing.T) {
	// Mirrors the published fts OVERALL row: agg holds sums across n
	// questions, not means — overallMetrics must divide by n, not pass
	// sums straight through (that would silently defeat every floor).
	a := &agg{n: 2, r1: 1.0, r5: 1.6, r10: 1.8, mrr: 1.5, ndcg: 1.4}
	got := overallMetrics(a)
	want := map[string]float64{"r1": 0.5, "r5": 0.8, "r10": 0.9, "mrr10": 0.75, "ndcg10": 0.7}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("overallMetrics(%+v) = %v, want %v", a, got, want)
	}
}

func TestOverallMetricsZeroQuestions(t *testing.T) {
	got := overallMetrics(&agg{})
	for k, v := range got {
		if v != 0 {
			t.Errorf("overallMetrics(empty)[%q] = %v, want 0", k, v)
		}
	}
}

func TestCheckFloors(t *testing.T) {
	// Published fts OVERALL numbers (docs/benchmarks.md).
	overall := map[string]float64{"r1": 0.429, "r5": 0.751, "r10": 0.832, "mrr10": 0.758, "ndcg10": 0.738}

	t.Run("empty floors is never a violation", func(t *testing.T) {
		if got := checkFloors(overall, map[string]float64{}); len(got) != 0 {
			t.Errorf("expected no violations, got %v", got)
		}
	})

	t.Run("floors below observed pass", func(t *testing.T) {
		got := checkFloors(overall, map[string]float64{"r5": 0.74, "ndcg10": 0.72})
		if len(got) != 0 {
			t.Errorf("expected no violations, got %v", got)
		}
	})

	t.Run("single violation formatted", func(t *testing.T) {
		got := checkFloors(overall, map[string]float64{"r5": 0.80})
		if len(got) != 1 {
			t.Fatalf("expected 1 violation, got %v", got)
		}
		want := "FLOOR VIOLATION: r5 = 0.751 < 0.800"
		if got[0] != want {
			t.Errorf("violation = %q, want %q", got[0], want)
		}
	})

	t.Run("multiple violations in stable sorted order", func(t *testing.T) {
		got := checkFloors(overall, map[string]float64{"r5": 0.90, "r1": 0.99, "ndcg10": 0.1})
		if len(got) != 2 {
			t.Fatalf("expected 2 violations (r1, r5 fail; ndcg10 passes), got %v", got)
		}
		if got[0] != "FLOOR VIOLATION: r1 = 0.429 < 0.990" {
			t.Errorf("got[0] = %q", got[0])
		}
		if got[1] != "FLOOR VIOLATION: r5 = 0.751 < 0.900" {
			t.Errorf("got[1] = %q", got[1])
		}
	})

	t.Run("exact equality is not a violation", func(t *testing.T) {
		got := checkFloors(overall, map[string]float64{"r5": 0.751})
		if len(got) != 0 {
			t.Errorf("expected no violation at exact equality, got %v", got)
		}
	})
}
