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

// neverDecayScenarios is the never-decay half of the trap fixture: the
// scenarios whose category the shipped decay factor never penalises. Every
// claim that decay leaves the trap score untouched is a claim about THESE
// scenarios, so they are selected by the shipped factor rather than by a name in
// this file — a scenario added to a decaying category does not silently become
// part of the invariance, and one moved out of `fact` does not either.
func neverDecayScenarios(scenarios []TrapScenario) []TrapScenario {
	var out []TrapScenario
	for _, sc := range scenarios {
		if !categoryDecays(sc.effectiveCategory()) {
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
// regression. TestRecencyTrapDecayingCategories reports that half.
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
		t.Errorf("at default the correct old memory should win nearly always (fact never decays), got %.3f", cw)
	}
}

// trapMustWinScenarios are the decaying-category scenarios whose correct memory
// is PINNED, so its decay factor is 1.0 at any age. Each still faces a fresh
// UNPINNED distractor in the same category, and each has to win anyway: pinning
// is the only control a user has over a category that decays, so if the ranker
// multiplied a pinned row down the control would be decorative.
//
// Named rather than shape-detected because the shape is a fixture field, and a
// fixture that classifies its own rows is one that can silently reclassify them
// when a line is edited. TestRecencyTrapDecayingCategories fails if this list is
// empty, if a name is missing from the fixture, or if a named scenario is not
// pinned.
var trapMustWinScenarios = []string{
	"vault_one_way",
	"link_graph_use",
	"dry_run_default",
	"pool_deadlock",
	"sqlite_driver",
}

// trapWinsByScenario keys each scenario's correct-wins fraction by name, which
// is 0 or 1 today (one probe per scenario) but is read as a fraction so a
// multi-probe scenario does not change the comparison's meaning.
func trapWinsByScenario(outcomes []TrapOutcome) map[string]float64 {
	sum, n := map[string]float64{}, map[string]int{}
	for _, o := range outcomes {
		if o.CorrectWins {
			sum[o.Scenario]++
		}
		n[o.Scenario]++
	}
	out := make(map[string]float64, len(sum))
	for name, c := range n {
		out[name] = sum[name] / float64(c)
	}
	return out
}

// trapTop1ByScenario reports which scenarios put the old correct memory first.
func trapTop1ByScenario(outcomes []TrapOutcome) map[string]bool {
	out := map[string]bool{}
	for _, o := range outcomes {
		if o.CorrectTop1 {
			out[o.Scenario] = true
		}
	}
	return out
}

// decayingCount is the number of fixture scenarios in a category the shipped
// decay factor penalises.
func decayingCount(scenarios []TrapScenario) int {
	n := 0
	for _, sc := range scenarios {
		if categoryDecays(sc.effectiveCategory()) {
			n++
		}
	}
	return n
}

// neverDecayingCount is its complement: the scenarios decay cannot touch at all.
func neverDecayingCount(scenarios []TrapScenario) int {
	return len(scenarios) - decayingCount(scenarios)
}

// TestRecencyTrapDecayingCategories is the measurement the fact-only fixture
// could not make (#561): the trap score split by whether the scenario's
// category is one the shipped decay factor ever penalises, reported with decay
// off and on. The fact scenarios are the never-decay half and MUST be
// bit-identical across the two — that invariance is what makes category-aware
// decay safe as a default — while the decaying half is where the same
// invariance would be a MISS: an old-but-correct memory competing against a
// fresh distractor in a category decay actually reorders.
func TestRecencyTrapDecayingCategories(t *testing.T) {
	scenarios := loadTrapTestdata(t)

	// The five decaying categories the audit named. A fixture that quietly lost
	// one of them would still parse and still pass the score assertions below,
	// so their presence is asserted rather than assumed.
	byCategory := map[string]int{}
	for _, sc := range scenarios {
		byCategory[sc.effectiveCategory()]++
	}
	for _, cat := range []string{"decision", "gotcha", "dependency", "architecture", "pattern"} {
		if !categoryDecays(cat) {
			t.Fatalf("fixture category %q no longer decays; the report's never-decay/decaying split is stale", cat)
		}
		if byCategory[cat] == 0 {
			t.Errorf("no trap scenario in the decaying category %q", cat)
		}
	}
	if decayingCount(scenarios) == 0 {
		t.Error("every trap scenario is in a never-decay category: decay is inert for this suite again")
	}
	if neverDecayingCount(scenarios) == 0 {
		t.Error("no trap scenario in a never-decay category: the frontier's fact half is gone")
	}

	ctx := context.Background()
	off := memory.DefaultSearchParams()
	off.DecayEnabled = false
	on := memory.DefaultSearchParams()

	offOutcomes, err := RunRecencyTrap(ctx, scenarios, off)
	if err != nil {
		t.Fatalf("decay off: %v", err)
	}
	onOutcomes, err := RunRecencyTrap(ctx, scenarios, on)
	if err != nil {
		t.Fatalf("decay on: %v", err)
	}
	t.Logf("recency trap by category (report-only):\n%s", FormatTrap(offOutcomes, onOutcomes))

	offByName := trapWinsByScenario(offOutcomes)
	onByName := trapWinsByScenario(onOutcomes)
	onTop1 := trapTop1ByScenario(onOutcomes)

	// A PINNED old-but-correct memory in a category decay reorders must still
	// take the top slot against a fresh unpinned distractor in the same
	// category: pinning is the user's control over that category, so it is
	// load-bearing rather than decorative.
	pinned := map[string]bool{}
	present := map[string]bool{}
	for _, sc := range scenarios {
		present[sc.Name] = true
		pinned[sc.Name] = sc.Pinned
	}
	for _, name := range trapMustWinScenarios {
		if !present[name] {
			t.Errorf("must-win trap scenario %q is not in the fixture", name)
			continue
		}
		if !pinned[name] {
			t.Errorf("must-win trap scenario %q is not pinned: the assertion would be about decay, not about the pin", name)
		}
		if !onTop1[name] {
			t.Errorf("%s: PINNED old-but-correct memory in a decaying category is not top-1 under decay "+
				"(decay-off wins %.3f, decay-on wins %.3f)", name, offByName[name], onByName[name])
		}
	}

	// Findability everywhere: decay reorders the window it is given, so it may
	// cost a rank and never the answer's presence. Asserted over the WHOLE
	// fixture, decaying half included, because that is the guarantee production
	// makes about the categories it reorders.
	for _, o := range onOutcomes {
		if !o.CorrectFound {
			t.Errorf("%s (%s): correct old memory not retrieved at all under decay (findability)", o.Scenario, o.Category)
		}
	}

	// The never-decay half is invariant by construction, and that is the whole
	// argument for defaulting category-aware decay: assert it exactly, so a
	// category added to the trap fixture on the never-decay side cannot quietly
	// become a decaying one.
	for _, sc := range scenarios {
		if categoryDecays(sc.effectiveCategory()) {
			continue
		}
		if onByName[sc.Name] != offByName[sc.Name] {
			t.Errorf("%s (%s never decays): decay changed the trap verdict, off=%.3f on=%.3f",
				sc.Name, sc.effectiveCategory(), offByName[sc.Name], onByName[sc.Name])
		}
	}

	// Non-inertness, stated as a bound rather than left to the log: at least
	// one decaying scenario must change verdict between decay-off and decay-on.
	// A suite where none does cannot tell a working decay from a no-op one, and
	// that is the failure #561 is about.
	changed := 0
	for _, sc := range scenarios {
		if !categoryDecays(sc.effectiveCategory()) {
			continue
		}
		if onByName[sc.Name] != offByName[sc.Name] {
			changed++
		}
	}
	if changed == 0 {
		t.Errorf("no decaying-category trap scenario changed under decay: the suite measures nothing about it "+
			"(%d decaying scenarios)", decayingCount(scenarios))
	}
	t.Logf("decaying scenarios whose verdict decay moved: %d of %d", changed, decayingCount(scenarios))
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
		t.Errorf("expected trap correct-wins to stay flat under decay (fact never decays): off %.3f on %.3f",
			rows[0].trapWins, rows[1].trapWins)
	}
}
