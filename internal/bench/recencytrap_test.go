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

// decayingScenarios is the complement: the scenarios the shipped decay factor can
// reorder, which is the half that can MOVE under a parameter that changes the
// window the decay reorders.
func decayingScenarios(scenarios []TrapScenario) []TrapScenario {
	var out []TrapScenario
	for _, sc := range scenarios {
		if categoryDecays(sc.effectiveCategory(), sc.Correct.Content) {
			out = append(out, sc)
		}
	}
	return out
}

// neverDecayScenarios is the never-decay half of the trap fixture: the
// scenarios whose category the shipped decay factor never penalises. Every
// claim that decay leaves the trap score untouched is a claim about THESE
// scenarios, so they are selected by the shipped factor rather than by a name in
// this file — a scenario added to a decaying category does not silently become
// part of the invariance, and one moved out of `fact` does not either.
func neverDecayScenarios(scenarios []TrapScenario) []TrapScenario {
	var out []TrapScenario
	for _, sc := range scenarios {
		if !categoryDecays(sc.effectiveCategory(), sc.Correct.Content) {
			out = append(out, sc)
		}
	}
	return out
}

// TestRecencyTrapAtDefault: at the production default (decay on), the correct
// old memory wins its never-decay trap scenarios — the FTS ranking already
// favors the direct keyword match, and because those scenarios are `fact`
// category, decay cannot demote them. This is the invariant that makes
// category-aware decay safe as a default, and it is scoped to the never-decay
// half on purpose: the fixture also carries decaying scenarios (#561), and an
// old memory there losing to a fresh distractor is decay working, not a
// regression.
func TestRecencyTrapAtDefault(t *testing.T) {
	scenarios := neverDecayScenarios(loadTrapTestdata(t))
	if len(scenarios) < 12 {
		t.Fatalf("never-decay trap fixture has %d scenarios, want >= 12", len(scenarios))
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
	t.Logf("trap correct-wins at default (never-decay scenarios only): %.3f", cw)
	if cw < 0.9 {
		t.Errorf("at default the correct old memory should win nearly always (a timeless fact never decays), got %.3f", cw)
	}
}

// TestDecayFrontier reports the decay-on/off tradeoff over both suites. The
// staleness suite (dependency category) wants fresh-wins HIGH; the
// recency-trap suite (fact category, never-decay) wants correct-wins HIGH.
// Category-aware decay should help staleness WITHOUT hurting the trap suite —
// the free lunch a blanket age-only recency prior could not achieve. The test
// prints the frontier and asserts both properties.
//
// The trap column is the never-decay half of the fixture, which is the half this
// claim is about: the suite also carries decaying scenarios since #561, and
// their score moves under decay by design. Reading the frontier off the pooled
// number would put that movement in the wrong column.
func TestDecayFrontier(t *testing.T) {
	// Parallel: opens its own stores and reads no process-global state, so it
	// overlaps the corpus-wide tests instead of running ahead of them.
	t.Parallel()
	stale := loadStalenessTestdata(t)
	traps := neverDecayScenarios(loadTrapTestdata(t))
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
		t.Errorf("expected trap correct-wins to stay flat under decay (a timeless fact never decays): off %.3f on %.3f",
			rows[0].trapWins, rows[1].trapWins)
	}
}

// TestRecencyTrapDatedFactIsItsOwnRow holds the dated-fact half of the fixture:
// a fact whose content carries a calendar date decays, so it must be counted in
// the decaying class under its own row, apart from the undated facts decay never
// touches. Pooled under one `fact` row the two classes would read as one number.
func TestRecencyTrapDatedFactIsItsOwnRow(t *testing.T) {
	scenarios := loadTrapTestdata(t)
	dated := 0
	for _, sc := range scenarios {
		if sc.effectiveCategory() == "fact" && memory.ContentCarriesDate(sc.Correct.Content) {
			dated++
		}
	}
	if dated == 0 {
		t.Fatal("no dated-fact trap scenario: the dated half of the fact rule is unmeasured")
	}
	outcomes, err := RunRecencyTrap(context.Background(), scenarios, memory.DefaultSearchParams())
	if err != nil {
		t.Fatalf("RunRecencyTrap: %v", err)
	}
	rows := map[string]TrapSummary{}
	for _, s := range SummarizeTrap(outcomes) {
		rows[s.Category] = s
	}
	if r, ok := rows["fact (dated)"]; !ok || !r.Decays || r.Probes < dated {
		t.Errorf("dated-fact row = %+v (present %v), want a decaying row with >= %d probes", r, ok, dated)
	}
	if r, ok := rows["fact"]; !ok || r.Decays {
		t.Errorf("undated fact row = %+v (present %v), want a never-decay row", r, ok)
	}
}
