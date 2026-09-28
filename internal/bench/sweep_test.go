package bench

import (
	"context"
	"fmt"
	"math"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// sweepFixture seeds the committed dataset into a fresh store, mirroring what
// runBench does before a sweep.
func sweepFixture(t *testing.T) (*memory.Store, []Query) {
	t.Helper()
	ds, vecs := loadTestdataDataset(t)
	store, db := newBenchStoreWithDB(t)
	ctx := context.Background()
	queries, err := Seed(ctx, store, db, ds, vecs)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return store, queries
}

// tinySweepFixture is a four-memory corpus with two queries, for the sweep
// behaviours that are about the CODEPATH (which point is the reference, what
// happens when there is none) rather than about this corpus's ranking. The
// committed dataset's 551 embeddings are the expensive part of this package and
// both of those properties hold over four memories exactly as well.
func tinySweepFixture(t *testing.T) (*memory.Store, []Query) {
	t.Helper()
	dim := 8
	vec := func(vals ...float32) []float32 {
		out := make([]float32, dim)
		copy(out, vals)
		return out
	}
	ds := Dataset{
		Project: "sweep-tiny",
		Memories: []MemorySpec{
			{Key: "k8s", Category: "fact", Content: "kubernetes cluster upgrade", Importance: 0.7},
			{Key: "pg", Category: "fact", Content: "postgres connection pool", Importance: 0.7},
			{Key: "lb", Category: "fact", Content: "load balancer health check", Importance: 0.7},
			{Key: "dns", Category: "fact", Content: "dns resolver timeout", Importance: 0.7},
		},
		Queries: []QuerySpec{
			{Name: "q_k8s", Text: "kubernetes upgrade", Rel: map[string]int{"k8s": 1}},
			{Name: "q_pg", Text: "postgres pool", Rel: map[string]int{"pg": 1}},
		},
	}
	vecs := Vectors{
		// one per memory and one per query: Seed refuses a query with no
		// fixture vector rather than scoring it with a zero vector.
		"k8s":   vec(1, 0, 0, 0, 0, 0, 0, 0),
		"pg":    vec(0, 1, 0, 0, 0, 0, 0, 0),
		"lb":    vec(0, 0, 1, 0, 0, 0, 0, 0),
		"dns":   vec(0, 0, 0, 1, 0, 0, 0, 0),
		"q_k8s": vec(1, 0, 0, 0, 0, 0, 0, 0),
		"q_pg":  vec(0, 1, 0, 0, 0, 0, 0, 0),
	}
	store, db := newBenchStoreWithDB(t)
	queries, err := Seed(context.Background(), store, db, ds, vecs)
	if err != nil {
		t.Fatalf("seed tiny corpus: %v", err)
	}
	if len(queries) == 0 {
		t.Fatal("the tiny corpus produced no queries")
	}
	return store, queries
}

func TestSweep(t *testing.T) {
	store, queries := sweepFixture(t)
	ctx := context.Background()

	// Two points: the production default and an off-default leg weighting.
	def := memory.DefaultSearchParams()
	alt := def
	alt.VecWeight = 0.9
	alt.FTSWeight = 0.1
	points, err := Sweep(ctx, store, queries, []memory.SearchParams{def, alt})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("got %d points, want 2", len(points))
	}

	// Sorted by NDCG@10 descending.
	if points[0].Result.NDCG10 < points[1].Result.NDCG10 {
		t.Errorf("points not sorted by NDCG: %.3f then %.3f", points[0].Result.NDCG10, points[1].Result.NDCG10)
	}

	// Cross-check the default point against the ablation runner: default
	// params == the hybrid ablation.
	byCond := byCondition(runTestdata(t))
	find := func(p memory.SearchParams) Result {
		for _, pt := range points {
			// DeepEqual, not ==: SearchParams carries a Scope map and is
			// therefore not comparable with ==.
			if reflect.DeepEqual(pt.Params, p) {
				return pt.Result
			}
		}
		t.Fatalf("sweep point not found for %+v", p)
		return Result{}
	}
	if got, want := find(def).NDCG10, byCond[CondHybrid].NDCG10; got != want {
		t.Errorf("default sweep point NDCG %.6f != hybrid ablation %.6f", got, want)
	}
}

func TestSweepGrid(t *testing.T) {
	grid := SweepGrid()
	if len(grid) != 6 {
		t.Fatalf("grid size %d, want 6 (6 vec weights)", len(grid))
	}
	def := memory.DefaultSearchParams()
	foundDefault := 0
	for _, p := range grid {
		if got := p.FTSWeight + p.VecWeight; got < 0.999 || got > 1.001 {
			t.Errorf("leg weights not normalized: fts=%.2f vec=%.2f", p.FTSWeight, p.VecWeight)
		}
		if p.RRFK != def.RRFK {
			t.Errorf("non-swept knobs must stay at defaults: %+v", p)
		}
		if reflect.DeepEqual(p, def) {
			foundDefault++
		}
	}
	if foundDefault == 0 {
		t.Error("grid must include the production default point")
	}
	// Exactly one, because the sweep's interval column compares every other point
	// against the default (SweepPoint.VsDefault): a grid listing it twice makes the
	// second one a comparison against a copy of itself. Free to check here, and
	// the assertion pairAgainstDefault's comment leans on.
	if foundDefault > 1 {
		t.Errorf("grid lists the production default %d times; every point but one is measured against it", foundDefault)
	}
}

// TestSweepPairsEveryOtherPointAgainstTheDefaultAndPrintsIt: the sweep sorts by
// NDCG@10, which is a point-estimate order, and a point estimate is not a finding.
// Every point but the default therefore carries the paired per-query difference
// against the default, and the report prints it — the claim in docs/benchmarks.md
// that the grid's top four points are not separable rests on this column, so it has
// to be in the printed table at the precision the doc quotes.
//
// One test because one seed is the expensive part; and the four-memory fixture,
// NOT the committed corpus, which is a budget decision rather than a shortcut.
// Measured on this machine with `go test -race ./internal/bench -count=1`: the
// committed 551-memory corpus puts this package's test binary at 471s on this
// branch (478s on origin/main, before the commits that brought the ceiling
// problem in), against Go's 600s per-binary default — so ~130s is all the headroom
// there is. A second full seed of 551 memories plus a six-point sweep measured
// 15s without -race; at this tree's race factor that is ~230s, which added to
// 471s is the timeout. Nothing here is corpus-dependent: which point is the
// reference, that the interval is the one recomputed from the same two Results,
// that it reaches the report. A four-memory corpus pins all of it. The
// corpus-scale half of the claim is held elsewhere and for free — TestSweepGrid
// pins the grid's six points and the default's presence without touching a store,
// and TestBenchmarksDocSweepTableMatchesTheReport pins the published table's six
// rows. The real numbers come from `ghost bench --sweep`, which is where the doc
// says they come from.
func TestSweepPairsEveryOtherPointAgainstTheDefaultAndPrintsIt(t *testing.T) {
	store, queries := tinySweepFixture(t)
	ctx := context.Background()

	wide := memory.DefaultSearchParams()
	wide.VecWeight, wide.FTSWeight = 0.8, 0.2
	narrow := memory.DefaultSearchParams()
	narrow.VecWeight, narrow.FTSWeight = 0.3, 0.7
	grid := []memory.SearchParams{memory.DefaultSearchParams(), wide, narrow}
	for _, p := range grid[1:] {
		if isDefaultParams(p) {
			t.Fatal("the fixture grid is supposed to hold exactly one default")
		}
	}

	points, err := Sweep(ctx, store, queries, grid)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	var def *SweepPoint
	for i := range points {
		if isDefaultParams(points[i].Params) {
			if def != nil {
				t.Fatalf("two points are the default (positions %d and beyond); one of them is compared against itself", i)
			}
			def = &points[i]
		}
	}
	if def == nil {
		t.Fatal("the grid must contain the default, so there is something to pair against")
	}

	// The data side.
	for i := range points {
		pt := &points[i]
		if pt == def {
			if pt.VsDefault != nil {
				t.Errorf("%s is the default and carries an interval against itself: %+v", pt.Result.Condition, *pt.VsDefault)
			}
			continue
		}
		if pt.VsDefault == nil {
			t.Errorf("%s carries no interval against the default, so its position in the sort is a bare point estimate", pt.Result.Condition)
			continue
		}
		want, err := pairedCI(pt.Result, def.Result)
		if err != nil {
			t.Fatalf("recomputing the interval for %s: %v", pt.Result.Condition, err)
		}
		if *pt.VsDefault != want {
			t.Errorf("%s interval %+v, recomputed %+v", pt.Result.Condition, *pt.VsDefault, want)
		}
		if pt.VsDefault.Queries != len(pt.Result.PerQuery) {
			t.Errorf("%s paired %d queries, scored %d", pt.Result.Condition, pt.VsDefault.Queries, len(pt.Result.PerQuery))
		}
		if pt.VsDefault.Lo > pt.VsDefault.Mean || pt.VsDefault.Hi < pt.VsDefault.Mean {
			t.Errorf("%s interval [%+.4f, %+.4f] does not contain its mean %+.4f",
				pt.Result.Condition, pt.VsDefault.Lo, pt.VsDefault.Hi, pt.VsDefault.Mean)
		}
	}

	// The text side.
	out := FormatSweep(points)
	if !strings.Contains(out, "vs default (paired 95%)") {
		t.Errorf("the table has no interval column:\n%s", out)
	}
	seen := 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "vec=") {
			continue
		}
		seen++
		cond := strings.Fields(line)[0]
		// The default row is found by the default point's own condition name, not
		// by a hardcoded vec weight: if the shipped default moves, this has to keep
		// testing the row that moved with it rather than silently stop applying.
		if cond == def.Result.Condition {
			if !strings.Contains(line, "this is the default") {
				t.Errorf("the default row does not say it is the reference row:\n%s", line)
			}
			if intervalCellRE.MatchString(line) {
				t.Errorf("the default row also printed an interval against itself:\n%s", line)
			}
			continue
		}
		m := intervalCellRE.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("row %q carries no interval at four decimals:\n%s", cond, line)
			continue
		}
		// Parsed, not compared as strings: "-0.0043" sorts AFTER "+0.0030"
		// bytewise, so a lexicographic lo<=hi check passes every inverted
		// interval this column could print.
		mean, err1 := strconv.ParseFloat(m[1], 64)
		lo, err2 := strconv.ParseFloat(m[2], 64)
		hi, err3 := strconv.ParseFloat(m[3], 64)
		if err1 != nil || err2 != nil || err3 != nil {
			t.Errorf("row %s prints unparseable numbers: %v %v %v", cond, err1, err2, err3)
			continue
		}
		if lo > hi {
			t.Errorf("row %s prints an inverted interval: %s", cond, line)
		}
		if lo > mean || hi < mean {
			t.Errorf("row %s prints an interval that excludes its own mean: %s", cond, line)
		}
		// The interval is the point MINUS the default, so a point the sort put
		// above the default must not print a mean that says it lost. This is
		// the sign convention the report has to keep, stated as a check.
		if pt := findPoint(points, cond); pt != nil && mean < 0 && pt.Result.NDCG10 > def.Result.NDCG10 {
			t.Errorf("%s scores above the default (%.4f > %.4f) and reports a negative difference: %s",
				cond, pt.Result.NDCG10, def.Result.NDCG10, line)
		}
	}
	if seen != len(points) {
		t.Errorf("parsed %d rows, want %d", seen, len(points))
	}
}

// TestSweepMeasuresAgainstTheDefaultNotTheFirstRow: the reference for every
// point's interval is the point holding the shipped default, never whichever
// point the sort placed first. Those are different points whenever the default
// is not the top scorer, and the regression this guards is one this branch has
// already had: comparing against points[0] passes every numeric assertion on a
// fixture whose points all tie, because every per-query difference is then
// exactly zero.
//
// Hand-built points, not the committed corpus and not the four-memory fixture,
// for two reasons: the values have to be KNOWN (a real search cannot be told
// which point should win by how many), and this is pure arithmetic over
// Result.PerQuery, so it costs no store and no seed.
func TestSweepMeasuresAgainstTheDefaultNotTheFirstRow(t *testing.T) {
	wide := memory.DefaultSearchParams()
	wide.VecWeight, wide.FTSWeight = 0.8, 0.2
	narrow := memory.DefaultSearchParams()
	narrow.VecWeight, narrow.FTSWeight = 0.3, 0.7

	// The default scores 0.5 on every query, so whichever way the sort runs,
	// points[0] is NOT what the others must be compared against.
	per := func(vals ...float64) []QueryScore {
		out := make([]QueryScore, len(vals))
		for i, v := range vals {
			out[i] = QueryScore{Name: fmt.Sprintf("q%d", i), NDCG: v}
		}
		return out
	}
	// `wide` beats the default on q0 and loses on q1: a mean of zero with a
	// non-degenerate interval, so a wrong reference shows up as a different
	// pair of edges rather than as an all-zero one.
	widePt := SweepPoint{
		Params: wide,
		Result: Result{Condition: "vec=0.80", NDCG10: 0.75, PerQuery: per(1.0, 0.0)},
	}
	defPt := SweepPoint{
		Params: memory.DefaultSearchParams(),
		Result: Result{Condition: "vec=0.70", NDCG10: 0.50, PerQuery: per(0.5, 0.5)},
	}
	narrowPt := SweepPoint{
		Params: narrow,
		Result: Result{Condition: "vec=0.30", NDCG10: 0.25, PerQuery: per(0.0, 0.0)},
	}
	// Sorted order deliberately puts the default in the MIDDLE: a reference
	// taken from points[0] would compare the other two against the wrong point,
	// and against each other's worst case at that.
	points := []SweepPoint{widePt, defPt, narrowPt}
	if err := pairAgainstDefault(points); err != nil {
		t.Fatalf("pairAgainstDefault: %v", err)
	}

	if points[1].VsDefault != nil {
		t.Errorf("the default point carries an interval against itself: %+v", *points[1].VsDefault)
	}
	for _, i := range []int{0, 2} {
		got := points[i].VsDefault
		if got == nil {
			t.Fatalf("%s has no interval", points[i].Result.Condition)
		}
		if got.Leg != "vec=0.70" {
			t.Errorf("%s is measured against %q, want the default's condition vec=0.70", points[i].Result.Condition, got.Leg)
		}
	}
	// wide - default = (0.5, -0.5): mean 0, and a real interval around it.
	if w := points[0].VsDefault; math.Abs(w.Mean) > 1e-12 {
		t.Errorf("vec=0.80 mean = %+.6f, want 0 (it wins one query and loses one)", w.Mean)
	}
	if w := points[0].VsDefault; w.Lo >= 0 || w.Hi <= 0 {
		t.Errorf("vec=0.80 interval [%+.4f, %+.4f] does not straddle zero; a two-query sample must", w.Lo, w.Hi)
	}
	// narrow - default = (-0.5, -0.5): entirely negative, which is a different
	// interval from wide's, and is what a points[0]-based comparison would lose.
	if n := points[2].VsDefault; n.Hi >= 0 {
		t.Errorf("vec=0.30 interval [%+.4f, %+.4f] reaches zero, but it loses both queries", n.Lo, n.Hi)
	}
	if w, n := points[0].VsDefault, points[2].VsDefault; w.Lo == n.Lo && w.Hi == n.Hi {
		t.Errorf("vec=0.80 and vec=0.30 share the interval [%+.4f, %+.4f]; the reference is the same point for both", w.Lo, w.Hi)
	}
}

// TestSweepRefusesAGridThatHoldsTheDefaultTwice: the two copies are
// indistinguishable, so "the first one" would be an artefact of the sort. The
// comparison is refused rather than made against a guess.
func TestSweepRefusesAGridThatHoldsTheDefaultTwice(t *testing.T) {
	wide := memory.DefaultSearchParams()
	wide.VecWeight, wide.FTSWeight = 0.8, 0.2
	mk := func(cond string, ndcg float64) Result {
		return Result{Condition: cond, NDCG10: ndcg, PerQuery: []QueryScore{{Name: "q0", NDCG: ndcg}}}
	}
	points := []SweepPoint{
		{Params: memory.DefaultSearchParams(), Result: mk("vec=0.70", 0.9)},
		{Params: wide, Result: mk("vec=0.80", 0.5)},
		{Params: memory.DefaultSearchParams(), Result: mk("vec=0.70", 0.4)},
	}
	err := pairAgainstDefault(points)
	if err == nil {
		t.Fatal("a grid holding the default twice was accepted")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("the error does not say what is wrong with the grid: %v", err)
	}
	// And it must not have half-filled the column on the way to failing.
	for i, p := range points {
		if i != 0 && p.VsDefault != nil {
			t.Errorf("point %d was paired before the grid was refused: %+v", i, *p.VsDefault)
		}
	}
}

// TestFormatSweepAsksTheGridWhetherItHasADerivedDefault: SweepPoint is exported,
// so FormatSweep can be handed a hand-built slice it never paired. Deciding
// "no default in grid" from one row's nil VsDefault let a slice that DOES hold
// the default print that claim beside it.
func TestFormatSweepAsksTheGridWhetherItHasADerivedDefault(t *testing.T) {
	wide := memory.DefaultSearchParams()
	wide.VecWeight, wide.FTSWeight = 0.8, 0.2
	points := []SweepPoint{
		// The default, unpaired — as a hand-built slice would carry it.
		{Params: memory.DefaultSearchParams(), Result: Result{Condition: "vec=0.70", NDCG10: 0.7}},
		// A non-default point with no interval.
		{Params: wide, Result: Result{Condition: "vec=0.80", NDCG10: 0.8}},
	}
	out := FormatSweep(points)
	if strings.Contains(out, "no default in grid") {
		t.Errorf("the grid DOES hold the default and the report says it does not:\n%s", out)
	}
	if !strings.Contains(out, "this is the default") {
		t.Errorf("the default row is not marked:\n%s", out)
	}
	// The unpaired non-default row says nothing rather than an interval.
	row := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "vec=0.80") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("no row for vec=0.80:\n%s", out)
	}
	if intervalCellRE.MatchString(row) {
		t.Errorf("a row with no interval printed one: %q", row)
	}
	if strings.Contains(row, "no default in grid") {
		t.Errorf("a row with no interval claimed the grid has no default, which is false: %q", row)
	}
}

// TestFormatSweepCaveatNamesNoWeightItWasNotGiven: the footer used to say
// "vec=0.50 here (#708), the other five reproduce exactly" for every grid it was
// handed, so a two-point grid with no 0.50 in it printed a claim about a row that
// was not on the page and a count that was not true. The caveat has to describe
// the mechanism and cite the issue, and must not name a weight or a count.
func TestFormatSweepCaveatNamesNoWeightItWasNotGiven(t *testing.T) {
	wide := memory.DefaultSearchParams()
	wide.VecWeight, wide.FTSWeight = 0.8, 0.2
	points := []SweepPoint{
		{Params: memory.DefaultSearchParams(), Result: Result{Condition: "vec=0.70", NDCG10: 0.7, Queries: 2}},
		{Params: wide, Result: Result{Condition: "vec=0.80", NDCG10: 0.8, Queries: 2}},
	}
	out := FormatSweep(points)
	caveat := out[strings.Index(out, "Caveat"):]
	if caveat == "" {
		t.Fatalf("the report prints no caveat at all:\n%s", out)
	}
	if !strings.Contains(caveat, "#708") {
		t.Errorf("the caveat does not point at the issue that tracks it:\n%s", caveat)
	}
	// No weight, and no count of the other rows.
	if strings.Contains(caveat, "vec=") {
		t.Errorf("the caveat names a specific weight, which is a claim about a grid it was not given:\n%s", caveat)
	}
	if strings.Contains(caveat, "other five") || strings.Contains(caveat, "other ") {
		t.Errorf("the caveat counts rows, which is a claim about a grid it was not given:\n%s", caveat)
	}
}

// findPoint returns the sweep point printed as cond, or nil.
func findPoint(points []SweepPoint, cond string) *SweepPoint {
	for i := range points {
		if points[i].Result.Condition == cond {
			return &points[i]
		}
	}
	return nil
}

// TestSweepWithoutADefaultComparesAgainstNothing: a grid with no shipped default
// has no point that is the one in production, so pairing the members against each
// other would compare against a guess. The sweep must decline rather than pick one,
// and the rows must say the comparison was not made.
//
// A hand-built four-memory store rather than the committed corpus: what is under
// test is the absence of a reference point, which no amount of ranking makes more
// true, and the corpus seed is the expensive part of this package.
func TestSweepWithoutADefaultComparesAgainstNothing(t *testing.T) {
	ctx := context.Background()
	store, queries := tinySweepFixture(t)

	alt := memory.DefaultSearchParams()
	alt.VecWeight, alt.FTSWeight = 0.9, 0.1
	far := memory.DefaultSearchParams()
	far.VecWeight, far.FTSWeight = 0.3, 0.7
	if isDefaultParams(alt) || isDefaultParams(far) {
		t.Fatal("the fixture grid is supposed to exclude the default")
	}

	points, err := Sweep(ctx, store, queries, []memory.SearchParams{alt, far})
	if err != nil {
		t.Fatalf("Sweep over a grid with no default: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("got %d points, want 2", len(points))
	}
	for _, pt := range points {
		if pt.VsDefault != nil {
			t.Errorf("%s carries an interval against an unspecified default: %+v", pt.Result.Condition, *pt.VsDefault)
		}
	}
	if got := FormatSweep(points); !strings.Contains(got, "no default in grid") {
		t.Errorf("the report does not say the comparison was not made:\n%s", got)
	}
}

// intervalCellRE matches the interval a sweep row prints: a signed mean and a
// signed pair of edges, at the four decimals the report and the published table
// both use. Package-level because two tests read the same column — one off the
// report, one off the doc — and two copies of a regex is one more thing to keep
// in step.
var intervalCellRE = regexp.MustCompile(`([+-]\d\.\d{4}) \[([+-]\d\.\d{4}), ([+-]\d\.\d{4})\]`)

// TestBenchmarksDocSweepTableMatchesTheReport: docs/benchmarks.md publishes this
// table, and before #561 the sweep's separability claim lived in a sentence
// describing a paired bootstrap that no command performed — a published number
// with nothing behind it, which is the failure this whole issue is about. The
// claim now lives in the report, so the doc has to hold the shape the report
// prints: the same header, one row per grid point, and an interval on every row
// except the reference one.
//
// The header is compared against sweepHeader rather than a literal here, so
// renaming a column fails this test until the doc's table is re-quoted.
func TestBenchmarksDocSweepTableMatchesTheReport(t *testing.T) {
	const doc = "../../docs/benchmarks.md"
	raw, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("read %s: %v", doc, err)
	}
	const anchor = "### Parameter sweep"
	i := strings.Index(string(raw), anchor)
	if i < 0 {
		t.Fatalf("no %q section in %s; the test cannot tell whether the published table is stale", anchor, doc)
	}
	section := string(raw)[i:]
	// The table is the first fenced block in the section.
	start := strings.Index(section, "```")
	if start < 0 {
		t.Fatalf("no fenced table in the %s section of %s", anchor, doc)
	}
	body := section[start+3:]
	if end := strings.Index(body, "```"); end >= 0 {
		body = body[:end]
	}

	lines := strings.Split(strings.TrimSpace(body), "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], " ") != sweepHeader() {
		t.Errorf("the published table header is not the one the report prints\n got: %q\nwant: %q", lines[0], sweepHeader())
	}
	rowRE := regexp.MustCompile(`^vec=\d\.\d\d\b`)
	rows := 0
	for _, line := range lines[1:] {
		if !rowRE.MatchString(line) {
			continue
		}
		rows++
		cond := strings.Fields(line)[0]
		// Derived from the shipped default, not written down: the sibling test
		// states that rule for the report, and a hardcoded "vec=0.70" here would
		// keep checking a stale doc row if the default's leg weight ever moved.
		if cond == defaultCondition() {
			if !strings.Contains(line, "this is the default") {
				t.Errorf("the published default row does not mark itself the reference: %q", line)
			}
			if !strings.Contains(line, "<- current default") {
				t.Errorf("the published default row is not marked as the current default: %q", line)
			}
			continue
		}
		if !intervalCellRE.MatchString(strings.TrimRight(line, " ")) {
			t.Errorf("the published row %s carries no interval at four decimals: %q", cond, line)
		}
	}
	if want := len(SweepGrid()); rows != want {
		t.Errorf("the published table has %d grid rows, want %d", rows, want)
	}
}

// defaultCondition is the condition name the sweep prints for the shipped
// default, derived from it rather than written down. A hardcoded "vec=0.70"
// makes every check that names it silently stop applying — or keep applying to a
// stale row — if the default's leg weight ever moves.
func defaultCondition() string {
	p := memory.DefaultSearchParams()
	return fmt.Sprintf("vec=%.2f", p.VecWeight)
}
