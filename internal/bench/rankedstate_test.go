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
	// Pinned rather than "at least one": the report claims a three-deep chain,
	// and a count that is only checked for non-emptiness lets a fixture edit
	// quietly take the claim away.
	if depth != 1 {
		t.Errorf("%d memories supersede more than one row, want exactly 1 (the three-deep chain the report describes)", depth)
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
// superseder, so both act on RANK rather than on membership (DecayReselect ships
// false, so relevance owns the window cut). Every ranking metric here is
// therefore sensitive — NDCG@10 included, since it discounts by position — and
// the report reads all of them. R@1 and MRR@10 are the columns the assertions
// use because they are the least discounted and so the first to move.
//
// The headline table is reported beside it and must read the same in all four
// configurations: it is seeded from a file with no ages and no edges, so the two
// numbers are comparable and the difference between them is the state, not the
// corpus.
func TestRankedStateSuiteIsNotInert(t *testing.T) {
	ctx := context.Background()
	queries, ds, store := seedRankedStateFixture(t)

	runs := make([]RankedStateRun, 0, len(RankedStateConfigs()))
	for _, cfg := range RankedStateConfigs() {
		res, err := RunRankedState(ctx, store, queries, cfg.Params)
		if err != nil {
			t.Fatalf("%s: %v", cfg.Label, err)
		}
		if res[0].Queries != len(ds.Queries) {
			t.Fatalf("%s: scored %d queries, want all %d (every probe is graded here)", cfg.Label, res[0].Queries, len(ds.Queries))
		}
		answers, err := AnswerRanks(ctx, store, queries, cfg.Params)
		if err != nil {
			t.Fatalf("%s answer ranks: %v", cfg.Label, err)
		}
		runs = append(runs, RankedStateRun{Config: cfg, Result: res, Answers: answers})
	}
	t.Logf("ranking-state suite (report-only):\n%s", FormatRankedState(runs))

	hybrid := func(i int) Result { return runs[i].Result[2] }
	off, decay, demote := hybrid(0), hybrid(1), hybrid(2)

	// The single legs take no SearchParams, so they are the control: if they
	// moved between configurations, something other than the two ranking paths
	// is changing the corpus between runs.
	last := runs[len(runs)-1].Result
	if runs[0].Result[0].Recall10 != last[0].Recall10 || runs[0].Result[1].Recall10 != last[1].Recall10 ||
		runs[0].Result[0].MRR10 != last[0].MRR10 || runs[0].Result[1].MRR10 != last[1].MRR10 {
		t.Errorf("a single-leg condition changed between configurations, so the corpus is not stable across runs: "+
			"fts R@10 %.3f->%.3f MRR %.3f->%.3f, vector R@10 %.3f->%.3f MRR %.3f->%.3f",
			runs[0].Result[0].Recall10, last[0].Recall10, runs[0].Result[0].MRR10, last[0].MRR10,
			runs[0].Result[1].Recall10, last[1].Recall10, runs[0].Result[1].MRR10, last[1].MRR10)
	}

	// Decay is non-inert: with the demote off, turning decay on has to move a
	// rank.
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
	for _, run := range runs {
		if run.Result[2].Recall10 != off.Recall10 {
			t.Errorf("%s: recall@10 = %.3f, want %.3f — a ranking change dropped an answer out of the window",
				run.Config.Label, run.Result[2].Recall10, off.Recall10)
		}
	}

	// The interaction the aggregate table cannot express: the shipped pair is
	// not the sum of the halves, and it is worth naming the counts rather than
	// only the ratio, because the answer is "the ones whose answer is old" and a
	// reader can only act on that form of the finding.
	shipped := ranksByProbe(runs[3])
	lostByDecay := probesDemoted(runs[0], runs[1])
	lostByDemoteAlone := probesDemoted(runs[0], runs[2])
	lostByPair := probesDemoted(runs[2], runs[3])
	t.Logf("probes whose answer lost the top slot: decay alone (vs both off) %d, demote alone (vs both off) %d, "+
		"shipped pair (vs demote alone) %d — the rows decay pushes down are the rows the demote promotes, so it recovers none of them",
		len(lostByDecay), len(lostByDemoteAlone), len(lostByPair))
	if len(shipped) != len(ds.Queries) {
		t.Errorf("answer ranks cover %d probes, the suite has %d", len(shipped), len(ds.Queries))
	}
	if len(lostByDecay) == 0 {
		t.Error("decay alone demoted no probe relative to both off, so the four configurations are indistinguishable")
	}
	if len(lostByPair) == 0 {
		t.Error("the shipped configuration lost no probe relative to the demote alone, so decay and the demote do not interact on this corpus")
	}

	// The headline table, same four configurations, one number. The assertion is
	// that the four are EQUAL — that the headline corpus is still the inert
	// reference this suite is compared against. It is deliberately not an
	// assertion that they equal 0.818: pinning the value here would duplicate
	// TestBenchRegressionFloors' floors, and the claim this suite needs is the
	// invariance, not the level.
	headline, hvecs := loadTestdataDataset(t)
	hstore, hdb := newSeedingStore(t)
	t.Cleanup(func() { _ = hstore.Close() })
	hqueries, err := Seed(ctx, hstore, hdb, headline, hvecs)
	if err != nil {
		t.Fatalf("seed headline: %v", err)
	}
	var headlineNDCG []float64
	for _, run := range runs {
		res, err := RunRankedState(ctx, hstore, hqueries, run.Config.Params)
		if err != nil {
			t.Fatalf("headline %s: %v", run.Config.Label, err)
		}
		headlineNDCG = append(headlineNDCG, res[2].NDCG10)
	}
	for i := 1; i < len(headlineNDCG); i++ {
		if headlineNDCG[i] != headlineNDCG[0] {
			t.Errorf("headline hybrid NDCG@10 moved under %s (%.4f vs %.4f): the headline corpus now carries state, "+
				"so it is no longer the inert reference this suite is compared against", runs[i].Config.Label, headlineNDCG[i], headlineNDCG[0])
		}
	}
	t.Logf("headline hybrid NDCG@10, all four configurations: %.4f (inert, as intended)", headlineNDCG[0])
}

// rankMoved reports whether a configuration changed either of the two columns the
// suite's assertions read: the head of the ranking (R@1) and the mean position of
// the answer (MRR@10). Both are discounted by position; R@1 is the least
// discounted and moves first, which is why these two rather than NDCG@10, whose
// change is a consequence of the same reorder seen at a different rate.
func rankMoved(a, b Result) bool {
	return a.Recall1 != b.Recall1 || a.MRR10 != b.MRR10
}

// ranksByProbe indexes one configuration's per-probe answer ranks.
func ranksByProbe(run RankedStateRun) map[string]int {
	out := make(map[string]int, len(run.Answers))
	for _, a := range run.Answers {
		out[a.Probe] = a.Rank
	}
	return out
}

// probesDemoted names the probes whose answer rank got WORSE going from one
// configuration to the next. An answer that was first and is no longer first is
// the shape the decay cost takes here, so that is the comparison.
func probesDemoted(from, to RankedStateRun) map[string]bool {
	before := ranksByProbe(from)
	out := map[string]bool{}
	for _, a := range to.Answers {
		if a.Rank > before[a.Probe] {
			out[a.Probe] = true
		}
	}
	return out
}
