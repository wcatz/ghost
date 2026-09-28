package bench

import (
	"context"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestDecayReselectProbe measures the ship gate for DecayReselect: graded
// hybrid NDCG, staleness fresh-wins, and recency-trap correct-wins with the
// flag off (default) vs on. Report-only — the ship decision is manual until
// a default flips. Run with GHOST_BENCH_PROBE=1. (FTS/vector ablations do
// not pass through decayRank, so only hybrid is graded here.)
//
// The trap column is reported for BOTH classes of the fixture, and the split is
// the point rather than tidiness. decayRank multiplies base by
// DecayFactor(category, ...), which is exactly 1.0 for every never-decay
// category — so for those scenarios base*decay == base, the second sort is a
// stable no-op, and taking the top limit*2 by base before re-selecting the top
// limit by base returns the same set in the same order. **The never-decay half
// therefore CANNOT move under this flag whatever the ranker does**: that column
// is arithmetic, not evidence, and reading it as a regression signal reads a
// tautology. It is printed because it is the check that the flag is not silently
// reaching a corpus that cannot express the question.
//
// The decaying half is the half that CAN move, and since #561 it is the half that
// exists for the purpose: an old correct memory whose factor is well below 1.0
// can be rescued by the wider window or lost by it. Its before/after is the
// informative column, so both are printed and labelled.
func TestDecayReselectProbe(t *testing.T) {
	if os.Getenv("GHOST_BENCH_PROBE") == "" {
		t.Skip("set GHOST_BENCH_PROBE=1 to run the decay-reselect probe")
	}
	ctx := context.Background()
	stale := loadStalenessTestdata(t)
	all := loadTrapTestdata(t)
	traps := []struct {
		label string
		set   []TrapScenario
	}{
		{"never-decay", neverDecayScenarios(all)},
		{"decaying", decayingScenarios(all)},
	}

	ds, vecs := loadTestdataDataset(t)
	store, db := newBenchStoreWithDB(t)
	queries, err := Seed(ctx, store, db, ds, vecs)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	run := func(reselect bool) {
		p := memory.DefaultSearchParams()
		p.DecayReselect = reselect
		t.Logf("=== DecayReselect=%v ===", reselect)

		pts, err := Sweep(ctx, store, queries, []memory.SearchParams{p})
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if len(pts) != 1 {
			t.Fatalf("sweep returned %d points, want 1", len(pts))
		}
		t.Logf("graded hybrid NDCG@10=%.3f R@10=%.3f", pts[0].Result.NDCG10, pts[0].Result.Recall10)

		so, err := RunStaleness(ctx, stale, p, false)
		if err != nil {
			t.Fatalf("staleness: %v", err)
		}
		t.Logf("staleness fresh-found=%.3f fresh-wins=%.3f",
			staleFound(so), freshWins(so))

		for _, h := range traps {
			to, err := RunRecencyTrap(ctx, h.set, p)
			if err != nil {
				t.Fatalf("trap %s: %v", h.label, err)
			}
			t.Logf("trap correct-wins (%s, n=%d)=%.3f", h.label, len(h.set), TrapCorrectWins(to))
		}
	}

	run(false)
	run(true)
}

// staleFound reports the fraction of staleness probes where the fresh version
// was retrieved at all.
func staleFound(outcomes []ProbeOutcome) float64 {
	if len(outcomes) == 0 {
		return 0
	}
	n := 0
	for _, o := range outcomes {
		if o.FreshFound {
			n++
		}
	}
	return float64(n) / float64(len(outcomes))
}
