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
func TestDecayReselectProbe(t *testing.T) {
	if os.Getenv("GHOST_BENCH_PROBE") == "" {
		t.Skip("set GHOST_BENCH_PROBE=1 to run the decay-reselect probe")
	}
	ctx := context.Background()
	stale := loadStalenessTestdata(t)
	traps := loadTrapTestdata(t)

	ds, vecs := loadTestdataDataset(t)
	store := newBenchStore(t)
	queries, err := Seed(ctx, store, ds, vecs)
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

		to, err := RunRecencyTrap(ctx, traps, p)
		if err != nil {
			t.Fatalf("trap: %v", err)
		}
		t.Logf("trap correct-wins=%.3f", TrapCorrectWins(to))
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
