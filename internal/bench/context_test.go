package bench

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// contextFixtureInstant is the clock every test in this file measures at. It is a
// constant rather than time.Now() so a fixture row's validity state is a fact
// about the fixture: a test that passed because a window happened to be open
// today and failed in 2099 is a test with a date on it.
var contextFixtureInstant = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// contextFixtureAt is the fixed value form the corpus stamps are written in.
func contextFixtureAt(s string) *string { return &s }

// contextFixtureKey is the key of the `_global` row, which is NOT in the dataset:
// the seeder writes one project, so a global row has to be added beside it. It is
// a named constant because two helpers have to agree on it and a typo would show
// up as a fixture that quietly lost the row that makes diversity say anything.
const contextFixtureKey = "global_rule"

// contextFixture is the small corpus the context metrics are measured on, and it
// exists because the graded corpus cannot measure them.
//
// The 549-row corpus holds no resolved row, no `_global` row and no supersession
// edge — docs/benchmarks.md says so — and the only contamination arm reachable
// through a real assemble.Run is the resolved one, because stage 2 drops an
// expired or not-yet-valid row and stage 3 drops a scope contradiction before
// either can be admitted. So a corpus without a resolved row scores contamination
// 0.000, which is a true reading of a corpus that holds nothing contaminable and
// a measurement that cannot tell a filter working from a filter absent.
//
// The four validity rows are the spec's contamination fixture, and the two clean
// ones earn their place the way the two dirty ones cannot: `valid_current` states
// no window at all, and `valid_window_open` states one that contains the clock, so
// a classifier keyed on "this row has a validity column" flags both and fails here.
// The resolved row is what makes the rate non-zero, and the `_global` row is what
// makes diversity have something to divide.
//
// Two queries, and the shapes are chosen so every number in the report is
// explainable rather than merely observed:
//
//   - q_pool's vector sets every component, so it has a positive cosine with every
//     embedded row and the keyword leg names every one of them too — including
//     "sqlite", which is how a `_global` row reaches two of the three blocks and
//     gives the bucket table a query count as well as a row count.
//   - q_global's vector is orthogonal to every project row, so its whole block is
//     one keyword-leg hit on the global row. A fixture whose second query returned
//     the whole corpus could not tell a narrow block from a lopsided one.
//
// The admitted sets are therefore: q_pool the five project rows stage 2 leaves plus
// the global row (6), q_global the global row alone (1) — seven rows over two
// answered queries, of which three are graded relevant and one is contaminated.
func contextFixture(t *testing.T) (Dataset, Vectors) {
	t.Helper()
	dim := 8
	vec := func(at int) []float32 {
		out := make([]float32, dim)
		out[at] = 1
		return out
	}
	// One component per row, all set: a query whose vector points at every row
	// keeps the whole corpus inside the retrieval window, so the assertions below
	// are about the pipeline's verdicts rather than about which rows a leg
	// happened to rank.
	every := make([]float32, dim)
	for i := range every {
		every[i] = 1
	}
	ds := Dataset{
		Project: "ctx-tiny",
		Memories: []MemorySpec{
			{Key: "live_fact", Category: "fact", Content: "kubernetes cluster upgrade runs on tuesday", Importance: 0.7},
			{Key: "resolved_fact", Category: "fact", Content: "postgres connection pool size is 20", Importance: 0.7},
			{Key: "stale_fact", Category: "fact", Content: "dns resolver timeout is 5 seconds", Importance: 0.3},
			{Key: "valid_window_open", Category: "fact", Content: "backup snapshots horizon is 30 days", Importance: 0.5,
				ValidFrom: contextFixtureAt("2024-01-01"), ValidUntil: contextFixtureAt("2199-12-31"), VerifiedAt: contextFixtureAt("2026-01-01")},
			{Key: "valid_current", Category: "fact", Content: "wallet governance policy needs two reviewers", Importance: 0.5},
			{Key: "future_scheduled", Category: "fact", Content: "mirror cutover is scheduled for the next window", Importance: 0.5,
				ValidFrom: contextFixtureAt("2099-01-01"), ValidUntil: contextFixtureAt("2199-12-31")},
			{Key: "expired_policy", Category: "fact", Content: "the decommissioned dbsync window is closed", Importance: 0.5,
				ValidFrom: contextFixtureAt("2020-01-01"), ValidUntil: contextFixtureAt("2020-06-01")},
		},
		Queries: []QuerySpec{
			// One term per row, so every candidate is retrieved by the keyword leg
			// and the admitted set is the corpus minus the two rows stage 2 retires.
			{Name: "q_pool", Text: "kubernetes postgres dns backup wallet mirror decommissioned sqlite",
				Rel: map[string]int{"live_fact": 2, "resolved_fact": 3}},
			// Ungraded here and graded in contextFixtureStore, because the row it
			// answers about is written after the seeder has built the query's
			// relevance map.
			{Name: "q_global", Text: "sqlite fts5 storage engine"},
		},
	}
	vecs := Vectors{
		"live_fact": vec(0), "resolved_fact": vec(1), "stale_fact": vec(2),
		"valid_window_open": vec(3), "valid_current": vec(4),
		"future_scheduled": vec(5), "expired_policy": vec(6),
		"q_pool": every, "q_global": vec(7),
	}
	return ds, vecs
}

// contextFixtureStore seeds contextFixture, adds the `_global` row and the one
// resolved row the metrics need, and returns the store with the clock the
// measurement reads at. The global row goes in through EnsureProject + the same
// corpus write as every other row, so the `_global` bucket the report counts is one
// a production store could actually hold rather than one a test asserted into
// existence.
func contextFixtureStore(t *testing.T) (*memory.Store, []Query, time.Time) {
	t.Helper()
	ds, vecs := contextFixture(t)
	store, db := newBenchStoreWithDB(t)
	ctx := context.Background()
	queries, at, err := SeedAt(ctx, store, db, ds, vecs, contextFixtureInstant)
	if err != nil {
		t.Fatalf("seed the context fixture: %v", err)
	}
	seedContextGlobal(t, store, ds.Project)
	// The seeder translates a query's relevance keys from the dataset's own rows,
	// and the `_global` row is written after it returns — so q_global's one grade
	// is attached here. A fixture that asserted the row was retrieved without
	// grading it would report a precision of 0 over it, which is a claim about a
	// fixture rather than about the block.
	for i := range queries {
		if queries[i].Name == "q_global" {
			queries[i].Rel[corpusID(globalProject, contextFixtureKey)] = 1
		}
	}
	return store, queries, at
}

// seedContextGlobal writes the fixture's `_global` row and resolves the one row
// the contamination arm has to fire on.
func seedContextGlobal(t *testing.T, store *memory.Store, project string) {
	t.Helper()
	ctx := context.Background()
	if err := store.EnsureProject(ctx, globalProject, globalProject, "global"); err != nil {
		t.Fatalf("ensure the global project: %v", err)
	}
	if _, err := store.CreateWithIDFromCorpus(ctx, globalProject, corpusID(globalProject, contextFixtureKey), memory.Memory{
		Category: "fact", Content: "sqlite fts5 is the storage engine", Importance: 0.7, Source: "mcp",
	}); err != nil {
		t.Fatalf("create the global row: %v", err)
	}
	// SetResolved is the production write path and stamps resolved_at with the
	// store's own clock. Nothing measured here reads that value — the demote is
	// multiplicative and the leak arm asks only whether the column is set — so a
	// wall-clock stamp cannot move the report.
	if n, err := store.SetResolved(ctx, []string{corpusID(project, "resolved_fact")}); err != nil || n != 1 {
		t.Fatalf("SetResolved stamped %d rows (err %v), want 1", n, err)
	}
}

// assembledIDs runs the same request RunContext builds, one query at a time, and
// returns the admitted ids per query. The assertions about WHICH rows reached the
// block read the block rather than the report, because a report that says
// "contamination 1/7" and a block holding the wrong seven rows are both
// self-consistent and only one of them is the pipeline's behaviour.
//
// It runs with the cutoff OFF, because its callers measure the contamination
// classifier: whether an admitted row is flagged, and which rows reached the
// block at all. The relevance cutoff (#954) is orthogonal — it removes low-scoring
// rows before admission — and a resolved contamination row that happens to score
// low would be cut here, hiding the very leak this fixture exists to expose. So
// the classifier is exercised on the block as the selection stages leave it, the
// same subject the report's contamination column is about.
func assembledIDs(t *testing.T, store *memory.Store, queries []Query, at time.Time) map[string]int {
	t.Helper()
	held := map[string]int{}
	for _, q := range queries {
		res, err := assemble.Run(context.Background(), store, contextRequestWithCutoff(q, at, 0))
		if err != nil {
			t.Fatalf("assemble.Run for %q: %v", q.Name, err)
		}
		for _, it := range res.Items {
			held[it.ID]++
		}
	}
	return held
}

// TestContextFixtureWithholdsWhatItShouldAndFlagsWhatItLeaks is the contamination
// fixture: the two invalid rows are absent from the block, the two clean ones are
// present and unflagged, the resolved row is present and flagged, and the rate is
// computed over exactly the rows that are present.
//
// The middle of that list is what stops a classifier that flags too much, and the
// end is what stops one that flags too little. Both halves have to hold for the
// number to mean anything: a rate of 1.000 is as useless as a rate of 0.000, and
// only a fixture carrying both kinds of row can tell them apart.
func TestContextFixtureWithholdsWhatItShouldAndFlagsWhatItLeaks(t *testing.T) {
	store, queries, at := contextFixtureStore(t)
	// Cutoff off, for the reason assembledIDs documents: this fixture measures the
	// contamination classifier, which is orthogonal to the relevance cutoff.
	rep, err := runContextAt(context.Background(), store, queries, at, 0)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}

	held := assembledIDs(t, store, queries, at)
	for _, key := range []string{"future_scheduled", "expired_policy"} {
		if held[corpusID("ctx-tiny", key)] != 0 {
			t.Errorf("%q was admitted %d times, but stage 2 retires it against this clock", key, held[corpusID("ctx-tiny", key)])
		}
	}
	for _, key := range []string{"valid_window_open", "valid_current", "live_fact", "stale_fact"} {
		if held[corpusID("ctx-tiny", key)] == 0 {
			t.Errorf("%q was never admitted, so the fixture no longer covers a row the pipeline is supposed to keep", key)
		}
	}
	if held[corpusID("ctx-tiny", "resolved_fact")] == 0 {
		t.Fatal("the resolved row was never admitted, so contamination cannot be non-zero on this fixture")
	}
	if held[corpusID(globalProject, contextFixtureKey)] == 0 {
		t.Error("the _global row was never admitted, so diversity has only one bucket to count")
	}

	// Two answered queries: q_pool admits five project rows plus the global one,
	// q_global admits the global row alone.
	if rep.Answered != 2 {
		t.Errorf("answered %d queries, want 2", rep.Answered)
	}
	if rep.Items != 7 {
		t.Errorf("pooled %d admitted rows, want 7 (6 for q_pool + 1 for q_global)", rep.Items)
	}
	// q_pool grades two of its six rows; q_global grades its one. So 3 of 7.
	if got, want := rep.Precision, (Ratio{Num: 3, Den: 7}); got != want {
		t.Errorf("context precision = %+v, want %+v", got, want)
	}
	// One contaminated row, the resolved one, out of the same seven.
	if got, want := rep.Contamination, (Ratio{Num: 1, Den: 7}); got != want {
		t.Errorf("contamination = %+v, want %+v", got, want)
	}
	if got := rep.Arms[0]; got.Name != assemble.LeakResolved || got.Count != 1 {
		t.Errorf("first arm = %+v, want %s at 1", got, assemble.LeakResolved)
	}
	// Every arm is present, zeros included, in the assembler's own order: a
	// report that lists only the arms that fired cannot tell a measured zero from
	// an arm nobody looked for.
	if len(rep.Arms) != len(assemble.LeakArms) {
		t.Fatalf("the report carries %d arms, want %d", len(rep.Arms), len(assemble.LeakArms))
	}
	for i, arm := range rep.Arms {
		if arm.Name != assemble.LeakArms[i] {
			t.Errorf("arm %d = %q, want %q (the assembler's order)", i, arm.Name, assemble.LeakArms[i])
		}
		if i > 0 && arm.Count != 0 {
			t.Errorf("arm %q fired %d times on a fixture whose only leak is a resolved row", arm.Name, arm.Count)
		}
	}
}

// TestContextMeasuresTheBlockNotTheRanking: the graded corpus is the input, the
// assembler's budget is the bound, and the denominators are the rows a caller
// actually receives. Everything here is about the block — how much of it is
// relevant, how much of it leaked, what it cost — rather than about which row
// came first, which is what the three existing ablations score.
func TestContextMeasuresTheBlockNotTheRanking(t *testing.T) {
	store, queries, at := contextFixtureStore(t)
	// Cutoff off: this fixture measures the report's BLOCK accounting — buckets,
	// dominant share, cost — on the selection stages' own output, which is the
	// subject, rather than on a block the relevance cutoff (#954) had shortened.
	rep, err := runContextAt(context.Background(), store, queries, at, 0)
	if err != nil {
		t.Fatalf("RunContext: %v", err)
	}

	if rep.Queries != 2 {
		t.Errorf("measured %d queries, want 2", rep.Queries)
	}
	if got, want := rep.ResultRate, (Ratio{Num: 2, Den: 2}); got != want {
		t.Errorf("result rate = %+v, want %+v", got, want)
	}
	// The budget is the tool's own, and a report that measured a different bound
	// would be a report about a surface nobody ships.
	if rep.Budget.MaxItems != ContextItems || rep.Budget.MaxBytes != ContextBytes {
		t.Errorf("measured a budget of %d items / %d bytes, want the tool's %d / %d",
			rep.Budget.MaxItems, rep.Budget.MaxBytes, ContextItems, ContextBytes)
	}
	if rep.Budget.OverByteCap != 0 {
		t.Errorf("%d responses exceeded the %d-byte cap, so the byte bound is not being honoured", rep.Budget.OverByteCap, ContextBytes)
	}
	if rep.Budget.MaxResponse <= 0 {
		t.Error("no response was measured, so byte adherence is unmeasured rather than clean")
	}
	if rep.Budget.MaxTokens <= 0 {
		t.Error("no token estimate was measured, so the cost of a block is unmeasured")
	}
	// The fixture is two queries of seven rows and neither fills the item cap, so
	// nothing is trimmed and the trim counters have to say so rather than read as
	// a budget that was never reached.
	if rep.Budget.TrimmedQueries.Defined() && rep.Budget.TrimmedQueries.Num != 0 {
		t.Errorf("%d answered queries were trimmed on a fixture that fits the cap", rep.Budget.TrimmedQueries.Num)
	}

	// Two buckets, and the `_global` row is its own. One bucket would mean the
	// fixture lost the row that makes the diversity number say anything.
	if len(rep.Diversity.Buckets) != 2 {
		t.Fatalf("the report carries %d buckets (%+v), want the project and %s", len(rep.Diversity.Buckets), rep.Diversity.Buckets, globalProject)
	}
	byBucket := map[string]BucketShare{}
	for _, b := range rep.Diversity.Buckets {
		byBucket[b.Bucket] = b
	}
	// The `_global` row is in BOTH blocks: q_pool's text names "sqlite" and
	// q_global's whole answer is that row. So the bucket carries one row per query
	// while the project's five rows all come from q_pool — which is the case a
	// per-query count exists for, since one bucket's 2/7 share hides that it
	// reached every block.
	if got := byBucket[globalProject]; got.Items != 2 || got.Queries != 2 || got.Share.Den != 7 {
		t.Errorf("%s bucket = %+v, want 2 rows over 2 queries of the 7 admitted", globalProject, got)
	}
	if got := byBucket["ctx-tiny"]; got.Items != 5 || got.Queries != 1 || got.Share.Den != 7 {
		t.Errorf("project bucket = %+v, want 5 rows over 1 query of the 7 admitted", got)
	}
	// The dominant bucket per query: q_pool is 5 (all project) and q_global is 1
	// (all global), so the mean is 3 over the two answered queries.
	if got, want := rep.Diversity.TopBucket, (Ratio{Num: 6, Den: 2}); got != want {
		t.Errorf("dominant bucket = %+v, want %+v", got, want)
	}

	// The cost is per ANSWERED query, in token ESTIMATES (bytes/4, rounded up per
	// item) — there is no tokenizer in the pipeline, so a report that called these
	// tokens without saying so would be claiming a precision it does not have.
	if rep.Cost.TokensPerQuery.Den != 2 {
		t.Errorf("token cost averaged over %d queries, want 2", rep.Cost.TokensPerQuery.Den)
	}
	if rep.Cost.TokensPerQuery.Num <= 0 {
		t.Error("the token cost summed to zero over a block that carries content")
	}
	if rep.Cost.BytesPerQuery.Den != 2 || rep.Cost.BytesPerQuery.Num <= 0 {
		t.Errorf("byte cost = %+v, want a positive sum over the two answered queries", rep.Cost.BytesPerQuery)
	}
}

// TestContextEmptyResultCountsOnlyTowardTheResultRate: an empty block is not a
// clean block and not a precise one, it is a block with no rows. Folding it into
// precision would let a system improve its score by returning nothing, which is
// the shape of the defect the abstention work is about, and folding it into
// contamination would flatter it for the same reason. It is the result rate's
// only contribution.
//
// Driven through measureQuery rather than through a store, because reaching an
// empty block through assemble.Run needs a query neither retrieval leg can match
// — and a fixture built on that would be a test of the store's empty-return
// behaviour rather than of the rule.
func TestContextEmptyResultCountsOnlyTowardTheResultRate(t *testing.T) {
	rep := ContextReport{Queries: 1}
	before := rep
	empty := assemble.Result{Outcome: assemble.OutcomeEmpty, Trace: &assemble.Trace{}}
	if err := measureQuery(empty, Query{Name: "q"}, &rep); err != nil {
		t.Fatalf("measureQuery: %v", err)
	}
	if !reflect.DeepEqual(rep, before) {
		t.Errorf("an empty result moved the report: %+v, want %+v", rep, before)
	}

	// And one that carries rows moves every one of them.
	res := assemble.Result{
		Outcome: assemble.OutcomeAnswerable,
		Items:   []assemble.Item{{ID: "a", Bucket: "p", Bytes: 8, Tokens: 2}, {ID: "b", Bucket: "p", Bytes: 8, Tokens: 2}},
		Bytes:   64, Tokens: 4,
		Trace: &assemble.Trace{Signals: map[string]assemble.Signals{
			"a": {ValidityState: "valid", ScopeMatched: true, ProjectMatch: true},
			"b": {ValidityState: "valid", ScopeMatched: true, ProjectMatch: true},
		}},
	}
	if err := measureQuery(res, Query{Name: "q", Rel: Relevance{"a": 1}}, &rep); err != nil {
		t.Fatalf("measureQuery: %v", err)
	}
	if rep.Answered != 1 || rep.Items != 2 || rep.Relevant != 1 {
		t.Errorf("after a two-row block the report is %+v, want 1 answered / 2 items / 1 relevant", rep)
	}
	if got, want := rep.Precision, (Ratio{Num: 1, Den: 2}); got != want {
		t.Errorf("precision = %+v, want %+v", got, want)
	}
	if got, want := rep.Cost.TokensPerQuery, (Ratio{Num: 4, Den: 1}); got != want {
		t.Errorf("token cost = %+v, want %+v", got, want)
	}
}

// contextTrimFixture is the corpus that makes the BUDGET table falsifiable.
//
// contextFixture holds 8 rows against the tool's 10-item cap, so nothing in it is
// ever trimmed and every trim ratio in that report is a step function with a single
// reachable value. 1.000 is a true reading there, and it is also exactly what a
// trim that had stopped firing altogether would print: a ratio that cannot go below
// 1.000 cannot tell a budget that binds from a budget that was removed, and the
// graded corpus's 220/220 was true only because the cap binds on every one of its
// queries.
//
// So this fixture is 12 rows against that cap, with its two queries shaped to land
// on either side of it: q_wide reaches all 12 and is trimmed, q_narrow reaches three
// and is not. Every row is graded relevant, which is what makes "how many of the
// relevant rows did the cap cost" a number this fixture can NAME rather than one
// that happens to coincide with the admitted count — the two coincide only when the
// other trim dropped nothing.
//
// The vector shapes are the reason each candidate set is the size it is. q_wide
// sets every component, so it has a positive cosine with every embedded row;
// q_narrow sets the one component no row uses, so its vector leg returns nothing
// and the keyword leg alone decides its three. That is why dim is rows+1.
func contextTrimFixture(t *testing.T) (Dataset, Vectors) {
	t.Helper()
	const rows = 12
	dim := rows + 1
	vec := func(at int) []float32 {
		out := make([]float32, dim)
		out[at] = 1
		return out
	}
	every := make([]float32, dim)
	for i := range every {
		every[i] = 1
	}

	// "quorum" is in every row so q_wide's keyword leg reaches all 12; "zephyr" is
	// in three of them so q_narrow's reaches three. Nothing else is shared, so no
	// third set of candidates can appear by accident.
	const shared = "quorum"
	const narrow = "zephyr"
	memories := make([]MemorySpec, 0, rows)
	vecs := Vectors{}
	keys := make([]string, 0, rows)
	wideRel := make(map[string]int, rows)
	for i := 0; i < rows; i++ {
		key := fmt.Sprintf("row_%02d", i)
		keys = append(keys, key)
		content := fmt.Sprintf("%s topic %02d", shared, i)
		if i < 3 {
			content += " " + narrow
		}
		memories = append(memories, MemorySpec{Key: key, Category: "fact", Content: content, Importance: 0.5})
		vecs[key] = vec(i)
		wideRel[key] = 1
	}
	narrowRel := map[string]int{keys[0]: 1}

	ds := Dataset{
		Project:  "ctx-trim",
		Memories: memories,
		Queries: []QuerySpec{
			// Only the shared term, so the keyword leg reaches all twelve.
			{Name: "q_wide", Text: shared, Rel: wideRel},
			// Only the term three rows carry. It must NOT also name the shared one:
			// the keyword leg ORs a query's terms, so a narrow query naming both
			// reaches all twelve and is trimmed like the wide one — the opposite of
			// the property this fixture exists to make reachable.
			{Name: "q_narrow", Text: narrow, Rel: narrowRel},
		},
	}
	vecs["q_wide"] = every
	vecs["q_narrow"] = vec(rows)
	return ds, vecs
}

// contextTrimReport seeds contextTrimFixture and measures it, failing the test on a
// seeding error. The fixture's queries are graded against the dataset's own keys
// here, so unlike contextFixture's `_global` row nothing has to be patched onto a
// relevance map after the seeder has built it.
func contextTrimReport(t *testing.T) ContextReport {
	t.Helper()
	ds, vecs := contextTrimFixture(t)
	store, db := newBenchStoreWithDB(t)
	queries, at, err := SeedAt(context.Background(), store, db, ds, vecs, contextFixtureInstant)
	if err != nil {
		t.Fatalf("seed the trim fixture: %v", err)
	}
	// The no-answer bar (#955) is off here: this fixture's query vectors are
	// synthetic and sit below any real cosine bar, and what it measures is the
	// budget's trim, which is a statement about a block that was ANSWERED. The bar
	// has its own tests (noanswer_sweep_test.go and the assembler's).
	rep, err := runContextBar(context.Background(), store, queries, at, config.DefaultRelevanceCutoff, 0)
	if err != nil {
		t.Fatalf("runContextBar: %v", err)
	}
	return rep
}

// TestContextTrimRatiosCountEveryAnsweredQuery is the reviewer's case, end to end.
//
// It is the assertion that a trim ratio CAN FALL, and there are two ways it fails,
// both of which this corpus reaches at once. Counting only the queries a trim
// shortened puts 1.000 (1/1) here for a trim that shortened one of two — the
// denominator is the numerator, so the ratio is a constant. And returning early
// when a trim fired on no rows leaves the OTHER trim's ratios at n/a, which reads
// as "not measured" for a pass that ran on every query and cut nothing.
//
// The populations are all named rather than inherited, so this also pins what each
// denominator is, and all three DIFFER here — which is what makes the fixture able to
// catch a rule that reaches for the wrong one:
//
//   - rows the cap reached: 15 (q_wide's 10 kept + its 2 cut, q_narrow's 3 kept; a
//     fit-dropped row would have passed the cap on its way, so nothing else is in it)
//   - GRADED rows the cap reached: 13 (q_wide grades all 12 rows it reached — 10 kept
//     plus the 2 cut — and q_narrow grades 1 of its 3 kept rows)
//   - answered queries: 2, one trimmed and one not
//
// So the cap's cost in relevant rows reads 2/13. Note that 13 is ALSO the pooled
// admitted count here (10 + 3) — a coincidence of this fixture, not the definition,
// and the fixture is built so the two cannot be told apart by accident elsewhere: the
// assertions below pin all three denominators, and
// TestContextTrimRatiosGiveEachStageTheRowsItActuallySaw pins the both-trims-fire case
// where they diverge.
func TestContextTrimRatiosCountEveryAnsweredQuery(t *testing.T) {
	rep := contextTrimReport(t)
	if rep.Answered != 2 || rep.Items != 13 {
		t.Fatalf("answered %d queries with %d rows, want 2 and 13 (a trimmed 10-row block plus a 3-row one)", rep.Answered, rep.Items)
	}
	for _, tc := range []struct {
		what string
		got  Ratio
		want Ratio
	}{
		{"cap trimmed 1 of 2 answered queries", rep.Budget.TrimmedQueries, Ratio{Num: 1, Den: 2}},
		// 15 rows: q_wide's 10 kept plus its 2 cut, and q_narrow's 3 kept. The
		// untrimmed query contributes, which it did not before.
		{"cap cut 2 of the 15 rows that reached it", rep.Budget.TrimmedItems, Ratio{Num: 2, Den: 15}},
		// 13 GRADED rows — not 15, and not the 10 that survived. q_wide grades all
		// twelve rows it reached and q_narrow grades one of its three, so the three
		// denominators differ here, which is the point: a reader handed "2 of 13"
		// can tell what it is a fraction of, and cannot be handed 2/10 by a rule
		// that reads "the admitted count" and gets the right answer here for the
		// wrong reason.
		{"cap cut 2 of the 13 graded rows it saw", rep.Budget.TrimmedRelevant, Ratio{Num: 2, Den: 13}},
		// The fit pass ran on both queries and cut nothing, which is 0.000 over a
		// real population — not n/a, and not 1.000.
		{"fit trimmed 0 of 2 answered queries", rep.Budget.FittedQueries, Ratio{Num: 0, Den: 2}},
		{"fit cut 0 of the 13 rows that reached it", rep.Budget.FittedItems, Ratio{Num: 0, Den: 13}},
		{"fit cut 0 of the 11 graded rows it saw", rep.Budget.FittedRelevant, Ratio{Num: 0, Den: 11}},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %+v, want %+v", tc.what, tc.got, tc.want)
		}
	}

	// A trim ratio is meaningless as a string unless it can print a fraction that
	// is not whole, so the rendering is checked here too rather than left to the
	// format test's fixture, which has no trim to render.
	if got := rep.Budget.TrimmedQueries.String(); got != "0.500 (1/2)" {
		t.Errorf("trimmed-queries rendering = %q, want %q", got, "0.500 (1/2)")
	}
}

// TestContextTrimRatiosGiveEachStageTheRowsItActuallySaw is the case the two
// trims' denominators used to share, which is the only way a caller can tell that
// they no longer do.
//
// It is the reviewer's second point as a test. A row the response-fit pass cuts has
// already passed the item cap on its way, so the cap saw every row and the fit pass
// only the ones the cap left: one shared "rows that reached it" puts the fit pass's
// denominator above the population it was ever shown, and the graded denominator
// that came with it divides by rows no stage looked at.
//
// The numbers are chosen so the two candidate answers differ — 3 admitted, 2 cut by
// the cap, 1 cut by the fit pass, all six graded:
//
//   - cap: 3 + 2 + 1 = 6 rows -> 2/6
//   - fit: 3 + 1 = 4 rows -> 1/4
//
// A shared denominator answers 2/6 and 1/6. The old `reached - cut` answers 2/4 and
// 1/5. Both are plausible-looking numbers, and neither is a claim a stage can make.
func TestContextTrimRatiosGiveEachStageTheRowsItActuallySaw(t *testing.T) {
	rel := Relevance{"a": 1, "b": 1, "c": 1, "d": 1, "e": 1, "f": 1}
	rep := ContextReport{Queries: 1, Answered: 1, Items: 3, Relevant: 3}
	// The cap cut d and e; the fit pass then cut f from what the cap left.
	noteTrim(&rep, []string{"d", "e"}, []string{"f"}, rel, 3, 3)

	for _, tc := range []struct {
		what string
		got  Ratio
		want Ratio
	}{
		{"cap trimmed queries", rep.Budget.TrimmedQueries, Ratio{Num: 1, Den: 1}},
		{"cap cut rows", rep.Budget.TrimmedItems, Ratio{Num: 2, Den: 6}},
		{"cap cut graded rows", rep.Budget.TrimmedRelevant, Ratio{Num: 2, Den: 6}},
		{"fit trimmed queries", rep.Budget.FittedQueries, Ratio{Num: 1, Den: 1}},
		{"fit cut rows", rep.Budget.FittedItems, Ratio{Num: 1, Den: 4}},
		{"fit cut graded rows", rep.Budget.FittedRelevant, Ratio{Num: 1, Den: 4}},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %+v, want %+v", tc.what, tc.got, tc.want)
		}
	}
}

// TestContextTrimRatiosCountQueriesThatWereNotTrimmed is the reviewer's
// hand-computation as a test: two queries trimmed, one not, and the denominator has
// to be every query the report measured.
//
// Counting only the trimmed queries answers 1.000 (2/2) — the denominator IS the
// numerator, so the number cannot tell "the budget shortened two of three queries"
// from "the budget shortened every query it was asked about", and a trim that had
// stopped firing altogether would print the same 1.000 it prints today. The untrimmed
// query still contributes to the items and relevant denominators, because it
// contributed rows to the populations those are fractions of.
//
// The three queries are shaped so the graded denominator is NOT the row denominator,
// which is the point: query 3 admits three rows and grades none of them, so its
// contribution to the graded ratio is 0/0 — it moves the row counts and the query
// count, and leaves the graded count alone. Reading the admitted count as the
// denominator instead would print 0/3 there and claim a measured share of a
// population that holds no graded rows at all.
func TestContextTrimRatiosCountQueriesThatWereNotTrimmed(t *testing.T) {
	rep := ContextReport{Queries: 3}
	// Query 1: two rows, both graded, and the cap cuts the third — which is graded.
	noteTrim(&rep, []string{"c1"}, nil, Relevance{"a1": 1, "a2": 1, "c1": 1}, 2, 2)
	// Query 2: four rows, one graded, and the cap cuts two — neither graded.
	noteTrim(&rep, []string{"d1", "d2"}, nil, Relevance{"b1": 1, "b2": 0, "b3": 0, "b4": 0}, 4, 1)
	// Query 3: three rows, none graded, and nothing trimmed at all.
	noteTrim(&rep, nil, nil, Relevance{"e1": 0, "e2": 0, "e3": 0}, 3, 0)

	for _, tc := range []struct {
		what string
		got  Ratio
		want Ratio
	}{
		{"cap trimmed queries", rep.Budget.TrimmedQueries, Ratio{Num: 2, Den: 3}},
		// The cap saw every row: 2+1, 4+2 and 3, so 12.
		{"cap cut rows", rep.Budget.TrimmedItems, Ratio{Num: 3, Den: 12}},
		// The cap saw 3 graded rows and 1 more, so 4 — which is not the 12 rows it
		// saw, and not the 9 that survived it. That is the population a sentence
		// about relevant rows is a fraction of.
		{"cap cut graded rows", rep.Budget.TrimmedRelevant, Ratio{Num: 1, Den: 4}},
		// The fit pass ran on all three queries and cut nothing, which is 0.000 over
		// a real population — not n/a, which would read as "not measured" for a pass
		// that ran on every query and saw every row the cap left.
		{"fit trimmed queries", rep.Budget.FittedQueries, Ratio{Num: 0, Den: 3}},
		// 9, not 12: the three rows the cap cut never reached the fit pass, so
		// including them would make this a fraction of a population it was never
		// shown.
		{"fit cut rows", rep.Budget.FittedItems, Ratio{Num: 0, Den: 9}},
		{"fit cut graded rows", rep.Budget.FittedRelevant, Ratio{Num: 0, Den: 3}},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %+v, want %+v", tc.what, tc.got, tc.want)
		}
	}

	// A ratio that can only ever print 1.000 is a ratio nobody can read a change
	// in, so the rendered fraction is checked here too.
	if got, want := rep.Budget.TrimmedQueries.String(), "0.667 (2/3)"; got != want {
		t.Errorf("trimmed-queries rendering = %q, want %q", got, want)
	}
}

// TestContextReportIsAFunctionOfTheCorpusAndTheClock: the report is published, so
// two runs of one binary have to produce the same bytes — the property #708 gave
// the ablations and the one the context table inherits, because it reads the same
// store. Two stores seeded from ONE corpus at ONE instant is the tightest form of
// the claim: everything else is shared, so any difference is the seed.
func TestContextReportIsAFunctionOfTheCorpusAndTheClock(t *testing.T) {
	ds, vecs := contextFixture(t)
	report := func() string {
		store, db := newBenchStoreWithDB(t)
		ctx := context.Background()
		queries, at, err := SeedAt(ctx, store, db, ds, vecs, contextFixtureInstant)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		seedContextGlobal(t, store, ds.Project)
		rep, err := RunContext(ctx, store, queries, at)
		if err != nil {
			t.Fatalf("RunContext: %v", err)
		}
		return FormatContext(rep)
	}
	first, second := report(), report()
	if first != second {
		t.Errorf("two seeds of one corpus produced two different context reports\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if !strings.Contains(first, "context precision") {
		t.Fatalf("the report has no precision row, so it is not the table this test compares:\n%s", first)
	}
}

// TestContextRequestIsTheBudgetTheToolSends pins the request the bench measures to
// the one ghost_memory_search builds, because a context metric is a claim about
// that surface and a request with a different budget measures a different one.
// `assemble.Run` REFUSES a budget that bounds nothing, so RunContext's request
// being accepted at all is the load-bearing half of this: an unbounded request
// would compare an unbounded tail against the tool's trimmed block and report the
// difference as a quality finding.
func TestContextRequestIsTheBudgetTheToolSends(t *testing.T) {
	req := ContextRequest(Query{ProjectID: "p", Text: "kubernetes", Vector: []float32{1, 0}}, contextFixtureInstant)
	if req.Source != assemble.SourceBench {
		t.Errorf("Source = %q, want %q (the measurement is labelled as such)", req.Source, assemble.SourceBench)
	}
	if req.Condition != assemble.CondHybrid {
		t.Errorf("Condition = %q, want %q: the tool's search runs hybrid", req.Condition, assemble.CondHybrid)
	}
	if req.Budget.MaxItems != 10 {
		t.Errorf("MaxItems = %d, want the tool's default limit of 10", req.Budget.MaxItems)
	}
	if req.Budget.MaxBytes != 2*memory.MaxContentLen {
		t.Errorf("MaxBytes = %d, want the tool's response cap of 2*MaxContentLen (%d)", req.Budget.MaxBytes, 2*memory.MaxContentLen)
	}
	if !req.Now.Equal(contextFixtureInstant) {
		t.Errorf("Now = %v, want the caller's clock %v", req.Now, contextFixtureInstant)
	}
	// The bench's own source and the tool's agree on the project mode for every
	// request either of them makes, because both name a project; the difference
	// between them only exists for an empty ProjectID, which this mode reports
	// rather than measures. Asserted so a future Source change cannot quietly make
	// the bench measure a mode the tool never runs.
	if req.ProjectID == "" {
		t.Error("the bench request carries no project, which is the one case where SourceBench and SourceSearch disagree")
	}
}

// TestContextInstantSitsInsideEveryGradedWindow guards the one thing that makes a
// fixed clock safe: it must not sit NEAR a boundary any corpus row states.
//
// Near is the operative word. The corpus deliberately holds a row whose window
// closed in 2020 and one whose window opens in 2099 — those two are what stage 2
// exercises, and a clock outside either is correct rather than wrong. What would be
// wrong is a clock days from a boundary, because then a small corpus edit moves a
// row in or out of the block and the published table changes for a reason that has
// nothing to do with retrieval. So the rule is a margin rather than a
// containment: every boundary in the corpus must be at least contextClockMargin
// away from the instant the report is measured at.
//
// A corpus edit that added a window closing next month fails here. It would not
// fail anywhere else: stage 2 would retire the row, the table would lose a row,
// and the numbers in docs/benchmarks.md would be quietly wrong.
func TestContextInstantSitsInsideEveryGradedWindow(t *testing.T) {
	// 90 days. Comfortably inside the corpus's own layout — its nearest open
	// boundary is 2024-01-01 and its nearest closed one 2020-06-01 — and far
	// enough out that a corpus edit cannot move a row across it by accident.
	const margin = 90 * 24 * time.Hour

	ds, _, err := BuiltinDataset()
	if err != nil {
		t.Fatalf("load the built-in dataset: %v", err)
	}
	now := ContextInstant()
	for _, m := range ds.Memories {
		for _, bound := range []struct {
			name string
			text *string
		}{{"valid_from", m.ValidFrom}, {"valid_until", m.ValidUntil}} {
			if bound.text == nil {
				continue
			}
			at, ok := memory.ParseStamp(*bound.text)
			if !ok {
				t.Errorf("memory %q states an unreadable %s %q, which stage 2 cannot read either", m.Key, bound.name, *bound.text)
				continue
			}
			if d := at.Sub(now); d > -margin && d < margin {
				t.Errorf("memory %q has %s = %s, only %v from the pinned context clock %s: move the clock or the boundary, not the margin",
					m.Key, bound.name, at.Format(time.RFC3339), absDuration(d), now.Format(time.RFC3339))
			}
		}
	}
	// The clock is UTC and second-resolution on purpose: the stamp it is paired
	// with is written as SQLite datetime text, and a caller that got a local-zone
	// instant would be measuring against a different day than the corpus was
	// stamped on.
	if got := ContextInstant(); got.Location() != time.UTC || got.Nanosecond() != 0 {
		t.Errorf("ContextInstant = %v, want a UTC instant with no sub-second part", got)
	}
}

// absDuration is |d|, so a margin test reads the same whichever side of the clock
// the boundary fell on.
func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// TestFormatContextPrintsTheCostTheDocsPromise is the review's finding, as a test.
//
// ContextCost was measured, documented in three places and never printed, which is
// the worst of both: docs/benchmarks.md, docs/cli.md and `benchUsage` all promise the
// block's cost, and a reader of the section the command produces cannot find it. A
// metric nothing renders is a metric nobody reads, and the three claims it made
// elsewhere were then false.
//
// So the cost is rendered, with its population, in the same num/den form as every
// other ratio — and this asserts on the OUTPUT rather than on the report struct,
// because the defect was precisely that a struct field can be correct and never reach
// a reader. The two maxima (largest response, largest block) are the other half of
// this table: a mean says what a block usually costs, a maximum says what the worst
// one cost, and a section that printed only one of them would let a caller size a
// budget from the wrong number.
func TestFormatContextPrintsTheCostTheDocsPromise(t *testing.T) {
	rep := ContextReport{
		Project: "p", Queries: 2, Answered: 2, Items: 20,
		Precision: Ratio{Num: 3, Den: 20}, Contamination: Ratio{Num: 0, Den: 20},
		Arms: []LeakArm{{Name: assemble.LeakResolved, Count: 0}},
		Budget: ContextBudget{
			MaxItems: ContextItems, MaxBytes: ContextBytes,
			MaxResponse: 2000, MaxTokens: 300,
		},
		Cost: ContextCost{
			TokensPerQuery: Ratio{Num: 500, Den: 2},  // 250 mean
			BytesPerQuery:  Ratio{Num: 4000, Den: 2}, // 2000 mean
		},
	}
	out := FormatContext(rep)
	for _, want := range []string{
		"250.000 (500/2)",   // the token mean, with its population
		"2000.000 (4000/2)", // the byte mean, with its population
		"2000 bytes",        // the maximum, which the mean must not be confused with
		"300 (est.)",        // the token maximum
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendered section does not print %q, so the cost never reaches a reader:\n%s", want, out)
		}
	}

	// The unit belongs on the value, not only in the population column, because the
	// two rows are the same kind of number and a reader scanning the value column
	// must be able to tell a token count from a byte count.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "2000.000 (4000/2)") && !strings.Contains(line, "byte") {
			t.Errorf("the byte row does not name its unit:\n%s", line)
		}
	}

	// And the section says which questions these are, so a row is not read as a
	// verdict on retrieval rather than on the bill.
	if !strings.Contains(out, "cost per answered query") {
		t.Errorf("the cost table has no heading naming its population:\n%s", out)
	}
}

// TestFormatContextNamesTheTableItComparesAgainst is the review's second finding.
//
// The preamble said "NDCG@10 and MRR@10 above", but `--context` returns immediately
// after this section and never prints the ablation table — so the reader was told to
// compare against rows that were not on their screen. The words have to name where
// those numbers actually live, because "above" was the only pointer the section gave.
func TestFormatContextNamesTheTableItComparesAgainst(t *testing.T) {
	out := FormatContext(ContextReport{Project: "p", Queries: 0})
	if strings.Contains(out, "above") {
		t.Errorf("the preamble points at a table this mode never prints:\n%s", out)
	}
	// The claim is still worth making — these metrics and the ranking metrics answer
	// different questions — so the names stay and only the false pointer goes.
	for _, want := range []string{"NDCG@10", "MRR@10", "BLOCK"} {
		if !strings.Contains(out, want) {
			t.Errorf("the preamble dropped %q, which is the comparison the section exists to draw:\n%s", want, out)
		}
	}
}

// TestFormatContextSaysNAWhenNothingWasMeasured: a ratio with no denominator is
// undefined, and printing it as 0.000 would be a claim — a corpus where nothing
// assembled has a contamination rate of zero, which is not the same fact. `n/a` is
// the convention the false-positive report already uses, and an undefined ratio is
// never averaged into anything here.
func TestFormatContextSaysNAWhenNothingWasMeasured(t *testing.T) {
	out := FormatContext(ContextReport{Project: "p", Queries: 0})
	if !strings.Contains(out, "n/a") {
		t.Errorf("an unmeasured report prints no n/a, so every ratio reads as a measured zero:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "0.000 (0/0)") {
			t.Errorf("an undefined ratio printed as a zero:\n%s", line)
		}
	}
	if !strings.Contains(out, "report-only") {
		t.Errorf("the report does not say it gates nothing, so a reader could take a number here for a floor:\n%s", out)
	}
	// A measured zero is a different claim and must still print as one.
	measured := FormatContext(ContextReport{
		Project: "p", Queries: 1, Answered: 1, Items: 4,
		Precision: Ratio{Num: 2, Den: 4}, Contamination: Ratio{Num: 0, Den: 4},
		Arms: []LeakArm{{Name: assemble.LeakResolved, Count: 0}},
	})
	if !strings.Contains(measured, "0.000 (0/4)") {
		t.Errorf("a measured zero is not printed with its denominator:\n%s", measured)
	}
}
