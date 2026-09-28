package bench

import (
	"context"
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
// committed dataset's 547 embeddings are the expensive part of this package and
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
// This package's race-instrumented test binary runs 471s against Go's 600s
// ceiling, so ~130s is all the headroom there is, and a second full seed of 547
// memories plus a six-point sweep measured 15s without -race — roughly 220s with
// it, which is the timeout. Nothing here is corpus-dependent: which point is the
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
		if cond == "vec=0.70" {
			if !strings.Contains(line, "this is the default") {
				t.Errorf("the published default row does not mark itself the reference: %q", line)
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
