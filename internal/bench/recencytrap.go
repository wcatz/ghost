package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"

	"github.com/wcatz/ghost/internal/memory"
)

// The recency-trap suite is the safety counterweight to the staleness suite.
// Staleness rewards newest-wins; the trap punishes it. Each scenario pits an
// OLD but still-correct memory against NEWER keyword-overlapping distractors,
// and a probe whose right answer is the old one. correct-wins = the correct
// memory outranks every trap. A global recency prior tuned to ace staleness
// will fail these once its weight is high enough to promote the fresh
// distractor — which is exactly the bound this suite measures. See
// docs/benchmarks.md Phase 3.
//
// The suite spans two kinds of category, and the split is the measurement. A
// scenario whose category the shipped decay factor never penalises (fact,
// preference, convention) cannot be moved by age at all, so its score is the
// same whether decay is on or off — which is exactly why category-aware decay
// can ship on by default. A scenario in a DECAYING category (decision, gotcha,
// dependency at tau 30; architecture, pattern at tau 45) is the case that
// invariant hides: the correct memory is old enough to be multiplied down and
// the distractor is fresh enough not to be. Reporting only the never-decay half
// (as this suite did until #561) measures a ranking path decay never takes, so
// the 0.929 that justified the default said nothing about the categories the
// default actually reorders. The report therefore prints the two classes apart
// and never pools them into one number.

// TrapVersion is a memory with a controlled age.
type TrapVersion struct {
	Content string `json:"content"`
	AgeDays int    `json:"age_days"`
}

// TrapScenario: the correct answer is old; traps are newer distractors.
type TrapScenario struct {
	Name string `json:"name"`
	// Category is the category every memory in this scenario is stored under,
	// which decides whether decay can move the correct answer. Empty means
	// "fact": the never-decay class the fixture was written in, kept as the
	// default so the original scenarios needed no edit to keep meaning what
	// they meant.
	Category string `json:"category,omitempty"`
	// Pinned pins the scenario's CORRECT memory only. A pinned row carries a
	// decay factor of exactly 1.0 at any age (memory.DecayFactor), which is the
	// one production escape hatch from a decaying category, so the fixture
	// measures it: a pinned old-but-correct memory has to outrank a fresh
	// UNPINNED distractor in the same category. Traps are never pinned — a
	// pinned distractor would not be a trap.
	Pinned  bool          `json:"pinned,omitempty"`
	Correct TrapVersion   `json:"correct"`
	Traps   []TrapVersion `json:"traps"`
	Probes  []struct {
		Text string `json:"text"`
	} `json:"probes"`
}

// neverDecayCategory is the category a scenario without an explicit one is
// seeded under. It is the class the shipped decay factor exempts
// (memory.DecayFactor returns 1.0 for it at any age), so an unlabelled scenario
// measures the ranking path decay does not touch.
const neverDecayCategory = "fact"

// effectiveCategory is the scenario's category, defaulted.
func (s TrapScenario) effectiveCategory() string {
	if s.Category == "" {
		return neverDecayCategory
	}
	return s.Category
}

// decayProbeAgeDays is the age at which a category is asked whether decay could
// ever penalise it. memory.DecayFactor returns 1.0 for every category at age 0
// (nothing is old enough to be multiplied down), so a zero-age probe cannot
// tell the classes apart; 1000 days puts every decaying category on its floor
// (0.3 for pattern/architecture, 0.15 for the rest) while the exempt three stay
// at exactly 1.0. The question is asked of the SHIPPED factor rather than of a
// list of names kept here, so a category that starts or stops decaying is
// reclassified by the code that decides it.
const decayProbeAgeDays = 1000.0

// categoryDecays reports whether the shipped category-aware time decay would
// ever reorder a row in this category.
func categoryDecays(category string) bool {
	return memory.DecayFactor(category, false, decayProbeAgeDays) < 1.0
}

// LoadTrapScenarios reads trap scenarios, one JSON per line. A scenario whose
// category is one of the report's own row labels is a hard error: those labels
// are how the aggregation marks a pooled row, and a category that collides with
// one would be counted into the pool and rendered under the pool's heading, which
// is a wrong number rather than a visibly broken one.
func LoadTrapScenarios(r io.Reader) ([]TrapScenario, error) {
	var out []TrapScenario
	if err := decodeJSONL(r, func(raw json.RawMessage) error {
		var s TrapScenario
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		if len(s.Traps) == 0 {
			return fmt.Errorf("trap scenario %q needs at least one trap", s.Name)
		}
		if s.Category == trapDecayingLabel || s.Category == trapNeverDecayLabel || s.Category == trapAllLabel {
			return fmt.Errorf("trap scenario %q uses %q as its category, which is a report row label", s.Name, s.Category)
		}
		out = append(out, s)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// TrapOutcome is the judgment for one probe.
type TrapOutcome struct {
	Scenario      string
	Category      string // the scenario's effective category
	Decays        bool   // whether the shipped decay factor can reorder this class
	CorrectPinned bool   // the correct (old) memory is pinned, so decay cannot touch it
	CorrectFound  bool   // the correct (old) memory was retrieved at all
	CorrectWins   bool   // correct outranks every trap present in the results
	CorrectTop1   bool   // correct is the overall top result
}

// RunRecencyTrap seeds every scenario (correct + traps) with backdated
// created_at, probes with SearchHybridParams over the FTS path (mirroring the
// staleness suite), and judges whether the correct old memory outranks its newer
// distractors. Each scenario is stored under its own category, so the suite
// spans both the never-decay and the decaying classes.
//
// The two classes are seeded into SEPARATE projects, which is what keeps the two
// numbers independent. They share a store (a scenario's own distractors are its
// clutter, and that is the contest being judged) but not a project: pooling them
// would make each class's window depend on how many scenarios the OTHER class
// happens to contribute, so adding the decaying fixtures silently moved a
// never-decay scenario's correct memory out of the top-10 window. Splitting by
// class is also what keeps the never-decay score the number the published
// frontier quotes — same fourteen scenarios, same project, same window.
func RunRecencyTrap(ctx context.Context, scenarios []TrapScenario, p memory.SearchParams) ([]TrapOutcome, error) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		return nil, err
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer store.Close() //nolint:errcheck

	projects := map[bool]string{}
	projectFor := func(category string) (string, error) {
		decays := categoryDecays(category)
		project, ok := projects[decays]
		if !ok {
			project = "recencytrap"
			if decays {
				project = "recencytrap-decaying"
			}
			projects[decays] = project
			if err := store.EnsureProject(ctx, project, "/bench/"+project, project); err != nil {
				return "", err
			}
		}
		return project, nil
	}

	type seeded struct {
		correctID string
		trapIDs   []string
	}
	seed := make([]seeded, len(scenarios))
	scenarioProject := make([]string, len(scenarios))
	for i, sc := range scenarios {
		category := sc.effectiveCategory()
		project, err := projectFor(category)
		if err != nil {
			return nil, err
		}
		scenarioProject[i] = project
		cid, err := store.Create(ctx, project, memory.Memory{
			Category: category, Content: sc.Correct.Content, Importance: 0.7, Source: "mcp",
		})
		if err != nil {
			return nil, fmt.Errorf("seed %s correct: %w", sc.Name, err)
		}
		if err := backdate(ctx, db, cid, sc.Correct.AgeDays); err != nil {
			return nil, err
		}
		if sc.Pinned {
			// Through the production writer rather than a raw UPDATE, so the
			// pin is set the way a user's `ghost_memory_save {pin: true}` sets
			// it and the history row the benchmark seeds is the one it would
			// really produce.
			if err := store.TogglePin(ctx, cid, true); err != nil {
				return nil, fmt.Errorf("pin %s correct: %w", sc.Name, err)
			}
		}
		var tids []string
		for j, tv := range sc.Traps {
			tid, err := store.Create(ctx, project, memory.Memory{
				Category: category, Content: tv.Content, Importance: 0.7, Source: "mcp",
			})
			if err != nil {
				return nil, fmt.Errorf("seed %s trap%d: %w", sc.Name, j, err)
			}
			if err := backdate(ctx, db, tid, tv.AgeDays); err != nil {
				return nil, err
			}
			tids = append(tids, tid)
		}
		seed[i] = seeded{correctID: cid, trapIDs: tids}
	}

	var outcomes []TrapOutcome
	for i, sc := range scenarios {
		category := sc.effectiveCategory()
		for _, probe := range sc.Probes {
			results, err := store.SearchHybridParams(ctx, scenarioProject[i], probe.Text, nil, scoreK, p)
			if err != nil {
				return nil, fmt.Errorf("trap %s: %w", sc.Name, err)
			}
			ranked := make([]string, len(results))
			for k, m := range results {
				ranked[k] = m.ID
			}
			// A correct memory that beats every retrieved trap "wins"; the
			// judge reuses judgeProbe with correct as the "fresh" role.
			found, wins, top1 := judgeProbe(ranked, seed[i].correctID, seed[i].trapIDs)
			outcomes = append(outcomes, TrapOutcome{
				Scenario: sc.Name, Category: category, Decays: categoryDecays(category),
				CorrectPinned: sc.Pinned, CorrectFound: found, CorrectWins: wins, CorrectTop1: top1,
			})
		}
	}
	return outcomes, nil
}

// TrapCorrectWins is the fraction of probes where the correct old memory
// outranked every trap. It pools the never-decay and decaying classes, so it is
// only comparable with another pooled number — see TrapSummary for the split.
func TrapCorrectWins(outcomes []TrapOutcome) float64 {
	if len(outcomes) == 0 {
		return 0
	}
	wins := 0
	for _, o := range outcomes {
		if o.CorrectWins {
			wins++
		}
	}
	return float64(wins) / float64(len(outcomes))
}

// Pooled row labels. They are rows in the report rather than categories, so a
// scenario may not use one: LoadTrapScenarios refuses, because a category
// colliding with a label would be counted into the pool it names and rendered
// under the pool's heading.
const (
	trapDecayingLabel   = "decaying (pooled)"
	trapNeverDecayLabel = "never-decay (pooled)"
	trapAllLabel        = "all probes"
)

// TrapSummary is one row of the trap report: a category, or one of the two
// pooled classes. The two class rows are what the report is read for, and they
// are deliberately kept apart — a pooled score lets the never-decay half carry
// the decaying half, which is the mistake #561 found.
//
// Pinned counts the probes whose CORRECT memory is pinned, and PinnedWins is the
// share of THOSE that the correct memory won. It is a column of its own because
// pinning is the actionable half of the decay cost: without it a reader sees
// only that decay loses, not that the user's one control over it wins.
type TrapSummary struct {
	Category    string
	Decays      bool
	Probes      int
	Pinned      int
	PinnedWins  float64
	Found       int
	CorrectWins float64
	CorrectTop1 float64
}

// SummarizeTrap aggregates outcomes by category, then appends the two pooled
// class rows and an all-probes row. Order is fixture-independent — decaying
// categories alphabetically, then never-decay ones, then the pools — so adding
// a scenario does not reshuffle the table.
func SummarizeTrap(outcomes []TrapOutcome) []TrapSummary {
	byCategory := map[string]*TrapSummary{}
	for _, o := range outcomes {
		s := byCategory[o.Category]
		if s == nil {
			s = &TrapSummary{Category: o.Category, Decays: o.Decays}
			byCategory[o.Category] = s
		}
		s.Probes++
		if o.CorrectPinned {
			s.Pinned++
		}
		if o.CorrectPinned && o.CorrectWins {
			s.PinnedWins++
		}
		if o.CorrectFound {
			s.Found++
		}
		if o.CorrectWins {
			s.CorrectWins++
		}
		if o.CorrectTop1 {
			s.CorrectTop1++
		}
	}
	var out []TrapSummary
	for _, decays := range []bool{true, false} {
		for _, name := range sortedCategories(byCategory, decays) {
			out = append(out, poolTrap(poolTrapMatch{name: name, decays: decays}, byCategory))
		}
	}
	for _, p := range []poolTrapMatch{
		{classOnly: true, decays: true, name: trapDecayingLabel},
		{classOnly: true, decays: false, name: trapNeverDecayLabel},
		{anyClass: true, name: trapAllLabel},
	} {
		if s := poolTrap(p, byCategory); s.Probes > 0 {
			out = append(out, s)
		}
	}
	return out
}

// poolTrapMatch selects which category rows a pooled row sums: one named
// category, every category of one decay class (classOnly), or — with anyClass,
// the only mode that ignores both — all of them.
type poolTrapMatch struct {
	name      string
	decays    bool
	classOnly bool
	anyClass  bool
}

// poolTrap sums the matching category rows' counts and divides them by the
// probe count, so a pooled row is a real ratio rather than an average of ratios
// (which would weight a one-probe category like a twenty-probe one).
func poolTrap(match poolTrapMatch, byCategory map[string]*TrapSummary) TrapSummary {
	pool := TrapSummary{Category: match.name, Decays: match.decays}
	for name, s := range byCategory {
		switch {
		case match.anyClass:
		case match.classOnly:
			if s.Decays != match.decays {
				continue
			}
		default:
			if name != match.name || s.Decays != match.decays {
				continue
			}
		}
		pool.Probes += s.Probes
		pool.Pinned += s.Pinned
		pool.PinnedWins += s.PinnedWins
		pool.Found += s.Found
		pool.CorrectWins += s.CorrectWins
		pool.CorrectTop1 += s.CorrectTop1
	}
	if pool.Probes > 0 {
		pool.CorrectWins /= float64(pool.Probes)
		pool.CorrectTop1 /= float64(pool.Probes)
	}
	// PinnedWins is a ratio over the PINNED probes, not over all of them: a row
	// with one pinned probe out of twenty would read 0.05 as a mean-of-ratios
	// and 1.000 as the share of the probes it is a claim about.
	if pool.Pinned > 0 {
		pool.PinnedWins /= float64(pool.Pinned)
	}
	return pool
}

// sortedCategories lists the category names in a stable order: the decaying
// ones first, each group alphabetical, so a new category appears in a
// predictable place rather than at the mercy of map iteration.
func sortedCategories(byCategory map[string]*TrapSummary, decays bool) []string {
	var out []string
	for name, s := range byCategory {
		if s.Decays == decays {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// FormatTrap renders the trap report for the pair of runs made with decay off
// and on. Two outcome sets rather than one so the column that matters — the
// delta — is measured rather than assumed, and so a suite in which decay is
// inert (every scenario in a never-decay category) reads as a column of zeros
// instead of looking like a healthy invariance.
//
// The pinned column is a ratio over the pinned probes in that row, printed as a
// count beside it, because a row with no pinned probe has no claim to make and
// printing 0.000 for it would read as "pinning does not work here".
func FormatTrap(off, on []TrapOutcome) string {
	offRows := SummarizeTrap(off)
	onByCategory := map[string]TrapSummary{}
	for _, r := range SummarizeTrap(on) {
		onByCategory[r.Category] = r
	}

	var b bytes.Buffer
	b.WriteString("recency trap: the OLD memory is the correct answer, the NEWER ones are distractors.\n")
	b.WriteString("wins = the old correct memory outranks every distractor in the window; @1 = it is the top result;\n")
	b.WriteString("pinned = over the probes whose correct memory is PINNED, which is the one control a user has.\n\n")
	fmt.Fprintf(&b, "%-22s %-7s %4s %10s %10s %9s %10s %14s\n",
		"category", "decays", "n", "wins(off)", "wins(on)", "delta", "@1(on)", "pinned(on)")
	for _, r := range offRows {
		other := onByCategory[r.Category]
		pinned := "-"
		if other.Pinned > 0 {
			pinned = fmt.Sprintf("%.3f (%d)", other.PinnedWins, other.Pinned)
		}
		fmt.Fprintf(&b, "%-22s %-7s %4d %10.3f %10.3f %+9.3f %10.3f %14s\n",
			r.Category, trapDecaysLabel(r), r.Probes, r.CorrectWins, other.CorrectWins,
			other.CorrectWins-r.CorrectWins, other.CorrectTop1, pinned)
	}
	total := 0
	if all, ok := onByCategory[trapAllLabel]; ok {
		total = all.Probes
	}
	fmt.Fprintf(&b, "\n%d probes. Report-only: the never-decay row is the frontier's claim, the decaying rows are its cost.\n", total)
	return b.String()
}

// trapDecaysLabel renders a row's decay class, distinguishing the all-probes
// row (which spans both) from the two pooled rows (which are each one class).
func trapDecaysLabel(r TrapSummary) string {
	switch r.Category {
	case trapAllLabel:
		return "mixed"
	case trapDecayingLabel:
		return "yes"
	case trapNeverDecayLabel:
		return "no"
	default:
		if r.Decays {
			return "yes"
		}
		return "no"
	}
}
