package bench

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// loadMaintenanceFixture loads the committed maintenance-state corpus, its graded
// questions and their vector fixture.
func loadMaintenanceFixture(t *testing.T) ([]MaintenanceMemory, []MaintenanceQuery, Vectors) {
	t.Helper()
	mems, err := loadFile("testdata/maintenance_memories.jsonl", LoadMaintenanceMemories)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	qs, err := loadFile("testdata/maintenance_queries.jsonl", LoadMaintenanceQueries)
	if err != nil {
		t.Fatalf("load questions: %v", err)
	}
	vecs, err := loadFile("testdata/maintenance_embeddings.json", LoadVectors)
	if err != nil {
		t.Fatalf("load vectors: %v", err)
	}
	return mems, qs, vecs
}

// newSeedingStore returns a store plus the db handle seeding needs, since
// backdating created_at is raw SQL on the store the suite owns.
func newSeedingStore(t *testing.T) (*memory.Store, *sql.DB) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	t.Cleanup(func() { _ = store.Close() })
	return store, db
}

// TestMaintenanceStateReport runs the suite and logs the report. Report-only, in
// both directions: no metric floor is asserted, because the suite's whole purpose
// is to move when ranking changes, and a floor would turn today's numbers into a
// gate on tomorrow's fix. What is enforced is that the suite runs, scores every
// question, and reports the columns and the lost questions. See
// docs/benchmarks.md ("Maintenance state") for the numbers and for why each one
// is a report rather than a gate.
func TestMaintenanceStateReport(t *testing.T) {
	mems, qs, vecs := loadMaintenanceFixture(t)
	results, err := RunMaintenance(context.Background(), mems, qs, vecs, memory.DefaultSearchParams())
	if err != nil {
		t.Fatalf("RunMaintenance: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("want fts/vector/hybrid results, got %d", len(results))
	}
	for _, r := range results {
		if r.Queries != len(qs) {
			t.Errorf("%s: scored %d questions, want all %d — a skipped question would hide a findability regression",
				r.Condition, r.Queries, len(qs))
		}
		if len(r.Outcomes) != len(qs) {
			t.Errorf("%s: %d outcomes, want %d", r.Condition, len(r.Outcomes), len(qs))
		}
	}
	// Nothing here asserts a metric, a floor, or even that a particular question
	// was lost: this suite is report-only, and a guard of the form "at least one
	// question must be outranked" would fail the moment a ranking change makes
	// every question win. What the report must get right is checked against
	// synthetic outcomes in TestFormatMaintenanceSeparatesLostQuestions.
	report := FormatMaintenance(results)
	if !strings.Contains(report, "live-wins") || !strings.Contains(report, "shared _global answers") {
		t.Errorf("report is missing its columns:\n%s", report)
	}
	t.Logf("maintenance-state suite (report-only, DefaultSearchParams):\n%s", report)
}

// The headers FormatMaintenance prints for the two ways a question can be lost.
const (
	outrankedHeader = "where a resolved/_global/superseded copy outranked the answer"
	notFoundHeader  = "whose answer was not retrieved at all"
)

// TestFormatMaintenanceSeparatesLostQuestions checks the report's one real
// correctness claim — an answer that was never retrieved is not reported as
// outranked by something — against synthetic outcomes rather than against a
// live run. That is deliberate on both sides: a live assertion would be a floor
// on today's ranking (this suite is report-only, and the finding it documents is
// expected to be fixed), and the live report's own wording would then be the
// thing under test, which is how the two failure modes got conflated in the
// first place.
func TestFormatMaintenanceSeparatesLostQuestions(t *testing.T) {
	both := []MaintenanceOutcome{
		{Query: "q_outranked", Probe: probeLive, Found: true, LiveWins: false},
		{Query: "q_evicted", Probe: probeGlobal, Found: false, LiveWins: false},
		{Query: "q_won", Probe: probeLive, Found: true, LiveWins: true},
	}
	report := FormatMaintenance([]MaintenanceResult{{Condition: CondHybrid, Queries: 3, Outcomes: both}})

	outranked, notFound := lostSections(t, report, true, true)
	if !strings.Contains(outranked, "q_outranked") {
		t.Errorf("the outranked list does not name the question a copy outranked:\n%s", report)
	}
	if !strings.Contains(notFound, "q_evicted") {
		t.Errorf("the not-retrieved list does not name the evicted answer:\n%s", report)
	}
	for _, absent := range []struct{ name, section, why string }{
		{"q_evicted", outranked, "its answer was never returned, so nothing outranked it"},
		{"q_outranked", notFound, "its answer was returned and outranked, so it was not lost"},
		{"q_won", outranked + notFound, "it won"},
	} {
		if strings.Contains(absent.section, absent.name) {
			t.Errorf("%s is listed as lost (%s)", absent.name, absent.why)
		}
	}

	// Only the not-retrieved list non-empty: the outranked header must not be
	// printed at all, so a reader is not sent looking for a distractor that beat
	// an answer which was never in the window.
	onlyEvicted := FormatMaintenance([]MaintenanceResult{{Condition: CondHybrid, Queries: 1, Outcomes: both[1:2]}})
	if strings.Contains(onlyEvicted, outrankedHeader) {
		t.Errorf("an outranked list is printed with nothing to put in it:\n%s", onlyEvicted)
	}
	if _, ev := lostSections(t, onlyEvicted, false, true); !strings.Contains(ev, "q_evicted") {
		t.Errorf("the not-retrieved list is missing its only entry:\n%s", onlyEvicted)
	}

	// Three conditions, so a section is provably scoped to its own. Two leaks to
	// catch, and they run in different directions: fts-only loses q_evicted to a
	// copy while hybrid never retrieves it at all, so hybrid's outranked list
	// must not claim it; and vector-only renders *after* hybrid and loses
	// q_third, so hybrid's lists must not swallow a name that is only its
	// neighbour's. A helper that sliced to the end of the report would fail the
	// second and pass the first, which is why both directions are here.
	//
	// The Shared counts are non-zero on purpose: FormatMaintenance prints a
	// shared-row probe line per condition before the lost-question headers, and
	// that line carries the same "<condition>: " prefix. Without one here, a
	// helper anchoring on the prefix alone would look correct.
	threeConditions := FormatMaintenance([]MaintenanceResult{
		{Condition: CondFTS, Queries: 2, Shared: SharedProbe{Queries: 1, Found: 1}, Outcomes: []MaintenanceOutcome{
			{Query: "q_outranked", Found: true, LiveWins: false},
			{Query: "q_evicted", Found: true, LiveWins: false},
		}},
		{Condition: CondHybrid, Queries: 2, Shared: SharedProbe{Queries: 1}, Outcomes: both[:2]},
		{Condition: CondVector, Queries: 1, Outcomes: []MaintenanceOutcome{
			{Query: "q_third", Found: true, LiveWins: false},
		}},
	})
	ftsOutranked, _ := lostSections(t, maintenanceChunk(t, threeConditions, CondFTS), true, false)
	if !strings.Contains(ftsOutranked, "q_evicted") {
		t.Errorf("the fts-only outranked list does not name the question it lost to a copy:\n%s", threeConditions)
	}
	hybridChunk := maintenanceChunk(t, threeConditions, CondHybrid)
	hybridOutranked, hybridNotFound := lostSections(t, hybridChunk, true, true)
	if strings.Contains(hybridOutranked, "q_evicted") {
		t.Errorf("q_evicted is in the hybrid outranked list although hybrid never retrieved it:\n%s", hybridChunk)
	}
	if !strings.Contains(hybridNotFound, "q_evicted") {
		t.Errorf("q_evicted is not in the hybrid not-retrieved list:\n%s", hybridChunk)
	}
	if strings.Contains(hybridOutranked+hybridNotFound, "q_third") {
		t.Errorf("the hybrid sections name q_third, which only vector-only lost — the chunk is not bounded by the next condition:\n%s", threeConditions)
	}
	vectorOutranked, _ := lostSections(t, maintenanceChunk(t, threeConditions, CondVector), true, false)
	if !strings.Contains(vectorOutranked, "q_third") {
		t.Errorf("the vector-only outranked list does not name the question it lost to a copy:\n%s", threeConditions)
	}
}

// maintenanceChunk returns the part of a rendered report belonging to one
// condition: from that condition's first lost-question header to the next
// condition's, or the end. FormatMaintenance prints two colon-carrying
// per-condition lines — the shared-row probe line and the lost-question headers —
// and the probe line comes first, so the anchor has to skip it; see
// lostSectionAnchor. A question name is only unique within its own condition, so
// this is also what stops one condition's lists from answering for another's.
func maintenanceChunk(t *testing.T, report, condition string) string {
	t.Helper()
	at := lostSectionAnchor(report, condition, 0)
	if at < 0 {
		t.Fatalf("report has no lost-question section for %s:\n%s", condition, report)
	}
	chunk := report[at:]
	for _, other := range []string{CondFTS, CondVector, CondHybrid} {
		if other == condition {
			continue
		}
		if i := lostSectionAnchor(report, other, at); i >= 0 {
			return report[at:i]
		}
	}
	return chunk
}

// lostSectionAnchor returns the offset just past "<condition>: " at that
// condition's first lost-question header at or after from, or -1. The digit is
// load-bearing: FormatMaintenance prints the shared-row probe line as
// "<condition>: shared _global answers found ..." before the lost-question
// headers, so anchoring on the colon alone would start the chunk at the probe
// line and end it at the next condition's probe line — no lost-question section
// at all. Anchoring on the count is what skips it.
func lostSectionAnchor(report, condition string, from int) int {
	prefix := "\n" + condition + ": "
	for i := from; i < len(report); {
		j := strings.Index(report[i:], prefix)
		if j < 0 {
			return -1
		}
		at := i + j + len(prefix)
		if at < len(report) && report[at] >= '0' && report[at] <= '9' {
			return at
		}
		i = at
	}
	return -1
}

// lostSections returns the two lost-question lists from one condition's chunk of
// a rendered report. The lists are bounded by their headers and, at the end, by
// the chunk — never by a fixed offset — and a missing header is a failure rather
// than an empty string that every containment check would pass.
func lostSections(t *testing.T, chunk string, wantOutranked, wantNotFound bool) (outranked, notFound string) {
	t.Helper()
	section := func(header string, endsAt ...string) string {
		i := strings.Index(chunk, header)
		if i < 0 {
			return ""
		}
		rest := chunk[i+len(header):]
		for _, end := range endsAt {
			if j := strings.Index(rest, end); j >= 0 {
				rest = rest[:j]
			}
		}
		return rest
	}
	if wantOutranked {
		outranked = section(outrankedHeader, notFoundHeader)
		if outranked == "" {
			t.Fatalf("report has no outranked section:\n%s", chunk)
		}
	}
	if wantNotFound {
		notFound = section(notFoundHeader)
		if notFound == "" {
			t.Fatalf("report has no not-retrieved section:\n%s", chunk)
		}
	}
	return outranked, notFound
}

// TestMaintenanceFixtureCarriesState is the meta-test that keeps this suite able
// to see what the graded dataset structurally cannot: a resolved row, a shared
// _global row, a supersedes edge, and a created_at spread across categories.
// Without it, a later edit that quietly drops that state turns this suite into a
// second copy of the graded one — reporting the same numbers and the same
// blindness again.
func TestMaintenanceFixtureCarriesState(t *testing.T) {
	mems, qs, vecs := loadMaintenanceFixture(t)
	if len(mems) < 40 {
		t.Errorf("corpus has %d memories, want >= 40", len(mems))
	}
	if len(qs) < 20 {
		t.Errorf("fixture has %d questions, want >= 20", len(qs))
	}

	var resolved, globals, globalDecaying, superseded int
	youngest, oldest := -1, 0
	for _, m := range mems {
		switch {
		case m.AgeDays > oldest:
			oldest = m.AgeDays
		case youngest < 0 || m.AgeDays < youngest:
			youngest = m.AgeDays
		}
		switch {
		case m.Resolved:
			resolved++
			// SetResolved exempts convention and preference, so a corpus entry
			// in one of those describes a state no store can hold, and
			// seedMaintenance refuses it.
			if m.Category == "convention" || m.Category == "preference" {
				t.Errorf("memory %q is resolved but %q is exempt from SetResolved", m.Key, m.Category)
			}
		case m.Project == globalProject:
			globals++
			if decaysWithAge(m.Category) {
				globalDecaying++
			}
		}
		superseded += len(m.Supersedes)
	}
	if resolved == 0 {
		t.Error("no resolved memory: the suite cannot measure a resolve verdict")
	}
	if globals == 0 {
		t.Error("no _global memory: the suite cannot measure a shared row in a project search")
	}
	if globalDecaying == 0 {
		t.Error("every _global memory is in a never-decay category, so the suite would only ever show the decay-exempt case")
	}
	if superseded == 0 {
		t.Error("no supersedes edge: the suite cannot measure the supersede demote")
	}
	if oldest-youngest < 300 {
		t.Errorf("created_at spans %d days (youngest %d, oldest %d); decay needs a wide spread to be observable",
			oldest-youngest, youngest, oldest)
	}

	supersededKeys := map[string]bool{}
	keys := make(map[string]MaintenanceMemory, len(mems))
	for _, m := range mems {
		keys[m.Key] = m
		for _, older := range m.Supersedes {
			supersededKeys[older] = true
		}
	}

	var withResolved, withGlobal, withSuperseded int
	probes := map[string]int{}
	fair := 0
	for _, q := range qs {
		probes[q.Probe]++
		answer, _, err := answerKey(q)
		if err != nil {
			t.Fatalf("query %q: %v", q.Name, err)
		}
		answerMem, ok := keys[answer]
		if !ok {
			t.Errorf("query %q names answer %q, which is not in the corpus", q.Name, answer)
			continue
		}
		for _, d := range q.Distractors {
			m, ok := keys[d]
			if !ok {
				t.Errorf("query %q names distractor %q, which is not in the corpus", q.Name, d)
				continue
			}
			switch {
			case m.Resolved:
				withResolved++
			case m.Project == globalProject:
				withGlobal++
			case supersededKeys[d]:
				withSuperseded++
			default:
				t.Errorf("query %q: distractor %q is neither resolved, _global nor superseded — this suite grades maintenance state, so a plain distractor is not one",
					q.Name, d)
			}
			// The anti-construction guard. Importance does not lead the
			// ranking (bm25 does; importance only breaks ties), but a corpus
			// where every distractor is weaker than its answer would let a
			// broken ranking look healthy for the same reason the recency-trap
			// fixture's 0.929 was guaranteed by construction.
			if q.Probe == probeLive && m.Importance >= answerMem.Importance {
				fair++
				break
			}
		}
	}
	for kind, n := range map[string]int{"resolved": withResolved, "_global": withGlobal, "superseded": withSuperseded} {
		if n == 0 {
			t.Errorf("no question has a %s distractor", kind)
		}
	}
	if probes[probeGlobal] == 0 {
		t.Error(`no "global" probe: the suite would not show whether demoting shared rows makes them unfindable`)
	}
	if probes[probeLive] == 0 {
		t.Error(`no "live" probe: the suite would not measure the live-wins rate at all`)
	}
	if fair < 10 {
		t.Errorf("only %d live-probe questions have a distractor at least as important as the answer; the suite is too easy by construction", fair)
	}

	// Every memory and question needs a vector, or the vector leg silently
	// measures an empty corpus.
	for _, m := range mems {
		if _, ok := vecs[m.Key]; !ok {
			t.Errorf("no fixture vector for memory %q", m.Key)
		}
	}
	for _, q := range qs {
		if _, ok := vecs[q.Name]; !ok {
			t.Errorf("no fixture vector for query %q", q.Name)
		}
	}
}

// decaysWithAge reports whether a category's decay factor drops below 1 as the
// row ages. It asks the shipped policy (memory.DecayFactor) rather than
// restating the category list, so a change to that policy updates this guard
// with it instead of leaving a second copy to drift.
func decaysWithAge(category string) bool {
	return memory.DecayFactor(category, false, 10_000) < 1
}

// TestMaintenanceSupersedeEdgesMoveLiveWins is the one behavioural assertion in
// this file: with decay off, so the two time signals are isolated, consuming the
// corpus's supersedes edges must raise the live-wins rate. If it does not, the
// edges are not reaching the ranking and the suite is measuring relevance alone —
// which is the state the graded bench is permanently in.
//
// The decay comparison is deliberately NOT asserted. Its current direction (decay
// on lowers live-wins here, because a resolved fact never decays while the live
// dependency copy does) is a finding about the interaction of category-exempt
// decay with resolve state, documented in docs/benchmarks.md for the ranking
// work — not an invariant this suite should freeze.
func TestMaintenanceSupersedeEdgesMoveLiveWins(t *testing.T) {
	mems, qs, vecs := loadMaintenanceFixture(t)
	noDecay := memory.DefaultSearchParams()
	noDecay.DecayEnabled = false
	demoteOff := noDecay
	demoteOff.SupersedeDemote = false

	withoutDemote, err := RunMaintenance(context.Background(), mems, qs, vecs, demoteOff)
	if err != nil {
		t.Fatalf("demote off: %v", err)
	}
	withDemote, err := RunMaintenance(context.Background(), mems, qs, vecs, noDecay)
	if err != nil {
		t.Fatalf("demote on: %v", err)
	}
	base, tuned := hybridResult(withoutDemote), hybridResult(withDemote)
	if base == nil || tuned == nil {
		t.Fatal("no hybrid result")
	}
	t.Logf("decay off, supersede demote: off live-wins=%.3f NDCG@10=%.3f | on live-wins=%.3f NDCG@10=%.3f",
		base.LiveWins, base.NDCG10, tuned.LiveWins, tuned.NDCG10)
	if tuned.LiveWins <= base.LiveWins {
		t.Errorf("supersedes edges did not raise live-wins: %.3f with demote vs %.3f without",
			tuned.LiveWins, base.LiveWins)
	}

	shipped, err := RunMaintenance(context.Background(), mems, qs, vecs, memory.DefaultSearchParams())
	if err != nil {
		t.Fatalf("shipped defaults: %v", err)
	}
	if got := hybridResult(shipped); got != nil {
		t.Logf("shipped defaults (report-only, see docs/benchmarks.md): live-wins=%.3f NDCG@10=%.3f answer-found=%.3f",
			got.LiveWins, got.NDCG10, got.Found)
	}
}

func hybridResult(results []MaintenanceResult) *MaintenanceResult {
	for i := range results {
		if results[i].Condition == CondHybrid {
			return &results[i]
		}
	}
	return nil
}

func TestLoadMaintenanceQueriesRejectsUnusableGrading(t *testing.T) {
	cases := []struct {
		name, line, want string
	}{
		{"no rel", `{"name":"q","text":"t"}`, "empty rel map"},
		{"ambiguous answer", `{"name":"q","text":"t","rel":{"a":3,"b":3}}`, "ambiguous"},
		{"zero gain", `{"name":"q","text":"t","rel":{"a":3,"b":0}}`, "non-positive gain"},
		{"distractor is the answer", `{"name":"q","text":"t","rel":{"a":3},"distractors":["a"]}`, "own answer"},
		{"distractor is relevant", `{"name":"q","text":"t","rel":{"a":3,"b":1},"distractors":["b"]}`, "cannot also be forbidden"},
		{"unknown probe", `{"name":"q","text":"t","rel":{"a":3},"probe":"sideways"}`, `want "live" or "global"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadMaintenanceQueries(strings.NewReader(c.line))
			if err == nil {
				t.Fatalf("LoadMaintenanceQueries accepted %s", c.line)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestLoadMaintenanceMemoriesRejectsUnusableRows(t *testing.T) {
	cases := []struct {
		name, line, want string
	}{
		{"no key", `{"content":"c","category":"fact"}`, "empty key"},
		{"no content", `{"key":"k","category":"fact"}`, "no content"},
		{"no category", `{"key":"k","content":"c"}`, "no category"},
		{"negative age", `{"key":"k","content":"c","category":"fact","age_days":-1}`, "negative age_days"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadMaintenanceMemories(strings.NewReader(c.line))
			if err == nil {
				t.Fatalf("LoadMaintenanceMemories accepted %s", c.line)
			} else if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q", err, c.want)
			}
		})
	}
	if _, err := LoadMaintenanceMemories(strings.NewReader(
		`{"key":"k","content":"a","category":"fact"}` + "\n" + `{"key":"k","content":"b","category":"fact"}`,
	)); err == nil || !strings.Contains(err.Error(), "duplicate memory key") {
		t.Errorf("duplicate keys must be rejected, got %v", err)
	}
}

// TestSeedMaintenanceRejectsBrokenState covers the two state mistakes a fixture
// edit can make that no metric would reveal: a supersedes edge pointing at a key
// that does not exist, and a resolved row in a category SetResolved exempts.
//
// Each case asserts the error text, not just that there is one. seedMaintenance
// rejects a memory with no fixture vector before either guard, so an
// err != nil assertion alone would pass on the wrong rejection — and would keep
// passing if the guard it names were deleted.
func TestSeedMaintenanceRejectsBrokenState(t *testing.T) {
	_, _, fixture := loadMaintenanceFixture(t)
	// A vector the synthetic rows borrow, so the seed loop runs to completion.
	donor, ok := fixture["ms_pg_live"]
	if !ok {
		t.Fatal("committed fixture has no ms_pg_live vector to borrow")
	}

	cases := []struct {
		name string
		mems []MaintenanceMemory
		want string
	}{
		{
			name: "supersedes edge to a key that does not exist",
			mems: []MaintenanceMemory{
				{Key: "a", Category: "fact", Content: "live", AgeDays: 5, Supersedes: []string{"missing"}},
			},
			want: `supersedes unknown key "missing"`,
		},
		{
			name: "resolved row in a category SetResolved exempts",
			mems: []MaintenanceMemory{
				{Key: "c", Category: "convention", Content: "shared", Resolved: true},
			},
			want: "exempt category",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, db := newSeedingStore(t)
			vecs := Vectors{}
			for _, m := range c.mems {
				vecs[m.Key] = donor
			}
			_, err := seedMaintenance(context.Background(), store, db, c.mems, nil, vecs)
			if err == nil {
				t.Fatal("seedMaintenance accepted it")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not mention %q — either the guard is not being reached, "+
					"or something else rejected the fixture first", err, c.want)
			}
		})
	}
}
