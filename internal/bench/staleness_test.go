package bench

import (
	"context"
	"os"
	"testing"
)

func TestJudgeProbe(t *testing.T) {
	cases := []struct {
		name              string
		ranked            []string
		fresh             string
		stale             []string
		found, wins, top1 bool
	}{
		{"fresh first", []string{"f", "s1", "s2"}, "f", []string{"s1", "s2"}, true, true, true},
		{"fresh beats present stale, not top", []string{"x", "f", "s1"}, "f", []string{"s1", "s2"}, true, true, false},
		{"stale outranks fresh", []string{"s1", "f"}, "f", []string{"s1"}, true, false, false},
		{"fresh missing", []string{"s1", "x"}, "f", []string{"s1"}, false, false, false},
		{"stale absent counts as win", []string{"x", "f"}, "f", []string{"s1"}, true, true, false},
		{"chain: middle version outranks", []string{"v2", "f", "v1"}, "f", []string{"v1", "v2"}, true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			found, wins, top1 := judgeProbe(c.ranked, c.fresh, c.stale)
			if found != c.found || wins != c.wins || top1 != c.top1 {
				t.Errorf("judgeProbe = (%v,%v,%v), want (%v,%v,%v)", found, wins, top1, c.found, c.wins, c.top1)
			}
		})
	}
}

// loadStalenessTestdata loads the committed scenario fixture.
func loadStalenessTestdata(t *testing.T) []StalenessScenario {
	t.Helper()
	f, err := os.Open("testdata/staleness.jsonl")
	if err != nil {
		t.Fatalf("open staleness fixture: %v", err)
	}
	defer f.Close() //nolint:errcheck
	scenarios, err := LoadStalenessScenarios(f)
	if err != nil {
		t.Fatalf("load staleness fixture: %v", err)
	}
	return scenarios
}

// TestStalenessReport runs the suite and logs the report. Per
// docs/benchmarks.md this is REPORT-ONLY in CI: the suite documents that
// search currently has no recency signal, so fresh-wins failures are printed,
// never asserted. Scenarios graduate to enforced assertions as
// supersedes-aware ranking ships. What IS enforced: the fixture loads with a
// healthy scenario count, both probe types are present, the suite runs
// without error, and the fresh version is at least RETRIEVED for every probe
// (pure findability — recency plays no part in it).
func TestStalenessReport(t *testing.T) {
	scenarios := loadStalenessTestdata(t)
	if len(scenarios) < 20 {
		t.Fatalf("fixture has %d scenarios, want >= 20", len(scenarios))
	}

	outcomes, err := RunStaleness(context.Background(), scenarios)
	if err != nil {
		t.Fatalf("RunStaleness: %v", err)
	}
	summaries := SummarizeStaleness(outcomes)
	if len(summaries) < 2 {
		t.Fatalf("want state + premise summaries, got %+v", summaries)
	}
	for _, o := range outcomes {
		if !o.FreshFound {
			t.Errorf("%s/%s: fresh version not retrieved at all (findability regression)", o.Scenario, o.ProbeType)
		}
	}
	t.Logf("staleness suite (report-only):\n%s", FormatStaleness(outcomes))
}
