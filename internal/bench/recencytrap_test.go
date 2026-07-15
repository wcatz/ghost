package bench

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func loadTrapTestdata(t *testing.T) []TrapScenario {
	t.Helper()
	f, err := os.Open("testdata/recency_trap.jsonl")
	if err != nil {
		t.Fatalf("open trap fixture: %v", err)
	}
	defer f.Close() //nolint:errcheck
	scenarios, err := LoadTrapScenarios(f)
	if err != nil {
		t.Fatalf("load trap fixture: %v", err)
	}
	return scenarios
}

// TestRecencyTrapAtDefault: with recency off (production default), the correct
// old memory wins its trap scenarios — the FTS ranking already favors the
// direct keyword match, and no recency demotes it. This is the invariant the
// recency prior must not break.
func TestRecencyTrapAtDefault(t *testing.T) {
	scenarios := loadTrapTestdata(t)
	if len(scenarios) < 12 {
		t.Fatalf("trap fixture has %d scenarios, want >= 12", len(scenarios))
	}
	outcomes, err := RunRecencyTrap(context.Background(), scenarios, memory.DefaultSearchParams())
	if err != nil {
		t.Fatalf("RunRecencyTrap: %v", err)
	}
	for _, o := range outcomes {
		if !o.CorrectFound {
			t.Errorf("%s: correct memory not retrieved (findability)", o.Scenario)
		}
	}
	cw := TrapCorrectWins(outcomes)
	t.Logf("trap correct-wins at default: %.3f", cw)
	if cw < 0.9 {
		t.Errorf("at default (no recency) the correct old memory should win nearly always, got %.3f", cw)
	}
}

// TestRecencyFrontier is the pivotal experiment: sweep RecencyWeight and report
// staleness fresh-wins vs trap correct-wins together. The staleness suite wants
// fresh-wins HIGH (newer wins); the trap wants correct-wins HIGH (older, correct
// answer wins). A global recency prior trades one for the other. This test
// prints the frontier and asserts only that the tension is real and measured —
// it does NOT pick a default (that decision uses this data). It is the evidence
// for whether a single global weight can serve both, or whether targeted
// supersedes links are required.
func TestRecencyFrontier(t *testing.T) {
	stale := loadStalenessTestdata(t)
	traps := loadTrapTestdata(t)
	ctx := context.Background()

	weights := []float64{0, 0.02, 0.05, 0.1, 0.15, 0.25, 0.5, 1, 2, 4, 8}
	type row struct {
		w                   float64
		freshWins, trapWins float64
		minOfBoth           float64
	}
	var rows []row
	for _, w := range weights {
		p := memory.DefaultSearchParams()
		p.RecencyWeight = w

		so, err := RunStaleness(ctx, stale, p)
		if err != nil {
			t.Fatalf("staleness w=%.2f: %v", w, err)
		}
		to, err := RunRecencyTrap(ctx, traps, p)
		if err != nil {
			t.Fatalf("trap w=%.2f: %v", w, err)
		}
		fw, tw := freshWins(so), TrapCorrectWins(to)
		m := fw
		if tw < m {
			m = tw
		}
		rows = append(rows, row{w, fw, tw, m})
	}

	var b string
	b += fmt.Sprintf("%-8s %-16s %-16s %-10s\n", "recency", "staleness-fresh", "trap-correct", "min(both)")
	best := rows[0]
	for _, r := range rows {
		b += fmt.Sprintf("%-8.2f %-16.3f %-16.3f %-10.3f\n", r.w, r.freshWins, r.trapWins, r.minOfBoth)
		if r.minOfBoth > best.minOfBoth {
			best = r
		}
	}
	t.Logf("recency frontier (staleness wants fresh HIGH, trap wants correct HIGH):\n%s\nbest min(both) = %.3f at w=%.2f", b, best.minOfBoth, best.w)

	// The tension must be real: at some weight the two metrics move oppositely.
	// (If they didn't, a global recency prior would be a free lunch — it isn't.)
	if rows[0].trapWins <= rows[len(rows)-1].trapWins {
		t.Errorf("expected trap correct-wins to DROP as recency rises (the trap): w=0 %.3f vs w=%.2f %.3f",
			rows[0].trapWins, rows[len(rows)-1].w, rows[len(rows)-1].trapWins)
	}
	if rows[len(rows)-1].freshWins <= rows[0].freshWins {
		t.Errorf("expected staleness fresh-wins to RISE as recency rises: w=0 %.3f vs w=%.2f %.3f",
			rows[0].freshWins, rows[len(rows)-1].w, rows[len(rows)-1].freshWins)
	}
}
