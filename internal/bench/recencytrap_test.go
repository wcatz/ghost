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

// TestRecencyTrapAtDefault: at the production default (decay on), the correct
// old memory wins its trap scenarios — the FTS ranking already favors the
// direct keyword match, and because the trap fixtures are `fact` category
// (never-decay), decay does not demote them. This is the invariant that makes
// category-aware decay safe as a default.
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
		t.Errorf("at default the correct old memory should win nearly always (fact never decays), got %.3f", cw)
	}
}

// TestDecayFrontier reports the decay-on/off tradeoff over both suites. The
// staleness suite (dependency category) wants fresh-wins HIGH; the
// recency-trap suite (fact category, never-decay) wants correct-wins HIGH.
// Category-aware decay should help staleness WITHOUT hurting the trap suite —
// the free lunch a blanket age-only recency prior could not achieve. The test
// prints the frontier and asserts both properties.
func TestDecayFrontier(t *testing.T) {
	stale := loadStalenessTestdata(t)
	traps := loadTrapTestdata(t)
	ctx := context.Background()

	type row struct {
		label               string
		freshWins, trapWins float64
	}
	var rows []row
	for _, on := range []bool{false, true} {
		p := memory.DefaultSearchParams()
		p.DecayEnabled = on

		so, err := RunStaleness(ctx, stale, p, false)
		if err != nil {
			t.Fatalf("staleness decay=%v: %v", on, err)
		}
		to, err := RunRecencyTrap(ctx, traps, p)
		if err != nil {
			t.Fatalf("trap decay=%v: %v", on, err)
		}
		rows = append(rows, row{
			label:     map[bool]string{false: "decay-off", true: "decay-on"}[on],
			freshWins: freshWins(so),
			trapWins:  TrapCorrectWins(to),
		})
	}

	var b string
	b += fmt.Sprintf("%-12s %-16s %-16s\n", "mode", "staleness-fresh", "trap-correct")
	for _, r := range rows {
		b += fmt.Sprintf("%-12s %-16.3f %-16.3f\n", r.label, r.freshWins, r.trapWins)
	}
	t.Logf("decay frontier:\n%s", b)

	// Decay must help staleness...
	if rows[1].freshWins <= rows[0].freshWins {
		t.Errorf("expected staleness fresh-wins to RISE with decay: off %.3f on %.3f",
			rows[0].freshWins, rows[1].freshWins)
	}
	// ...and must NOT hurt old-but-correct facts (trap suite is fact category,
	// which never decays — its correct-wins should stay flat).
	if rows[1].trapWins < rows[0].trapWins-0.02 {
		t.Errorf("expected trap correct-wins to stay flat under decay (fact never decays): off %.3f on %.3f",
			rows[0].trapWins, rows[1].trapWins)
	}
}
