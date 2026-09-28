package bench

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestFalsePositiveReport runs the no-answer set through the production search
// path and logs the baseline. Report-only, for the same reason the maintenance
// suite is: the numbers are the starting point the abstention work (#580) moves,
// and a floor here would be a gate on that work rather than on this change.
//
// What IS enforced is that the set can measure something at all — every flavor
// present, and results actually returned for queries nothing answers. A suite
// where search returned nothing for any of them would report a beautiful 0.000
// and mean nothing. What the two distributions do with each other is reported,
// never asserted; see the comment inside for why that line must stay unset.
func TestFalsePositiveReport(t *testing.T) {
	ds, vecs, err := BuiltinDataset()
	if err != nil {
		t.Fatalf("BuiltinDataset: %v", err)
	}
	negs := ds.Negatives
	if len(negs) < 20 {
		t.Fatalf("fixture has %d no-answer queries, want >= 20", len(negs))
	}
	flavors := map[string]int{}
	for _, n := range negs {
		flavors[n.Flavor]++
		if len(n.Rel) != 0 {
			t.Errorf("no-answer query %q grades %d memories", n.Name, len(n.Rel))
		}
	}
	for _, flavor := range []string{"off_domain", "near_miss"} {
		if flavors[flavor] == 0 {
			t.Errorf("no %s no-answer queries: one flavor alone cannot show what an abstain rule has to survive", flavor)
		}
	}

	store, db := newBenchStoreWithDB(t)
	ctx := context.Background()
	graded, err := Seed(ctx, store, db, ds, vecs)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	noAnswer, err := NegativeQueries(ds, vecs)
	if err != nil {
		t.Fatalf("NegativeQueries: %v", err)
	}
	// The no-answer set is measured by the RUNNER, per condition, and the
	// hybrid condition's measurement is what this report is built on: one
	// search per query rather than one per query per report.
	all := append(append([]Query{}, graded...), noAnswer...)
	results, err := Run(ctx, store, all)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	measured := NoAnswerFor(results, CondHybrid)
	if len(measured) != len(noAnswer) {
		t.Fatalf("runner measured %d no-answer queries, fixture has %d", len(measured), len(noAnswer))
	}
	rep, err := FalsePositives(measured, PerQueryFor(results, CondHybrid))
	if err != nil {
		t.Fatalf("FalsePositives: %v", err)
	}
	if rep.Queries != len(noAnswer) || rep.Answerable != len(graded) {
		t.Errorf("report counts %d/%d queries, want %d/%d", rep.Queries, rep.Answerable, len(noAnswer), len(graded))
	}
	if len(rep.Floors) != len(FalsePositiveFloors) {
		t.Errorf("report has %d floor rows, want %d", len(rep.Floors), len(FalsePositiveFloors))
	}
	// What follows asserts the shape of the measurement, never today's numbers.
	// In particular it does NOT assert that the no-answer and answerable
	// distributions are separated: that is a claim about the current ranking, and
	// an abstention fix that legitimately returns fewer, more similar rows for an
	// unanswered query would move the no-answer mean up and trip a test whose
	// whole job is to watch that fix land. The separation is reported instead
	// (mean top 0.584 vs 0.741), where a human reads it.
	//
	// The one outcome claim left is the flavor ordering below, and it is a
	// statement about the fixture: the near-miss set is supposed to be the hard
	// one, and if it is not, the set is mislabelled rather than the ranking wrong.
	if rep.MeanResults <= 0 {
		t.Errorf("mean results per no-answer query is %.1f: the search returned nothing, so the false-positive count is not being measured", rep.MeanResults)
	}
	if rep.Unseparable < 0 || rep.Unseparable > rep.Answerable {
		t.Errorf("unseparable count %d is outside 0..%d", rep.Unseparable, rep.Answerable)
	}
	// Every flavor must survive into the report with its own count: a pooled
	// mean would let the easy flavor flatter the hard one, which is the only
	// reason the two are kept apart.
	if len(rep.Flavors) != len(flavors) {
		t.Errorf("report has %d flavor rows, want %d", len(rep.Flavors), len(flavors))
	}
	byFlavor := map[string]FlavorStat{}
	for _, f := range rep.Flavors {
		if f.Queries != flavors[f.Flavor] {
			t.Errorf("flavor %s: report counts %d queries, fixture has %d", f.Flavor, f.Queries, flavors[f.Flavor])
		}
		byFlavor[f.Flavor] = f
	}
	// The near-miss set is the one an abstain rule has to survive. If a fixture
	// edit made it easier than the off-domain floor, the report would be
	// reporting the easy case and calling it the hard one.
	if byFlavor["near_miss"].MeanTop < byFlavor["off_domain"].MeanTop {
		t.Errorf("near-miss queries score %.3f on average against %.3f for off-domain ones: "+
			"the set meant to be the hard case is now the easy one",
			byFlavor["near_miss"].MeanTop, byFlavor["off_domain"].MeanTop)
	}
	// The maximum cannot be below the mean it is drawn from — that is arithmetic,
	// not a property of the ranking, so it stays asserted.
	if rep.NoAnswerMax < rep.MeanTop {
		t.Errorf("no-answer maximum %.3f is below the mean %.3f", rep.NoAnswerMax, rep.MeanTop)
	}

	// The deep report and the per-condition table describe the SAME run of the
	// shipped path, so their floor rows have to be the same numbers: Results is a
	// mean per query and Rate a fraction in both. A sum in one and a mean in the
	// other reads as "240 results per query" against a window of 10.
	sum := SummarizeNoAnswer(CondHybrid, measured)
	if len(sum.Floors) != len(rep.Floors) {
		t.Fatalf("the two reports have %d and %d floor rows", len(sum.Floors), len(rep.Floors))
	}
	for i := range sum.Floors {
		if sum.Floors[i] != rep.Floors[i] {
			t.Errorf("floor %.2f: per-condition table says %+v, deep report says %+v",
				sum.Floors[i].Floor, sum.Floors[i], rep.Floors[i])
		}
	}
	if rep.Floors[len(rep.Floors)-1].Results > scoreK {
		t.Errorf("floor %.2f reports %.2f results per query, more than the %d-row window",
			rep.Floors[len(rep.Floors)-1].Floor, rep.Floors[len(rep.Floors)-1].Results, scoreK)
	}
	t.Logf("no-answer queries against the graded corpus:\n%s", FormatFalsePositives(rep, CondHybrid))
}

// measureNoAnswers runs a set of no-answer queries through the shipped hybrid
// path and returns their measurements, which is what the runner hands the report.
// Tests that only care about the report build their input this way rather than
// searching again, so the report and the per-condition rate are always the same
// numbers.
func measureNoAnswers(t *testing.T, store *memory.Store, noAnswer []Query) []NoAnswerQuery {
	t.Helper()
	ctx := context.Background()
	rank := func(q Query) ([]string, error) {
		results, err := store.SearchHybrid(ctx, q.ProjectID, q.Text, q.Vector, scoreK)
		if err != nil {
			return nil, err
		}
		ids := make([]string, len(results))
		for i, m := range results {
			ids[i] = m.ID
		}
		return ids, nil
	}
	out := make([]NoAnswerQuery, 0, len(noAnswer))
	for _, q := range noAnswer {
		m, err := measureNoAnswer(ctx, store, q, rank)
		if err != nil {
			t.Fatalf("measure %s: %v", q.Name, err)
		}
		out = append(out, m)
	}
	return out
}

// answerableScores returns the runner's per-query scores for the shipped hybrid
// path over a small query set, which is what FalsePositives reads for its
// answerable contrast. It runs the same condition Run does, so the contrast and
// the graded table come off one search.
func answerableScores(t *testing.T, store *memory.Store, answerable []Query) []QueryScore {
	t.Helper()
	if len(answerable) == 0 {
		return nil
	}
	ctx := context.Background()
	res, err := runCondition(ctx, store, CondHybrid, answerable, func(q Query) ([]string, error) {
		results, err := store.SearchHybrid(ctx, q.ProjectID, q.Text, q.Vector, scoreK)
		if err != nil {
			return nil, err
		}
		ids := make([]string, len(results))
		for i, m := range results {
			ids[i] = m.ID
		}
		return ids, nil
	})
	if err != nil {
		t.Fatalf("answerable scores: %v", err)
	}
	return res.PerQuery
}

// vectorMemory is one row of a store built to order search deliberately: the
// vector is stored verbatim, so a test controls the exact cosine; ageDays
// backdates created_at, because the decay factor reorders the fused window and
// that is what decides which leg ends up on top; and global puts the row in the
// shared project, where a project-scoped search demotes it.
type vectorMemory struct {
	key, content, category string
	vec                    []float32
	ageDays                int
	global                 bool
}

// seedVectorStore creates a project (and the shared one, for global rows) and
// stores one memory per entry, returning the store and the key→store-ID map.
func seedVectorStore(t *testing.T, project string, mems []vectorMemory) (*memory.Store, map[string]string) {
	t.Helper()
	store, db := newSeedingStore(t)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, project, "/bench/"+project, project); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := store.EnsureProject(ctx, globalProject, globalProject, "global"); err != nil {
		t.Fatalf("EnsureProject global: %v", err)
	}
	ids := make(map[string]string, len(mems))
	for _, m := range mems {
		projectID := project
		if m.global {
			projectID = globalProject
		}
		id, err := store.Create(ctx, projectID, memory.Memory{
			Category: m.category, Content: m.content, Importance: 0.7, Source: "mcp",
		})
		if err != nil {
			t.Fatalf("create %s: %v", m.key, err)
		}
		if err := store.StoreEmbedding(ctx, id, m.vec, "bench"); err != nil {
			t.Fatalf("embed %s: %v", m.key, err)
		}
		if m.ageDays > 0 {
			if err := backdate(ctx, db, id, m.ageDays); err != nil {
				t.Fatalf("backdate %s: %v", m.key, err)
			}
		}
		ids[m.key] = id
	}
	return store, ids
}

// unitVector returns a 2-dim unit vector whose cosine with (1,0) is exactly c,
// so a test can state the score it expects instead of approximating it.
func unitVector(c float64) []float32 {
	return []float32{float32(c), float32(math.Sqrt(1 - c*c))}
}

// TestFalsePositivesScoresKeywordOnlyResults is the case a truncated vector list
// gets wrong. The top hybrid result arrives on the keyword leg alone: it matches
// the query text, and its cosine puts it below a short vector list, so the
// keyword reservation is the only reason it is in the window at all. Reading its
// score out of a truncated list gives 0, and a result scored 0 counts below every
// floor — so the report undercounts precisely the rows that came from the leg
// with no score of its own.
func TestFalsePositivesScoresKeywordOnlyResults(t *testing.T) {
	store, ids, kwonlyCosine, _ := keywordOnlyFixture(t)
	ctx := context.Background()
	q := Query{Name: "n1", ProjectID: "floorkeyword", Text: "zorbulon ring", Vector: unitVector(1)}

	results, err := store.SearchHybrid(ctx, q.ProjectID, q.Text, q.Vector, scoreK)
	if err != nil {
		t.Fatalf("hybrid: %v", err)
	}
	if len(results) == 0 || results[0].ID != ids["kwonly"] {
		t.Fatalf("expected the keyword-only row at the top of the window, got %d results starting with %v", len(results), resultKeys(results, ids))
	}
	// Every row in this fixture sits at or above the lowest floor, so every
	// returned result has to clear it. Counting fewer means one of them was
	// scored as a non-match.
	rep, err := FalsePositives(measureNoAnswers(t, store, []Query{q}), answerableScores(t, store, nil))
	if err != nil {
		t.Fatalf("FalsePositives: %v", err)
	}
	lowest := FalsePositiveFloors[0]
	if got := floorOf(t, rep, lowest).Results; got != float64(len(results)) {
		t.Errorf("floor %.2f counts %.0f results, want all %d returned; the top result is keyword-only (%v), scored %.2f",
			lowest, got, len(results), resultKeys(results, ids), kwonlyCosine)
	}
	if got := floorOf(t, rep, lowest).Queries; got != 1 {
		t.Errorf("floor %.2f counts %d queries with a hit, want 1", lowest, got)
	}
}

// keywordOnlyFixture builds a store where the top hybrid result reaches the window
// on the keyword leg alone, and returns that row's own cosine alongside the best
// cosine the vector leg found — two different numbers, which is the point.
//
// 24 rows outrank the keyword-only row on cosine, so the vector leg's fetched
// window cannot contain it; they are old and in a decaying category, so decay
// damps their fused score below the keyword-only row's, which is recent and in a
// never-decay category. The keyword reservation then puts it on top of a window it
// does not dominate semantically.
func keywordOnlyFixture(t *testing.T) (store *memory.Store, ids map[string]string, kwonlyCosine, legBest float32) {
	t.Helper()
	const project = "floorkeyword"
	var mems []vectorMemory
	for i := range 24 {
		mems = append(mems, vectorMemory{
			key:      fmt.Sprintf("bg%02d", i),
			content:  fmt.Sprintf("Background row %d covers subsystem telemetry counters.", i),
			category: "gotcha",
			vec:      unitVector(0.95 - float64(i)*0.001),
			ageDays:  400,
		})
	}
	mems = append(mems, vectorMemory{
		key:      "kwonly",
		content:  "The zorbulon ring is stored in the widget cache.",
		category: "fact", // never decays, so this row outranks the damped pool
		vec:      unitVector(0.5),
		ageDays:  5,
	})
	store, ids = seedVectorStore(t, project, mems)

	// Assert the scenario rather than assume it: if the keyword-only row ever
	// lands inside the vector leg's fetched window, the tests below would pass
	// without covering the case they exist for.
	fetched, err := store.SearchVector(context.Background(), project, unitVector(1), scoreK*2)
	if err != nil {
		t.Fatalf("vector leg: %v", err)
	}
	if len(fetched) == 0 {
		t.Fatal("vector leg returned nothing, so the fixture is not exercising anything")
	}
	for _, s := range fetched {
		if s.MemoryID == ids["kwonly"] {
			t.Fatal("the keyword-only row is inside the vector leg's fetched window, so this fixture no longer covers the case")
		}
	}
	return store, ids, 0.5, fetched[0].Score
}

// TestScoredWindowScoresTheWindowItReturns pins the definition both halves of
// the report share: the score is the best cosine among the rows production
// returns, not the vector leg's own best hit.
//
// The fixture keeps the leg's strongest match out of the window entirely — a
// shared _global row the status demotion halves, against twelve project rows that
// outrank it once demoted — so the two readings are different numbers and a
// report that took the leg's ranking would be measuring a row the caller never
// sees. That is the only way the two definitions are distinguishable here; in the
// keyword-only fixture they coincide, because its strongest background row is also
// inside the window.
func TestScoredWindowScoresTheWindowItReturns(t *testing.T) {
	const project = "windowtop"
	var mems []vectorMemory
	for i := range 12 {
		mems = append(mems, vectorMemory{
			key:      fmt.Sprintf("live%02d", i),
			content:  fmt.Sprintf("Live project row %d about the fleet scheduler.", i),
			category: "fact",
			vec:      unitVector(0.90 - float64(i)*0.001),
			ageDays:  5,
		})
	}
	mems = append(mems, vectorMemory{
		key:      "globalbest",
		content:  "A shared row that is the strongest vector match in the store.",
		category: "fact",
		vec:      unitVector(0.99), // the leg's best hit by a wide margin
		ageDays:  5,
		global:   true,
	})
	store, ids := seedVectorStore(t, project, mems)
	ctx := context.Background()
	q := Query{Name: "n1", ProjectID: project, Text: "fleet scheduler", Vector: unitVector(1)}

	results, err := store.SearchHybrid(ctx, q.ProjectID, q.Text, q.Vector, scoreK)
	if err != nil {
		t.Fatalf("hybrid: %v", err)
	}
	ranked := make([]string, len(results))
	for i, m := range results {
		ranked[i] = m.ID
	}
	cosines, err := resultCosines(ctx, store, q.Vector, ranked)
	if err != nil {
		t.Fatalf("resultCosines: %v", err)
	}
	var top float64
	for _, c := range cosines {
		top = math.Max(top, float64(c))
	}
	for _, m := range results {
		if m.ID == ids["globalbest"] {
			t.Skip("the shared row made the window after all, so this fixture no longer separates the two readings")
		}
	}
	legBest, err := store.SearchVector(ctx, project, q.Vector, 1)
	if err != nil {
		t.Fatalf("vector leg: %v", err)
	}
	if len(legBest) == 0 || float64(legBest[0].Score) <= top {
		t.Fatalf("fixture no longer separates the readings: leg best %.4f, window top %.4f", legBest[0].Score, top)
	}
	if want := 0.90 - 0*0.001; math.Abs(top-want) > 1e-6 {
		t.Errorf("top = %.4f, want %.4f — the best cosine among the returned rows, not the leg's best hit (%.4f), which is not in the window",
			top, want, legBest[0].Score)
	}
	if _, scored := cosines[ids["globalbest"]]; scored {
		t.Error("a row outside the window was scored")
	}

	// And the answerable half of the report has to measure the same thing, or
	// the gap between the two distributions is between two different
	// definitions rather than between two kinds of query. Reading this store's
	// answerable side off the leg's ranking instead of the window would report
	// 0.99 against the no-answer set's own window reading.
	// Graded, because the answerable scores the report reads are the runner's
	// per-query record and the runner only records a query it scored — a query
	// with an empty rel is measured as a no-answer query instead. The committed
	// corpus's answerable set is graded, so this is the faithful shape.
	answerable := Query{
		Name: "a1", ProjectID: project, Text: "fleet scheduler", Vector: unitVector(1),
		Rel: Relevance{ids["live00"]: 1},
	}
	noAnswer := []Query{{Name: "n1", ProjectID: project, Text: "nothing matches this", Vector: unitVector(1)}}
	rep, err := FalsePositives(measureNoAnswers(t, store, noAnswer), answerableScores(t, store, []Query{answerable}))
	if err != nil {
		t.Fatalf("FalsePositives: %v", err)
	}
	if math.Abs(rep.AnswerableTop-top) > 1e-6 {
		t.Errorf("AnswerableTop = %.4f, want %.4f — the answerable contrast must be scored over the returned window like the no-answer set, not over the vector leg's ranking (%.4f)",
			rep.AnswerableTop, top, legBest[0].Score)
	}
}

// TestCountHitsAboveFloorIsStrict pins the boundary rule directly, which the
// corpus cannot do: real cosines essentially never land exactly on 0.3/0.4/0.5,
// so this is the only place the strictness is enforced. A floor at 0.3 keeps a
// candidate scored above it, and drops one sitting on it — the same rule
// memory.filterVectorFloor applies, and the one NoAnswerMax/Unseparable is
// measured against. A returned row with no score in the map is not counted
// either: it has no comparable cosine, which reads as 0, below every floor.
func TestCountHitsAboveFloorIsStrict(t *testing.T) {
	cosines := map[string]float32{"on": 0.3, "above": 0.30001, "below": 0.29999, "unscored": 0}
	if got := countHitsAboveFloor(cosines, 0.3); got != 1 {
		t.Errorf("countHitsAboveFloor(0.3) = %d, want 1: only the row strictly above the floor counts, and an unscored row counts as no match", got)
	}
	if got := countHitsAboveFloor(cosines, 0.29999); got != 2 {
		t.Errorf("countHitsAboveFloor(0.29999) = %d, want 2 (the row on 0.3 and the one above it)", got)
	}
	// A row the caller was shown but whose vector could not be scored must not
	// be counted as a match, which is the whole reason the report scores rows
	// from their own stored vectors rather than from a leg's list.
	if got := countHitsAboveFloor(map[string]float32{}, 0.0); got != 0 {
		t.Errorf("countHitsAboveFloor over no scored rows = %d, want 0", got)
	}
}

// TestFalsePositivesUnseparableCountsExactTies pins the boundary convention.
// Ghost's production floor keeps a candidate when its score is strictly above the
// floor (filterVectorFloor), so a floor set at the no-answer maximum already
// refuses that query — and refuses an answerable query whose best score ties it.
// Counting the tie as separable would understate what such a floor costs, which
// is the number the abstention work reads.
func TestFalsePositivesUnseparableCountsExactTies(t *testing.T) {
	const project = "floodtie"
	// One memory, and both queries point the same way, so the two best cosines
	// are equal by construction rather than by luck.
	store, _ := seedVectorStore(t, project, []vectorMemory{{
		key: "only", content: "The only memory in this store.", category: "fact", vec: unitVector(1),
	}})
	q := unitVector(1)
	noAnswer := []Query{{Name: "n1", ProjectID: project, Text: "anything", Vector: q}}
	rep, err := FalsePositives(measureNoAnswers(t, store, noAnswer),
		answerableScores(t, store, []Query{{Name: "a1", ProjectID: project, Text: "only memory", Vector: q, Rel: Relevance{"x": 1}}}))
	if err != nil {
		t.Fatalf("FalsePositives: %v", err)
	}
	if rep.NoAnswerMax <= 0 {
		t.Fatalf("no-answer maximum is %.3f, so the tie was never established", rep.NoAnswerMax)
	}
	if rep.Unseparable != 1 {
		t.Errorf("Unseparable = %d, want 1: an answerable query whose best score ties the no-answer maximum is refused by any floor that refuses every no-answer query", rep.Unseparable)
	}
}

func floorOf(t *testing.T, rep FalsePositiveReport, floor float32) FloorCount {
	t.Helper()
	for _, f := range rep.Floors {
		if f.Floor == floor {
			return f
		}
	}
	t.Fatalf("report has no row for floor %.2f: %+v", floor, rep.Floors)
	return FloorCount{}
}

func resultKeys(results []memory.Memory, ids map[string]string) []string {
	out := make([]string, len(results))
	for i, m := range results {
		for key, id := range ids {
			if m.ID == id {
				out[i] = key
				break
			}
		}
	}
	return out
}

func TestLoadNegativesRejectsGradedQueries(t *testing.T) {
	cases := []struct {
		name, line, want string
	}{
		{"no name", `{"text":"t","flavor":"off_domain","rel":{}}`, "empty name"},
		{"no text", `{"name":"n","flavor":"off_domain","rel":{}}`, "no text"},
		{"no flavor", `{"name":"n","text":"t","rel":{}}`, "no flavor"},
		{"grades a memory", `{"name":"n","text":"t","flavor":"off_domain","rel":{"a":1}}`, "rel must stay empty"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := LoadNegatives(strings.NewReader(c.line)); err == nil {
				t.Fatalf("LoadNegatives accepted %s", c.line)
			} else if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestNegativeQueriesRequireVectors(t *testing.T) {
	ds := Dataset{Project: "p", Negatives: []NegativeQuery{{
		QuerySpec: QuerySpec{Name: "n_missing", Text: "t"},
		Flavor:    "off_domain",
	}}}
	if _, err := NegativeQueries(ds, Vectors{}); err == nil {
		t.Fatal("a no-answer query with no fixture vector was accepted")
	} else if !strings.Contains(err.Error(), "no fixture vector") {
		t.Errorf("error %q does not name the missing vector", err)
	}
}
