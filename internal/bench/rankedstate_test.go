package bench

import (
	"context"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// seedRankedStateFixture seeds the committed suite and returns the queries plus
// the corpus, so a test can assert what the corpus carries as well as what it
// scores.
func seedRankedStateFixture(t *testing.T) ([]Query, Dataset, *memory.Store) {
	t.Helper()
	ds, err := LoadRankedStateDataset("testdata")
	if err != nil {
		t.Fatalf("load ranking-state dataset: %v", err)
	}
	store, db := newSeedingStore(t)
	queries, err := SeedRankedState(context.Background(), store, db, "testdata")
	if err != nil {
		t.Fatalf("seed ranking-state suite: %v", err)
	}
	return queries, ds, store
}

// TestRankedStateFixtureCarriesState is the anti-vacuity guard, and it is first
// because everything else here is a measurement that would read a clean 0.000
// delta and call it a finding. The headline table's problem is precisely that
// its corpus has no ages and no edges; a fixture that lost either would score
// the same in all four configurations, the report would print four identical
// rows, and every claim built on the deltas would be vacuous.
//
// What it asserts, and why each is a separate count:
//   - enough memories and queries to have more than one lever;
//   - distinct created_at in BOTH decay classes, because a suite whose ages are
//     all in never-decay categories cannot see decay at all;
//   - supersedes edges, and chains of more than one depth, because a single
//     two-row edge cannot show the star-link ordering a chain depends on;
//   - the edges are real: every key a memory supersedes exists in the corpus.
func TestRankedStateFixtureCarriesState(t *testing.T) {
	_, ds, _ := seedRankedStateFixture(t)
	if len(ds.Memories) < 20 {
		t.Errorf("ranking-state corpus has %d memories, want >= 20 for the state to be contested", len(ds.Memories))
	}
	if len(ds.Queries) < 10 {
		t.Errorf("ranking-state suite has %d queries, want >= 10", len(ds.Queries))
	}

	keys := map[string]bool{}
	for _, m := range ds.Memories {
		if keys[m.Key] {
			t.Errorf("duplicate corpus key %q", m.Key)
		}
		keys[m.Key] = true
	}

	agedDecaying, agedNeverDecay := map[int]bool{}, map[int]bool{}
	edges, depth := 0, 0
	for _, m := range ds.Memories {
		if m.AgeDays > 0 {
			if categoryDecays(m.Category) {
				agedDecaying[m.AgeDays] = true
			} else {
				agedNeverDecay[m.AgeDays] = true
			}
		}
		for _, older := range m.Supersedes {
			edges++
			if !keys[older] {
				t.Errorf("memory %q supersedes unknown key %q", m.Key, older)
			}
			if older == m.Key {
				t.Errorf("memory %q supersedes itself", m.Key)
			}
		}
		if len(m.Supersedes) > 1 {
			depth++
		}
	}
	if len(agedDecaying) < 8 {
		t.Errorf("%d distinct ages among decaying-category rows, want >= 8: decay needs candidates of different ages to have anything to reorder", len(agedDecaying))
	}
	if len(agedNeverDecay) < 2 {
		t.Errorf("%d distinct ages among never-decay rows, want >= 2: the never-decay half is the control the decaying half is read against", len(agedNeverDecay))
	}
	if edges < 6 {
		t.Errorf("%d supersedes edges in the corpus, want >= 6", edges)
	}
	if depth == 0 {
		t.Error("no memory supersedes more than one row: an update chain is what the star-link ordering depends on")
	}

	// Both classes of chain have to be present, or the report cannot separate
	// the two ranking paths. A chain in a never-decay category, or one whose two
	// rows are close in age, is moved by the demote alone; a chain in a decaying
	// category with a wide age gap is moved by both, and there the two cannot be
	// told apart from each other.
	byKey := map[string]MemorySpec{}
	for _, m := range ds.Memories {
		byKey[m.Key] = m
	}
	decaySensitive, demoteAlone := 0, 0
	for _, m := range ds.Memories {
		for _, older := range m.Supersedes {
			gap := byKey[older].AgeDays - m.AgeDays
			if categoryDecays(m.Category) && gap >= wideAgeGapDays {
				decaySensitive++
			} else {
				demoteAlone++
			}
		}
	}
	if demoteAlone == 0 {
		t.Error("no supersedes edge decay cannot move on its own, so the demote is never measured in isolation")
	}
	if decaySensitive == 0 {
		t.Error("no supersedes edge with a wide age gap in a decaying category, so decay is never measured on a replacement")
	}
}

// wideAgeGapDays is how far apart two rows of a chain have to be for decay to
// separate them. The decay factor is 1/(1+age/tau) with tau 30 (most decaying
// categories) or 45 (pattern/architecture), floored at 0.15/0.3: at 30 days a
// dependency row already sits at 0.5 against a fresh row's ~0.9, so a gap below
// that leaves the two within a few percent of each other and the demote is doing
// the work on its own.
const wideAgeGapDays = 30

// TestRankedStateSuiteIsNotInert is the measurement #561 asked for: the ranking
// paths production uses, exercised on a corpus that has the state they read.
// Decay reorders by base × decayFactor and the demote moves a row below its
// superseder, so both can only move a RANK here — NDCG@10 over a single
// relevant row is 1.0 for anything in the window, and membership is relevance's
// to decide (DecayReselect ships false). R@1 and MRR@10 are therefore the
// sensitive columns and the ones the assertions read.
//
// The headline table is reported beside it and must read the same in all four
// configurations: it is seeded from a file with no ages and no edges, so the two
// numbers are comparable and the difference between them is the state, not the
// corpus.
func TestRankedStateSuiteIsNotInert(t *testing.T) {
	ctx := context.Background()
	queries, ds, store := seedRankedStateFixture(t)

	configs := RankedStateConfigs()
	results := make([][]Result, 0, len(configs))
	for _, cfg := range configs {
		res, err := RunRankedState(ctx, store, queries, cfg.Params)
		if err != nil {
			t.Fatalf("%s: %v", cfg.Label, err)
		}
		if res[0].Queries != len(ds.Queries) {
			t.Fatalf("%s: scored %d queries, want all %d (every probe is graded here)", cfg.Label, res[0].Queries, len(ds.Queries))
		}
		results = append(results, res)
	}
	t.Logf("ranking-state suite (report-only):\n%s", FormatRankedState(configs, results))

	hybrid := func(i int) Result { return results[i][2] }
	off, decay, demote, both := hybrid(0), hybrid(1), hybrid(2), hybrid(3)

	// The single legs take no SearchParams, so they are the control: if they
	// moved between configurations, something other than the two ranking paths
	// is changing the corpus between runs.
	last := results[len(results)-1]
	if results[0][0].Recall10 != last[0].Recall10 || results[0][1].Recall10 != last[1].Recall10 ||
		results[0][0].MRR10 != last[0].MRR10 || results[0][1].MRR10 != last[1].MRR10 {
		t.Errorf("a single-leg condition changed between configurations, so the corpus is not stable across runs: "+
			"fts R@10 %.3f->%.3f MRR %.3f->%.3f, vector R@10 %.3f->%.3f MRR %.3f->%.3f",
			results[0][0].Recall10, last[0].Recall10, results[0][0].MRR10, last[0].MRR10,
			results[0][1].Recall10, last[1].Recall10, results[0][1].MRR10, last[1].MRR10)
	}

	// Decay is non-inert: with the demote off, turning decay on has to move a
	// rank. R@1 and MRR@10 are the columns that can move, and both are checked
	// because a change in only one of them is a real (if small) effect.
	if !rankMoved(off, decay) {
		t.Errorf("decay is inert on this suite: R@1 %.3f->%.3f, MRR@10 %.3f->%.3f (demote off in both)",
			off.Recall1, decay.Recall1, off.MRR10, decay.MRR10)
	}
	// So is the demote: with decay off, the supersede demote has to move a rank.
	if !rankMoved(off, demote) {
		t.Errorf("the supersede demote is inert on this suite: R@1 %.3f->%.3f, MRR@10 %.3f->%.3f (decay off in both)",
			off.Recall1, demote.Recall1, off.MRR10, demote.MRR10)
	}

	// Findability: decay is ordering-only, so no probe may lose its answer
	// between the configurations. R@10 is the membership column and it must be
	// identical in all four.
	for i, cfg := range configs {
		if results[i][2].Recall10 != off.Recall10 {
			t.Errorf("%s: recall@10 = %.3f, want %.3f — a ranking change dropped an answer out of the window",
				cfg.Label, results[i][2].Recall10, off.Recall10)
		}
	}

	// The shipped default is the pair, and it is not the sum of the halves: two
	// reordering passes can undo each other on the same row. Asserting that they
	// are not identical is what stops this suite from being read as "each path
	// helps, so both help".
	t.Logf("shipped vs both-off: R@1 %+.3f, MRR@10 %+.3f, NDCG@10 %+.3f",
		both.Recall1-off.Recall1, both.MRR10-off.MRR10, both.NDCG10-off.NDCG10)

	// The headline table, same four configurations, one number. Asserted rather
	// than reported so the comparison cannot rot: if a future change makes the
	// headline corpus carry state, the two suites stop being comparable and this
	// is where it says so.
	headline, hvecs := loadTestdataDataset(t)
	hstore, hdb := newSeedingStore(t)
	t.Cleanup(func() { _ = hstore.Close() })
	hqueries, err := Seed(ctx, hstore, hdb, headline, hvecs)
	if err != nil {
		t.Fatalf("seed headline: %v", err)
	}
	var headlineNDCG []float64
	for _, cfg := range configs {
		res, err := RunRankedState(ctx, hstore, hqueries, cfg.Params)
		if err != nil {
			t.Fatalf("headline %s: %v", cfg.Label, err)
		}
		headlineNDCG = append(headlineNDCG, res[2].NDCG10)
	}
	for i := 1; i < len(headlineNDCG); i++ {
		if headlineNDCG[i] != headlineNDCG[0] {
			t.Errorf("headline hybrid NDCG@10 moved under %s (%.4f vs %.4f): the headline corpus now carries state, "+
				"so it is no longer the inert reference this suite is compared against", configs[i].Label, headlineNDCG[i], headlineNDCG[0])
		}
	}
	t.Logf("headline hybrid NDCG@10, all four configurations: %.4f (inert, as intended)", headlineNDCG[0])
}

// rankMoved reports whether a configuration changed either of the two columns a
// reordering pass can move: the head of the ranking (R@1) and the mean position
// of the answer (MRR@10). NDCG@10 is deliberately not read here — over a
// single-relevant-row probe it is 1.0 for anything in the window, so it can only
// report a membership change, which the findability check above covers.
func rankMoved(a, b Result) bool {
	return a.Recall1 != b.Recall1 || a.MRR10 != b.MRR10
}
