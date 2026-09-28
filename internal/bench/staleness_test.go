package bench

import (
	"context"
	"math"
	"os"
	"strconv"
	"strings"
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

// TestStalenessReportNamesFreshAt1 is the headline the docs publish, and it used
// to be one number (#561). "Fresh-wins 1.000" says the newest version outranks
// every older one — somewhere in the window. It does not say the caller is shown
// it first, and on this fixture the two are 1.000 and 0.583/0.458: an agent that
// reads the top result of a "what is X now" query gets the stale answer on
// roughly half the probes while every one of them is scored a win. Publishing
// only the first is the part of the report that overstates what it proves.
//
// So the report carries an all-probes row with all three columns, and the test
// pins that the aggregate is the arithmetic of the per-type rows rather than a
// number someone typed: a headline that cannot be derived from the rows above it
// is a headline nobody can check.
func TestStalenessReportNamesFreshAt1(t *testing.T) {
	scenarios := loadStalenessTestdata(t)
	outcomes, err := RunStaleness(context.Background(), scenarios, memory.DefaultSearchParams(), false)
	if err != nil {
		t.Fatalf("RunStaleness: %v", err)
	}
	report := FormatStaleness(outcomes)
	t.Logf("%s", report)

	if !strings.Contains(report, "fresh@1") {
		t.Fatalf("the staleness report has no fresh@1 column:\n%s", report)
	}
	perType := SummarizeStaleness(outcomes)
	if len(perType) < 2 {
		t.Fatalf("want a state and a premise row, got %+v", perType)
	}
	all := stalenessAll(outcomes)
	if all.Probes != len(outcomes) {
		t.Errorf("all-probes row counts %d probes, the run produced %d", all.Probes, len(outcomes))
	}
	// The aggregate is the sum of the rows, not a separate judgement.
	var probes, found, wins, top1 int
	for _, s := range perType {
		probes += s.Probes
		found += s.FreshFound
		wins += s.FreshWins
		top1 += s.FreshTop1
	}
	if all.Probes != probes || all.FreshFound != found || all.FreshWins != wins || all.FreshTop1 != top1 {
		t.Errorf("all-probes row %+v does not add up to the per-type rows (probes %d found %d wins %d top1 %d)",
			all, probes, found, wins, top1)
	}
	// fresh@1 is a real gap here, which is the whole reason the column exists:
	// asserting the gap would freeze a number, so the assertion is that the
	// column is PRESENT and that the two ratios are reported independently.
	if all.FreshWins <= 0 {
		t.Errorf("fresh-wins is %d of %d, so the fixture is not measuring what the report claims", all.FreshWins, all.Probes)
	}
	// The all-probes row has to be RENDERED, and "all" appears in plenty of other
	// places in this report (a scenario name, a failing-probe line), so the check
	// is on the row itself. Its numbers are compared against the aggregate rather
	// than merely searched for: a report that prints one thing and tests another
	// is the drift this whole change is about.
	row, ok := stalenessRow(report, "all")
	if !ok {
		t.Fatalf("report has no all-probes row:\n%s", report)
	}
	n := float64(all.Probes)
	for _, c := range []struct {
		col     string
		wantSum int
	}{
		{"fresh-found", all.FreshFound},
		{"fresh-wins", all.FreshWins},
		{"fresh@1", all.FreshTop1},
	} {
		if want := float64(c.wantSum) / n; math.Abs(row[c.col]-want) > 0.0005 {
			t.Errorf("all row's %s is %.3f, want %.3f (%d of %d probes)",
				c.col, row[c.col], want, c.wantSum, all.Probes)
		}
	}
	if int(row["n"]) != all.Probes {
		t.Errorf("all row's n is %d, want %d", int(row["n"]), all.Probes)
	}
}

// stalenessRow parses one row of the staleness table by its leading label and
// returns its numeric columns. It parses rather than searches for a substring
// because a table is a table: the test that guards it should not pass on a
// scenario name that happens to contain the label.
func stalenessRow(report, label string) (map[string]float64, bool) {
	for _, line := range strings.Split(report, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 || fields[0] != label {
			continue
		}
		out := map[string]float64{}
		for i, col := range []string{"n", "fresh-found", "fresh-wins", "fresh@1"} {
			v, err := strconv.ParseFloat(fields[i+1], 64)
			if err != nil {
				return nil, false
			}
			out[col] = v
		}
		return out, true
	}
	return nil, false
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
//
// The supersede demote is inert on this corpus for the complementary reason,
// and that half is asserted structurally rather than by a fourth search pass:
// the corpus declares no `supersedes` edge at all, and the demote is a hard no-op
// unless an edge joins two rows inside one window. A measurement here would have
// cost a whole pass over 551 memories to re-derive a fact about the fixture, and
// this package's test binary has been within a couple of hundred seconds of Go's
// 600s per-binary default — #677's four rows took it over, and it came back only
// because five tests were made concurrent (see the budget note in sweep_test.go
// for the measured figures and the CI job that failed at 600.038s). The
// ranking-state suite is where the demote is measured against edges that exist.
func TestDecayDoesNotPerturbGradedBench(t *testing.T) {
	// Parallel: this test seeds and searches the immutable headline corpus and only
	// reads it, and at 60-130s under -race it is one of the five that decide whether
	// this package fits Go's 600s per-binary default. The corpus grew by four rows in
	// #677 and took it over. There is no shared state to order against — the
	// package-level values are embedded bytes and one constant floor slice, and no
	// bench test sets an env var or the default logger — so the only thing running
	// these together buys is the overlap. Measured, in the commit that added this.
	t.Parallel()
	ds, vecs := loadTestdataDataset(t)
	ctx := context.Background()

	for _, m := range ds.Memories {
		if len(m.Supersedes) > 0 {
			t.Errorf("headline memory %q declares a supersedes edge on %v, so the headline corpus is no longer inert "+
				"to the demote and the ranking-state suite's comparison against it is no longer like-for-like", m.Key, m.Supersedes)
		}
		if m.AgeDays > 0 {
			t.Errorf("headline memory %q declares age_days=%d, so the headline corpus is no longer inert to decay", m.Key, m.AgeDays)
		}
	}

	store, db := newBenchStoreWithDB(t)
	queries, err := Seed(ctx, store, db, ds, vecs)
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
//
// The trap column is the never-decay half of the fixture: "the demote does not
// touch the trap" is a claim about scenarios decay cannot reorder either, so
// mixing in the decaying ones (#561) would attribute their movement to demote.
func TestSupersedeDemoteClearsFrontier(t *testing.T) {
	stale := loadStalenessTestdata(t)
	traps := neverDecayScenarios(loadTrapTestdata(t))
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

	// The other direction, which is what makes "the demote is a no-op on a corpus
	// with no edges" a measurement rather than a claim about demoteSuperseded's
	// early return: the SAME suite with the demote switched OFF. Without this,
	// "untouched" is only established against another demote-on configuration, so
	// a demote that reordered everything identically under both would pass.
	demoteOff := memory.DefaultSearchParams()
	demoteOff.SupersedeDemote = false
	offTrap, err := RunRecencyTrap(ctx, traps, demoteOff)
	if err != nil {
		t.Fatalf("trap demote-off: %v", err)
	}
	offTW := TrapCorrectWins(offTrap)

	t.Logf("supersede demote: staleness fresh-wins=%.3f, trap correct-wins=%.3f (default trap=%.3f, demote-off trap=%.3f)",
		sw, tw, baseTW, offTW)
	if sw < 0.9 {
		t.Errorf("supersede demote should flip staleness to near-1.0, got %.3f", sw)
	}
	if tw != baseTW {
		t.Errorf("supersede demote must not touch the trap (no supersession pairs there): %.3f vs default %.3f", tw, baseTW)
	}
	if tw != offTW {
		t.Errorf("with no supersedes edge anywhere in the suite, the demote changed the trap score: on %.3f, off %.3f — "+
			"so the frontier's 'the demote does not touch the trap' claim is not about the demote being inert",
			tw, offTW)
	}
}
