package bench

import (
	"context"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
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

// TestStalenessReport runs the suite and logs the report. What IS enforced:
// the fixture loads with a healthy scenario count, both probe types are
// present, the suite runs without error, and the fresh version is at least
// RETRIEVED for every probe under the production default (decay on) — decay
// only reorders the window, never dropping a relevant memory (findability).
func TestStalenessReport(t *testing.T) {
	scenarios := loadStalenessTestdata(t)
	if len(scenarios) < 20 {
		t.Fatalf("fixture has %d scenarios, want >= 20", len(scenarios))
	}

	// Production defaults (DecayEnabled true) — documents today's behavior.
	outcomes, err := RunStaleness(context.Background(), scenarios, memory.DefaultSearchParams(), false)
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
	t.Logf("staleness suite (report-only, default params):\n%s", FormatStaleness(outcomes))
}

// freshWins reports the fraction of probes where the fresh version outranked
// every superseded sibling.
func freshWins(outcomes []ProbeOutcome) float64 {
	if len(outcomes) == 0 {
		return 0
	}
	wins := 0
	for _, o := range outcomes {
		if o.FreshWins {
			wins++
		}
	}
	return float64(wins) / float64(len(outcomes))
}

// TestStalenessDecayProof proves the decay factor flips the suite: at
// DecayEnabled false fresh-wins is low, and with decay on it rises to a
// majority — the dependency-category fixture makes decay observable.
func TestStalenessDecayProof(t *testing.T) {
	scenarios := loadStalenessTestdata(t)
	ctx := context.Background()

	off := memory.DefaultSearchParams()
	off.DecayEnabled = false
	baseline, err := RunStaleness(ctx, scenarios, off, false)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	baseWins := freshWins(baseline)

	on := memory.DefaultSearchParams() // DecayEnabled true
	tunedOutcomes, err := RunStaleness(ctx, scenarios, on, false)
	if err != nil {
		t.Fatalf("tuned: %v", err)
	}
	tunedWins := freshWins(tunedOutcomes)

	t.Logf("fresh-wins: decay-off=%.3f, decay-on=%.3f", baseWins, tunedWins)
	if tunedWins <= baseWins {
		t.Errorf("decay did not improve fresh-wins: %.3f -> %.3f", baseWins, tunedWins)
	}
	if tunedWins < 0.5 {
		t.Errorf("decay should flip the suite to majority fresh-wins, got %.3f", tunedWins)
	}
	// Every fresh version must still be retrieved regardless of ranking.
	for _, o := range tunedOutcomes {
		if !o.FreshFound {
			t.Errorf("%s/%s: fresh not retrieved under decay", o.Scenario, o.ProbeType)
		}
	}
}

// TestDecayDoesNotPerturbGradedBench: the ghost bench dataset is seeded via
// store.Create, which never sets created_at, so every memory shares
// (effectively) the same timestamp — decay applies an identical factor to
// every candidate and cannot reorder them. Hybrid NDCG@10 and recall must be
// identical with decay on and off. This is why flipping DecayEnabled on in
// production is safe for the graded benchmarks.
func TestDecayDoesNotPerturbGradedBench(t *testing.T) {
	ds, vecs := loadTestdataDataset(t)
	ctx := context.Background()

	store := newBenchStore(t)
	queries, err := Seed(ctx, store, ds, vecs)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	off := memory.DefaultSearchParams()
	off.DecayEnabled = false
	on := memory.DefaultSearchParams()
	pts, err := Sweep(ctx, store, queries, []memory.SearchParams{off, on})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if pts[0].Result.NDCG10 != pts[1].Result.NDCG10 || pts[0].Result.Recall10 != pts[1].Result.Recall10 {
		t.Errorf("decay perturbed graded ranking (uniform timestamps should make it inert): off NDCG=%.4f on NDCG=%.4f",
			pts[0].Result.NDCG10, pts[1].Result.NDCG10)
	}
}

// TestSupersedeDemoteClearsFrontier is the headline result the recency-trap
// experiment set up: the targeted supersede demote flips the staleness suite
// AND leaves the recency-trap suite intact — the free lunch the global recency
// prior could not be. With SupersedeDemote on and ground-truth supersedes links
// seeded, staleness fresh-wins should reach ~1.0; the trap suite (whose
// distractors are NOT supersession pairs, so no links exist) is untouched.
func TestSupersedeDemoteClearsFrontier(t *testing.T) {
	stale := loadStalenessTestdata(t)
	traps := loadTrapTestdata(t)
	ctx := context.Background()

	p := memory.DefaultSearchParams()
	p.SupersedeDemote = true

	// Staleness with demote + seeded supersedes links: should flip to majority.
	so, err := RunStaleness(ctx, stale, p, true)
	if err != nil {
		t.Fatalf("staleness: %v", err)
	}
	sw := freshWins(so)

	// Trap with demote ON but NO supersedes links (distractors aren't
	// supersession): demote is inert, correct-wins must match the default.
	to, err := RunRecencyTrap(ctx, traps, p)
	if err != nil {
		t.Fatalf("trap: %v", err)
	}
	tw := TrapCorrectWins(to)

	baseTrap, err := RunRecencyTrap(ctx, traps, memory.DefaultSearchParams())
	if err != nil {
		t.Fatalf("trap baseline: %v", err)
	}
	baseTW := TrapCorrectWins(baseTrap)

	t.Logf("supersede demote: staleness fresh-wins=%.3f, trap correct-wins=%.3f (default trap=%.3f)", sw, tw, baseTW)
	if sw < 0.9 {
		t.Errorf("supersede demote should flip staleness to near-1.0, got %.3f", sw)
	}
	if tw != baseTW {
		t.Errorf("supersede demote must not touch the trap (no supersession pairs there): %.3f vs default %.3f", tw, baseTW)
	}
}
