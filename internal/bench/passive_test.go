package bench

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

// passiveGoldenPath is the committed report. It pins the numbers on the tree it
// was generated from, and it is the reason the honesty check is REPORTED rather
// than asserted: on a tree where #897 (PR #912) has not landed the header counts
// withheld rows as ranked out, the report says FAIL, and the golden says FAIL with
// it. When #912 merges, this file changes in exactly that block and nowhere else,
// which is the diff that proves the fix.
//
// Regenerate with GHOST_UPDATE_GOLDEN=1 go test ./internal/bench -run TestPassiveBaseline.
const passiveGoldenPath = "testdata/passive_report.golden"

// passiveEnv seeds a store and returns the corpus and the environment the surfaces
// read. The environment is pinned first: the session-start path resolves a default
// project from the user's config only when a directory matches nothing, which no
// read here does, but a bench that reads whatever the developer has configured is
// a bench whose golden is a property of a machine.
func passiveEnv(t *testing.T, blind PassiveBlind) (PassiveCorpus, *PassiveEnv) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c, err := NewPassiveCorpus()
	if err != nil {
		t.Fatalf("NewPassiveCorpus: %v", err)
	}
	env, closeEnv, err := OpenPassiveEnv(context.Background(), t.TempDir(), c, blind)
	if err != nil {
		t.Fatalf("OpenPassiveEnv: %v", err)
	}
	t.Cleanup(closeEnv)
	return c, env
}

func passiveRun(t *testing.T, blind PassiveBlind) PassiveReport {
	t.Helper()
	c, env := passiveEnv(t, blind)
	rep, err := RunPassive(context.Background(), env, c, PassiveSurfaces())
	if err != nil {
		t.Fatalf("RunPassive: %v", err)
	}
	return rep
}

// TestPassiveBaseline is the report on the corpus as designed, and its subtests
// share ONE seeded store: each seeding is ~200 writes, and under -race that is the
// cost of the test, in a package whose CI budget is already the tightest in the
// tree. The subtests are what the baseline is held to.
func TestPassiveBaseline(t *testing.T) {
	c, env := passiveEnv(t, BlindNone)
	rep, err := RunPassive(context.Background(), env, c, PassiveSurfaces())
	if err != nil {
		t.Fatalf("RunPassive: %v", err)
	}

	// The report is pinned to its committed text. A change that moves any figure —
	// a ranking change, a filter change, a corpus change — fails here and has to
	// say so in the golden, which is the review surface.
	t.Run("pinned", func(t *testing.T) {
		got := FormatPassive(rep)
		if os.Getenv("GHOST_UPDATE_GOLDEN") != "" {
			if err := os.WriteFile(passiveGoldenPath, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
		want, err := os.ReadFile(passiveGoldenPath)
		if err != nil {
			t.Fatalf("read golden: %v (generate it with GHOST_UPDATE_GOLDEN=1)", err)
		}
		if got != string(want) {
			t.Errorf("passive report moved.\n--- want\n%s\n--- got\n%s", want, got)
		}
	})

	// Two runs over two fresh stores print the same bytes, which is what lets a
	// golden pin them at all. The second store is the cost of this subtest.
	t.Run("deterministic", func(t *testing.T) {
		again := FormatPassive(passiveRun(t, BlindNone))
		if first := FormatPassive(rep); first != again {
			t.Errorf("two runs differ:\n%s\n---\n%s", first, again)
		}
	})

	// The claim the whole bench exists to make, stated as assertions rather than
	// as a golden: on every surface no must-not-appear row is rendered, no row of
	// another project is rendered, and the denominators are populations (a zero
	// over nothing is not a measurement).
	t.Run("withholds", func(t *testing.T) {
		if len(rep.Surfaces) != 4 {
			t.Fatalf("surfaces = %d, want 4 (session start, scoped session start, tool, resource)", len(rep.Surfaces))
		}
		for _, s := range rep.Surfaces {
			if s.Leaked.Num != 0 || len(s.LeakedIDs) != 0 {
				t.Errorf("%s leaked withheld rows: %v", s.Name, s.LeakedIDs)
			}
			if !s.Leaked.Defined() || s.Leaked.Den == 0 {
				t.Errorf("%s: leakage is over no withheld rows, so its zero measures nothing", s.Name)
			}
			if s.Contamination.Num != 0 {
				t.Errorf("%s rendered %d rows of another project", s.Name, s.Contamination.Num)
			}
			if !s.Contamination.Defined() {
				t.Errorf("%s rendered no rows at all", s.Name)
			}
		}
	})

	// The corpus has to make every budget bite: a block that never has to cut says
	// nothing about how it cuts. The caps here are the surfaces' own, observed from
	// the blocks, so a drift between the constants this file states and the
	// production budgets is a failure too.
	t.Run("budgets", func(t *testing.T) { checkPassiveBudgets(t, c, env) })
}

// checkPassiveBudgets is the budgets subtest of TestPassiveBaseline.
func checkPassiveBudgets(t *testing.T, c PassiveCorpus, env *PassiveEnv) {
	t.Helper()
	specs := PassiveSurfaces()
	read := func(i int, project string) string {
		b, err := specs[i].Read(context.Background(), env, project)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	count := func(block string) (own, global int) {
		byID := c.ByID()
		for _, id := range renderedIDs(block) {
			if byID[id].Project == PassiveGlobal {
				global++
			} else {
				own++
			}
		}
		return
	}

	// alpha holds far more admissible rows than any cap, so each surface is full.
	if own, global := count(read(0, "alpha")); own != passiveSessionProjectCap || global != passiveSessionGlobalCap {
		t.Errorf("session start renders %d project + %d global rows, want the caps %d + %d", own, global, passiveSessionProjectCap, passiveSessionGlobalCap)
	}
	if own, global := count(read(2, "alpha")); own+global != passiveToolLimit {
		t.Errorf("ghost_project_context renders %d rows, want its limit %d", own+global, passiveToolLimit)
	}
	// The resource reads the union at its cap and then makes a second request for
	// the Global section, so its own half is what the union left and its global
	// half is the section's cap.
	if own, global := count(read(3, "alpha")); own > passiveResourceCap || global != passiveResourceGlobalCap || own+global <= passiveResourceCap {
		t.Errorf("project resource renders %d own + %d global rows, want at most %d own, exactly %d global and more than %d in all (the second request)",
			own, global, passiveResourceCap, passiveResourceGlobalCap, passiveResourceCap)
	}
	// delta fits every cap, so nothing eligible is cut and the near-duplicate
	// and superseded rows are shown beside what they restate.
	byID := c.ByID()
	shown := map[string]bool{}
	for _, id := range renderedIDs(read(0, "delta")) {
		shown[id] = true
	}
	for _, r := range c.Rows {
		if r.Project == "delta" && r.eligible(false) && !shown[r.ID()] {
			t.Errorf("delta fits every cap yet %s (%s) is not shown", r.ID(), byID[r.ID()].Kind)
		}
	}
}

// TestPassiveCorpusGradesAreConsistent holds the fixture to the properties the
// measurements rely on, each of which fails silently if broken.
func TestPassiveCorpusGradesAreConsistent(t *testing.T) {
	c, err := NewPassiveCorpus()
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]map[PassiveKind]int{}
	for _, r := range c.Rows {
		if kinds[r.Project] == nil {
			kinds[r.Project] = map[PassiveKind]int{}
		}
		kinds[r.Project][r.Kind]++
	}
	// Every project (and `_global`) carries every class the bench grades.
	for _, p := range append(append([]string{}, PassiveProjects...), PassiveGlobal) {
		for _, k := range []PassiveKind{KindLive, KindPinned, KindResolved, KindExpired, KindFuture, KindScoped, KindSuperseded, KindDuplicate} {
			if kinds[p][k] == 0 {
				t.Errorf("%s holds no %s row", p, k)
			}
		}
	}
	// A withheld row must be the freshest and highest-ranked thing in its project,
	// or a filter that stopped working would be hidden by the ranking and the
	// leak figure would stay zero for the wrong reason.
	for _, p := range append(append([]string{}, PassiveProjects...), PassiveGlobal) {
		minAge, maxOtherImportance := 1<<30, float32(0)
		for _, r := range c.Rows {
			if r.Project != p {
				continue
			}
			if r.Grade(true) == GradeWithheld {
				if r.AgeDays < minAge {
					minAge = r.AgeDays
				}
			} else if r.Importance > maxOtherImportance {
				maxOtherImportance = r.Importance
			}
		}
		for _, r := range c.Rows {
			if r.Project != p || r.Grade(true) != GradeWithheld {
				continue
			}
			if r.Importance <= maxOtherImportance {
				t.Errorf("%s: withheld row importance %.2f does not beat every other row's %.2f", r.ID(), r.Importance, maxOtherImportance)
			}
			if r.AgeDays != minAge {
				t.Errorf("%s: withheld row is %d days old, newer rows exist", r.ID(), r.AgeDays)
			}
		}
	}
	// No two rows of one project share an importance unless they are the same
	// kind of row, because a tie is an order the product settles by a rounding
	// the architecture decides (see imp), and a golden that rests on one is a
	// golden that differs between a laptop and CI.
	for _, p := range append(append([]string{}, PassiveProjects...), PassiveGlobal) {
		owner := map[float32]PassiveRow{}
		for _, r := range c.Rows {
			if r.Project != p {
				continue
			}
			// Two withheld rows may tie: neither is ever shown, so their order
			// is not an order anything is measured by.
			if o, ok := owner[r.Importance]; ok && o.Kind != r.Kind && (o.Grade(true) != GradeWithheld || r.Grade(true) != GradeWithheld) {
				t.Errorf("%s: importance %.3f is shared by a %s row and a %s row, so their order is a tie", p, r.Importance, o.Kind, r.Kind)
			}
			owner[r.Importance] = r
		}
	}
	// Each bucket's expected set sits under its caps with room for one optional
	// row, so a miss is the ranking's and not arithmetic.
	for _, p := range PassiveProjects {
		expected := 0
		for _, r := range c.Rows {
			if r.Project == p && r.Grade(false) == GradeExpected {
				expected++
			}
		}
		if expected >= passiveSessionProjectCap {
			t.Errorf("%s has %d expected rows, which does not fit the session cap %d", p, expected, passiveSessionProjectCap)
		}
	}
	globalExpected := 0
	for _, r := range c.Rows {
		if r.Project == PassiveGlobal && r.Grade(false) == GradeExpected {
			globalExpected++
		}
	}
	if globalExpected >= passiveSessionGlobalCap {
		t.Errorf("_global has %d expected rows, which does not fit the session cap %d with room for an optional one", globalExpected, passiveSessionGlobalCap)
	}
}

// TestPassiveMeasurementCatchesADisabledFilter is the mutation check. A store
// seeded without the windows (or without the withdrawals) is the store a product
// whose validity (or resolved) filter had been removed would behave as if it held:
// the filter reads each row as live. The leakage figure must go non-zero on every
// surface, and name rows of the kind the filter exists for — and nothing else.
//
// This is the in-test half. The source half — deleting stage 2 from
// internal/assemble and watching the same figure move — was run by hand for the
// PR that added this file; it cannot be a test, because it edits the product.
func TestPassiveMeasurementCatchesADisabledFilter(t *testing.T) {
	for _, tc := range []struct {
		blind PassiveBlind
		kinds []PassiveKind
	}{
		{BlindValidity, []PassiveKind{KindExpired, KindFuture}},
		{BlindResolved, []PassiveKind{KindResolved}},
	} {
		t.Run(string(tc.blind), func(t *testing.T) {
			c, _ := NewPassiveCorpus()
			byID := c.ByID()
			rep := passiveRun(t, tc.blind)
			for _, s := range rep.Surfaces {
				if s.Leaked.Num == 0 {
					t.Errorf("%s: filter disabled (%s) and leakage is still 0, so the measurement cannot see this filter", s.Name, tc.blind)
				}
				for _, id := range s.LeakedIDs {
					ok := false
					for _, k := range tc.kinds {
						ok = ok || byID[id].Kind == k
					}
					if !ok {
						t.Errorf("%s: leaked %s (%s), which the %s filter is not responsible for", s.Name, id, byID[id].Kind, tc.blind)
					}
				}
			}
		})
	}
}

// TestPassiveScorerFlagsWhatItIsGiven proves the scorer is not vacuous: feed it
// blocks with a planted violation and it must say so, whatever the product does.
func TestPassiveScorerFlagsWhatItIsGiven(t *testing.T) {
	c, _ := NewPassiveCorpus()
	byID := c.ByID()
	line := func(id string) string {
		return fmt.Sprintf("- [%s] `%s` (0.9) «x»\n", byID[id].Category, id)
	}
	spec := PassiveSurfaceSpec{Name: "planted", ProjectCap: 15, GlobalCap: 8}

	t.Run("a withheld row", func(t *testing.T) {
		var sr PassiveSurfaceReport
		measurePassiveBlock(&sr, spec, c, byID, "alpha", line("bench:alpha:live-00")+line("bench:alpha:expired-00"))
		if sr.Leaked.Num != 1 || len(sr.LeakedIDs) != 1 || sr.LeakedIDs[0] != "bench:alpha:expired-00" {
			t.Errorf("leaked = %v %v, want the planted expired row", sr.Leaked, sr.LeakedIDs)
		}
	})
	t.Run("a resolved row", func(t *testing.T) {
		var sr PassiveSurfaceReport
		measurePassiveBlock(&sr, spec, c, byID, "alpha", line("bench:alpha:resolved-00"))
		if sr.Leaked.Num != 1 {
			t.Errorf("resolved row not flagged: %v", sr.Leaked)
		}
	})
	t.Run("another project's row", func(t *testing.T) {
		var sr PassiveSurfaceReport
		measurePassiveBlock(&sr, spec, c, byID, "alpha", line("bench:alpha:live-00")+line("bench:beta:live-00"))
		if sr.Contamination.Num != 1 || sr.Contamination.Den != 2 {
			t.Errorf("contamination = %v, want 1/2", sr.Contamination)
		}
	})
	t.Run("an out-of-scope row only when scoped", func(t *testing.T) {
		var open, scoped PassiveSurfaceReport
		block := line("bench:alpha:staging-00")
		measurePassiveBlock(&open, spec, c, byID, "alpha", block)
		spec.Scoped = true
		measurePassiveBlock(&scoped, spec, c, byID, "alpha", block)
		if open.Leaked.Num != 0 || scoped.Leaked.Num != 1 {
			t.Errorf("scope grading: open %v, scoped %v, want 0 and 1", open.Leaked, scoped.Leaked)
		}
	})
	t.Run("a redundant pair", func(t *testing.T) {
		var sr PassiveSurfaceReport
		spec.Scoped = false
		measurePassiveBlock(&sr, spec, c, byID, "delta", line("bench:delta:live-02")+line("bench:delta:dup-00")+line("bench:delta:live-00")+line("bench:delta:old-00"))
		// dup-00 restates live-02, which is shown; old-00 is replaced by live-00.
		if sr.Duplicates.Num != 2 {
			t.Errorf("duplicates = %v, want 2 redundant rows", sr.Duplicates)
		}
	})
	t.Run("a missed expected row", func(t *testing.T) {
		var sr PassiveSurfaceReport
		measurePassiveBlock(&sr, spec, c, byID, "delta", line("bench:delta:live-00"))
		if sr.Recall.Num != 1 || sr.Recall.Den < 6 || len(sr.MissedIDs) == 0 {
			t.Errorf("recall = %v, missed %d", sr.Recall, len(sr.MissedIDs))
		}
	})
}

// TestPassiveHonestyCheckReadsBothHeaderShapes: the check must fail the header an
// unfixed tree prints and pass the header a fixed one prints, for the same block.
// Without both, "FAIL" in the golden could be a parser that rejects everything and
// the day #912 merges would not show a PASS.
func TestPassiveHonestyCheckReadsBothHeaderShapes(t *testing.T) {
	c, _ := NewPassiveCorpus()
	byID := c.ByID()
	// delta, unscoped: 11 eligible rows (5 live, pinned, 2 old, 2 dup, 1 staging),
	// 2 withheld by a stage (expired, future), 1 resolved (never in the window).
	var block strings.Builder
	shown := []string{"live-00", "live-01", "live-02"}
	for _, k := range shown {
		fmt.Fprintf(&block, "- [%s] `bench:delta:%s` (0.9) «x»\n", byID["bench:delta:"+k].Category, k)
	}
	check := func(header string) PassiveSurfaceReport {
		var sr PassiveSurfaceReport
		text := "**Memories (" + header + "):**\n" + block.String()
		checkSessionHeaders(&sr, PassiveSurfaceSpec{Header: true}, c, "delta", text)
		return sr
	}
	// shown 3, cut 8, withheld 2, total 13.
	legacy := check("3 shown of 13 total — 10 not shown, ranked by a composite score; use x for the rest")
	if len(legacy.Findings) == 0 {
		t.Error("the unfixed header (withheld rows counted as ranked out) passed the honesty check")
	}
	fixed := check("3 shown of 13 total — 10 not shown: 8 ranked out by a composite score, 2 withheld rather than ranked out; use x for the rest")
	for _, f := range fixed.Findings {
		if f.Bucket == "project" {
			t.Errorf("the fixed header failed the check: %s", f.Problem)
		}
	}
	// A shown count that is not the number of lines is caught on its own.
	liar := check("5 shown of 13 total — 8 ranked out by a composite score, 2 withheld rather than ranked out; use x for the rest")
	found := false
	for _, f := range liar.Findings {
		found = found || strings.Contains(f.Problem, "says 5 shown, block renders 3")
	}
	if !found {
		t.Errorf("a header claiming 5 shown over 3 rendered rows was not caught: %v", liar.Findings)
	}
}

// TestBudgetCutOKRowsAreAbsentOnTheUnionSurfaces holds the optional grade on the
// shared-cap surfaces to what they render. A row graded cut-OK that IS rendered
// means the grade has drifted from the cap or the ranking (it is excusing a row
// the surface can show); a surface that renders every one means the grade
// excuses nothing and should go. Either way the grade is no longer evidence.
func TestBudgetCutOKRowsAreAbsentOnTheUnionSurfaces(t *testing.T) {
	c, env := passiveEnv(t, BlindNone)
	marked := 0
	for _, r := range c.Rows {
		if r.BudgetCutOK {
			marked++
		}
	}
	if marked == 0 {
		t.Fatal("no row is graded BudgetCutOK, so the grade is untested")
	}
	for _, spec := range PassiveSurfaces() {
		if !spec.CutLowestLiveOK {
			continue
		}
		for _, p := range PassiveProjects {
			block, err := spec.Read(context.Background(), env, p)
			if err != nil {
				t.Fatalf("%s/%s: %v", spec.Name, p, err)
			}
			rendered := map[string]bool{}
			for _, id := range renderedIDs(block) {
				rendered[id] = true
			}
			for _, r := range c.Rows {
				if r.Project == p && r.BudgetCutOK && rendered[r.ID()] {
					t.Errorf("%s/%s: %s is graded optional for a budget cut but the surface rendered it", spec.Name, p, r.ID())
				}
			}
		}
	}
}
