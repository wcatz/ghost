package memory

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// passiveFixture seeds a store with the two bucket shapes a session start
// reads: a project set with a behavioral/non-behavioral category mix and a
// pinned row, and a `_global` set whose order is pinned/importance/updated.
//
// created_at is written explicitly so the decay ranking is a function of the
// fixture rather than of the wall clock, which is what lets these tests assert
// an ORDER.
func passiveFixture(t *testing.T) *Store {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "passive.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db, nil)
	ctx := context.Background()
	if err := st.EnsureProject(ctx, "proj", "proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := st.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}
	// created_at values chosen so a pinned low-importance row outranks a
	// high-importance unpinned one, and a behavioral row outranks a
	// non-behavioral one of the same importance.
	rows := []struct {
		id      string
		project string
		cat     string
		imp     float32
		pinned  bool
		created string
	}{
		// project, unpinned, behavioral
		{"pp_beh_a", "proj", "gotcha", 0.80, false, "2026-01-01 00:00:00"},
		{"pp_beh_b", "proj", "convention", 0.70, false, "2026-01-02 00:00:00"},
		// a second gotcha, so the per-category cap has two rows to choose between
		// and a cap of 1 can actually bind
		{"pp_beh_c", "proj", "gotcha", 0.65, false, "2026-01-03 00:00:00"},
		// project, unpinned, non-behavioral
		{"pp_non_a", "proj", "architecture", 0.60, false, "2026-01-04 00:00:00"},
		{"pp_non_b", "proj", "fact", 0.50, false, "2026-01-05 00:00:00"},
		// project, pinned, lowest importance: the exemption must put it first
		{"pp_pin", "proj", "fact", 0.10, true, "2026-01-06 00:00:00"},
		// a resolved row: passive fetch excludes it in SQL
		{"pp_res", "proj", "preference", 0.99, false, "2026-01-06 00:00:00"},
		// global: pinned first regardless of importance
		{"gp_low", "_global", "preference", 0.10, true, "2026-01-07 00:00:00"},
		{"gp_mid", "_global", "preference", 0.50, false, "2026-01-08 00:00:00"},
		{"gp_high", "_global", "preference", 0.90, false, "2026-01-09 00:00:00"},
		{"gp_res", "_global", "preference", 0.99, false, "2026-01-10 00:00:00"},
	}
	for _, r := range rows {
		resolved := ""
		if r.id == "pp_res" || r.id == "gp_res" {
			resolved = "2026-01-11 00:00:00"
		}
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, resolved_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'manual', ?, ?, ?, ?, ?)`,
			r.id, r.project, r.cat, "content of "+r.id, r.imp, r.pinned, nullIfEmpty(resolved), r.created, r.created,
		); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	return st
}

// passiveFixtureCategory is the id-to-category map the fixture writes, so a test
// can read a row's category from its id instead of guessing it from the name.
var passiveFixtureCategory = map[string]string{
	"pp_beh_a": "gotcha",
	"pp_beh_b": "convention",
	"pp_beh_c": "gotcha",
	"pp_non_a": "architecture",
	"pp_non_b": "fact",
	"pp_pin":   "fact",
	"pp_res":   "preference",
	"gp_low":   "preference",
	"gp_mid":   "preference",
	"gp_high":  "preference",
	"gp_res":   "preference",
}

// projectPassivePolicy is the session-start project bucket: 3x over-fetch,
// decay order, a behavioral floor, and no loser-dropping (the cap does that).
func projectPassivePolicy() SlicePolicy {
	return SlicePolicy{
		Bucket:             "proj",
		Order:              "decay",
		TwoPass:            true,
		BehaviorFloor:      2,
		BehaviorCategories: []string{"gotcha", "convention"},
		CategoryWeights:    map[string]float64{"gotcha": 1.2},
		CategoryCaps:       map[string]int{"gotcha": 1},
		OverFetch:          15,
		DemotionThreshold:  0.9,
	}
}

// globalPassivePolicy is the `_global` bucket: 2x over-fetch, pinned/importance
// order, no two-pass, and the loser DROP that the hook applies to globals.
func globalPassivePolicy() SlicePolicy {
	return SlicePolicy{
		Bucket:            "_global",
		Order:             "pinned_importance_updated",
		OverFetch:         16,
		DemotionThreshold: 0.85,
		DropDemotedLosers: true,
	}
}

func passiveRequest(projectID string, policies ...SlicePolicy) CandidateRequest {
	return CandidateRequest{
		ProjectID: projectID,
		Mode:      ProjectScoped,
		Now:       time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		Fetch:     Fetch{Limit: 1},
		Params:    DefaultSearchParams(),
		Condition: CondHybrid,
		Passive:   policies,
	}
}

func passiveIDs(set *CandidateSet) []string {
	ids := make([]string, 0, len(set.Rows))
	for _, r := range set.Rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestCandidatesServesPassiveRetrieval is the RED test for the seam: a request
// with no query and a populated Passive set is served from the store, rather
// than refused as ErrPassiveUnsupported.
func TestCandidatesServesPassiveRetrieval(t *testing.T) {
	st := passiveFixture(t)
	set, err := st.Candidates(context.Background(), passiveRequest("proj", projectPassivePolicy()))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	// The order is the point, and it is not the decay order: the two-pass
	// reservation lifts pp_beh_a (gotcha, weighted 1.2) from third to second, and
	// the pinned row stays LAST because the exemption gives it its raw importance
	// of 0.10 rather than a decayed one. A pinned row is exempt from decay, not
	// promoted above every unpinned row — reading the exemption as a promotion is
	// the mistake this expectation exists to catch.
	assertIDs(t, passiveIDs(set), []string{"pp_beh_b", "pp_beh_a", "pp_non_b", "pp_non_a", "pp_beh_c", "pp_pin"})
}

// TestCandidatesPassiveExcludesResolvedInSQL: a resolved row is excluded by the
// fetch, not by a later stage. The trace therefore records no verdict for it,
// which is the honest statement — the row was never a candidate.
func TestCandidatesPassiveExcludesResolvedInSQL(t *testing.T) {
	st := passiveFixture(t)
	set, err := st.Candidates(context.Background(), passiveRequest("proj", projectPassivePolicy()))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	for _, id := range passiveIDs(set) {
		if id == "pp_res" {
			t.Fatalf("a resolved row reached the passive candidate set: %v", passiveIDs(set))
		}
	}
}

// TestCandidatesPassiveGlobalOrderIsPinnedThenImportanceThenUpdated: the
// `_global` bucket has its own order, and it is not the project bucket's decay
// order. The pinned row wins at the LOWEST importance.
func TestCandidatesPassiveGlobalOrderIsPinnedThenImportanceThenUpdated(t *testing.T) {
	st := passiveFixture(t)
	set, err := st.Candidates(context.Background(), passiveRequest("proj", globalPassivePolicy()))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	assertIDs(t, passiveIDs(set), []string{"gp_low", "gp_high", "gp_mid"})
}

// TestCandidatesPassiveTwoPassReservesBehavioralSlots: the behavioral
// reservation moves the reserved rows AHEAD of the plain score order, which is
// the whole effect. Without it the order would be pp_beh_b, pp_non_b, pp_beh_a,
// pp_non_a — the gotcha at index 2 is exactly the row the floor exists to lift.
func TestCandidatesPassiveTwoPassReservesBehavioralSlots(t *testing.T) {
	st := passiveFixture(t)
	pol := projectPassivePolicy()
	pol.OverFetch = 4
	set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	assertIDs(t, passiveIDs(set), []string{"pp_beh_b", "pp_beh_a", "pp_non_b", "pp_non_a"})
}

// TestCandidatesPassiveTwoPassCapStopsOneCategoryTakingEverySlot: with a cap of
// 1 on gotcha and a gotcha-heavy window, the reservation must not spend both
// slots on gotchas — that is what the per-category cap is for.
func TestCandidatesPassiveTwoPassCapStopsOneCategoryTakingEverySlot(t *testing.T) {
	st := passiveFixture(t)
	pol := projectPassivePolicy()
	pol.OverFetch = 5
	pol.BehaviorFloor = 2
	// The weight is what makes the scenario bite, and it has to clear BOTH
	// gotchas above the convention row: the two gotchas score 0.393 and 0.331
	// and the convention 0.700, so a weight below ~2.1 leaves the convention
	// second-best and the cap has nothing to push out. At 3.0 the two gotchas are
	// 1.180 and 0.994, both ahead of the convention's 0.700, so a reservation
	// without a cap spends BOTH slots on gotchas and the cap of 1 forces the
	// second to the convention row — which is the whole reason the cap exists,
	// since a gotcha-heavy corpus would otherwise leave convention, preference and
	// decision out of every block.
	pol.CategoryWeights = map[string]float64{"gotcha": 3.0}
	pol.CategoryCaps = map[string]int{"gotcha": 1}
	set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	got := passiveIDs(set)
	// The second reserved slot is the convention row, NOT the second gotcha.
	if got[1] != "pp_beh_b" {
		t.Errorf("reserved slots: got %v; the per-category cap must push the second gotcha out of the reservation", got[:2])
	}
	if got[0] != "pp_beh_a" {
		t.Errorf("the highest weighted behavioral row must take the first reserved slot: got %v", got[:2])
	}
	// Every reserved row is behavioral, and exactly one of them is a gotcha.
	behavioral := map[string]bool{"gotcha": true, "convention": true}
	gotchas := 0
	for _, id := range got[:2] {
		cat := passiveFixtureCategory[id]
		if !behavioral[cat] {
			t.Errorf("reserved row %q (%s) is not a behavioral category: the reservation is not applied over its own set: %v",
				id, cat, got)
		}
		if cat == "gotcha" {
			gotchas++
		}
	}
	if gotchas != 1 {
		t.Errorf("reserved gotchas: got %d, want exactly 1 — the cap of 1 did not bind: %v", gotchas, got)
	}
}

// TestCandidatesPassiveOverFetchBoundsTheRead: the over-fetch is the whole
// reason a passive fetch is not a store scan. A request that asks for 2 rows
// gets 2, whatever the store holds.
func TestCandidatesPassiveOverFetchBoundsTheRead(t *testing.T) {
	st := passiveFixture(t)
	for _, n := range []int{1, 2, 3, 5} {
		pol := projectPassivePolicy()
		pol.TwoPass = false
		pol.OverFetch = n
		set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
		if err != nil {
			t.Fatalf("OverFetch %d: %v", n, err)
		}
		if len(set.Rows) != n {
			t.Errorf("OverFetch %d returned %d rows: %v", n, len(set.Rows), passiveIDs(set))
		}
	}
}

// TestCandidatesPassiveReturnsTheTailBehindThePool is the backfill supply the
// whole seam exists for, at its only reachable size.
//
// A policy's selection REORDERS its window rather than truncating it, and the rows
// the pool leaves behind come back behind the selection — undemoted, in the
// policy's own order, so a later stage that drops a selected row has something to
// reach. The tail is only reachable when the window is WIDER than twice the item
// cap, which is why nothing else in this file reaches it: `passiveFixture` holds
// six rows and every policy here states an over-fetch the pool swallows whole, so
// `passiveSelect` returns an empty rest and the property is never exercised. An
// item cap of 1 makes the pool two rows and the four-row window leaves a tail of
// two, which is the smallest shape the claim is true of.
//
// The control run is what makes the tail's ORDER checkable rather than asserted
// twice: with the reservation off, the same policy returns the window in the SQL
// order, so the tail has to be that order with the selected rows taken out of it.
// A tail returned in the selection's order, reversed, or demoted would all differ
// from it.
func TestCandidatesPassiveReturnsTheTailBehindThePool(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()
	pol := projectPassivePolicy()
	pol.OverFetch = 6
	pol.ItemCap = 1 // pool = 2*1 = 2, so four of the six window rows are the tail
	// The gotcha weight is what makes the reservation REORDER rather than merely
	// confirm: at 3.0 the top gotcha outscores the convention row it trails in the
	// window order, so the pool is not a prefix of the control and the tail can be
	// checked against the window rather than against the selection.
	pol.CategoryWeights = map[string]float64{"gotcha": 3.0}
	pol.CategoryCaps = map[string]int{"gotcha": 1}

	control := pol
	control.TwoPass = false
	window, err := st.Candidates(ctx, passiveRequest("proj", control))
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	windowIDs := passiveIDs(window)
	if len(windowIDs) != pol.OverFetch {
		t.Fatalf("precondition: the window is %d rows, want the over-fetch of %d", len(windowIDs), pol.OverFetch)
	}

	set, err := st.Candidates(ctx, passiveRequest("proj", pol))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	got := passiveIDs(set)
	// The whole window, not the pool: the tail travels with the set.
	if len(got) != pol.OverFetch {
		t.Errorf("rows = %d %v, want the whole window of %d: the selection REORDERS rather than truncates, and the "+
			"rows the pool left are the assembler's backfill supply — returning only the selection would leave a "+
			"dropped row with nothing to backfill it from", len(got), got, pol.OverFetch)
	}
	if len(got) < 2*pol.ItemCap {
		t.Fatalf("precondition: %d rows is not enough to hold a pool of %d and a tail", len(got), 2*pol.ItemCap)
	}
	pool, tail := got[:2*pol.ItemCap], got[2*pol.ItemCap:]
	if len(tail) == 0 {
		t.Fatalf("precondition: the window is exactly the pool, so there is no tail and this test proves nothing")
	}
	// The reserved gotcha is lifted to the front of the pool even though the
	// window ranks it third, which is the reordering the tail is defined against.
	if pool[0] != "pp_beh_a" {
		t.Errorf("pool = %v, want the weighted gotcha first: the reservation is what makes the pool differ from the "+
			"window, so a test that could not see that difference would not be testing the tail", pool)
	}
	want := make([]string, 0, len(tail))
	for _, id := range windowIDs {
		if !containsStr(pool, id) {
			want = append(want, id)
		}
	}
	if !eqStrings(tail, want) {
		t.Errorf("tail = %v, want %v: the rows the pool left behind travel in the POLICY's order, so a stage that "+
			"drops a selected row backfills with the next row the policy ranked rather than with a row some other "+
			"stage chose", tail, want)
	}
}

// TestCandidatesPassiveRejectsAnEmptyPolicySet: an empty query with no policy
// is the request that cannot be served honestly. It is refused rather than
// answered with an empty set, because an empty set reads as an empty store.
func TestCandidatesPassiveRejectsAnEmptyPolicySet(t *testing.T) {
	st := passiveFixture(t)
	req := passiveRequest("proj")
	if _, err := st.Candidates(context.Background(), req); err == nil {
		t.Fatal("an empty-query request with no Passive policy must be refused, not served as an empty set")
	}
}

// TestCandidatesPassiveRejectsAPolicyWithNoBucket: a policy that names no
// bucket would silently fetch nothing under it.
func TestCandidatesPassiveRejectsAPolicyWithNoBucket(t *testing.T) {
	st := passiveFixture(t)
	req := passiveRequest("proj", SlicePolicy{Order: "decay", OverFetch: 5})
	if _, err := st.Candidates(context.Background(), req); err == nil {
		t.Fatal("a policy naming no bucket must be refused")
	}
}

// TestCandidatesPassiveNarrowsByScope: the scope predicate is applied in SQL,
// because the over-fetch chooses which rows are read at all — a row the session
// excluded must not spend any of the window.
func TestCandidatesPassiveNarrowsByScope(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()
	// One row matches the session scope and one contradicts it. The rest carry no
	// scope at all, which is NOT a contradiction: an unscoped row is the general
	// knowledge worth keeping, so it is eligible for every session and must
	// survive the narrowing.
	if _, err := st.db.Exec(
		`UPDATE memories SET scope = '{"area":"payments"}' WHERE id = 'pp_non_b'`); err != nil {
		t.Fatalf("set matching scope: %v", err)
	}
	if _, err := st.db.Exec(
		`UPDATE memories SET scope = '{"area":"storage"}' WHERE id = 'pp_non_a'`); err != nil {
		t.Fatalf("set conflicting scope: %v", err)
	}
	pol := projectPassivePolicy()
	pol.TwoPass = false
	req := passiveRequest("proj", pol)
	req.Scope = map[string]string{"area": "payments"}
	set, err := st.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	got := passiveIDs(set)
	for _, id := range got {
		if id == "pp_non_a" {
			t.Errorf("a row whose scope contradicts the session scope reached the window: %v", got)
		}
	}
	if !containsStr(got, "pp_non_b") {
		t.Errorf("the row matching the session scope was dropped: %v", got)
	}
	// The contradicting row is excluded by the FETCH, not by a later stage: the
	// over-fetch is the window, so it must not appear here at all — and every
	// other row does, including the unscoped ones, because an unscoped row is
	// eligible for every session.
	assertIDs(t, got, []string{"pp_beh_b", "pp_non_b", "pp_beh_a", "pp_beh_c", "pp_pin"})
}

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestCandidatesPassiveReportsNoLegs: a passive retrieval ran no leg, and the
// statuses must say so rather than reading as a leg that ran and matched
// nothing. The abstention floor reads these, and an arm that held a value for a
// leg that never ran is a verdict nobody made.
func TestCandidatesPassiveReportsNoLegs(t *testing.T) {
	st := passiveFixture(t)
	set, err := st.Candidates(context.Background(), passiveRequest("proj", projectPassivePolicy()))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	for _, name := range []string{"fts", "vector"} {
		leg := set.Legs[name]
		if leg.Applicable || leg.Attempted || leg.Available {
			t.Errorf("leg %q reports applicable=%v attempted=%v available=%v; a passive retrieval runs no leg",
				name, leg.Applicable, leg.Attempted, leg.Available)
		}
	}
}

// TestCandidatesPassiveRowsCarryNoLegRank: the -1 sentinel means "this leg did
// not retrieve the row" and rank 0 is a real first place. A passive retrieval ran
// no leg, so every row must carry the sentinel — a row reading rank 0 would clear
// the keyword floor arm on a rank nobody produced, and the assembler's outcome
// would then be a verdict about a retrieval that did not happen.
func TestCandidatesPassiveRowsCarryNoLegRank(t *testing.T) {
	st := passiveFixture(t)
	set, err := st.Candidates(context.Background(), passiveRequest("proj", projectPassivePolicy()))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	if len(set.Rows) == 0 {
		t.Fatal("fixture produced no rows")
	}
	for _, r := range set.Rows {
		if r.FTSRank != -1 {
			t.Errorf("row %s: FTSRank is %d; a passive retrieval ran no keyword leg, so it must carry the -1 sentinel (rank 0 is a real first place)",
				r.ID, r.FTSRank)
		}
		if r.VectorRank != -1 || r.VectorScore != -1 {
			t.Errorf("row %s: VectorRank %d / VectorScore %v; a passive retrieval ran no vector leg, so both must be the -1 sentinel",
				r.ID, r.VectorRank, r.VectorScore)
		}
	}
}

// TestCandidatesPassiveReadsEdges: memory_links records when an edge was
// invalidated, never the graph as it stood, and a passive read is a PRESENT read
// — so the graph IS read here, unlike an as_of read, because the conflict stage
// needs it. The assertion is that the status is not `not_applicable`, i.e. that
// the read was made; it deliberately does not assert `ok`, because "the read ran
// and found nothing" and "the read ran" are different claims and only the first
// is guaranteed here.
func TestCandidatesPassiveReadsEdges(t *testing.T) {
	st := passiveFixture(t)
	set, err := st.Candidates(context.Background(), passiveRequest("proj", projectPassivePolicy()))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	if set.EdgesStatus.Status == "not_applicable" {
		t.Error("a passive (present) read must read the link graph; not_applicable is reserved for a historical read")
	}
}

// passiveTieFixture seeds two buckets whose rows TIE on every key their policy's
// ORDER BY names, and writes each pair in the REVERSE of the order the tiebreak
// has to produce.
//
// Both halves matter. A tie is the only thing that can reach the trailing `id`,
// and `passiveFixture` has no tie on (pinned, importance, updated_at) or on the
// decay composite anywhere, which is why the goldens cannot see the tiebreaker at
// all. And the insertion order has to be the wrong way round: without the
// tiebreak the rows come back in whatever order the scan produced, which for a
// table scan is the order they were written, so a pair written high-id-first is
// the only fixture shape where dropping `, id` changes the answer rather than
// leaving it accidentally right.
//
// The two orders tie on DIFFERENT keys, which is the point of testing both: the
// `_global` order is pinned/importance/updated_at and never reads created_at, the
// decay order is the rank composite then importance/created_at and never reads
// updated_at. So each pair differs on the key its own order ignores.
func passiveTieFixture(t *testing.T) *Store {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "passive-tie.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db, nil)
	ctx := context.Background()
	for _, p := range []string{"tieproj", GlobalProjectID} {
		if err := st.EnsureProject(ctx, p, p, p); err != nil {
			t.Fatalf("EnsureProject %s: %v", p, err)
		}
	}
	rows := []struct {
		id, project, cat string
		imp              float32
		created, updated string
	}{
		// _global: identical pinned (false), importance and updated_at, and
		// different created_at — the key that order does not read.
		{"g_b", GlobalProjectID, "preference", 0.5, "2026-01-01 00:00:00", "2026-03-01 00:00:00"},
		{"g_a", GlobalProjectID, "preference", 0.5, "2026-01-09 00:00:00", "2026-03-01 00:00:00"},
		// project: identical importance, created_at and category, so the decay
		// composite is the same number for both, and different updated_at — the
		// key that order does not read.
		{"p_b", "tieproj", "fact", 0.6, "2026-02-02 00:00:00", "2026-04-01 00:00:00"},
		{"p_a", "tieproj", "fact", 0.6, "2026-02-02 00:00:00", "2026-04-02 00:00:00"},
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
			 VALUES (?, ?, ?, ?, 'manual', ?, 0, ?, ?)`,
			r.id, r.project, r.cat, "content of "+r.id, r.imp, r.created, r.updated,
		); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	return st
}

// TestThePassiveOrderBreaksTiesByID: both passive orders end in `id`, and the
// comment above each says why: a tie has no defined order, so without the
// tiebreak the block is a function of the DATABASE FILE rather than of the store's
// contents, which is exactly what a golden comparison across machines cannot
// tolerate. `passiveFixture` cannot see either tiebreak — nothing in it ties on
// (pinned, importance, updated_at) or on the decay composite — so this fixture
// exists to make both reachable.
//
// TwoPass is off deliberately: the reservation REORDERS the window it is given,
// so a test that left it on would be asserting the reservation's order and could
// pass with a missing tiebreak. With it off the order under test is the SQL
// order, unmodified.
func TestThePassiveOrderBreaksTiesByID(t *testing.T) {
	st := passiveTieFixture(t)
	for _, tc := range []struct {
		name   string
		policy SlicePolicy
		want   []string
	}{
		{
			name:   "the _global order",
			policy: SlicePolicy{Bucket: GlobalProjectID, Order: OrderPinnedImportanceUpdated, OverFetch: 4},
			want:   []string{"g_a", "g_b"},
		},
		{
			name:   "the decay order",
			policy: SlicePolicy{Bucket: "tieproj", Order: OrderDecay, OverFetch: 4},
			want:   []string{"p_a", "p_b"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			set, err := st.Candidates(context.Background(), passiveRequest("tieproj", tc.policy))
			if err != nil {
				t.Fatalf("passive Candidates: %v", err)
			}
			if got := passiveIDs(set); !eqStrings(got, tc.want) {
				t.Errorf("rows = %v, want %v: the two rows tie on every other key in this ORDER BY, so the order is "+
					"decided by the trailing `id` alone — and it was written the other way round", got, tc.want)
			}
		})
	}
}

func eqStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func assertIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ids: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids: got %v, want %v", got, want)
		}
	}
}

// TestDecayRankingSQLAtRoundTripsToTheConstant is the guard on the derivation.
// The passive order is produced by substituting the constant's clock, and the
// number of placeholders that produces is the number of bindings the caller must
// pass — so if a future edit to DecayRankingSQL changes the occurrence count,
// this fails rather than the query failing at run time with "missing argument".
func TestDecayRankingSQLAtRoundTripsToTheConstant(t *testing.T) {
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	// Run it twice, with and without the tier half, because the two are different
	// constants and the derivation has to hold for each: a store below the tier
	// floor gets the tier-less expression, and if the substitution only worked on
	// the tiered one the pre-tier path would fail at run time instead of here.
	for _, hasTier := range []bool{true, false} {
		rank, args := decayRankingSQLAt(now, hasTier)
		assertDecayDerivation(t, rank, args, hasTier)
	}
}

func assertDecayDerivation(t *testing.T, rank string, args []any, hasTier bool) {
	t.Helper()
	base := DecayRankingSQLWithTier(hasTier)
	occurrences := strings.Count(base, "julianday('now')")
	if occurrences == 0 {
		t.Fatal("the constant names no wall clock, so the derivation has nothing to substitute")
	}
	if len(args) != occurrences {
		t.Errorf("bindings: got %d, want one per julianday('now') occurrence (%d)", len(args), occurrences)
	}
	if got := strings.Count(rank, "julianday(?)"); got != occurrences {
		t.Errorf("placeholders: got %d, want %d", got, occurrences)
	}
	if strings.Contains(rank, "julianday('now')") {
		t.Error("the derived expression still reads the wall clock, so the window and the decay score could be made against different instants")
	}
	// Substituting back must reproduce the constant exactly: a derivation that
	// loses or reformats part of the formula is a second copy of it, free to
	// drift.
	if back := strings.ReplaceAll(rank, "julianday(?)", "julianday('now')"); back != base {
		t.Error("substituting the placeholders back does not reproduce the constant: the derivation changed the formula")
	}
}

// TestCandidatesPassiveGlobalDropsTheNearDuplicateLoser is where the two
// buckets part company. The global bucket's cap is tight enough that a
// near-duplicate restatement would spend a slot, so its loser is REMOVED; the
// project bucket only reorders and leaves the drop to the cap. Same pair, same
// threshold, opposite membership.
func TestCandidatesPassiveGlobalDropsTheNearDuplicateLoser(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()
	// A `duplicate` edge at strength 1 is what nearDuplicatePenaltyRows reads; a
	// `related` edge at 0.99 would need the same threshold, and the two are not
	// interchangeable, so the one the helper actually keys on is the one used.
	if err := st.CreateLink(ctx, "gp_mid", "gp_high", "duplicate", 1, "manual"); err != nil {
		t.Fatalf("link globals: %v", err)
	}
	if err := st.CreateLink(ctx, "pp_non_a", "pp_non_b", "duplicate", 1, "manual"); err != nil {
		t.Fatalf("link project: %v", err)
	}

	global, err := st.Candidates(ctx, passiveRequest("proj", globalPassivePolicy()))
	if err != nil {
		t.Fatalf("global passive: %v", err)
	}
	// gp_mid is the LOSER of the pair (it is the source pointing at the
	// stronger row), so the drop policy must remove it entirely rather than
	// merely rank it last.
	if containsStr(passiveIDs(global), "gp_mid") {
		t.Errorf("the _global policy drops near-duplicate losers, but gp_mid survived: %v", passiveIDs(global))
	}

	project, err := st.Candidates(ctx, passiveRequest("proj", projectPassivePolicy()))
	if err != nil {
		t.Fatalf("project passive: %v", err)
	}
	if !containsStr(passiveIDs(project), "pp_non_a") || !containsStr(passiveIDs(project), "pp_non_b") {
		t.Errorf("the project policy only reorders — both endpoints of a near-duplicate pair must survive: %v", passiveIDs(project))
	}
}

// TestCandidatesPassiveSupersedeDemotesWithoutDropping: a superseded memory may
// not outrank its replacement, but it is not removed — the assembler decides
// membership, and the retrieval reorders.
func TestCandidatesPassiveSupersedeDemotesWithoutDropping(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()
	// source supersedes target, so this makes pp_pin the replacement and
	// pp_beh_a the superseded row that must be demoted behind it.
	if err := st.CreateLink(ctx, "pp_pin", "pp_beh_a", "supersedes", 1, "manual"); err != nil {
		t.Fatalf("link supersede: %v", err)
	}
	set, err := st.Candidates(ctx, passiveRequest("proj", projectPassivePolicy()))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	got := passiveIDs(set)
	if !containsStr(got, "pp_beh_a") || !containsStr(got, "pp_pin") {
		t.Fatalf("a supersede must not remove either endpoint: %v", got)
	}
	// pp_pin scored BELOW pp_beh_a on the decay order (0.10 pinned vs 0.394),
	// so the demotion is only observable if the loser moved behind its target.
	if indexOf(got, "pp_beh_a") < indexOf(got, "pp_pin") {
		t.Errorf("the superseded row still outranks its replacement: %v", got)
	}
}

func indexOf(hay []string, needle string) int {
	for i, h := range hay {
		if h == needle {
			return i
		}
	}
	return -1
}

// TestCandidatesPassiveRejectsAPolicyWithNoOverFetch is the load-bearing refusal
// on this path. Passive retrieval runs at every session start, and a policy with
// no over-fetch would read the whole store to select fifteen rows from it — the
// cost is invisible from outside (the same rows come back) and the only place it
// can be caught is here, before the query runs.
func TestCandidatesPassiveRejectsAPolicyWithNoOverFetch(t *testing.T) {
	st := passiveFixture(t)
	req := passiveRequest("proj", SlicePolicy{Bucket: "proj", Order: OrderDecay})
	_, err := st.Candidates(context.Background(), req)
	if err == nil {
		t.Fatal("a passive policy with no over-fetch must be refused, not served as a whole-store read")
	}
	if !strings.Contains(err.Error(), "over-fetch") {
		t.Errorf("the refusal must name the missing over-fetch; got %v", err)
	}
}

// TestCandidatesPassiveRejectsAnUnknownOrder: the order is spliced into an
// ORDER BY, so an unrecognised value is validated rather than interpolated — a
// typo that reached the statement would be a syntax error at run time on a path
// that runs at every session start.
func TestCandidatesPassiveRejectsAnUnknownOrder(t *testing.T) {
	st := passiveFixture(t)
	req := passiveRequest("proj", SlicePolicy{Bucket: "proj", Order: "importance_first", OverFetch: 5})
	if _, err := st.Candidates(context.Background(), req); err == nil {
		t.Fatal("an unknown passive order must be refused")
	}
}

// TestCandidatesPassiveNearDuplicateDemotionIsSkippedUnderTheCap is the
// over-cap gate, and it is observable in the ORDER.
//
// Both shipped loaders gate their near-duplicate demotion on the selected set
// being wider than the cap — loadSessionContext on `len(memories) > 15`,
// GetTopMemories on `len(results) > limit` — and the reason is that a demotion is
// a REORDER. On a set that fits entirely under the cap it can only shuffle rows
// the answer shows in full, so the loaders skip it. Applying it anyway produces a
// different order for the same rows: a golden comparison against the old loader
// reads that as a regression on a small store and as nothing at all on a large
// one, which is the worst failure mode a baseline can have.
//
// The fixture makes it observable by pinning the LOSER's counterpart: with a
// pinned row on one side, nearDuplicatePenaltyRows makes the loser the EARLIER
// row, so the demotion moves it down rather than up.
func TestCandidatesPassiveNearDuplicateDemotionIsSkippedUnderTheCap(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()
	// pA carries the duplicate edge and is unpinned; pB is its pinned target.
	if err := st.CreateLink(ctx, "pp_beh_a", "pp_pin", "duplicate", 1, "manual"); err != nil {
		t.Fatalf("link: %v", err)
	}

	// Under the cap: the gate is off, so the pair keeps its selection order.
	under := projectPassivePolicy()
	under.OverFetch = 6
	under.ItemCap = 15 // every row in the window fits under the cap
	under.DemoteOnlyWhenOverCap = true
	under.TwoPass = false
	set, err := st.Candidates(ctx, passiveRequest("proj", under))
	if err != nil {
		t.Fatalf("under the cap: %v", err)
	}
	underIDs := passiveIDs(set)
	if indexOf(underIDs, "pp_beh_a") > indexOf(underIDs, "pp_pin") {
		t.Errorf("under the cap the demotion must be skipped, but the loser moved behind its target: %v", underIDs)
	}

	// Over the cap: the gate is on, and the same pair reorders.
	over := under
	over.ItemCap = 3
	set, err = st.Candidates(ctx, passiveRequest("proj", over))
	if err != nil {
		t.Fatalf("over the cap: %v", err)
	}
	overIDs := passiveIDs(set)
	if indexOf(overIDs, "pp_beh_a") < 0 || indexOf(overIDs, "pp_pin") < 0 {
		t.Fatalf("over the cap both endpoints must be in the window: %v", overIDs)
	}
	if indexOf(overIDs, "pp_beh_a") < indexOf(overIDs, "pp_pin") {
		t.Errorf("over the cap the demotion must run, but the loser still outranks its target: %v", overIDs)
	}
}

// TestTheOverCapGateIsExactAtTheItemCap is the boundary the near-duplicate gate
// above never reached: a selected set of EXACTLY ItemCap rows.
//
// The shipped loaders skip the reorder on `len(memories) > cap` — WIDER than the
// cap, not "at or wider" — and the passive gate is `<=`, so a set that fills the
// cap to the row is still a set the answer shows in full and the reorder is still
// the specification to skip. Relaxing `<=` to `<` runs it there, which is a
// different ORDER for the same rows: a golden comparison against the old loader
// would read it as a regression, and on a store whose cap happens to equal its
// memory count there is no other evidence at all.
//
// The fixture is `passiveFixture`'s, whose six unresolved project rows are the
// window, and the cap is set to that six so the comparison is on the boundary
// itself rather than near it. The three arms are the whole statement: at the cap
// the pair keeps its selection order, one row under it does too, and one row over
// it reorders. Without the third arm a gate that never fired would pass the first
// two.
func TestTheOverCapGateIsExactAtTheItemCap(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()
	// pp_beh_a carries the duplicate edge and is unpinned; pp_pin is its pinned
	// target, so nearDuplicatePenaltyRows makes the EARLIER row the loser and the
	// demotion has something real to move.
	if err := st.CreateLink(ctx, "pp_beh_a", "pp_pin", "duplicate", 1, "manual"); err != nil {
		t.Fatalf("link: %v", err)
	}
	pol := projectPassivePolicy()
	pol.OverFetch = 6   // the fixture's whole project window
	pol.TwoPass = false // the pool IS the window here, so the gate sees all six rows
	pol.DemoteOnlyWhenOverCap = true

	for _, tc := range []struct {
		name      string
		itemCap   int
		wantMoved bool
	}{
		{"one row under the cap", 7, false},
		{"exactly at the cap", 6, false},
		{"one row over the cap", 5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pol
			p.ItemCap = tc.itemCap
			set, err := st.Candidates(ctx, passiveRequest("proj", p))
			if err != nil {
				t.Fatalf("ItemCap %d: %v", tc.itemCap, err)
			}
			ids := passiveIDs(set)
			if len(ids) != p.OverFetch {
				t.Fatalf("ItemCap %d: window rows = %d, want the over-fetch of %d; the gate is decided on the "+
					"SELECTED set and the fixture is what sizes it", tc.itemCap, len(ids), p.OverFetch)
			}
			loser, target := indexOf(ids, "pp_beh_a"), indexOf(ids, "pp_pin")
			if loser < 0 || target < 0 {
				t.Fatalf("both endpoints must be in the window: %v", ids)
			}
			if moved := loser > target; moved != tc.wantMoved {
				t.Errorf("the near-duplicate loser moved = %v, want %v: on a set of %d selected rows against a cap "+
					"of %d the reorder runs only when the set is WIDER than the cap, and %v is the order either way",
					moved, tc.wantMoved, len(ids), tc.itemCap, ids)
			}
		})
	}
}

// TestCandidatesPassiveAnUnstatedThresholdFallsBackToTheStores is a data-loss
// guard, not a tidiness one. DemotionPenalties binds the threshold as
// `l.strength >= ?`, so a policy that states none would demote at 0.0 and make
// EVERY `related` edge a near-duplicate. On the `_global` bucket, whose policy
// DROPS losers, that deletes a memory over a 0.1-similarity edge — a row silently
// vanishing from a session-start block.
func TestCandidatesPassiveAnUnstatedThresholdFallsBackToTheStores(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()
	// A `related` edge at 0.1: nowhere near a near-duplicate at the store's
	// configured threshold, and a near-duplicate at zero.
	if err := st.CreateLink(ctx, "gp_mid", "gp_high", "related", 0.1, "manual"); err != nil {
		t.Fatalf("link: %v", err)
	}

	pol := globalPassivePolicy()
	pol.DemotionThreshold = 0 // the Go zero value: what a caller that states none sends
	set, err := st.Candidates(ctx, passiveRequest("proj", pol))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	// gp_mid must SURVIVE. At the store's configured threshold a 0.1 `related`
	// edge is not a near-duplicate, so no penalty is recorded and the global
	// bucket's drop-losers policy has nothing to drop. It would be absent if the
	// unstated threshold were taken as 0.0.
	if !containsStr(passiveIDs(set), "gp_mid") {
		t.Errorf("a 0.1 `related` edge removed a global row: an unstated threshold must fall back to the store's configured "+
			"one, not demote at 0.0 (got %v)", passiveIDs(set))
	}
}

// TestCandidatesPassiveRejectsARepeatedBucket: two policies naming one bucket
// fetch its rows twice and concatenate them, so the set carries every id twice —
// a duplicate row reaching the assembler, which would then report two rows that
// happened to rank equally.
func TestCandidatesPassiveRejectsARepeatedBucket(t *testing.T) {
	st := passiveFixture(t)
	req := passiveRequest("proj", projectPassivePolicy(), projectPassivePolicy())
	if _, err := st.Candidates(context.Background(), req); err == nil {
		t.Fatal("two policies naming one bucket must be refused, not served with every row twice")
	}
}

// TestCandidatesPassiveRejectsAWindowBeyondTheCeiling: the refusals above are
// about a request that states no bound; this one is about a request that states
// too large a one. A passive window is read, ordered and demoted in full with no
// leg to discard behind it, so an oversized one is a large read on a path that
// runs at every session start.
func TestCandidatesPassiveRejectsAWindowBeyondTheCeiling(t *testing.T) {
	st := passiveFixture(t)
	pol := projectPassivePolicy()
	pol.OverFetch = maxPassiveOverFetch + 1
	_, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
	if err == nil {
		t.Fatalf("a passive over-fetch above the ceiling of %d must be refused", maxPassiveOverFetch)
	}
	if !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("the refusal must name the ceiling; got %v", err)
	}
}

// TestCandidatesPassiveAsOfIsRefusedRatherThanSilentlyAnswered: Run dispatches
// on AsOf before Query, so a passive request carrying an as_of would be answered
// by the historical path — policies discarded, window the caller never stated —
// while the caller believed its over-fetches were in force. Nothing refused it
// before this.
func TestCandidatesPassiveAsOfIsRefusedRatherThanSilentlyAnswered(t *testing.T) {
	st := passiveFixture(t)
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	req := passiveRequest("proj", projectPassivePolicy())
	req.AsOf = &now
	req.Fetch = Fetch{Limit: 1}
	_, err := st.Candidates(context.Background(), req)
	if err == nil {
		t.Fatal("a passive request carrying an as_of must be refused, not routed to the historical path with its policies discarded")
	}
	if !strings.Contains(err.Error(), "passive") {
		t.Errorf("the refusal must name the passive shape; got %v", err)
	}
}

// TestCandidatesPassiveZeroAsOfIsStillRefused: the AsOf guards sit ABOVE the
// passive branch, so a zero instant cannot reach the policy check and be answered
// with the empty set that reads as "nothing existed at year 1". Candidates is
// reachable directly, so the store's own guard is the only one there is.
func TestCandidatesPassiveZeroAsOfIsStillRefused(t *testing.T) {
	st := passiveFixture(t)
	zero := time.Time{}
	req := passiveRequest("proj", projectPassivePolicy())
	req.AsOf = &zero
	_, err := st.Candidates(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), "zero time is not one") {
		t.Errorf("a zero as_of instant must be refused on the passive path too; got %v", err)
	}
}

// TestCandidatesPassiveIsNotWidened pins the meaning of the field rather than its
// absence: `Widened` reports a set LARGER than the requested window, which is how
// the query path's discarded tail travels. A passive fetch has no tail behind the
// window — it returns the whole window, selection first — so the honest value is
// false, and writing the opposite would report a widening that did not happen.
func TestCandidatesPassiveIsNotWidened(t *testing.T) {
	st := passiveFixture(t)
	for _, pol := range []SlicePolicy{projectPassivePolicy(), globalPassivePolicy()} {
		set, err := st.Candidates(context.Background(), passiveRequest("proj", pol))
		if err != nil {
			t.Fatalf("bucket %s: %v", pol.Bucket, err)
		}
		if set.Widened {
			t.Errorf("bucket %s: Widened is true, but a passive set is never larger than the window it read (%d rows)",
				pol.Bucket, len(set.Rows))
		}
	}
}

// TestCandidatesPassiveOverAnEmptyStoreReturnsAnEmptySetAndNoError is the store
// half of the passive empty path. It matters because the assembler's `no_memories`
// reason is reachable ONLY from it: the fake-retriever tests cannot produce an
// empty over-fetched window, so without this the reason has no end-to-end
// coverage — a store that ERRORED on an empty bucket would render the same
// "nothing was found" sentence as a store that had nothing, which is the
// retrieval-failure/empty-result conflation the seam exists to prevent.
func TestCandidatesPassiveOverAnEmptyStoreReturnsAnEmptySetAndNoError(t *testing.T) {
	st := passiveFixture(t)
	// A bucket that exists as a project but holds nothing in scope.
	if err := st.EnsureProject(context.Background(), "emptyproj", "emptyproj", "emptyproj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// A policy naming the EMPTY project, not the populated one: a passive read
	// filters on the policy's own bucket and ignores Mode, so a policy left
	// pointing at the fixture's project would read that project instead.
	empty := projectPassivePolicy()
	empty.Bucket = "emptyproj"
	req := passiveRequest("emptyproj", empty)
	set, err := st.Candidates(context.Background(), req)
	if err != nil {
		t.Fatalf("an empty bucket is not a retrieval failure: %v", err)
	}
	if len(set.Rows) != 0 {
		t.Errorf("rows: got %d, want 0", len(set.Rows))
	}
	// The statuses must be readable as "no leg ran" rather than as a leg that
	// answered and matched nothing, because the abstention floor reads them.
	for _, name := range []string{"fts", "vector"} {
		if leg := set.Legs[name]; leg.Applicable || leg.Attempted {
			t.Errorf("leg %q: applicable=%v attempted=%v; a passive retrieval runs no leg", name, leg.Applicable, leg.Attempted)
		}
	}
	// Widened is false for the same reason it is false for a populated bucket: an
	// empty set is not larger than the window.
	if set.Widened {
		t.Error("Widened is true on an empty passive set, which reports a widening that did not happen")
	}
}

// TestCandidatesPassiveReadsAStoreBelowTheTierFloor is the schema-floor case, and
// it exists because a passive read is the first whole-Memory read the memory
// package makes through a NON-MIGRATING handle. Every store Ghost opens itself
// migrates on the way in, and the session-start loaders that read through
// OpenReadDB each carry their own version probe — so without one here the
// session-start migration would fail on a pre-v19 store with "no such column:
// retention", where the loader it replaces renders the block perfectly well.
//
// A pre-v12 store is folded in: scope and retention arrived at different versions,
// and the fixture drops the table and recreates the columns, so the same test
// covers "the columns this build expects are not all there".
func TestCandidatesPassiveReadsAStoreBelowTheTierFloor(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()

	// The partial index the tier migration created reads BOTH columns in its WHERE
	// clause, and SQLite refuses to drop a column such an index reads — so the
	// index goes first, as the two other pre-v19 fixtures in the tree do
	// (mcpinit/retentioncolumn_test.go, memory/asof_test.go). It is recreated by
	// the next migration, so nothing is lost by removing it here. Every step
	// FAILS LOUDLY rather than skipping: a skip would leave the NULL-substitution
	// path with no coverage at all while the test reported nothing wrong, which is
	// the failure this test exists to catch.
	if _, err := st.db.Exec(`DROP INDEX IF EXISTS idx_memories_session_expiry`); err != nil {
		t.Fatalf("drop the partial index the tier migration created: %v", err)
	}
	for _, col := range []string{"retention", "expires_at", "scope"} {
		if _, err := st.db.Exec(`ALTER TABLE memories DROP COLUMN ` + col); err != nil {
			t.Fatalf("drop %s to build a pre-v12 store: %v", col, err)
		}
	}
	// Below the SCOPE floor as well as the tier one, so the fetch has to
	// substitute for three columns and suppress the scope predicate.
	if _, err := st.db.Exec(`PRAGMA user_version = 11`); err != nil {
		t.Fatalf("stamp an old schema version: %v", err)
	}

	// A session scope is REQUESTED, so the suppressed predicate is reachable rather
	// than inert: without the gate the fetch names a column this store does not
	// have and the whole read fails.
	req := passiveRequest("proj", projectPassivePolicy())
	req.Scope = map[string]string{"area": "payments"}

	// The claim itself, asserted directly rather than only through the read: on a
	// store below the scope floor the built statement must not NAME the scope
	// column at all. Asserting it this way rather than relying on the read to fail
	// is deliberate — the read is the thing being protected, so a driver that
	// tolerated the reference would leave this test green with the gate removed,
	// and the gate is the guarantee the loaders' version depends on.
	cols, err := passiveColumnsFor(st)
	if err != nil {
		t.Fatalf("resolve the store's shape: %v", err)
	}
	if cols.HasScope {
		t.Error("a store stamped below the scope floor must not report the scope column as present")
	}
	pol := projectPassivePolicy()
	q, _ := passiveFetchSQL(pol, req, cols)
	if strings.Contains(q, "json_each") {
		t.Errorf("the scope predicate is present in a statement for a store with no scope column; it names a column that is not there, "+
			"which is `no such column: scope` at run time: %s", q)
	}

	set, err := st.Candidates(ctx, req)
	if err != nil {
		t.Fatalf("a pre-tier, pre-scope store must still be read, not refused: %v", err)
	}
	if len(set.Rows) == 0 {
		t.Fatal("the pre-tier read returned no rows; the NULL substitutions dropped everything")
	}
	for _, r := range set.Rows {
		// `project`, NOT "": scanMemories resolves an empty tier once for every
		// reader, so a row hydrated through `NULL AS retention` arrives as
		// `project`. Asserting "" here would be asserting a value the shared
		// scanner cannot produce. The intent is that a pre-tier row behaves as a
		// project row rather than as a fourth tier, and that is the scanner's
		// contract — the one place that resolves an absent column, for every
		// reader.
		if r.Retention != RetentionProject {
			t.Errorf("row %s: Retention is %q, want %q — a store with no tier column holds project rows by definition",
				r.ID, r.Retention, RetentionProject)
		}
		if RetentionExempt(r.Memory) {
			t.Errorf("row %s reads as tier-exempt on a store that has no tier column, so its demotion protection is unearned", r.ID)
		}
	}
}

// TestCandidatesPassiveSchemaVersionIsReadThroughTheSnapshot is a DEADLOCK
// guard, and it is the kind of bug that shows up as a hung test rather than a
// failed one.
//
// `Candidates` opens a read transaction before dispatching, and the pool is
// pinned at MaxOpenConns(1): that transaction holds the only connection. A
// version probe issued on the POOL while it is open waits for a connection that
// cannot be handed out. The first version of this code read PRAGMA user_version
// from the primary handle and hung the whole package for its timeout.
func TestCandidatesPassiveSchemaVersionIsReadThroughTheSnapshot(t *testing.T) {
	st := passiveFixture(t)
	done := make(chan error, 1)
	go func() {
		_, err := st.Candidates(context.Background(), passiveRequest("proj", projectPassivePolicy()))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("passive Candidates: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("passive Candidates did not return: the schema version is being read on the pool while the read transaction holds the only connection")
	}
}

// TestCandidatesPassiveRefusesPoliciesWithAQuery is the mirror of the as_of
// refusal. A request with a query is answered by the fusion path, which reads no
// policy and sizes its own window from Fetch.Limit — so a caller that believes
// its over-fetches are in force would be answered without them.
func TestCandidatesPassiveRefusesPoliciesWithAQuery(t *testing.T) {
	st := passiveFixture(t)
	req := passiveRequest("proj", projectPassivePolicy())
	req.Query = "some query"
	req.Fetch = Fetch{Limit: 5, FTSTopK: 10, VectorTopK: 10}
	if _, err := st.Candidates(context.Background(), req); err == nil {
		t.Fatal("a request carrying passive policies must have an empty query, or the policies are silently discarded")
	}
}

// TestCandidatesPassiveAPersistentRowIsNotANearDuplicateLoser is the protection
// map, and a pin-only map here is a hole DemotionPenalties' own doc names: an
// unpinned `persistent` row on the losing end of a pair is penalised here and
// spared by every other caller in the tree. On a bucket that drops losers,
// penalised means REMOVED — a persistent memory silently leaving the
// session-start block because something restated it.
//
// The supersede map is the mirror image and is deliberately tier-ONLY, so the two
// are separate maps rather than one. The fixture makes the near-duplicate case
// observable by pinning the WINNER, which is what makes the loser the
// lower-importance end.
func TestCandidatesPassiveAPersistentRowIsNotANearDuplicateLoser(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()
	// gp_high is the pinned, higher-importance winner; gp_mid is the loser.
	if err := st.CreateLink(ctx, "gp_mid", "gp_high", "duplicate", 1, "manual"); err != nil {
		t.Fatalf("link: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE memories SET retention = 'persistent' WHERE id = 'gp_mid'`); err != nil {
		t.Fatalf("set retention: %v", err)
	}

	pol := globalPassivePolicy() // DropDemotedLosers, the case that removes
	set, err := st.Candidates(ctx, passiveRequest("proj", pol))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	if !containsStr(passiveIDs(set), "gp_mid") {
		t.Errorf("a persistent row was treated as a near-duplicate loser and removed; the protection map must include the tier "+
			"(got %v)", passiveIDs(set))
	}
}

// TestCandidatesPassiveSkipsTheEvidenceReadOnAPreProvenanceStore: the version
// tolerance covered the SELECT list, and the read AFTER it did not follow.
//
// `memory_provenance` is a TABLE migrateV18 creates, so a store at v12..v17 has
// never had it — and that read's error is returned, so on exactly the range
// passiveColumnsFor exists to support the whole retrieval failed with "no such
// table: memory_provenance" where the loader it replaces renders the block
// perfectly well. The demotion lookups degrade to "no penalty" and the edge status
// to "found nothing"; this one had to degrade the same way, and the fixture has to
// DROP the table rather than merely stamp an old version, because a stamped version
// on a fully-migrated database still has every table physically present.
func TestCandidatesPassiveSkipsTheEvidenceReadOnAPreProvenanceStore(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()
	if _, err := st.db.Exec(`DROP TABLE IF EXISTS memory_provenance`); err != nil {
		t.Fatalf("drop the evidence table: %v", err)
	}
	if _, err := st.db.Exec(`PRAGMA user_version = 17`); err != nil {
		t.Fatalf("stamp a pre-provenance version: %v", err)
	}

	set, err := st.Candidates(ctx, passiveRequest("proj", projectPassivePolicy()))
	if err != nil {
		t.Fatalf("a pre-provenance store must still be read; the evidence read is not evidence of absence: %v", err)
	}
	if len(set.Rows) == 0 {
		t.Fatal("the pre-provenance read returned no rows")
	}
	for _, r := range set.Rows {
		if r.Evidence != (EvidenceCounts{}) {
			t.Errorf("row %s: Evidence is %+v, want the zero a store with no provenance table can honestly report", r.ID, r.Evidence)
		}
	}
}

// TestCandidatesPassiveANearDuplicateLoserIsDecidedOnThePostSupersedeOrder is the
// id-order reuse, and it is a MEMBERSHIP decision rather than a presentation one.
//
// `nearDuplicatePenaltyRows` decides which member of a pair loses from the order it
// is handed, so it has to be the order the supersede demote left behind. Reusing
// the pre-demote slice ranks a row that has just been pushed down as if it had not
// moved.
//
// The fixture is built so the two orders disagree about which row loses, and the
// bucket DROPS losers so the difference is a row leaving the block:
//
//	global order by pin/importance   gp_low (pinned), gp_high, gp_mid
//	after `gp_low supersedes gp_high`  gp_low, gp_mid, gp_high   <- gp_high demoted
//	near-duplicate pair gp_mid/gp_high, loser is the LATER one
//	  correct order -> gp_high is later -> gp_high is dropped
//	  stale  order  -> gp_mid  is later -> gp_mid  is dropped
//
// So asserting which of the two survives pins the order the lookup was given, and
// both shipped readers rebuild the slice after the reorder for exactly this reason
// (hook.go:1219, store.go:3955).
func TestCandidatesPassiveANearDuplicateLoserIsDecidedOnThePostSupersedeOrder(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()
	if err := st.CreateLink(ctx, "gp_low", "gp_high", "supersedes", 1, "manual"); err != nil {
		t.Fatalf("link supersede: %v", err)
	}
	if err := st.CreateLink(ctx, "gp_mid", "gp_high", "duplicate", 1, "manual"); err != nil {
		t.Fatalf("link duplicate: %v", err)
	}
	set, err := st.Candidates(ctx, passiveRequest("proj", globalPassivePolicy()))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	got := passiveIDs(set)
	if !containsStr(got, "gp_mid") {
		t.Errorf("gp_mid must survive: the supersede demote put gp_high behind it, so the near-duplicate lookup saw "+
			"gp_high as the later member and dropped it. Dropping gp_mid instead means the lookup was handed the order "+
			"from BEFORE the supersede demote (got %v)", got)
	}
	if containsStr(got, "gp_high") {
		t.Errorf("gp_high is the later member after the supersede demote and is the near-duplicate loser, so it must "+
			"be dropped (got %v)", got)
	}
}

// TestCandidatesPassiveAnUnreadableVersionStillAttemptsTheEvidenceRead is the third
// state, and it is the one the previous fix got backwards.
//
// Substituting for a column that might not exist is the safe direction, so an
// unreadable version leaves the COLUMNS at their floor. Skipping a READ that might
// have succeeded is the opposite: a skipped read reports "no recorded evidence",
// which is a claim about support that the store may well contradict. The flag
// therefore distinguishes "known to be below the floor" from "unknown", and only
// the former skips.
//
// The fixture cannot make the PRAGMA fail on demand, so the two halves are pinned
// separately: the flag's three states through the resolver, and the tolerated
// failure through the helper that decides it.
func TestCandidatesPassiveAnUnreadableVersionStillAttemptsTheEvidenceRead(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()

	// Known and current: the read happens.
	cols, err := passiveColumnsFor(st)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !cols.ProvenanceKnown || !cols.HasProvenance {
		t.Errorf("a current store: ProvenanceKnown=%v HasProvenance=%v, want both true — an unknown state would skip the read on a store that has the table",
			cols.ProvenanceKnown, cols.HasProvenance)
	}

	// Known and below the floor: the read is skipped, and the flag says so.
	if _, err := st.db.Exec(`PRAGMA user_version = 17`); err != nil {
		t.Fatalf("stamp a pre-provenance version: %v", err)
	}
	cols, err = passiveColumnsFor(st)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !cols.ProvenanceKnown {
		t.Error("ProvenanceKnown must be true for a version that WAS read, even when the read says the table is absent")
	}
	if cols.HasProvenance {
		t.Error("a store at v17 has never had memory_provenance, so HasProvenance must be false")
	}

	// The tolerated failure is specifically a missing TABLE, so the guard cannot
	// swallow a real one.
	if !isMissingTable(errors.New("SQL logic error: no such table: memory_provenance")) {
		t.Error("a missing table must be recognised as the tolerated failure")
	}
	for _, other := range []string{
		"SQL logic error: database is locked",
		"candidates: evidence counts: context deadline exceeded",
		"no such column: memory_id",
	} {
		if isMissingTable(errors.New(other)) {
			t.Errorf("%q must NOT be tolerated: only an absent table means 'never recorded', and anything else would turn a "+
				"failed read into a false claim that no memory is supported", other)
		}
	}
	if isMissingTable(nil) {
		t.Error("a nil error is not a missing table")
	}
	_ = ctx
}

// TestPassiveColumnsEvidenceReadModeIsThreeStates is the pinning of the branch a
// test cannot otherwise reach: whether the store is KNOWN to have the evidence
// table, or merely not known to lack it, decides between reading it, reading it
// tolerantly, and not reading it at all — and an unreadable version must never
// take the skipping branch, because a skipped read reports "no recorded evidence"
// as a fact about support.
func TestPassiveColumnsEvidenceReadModeIsThreeStates(t *testing.T) {
	for _, tc := range []struct {
		name string
		cols passiveColumns
		want evidenceReadMode
	}{
		{"known present", passiveColumns{HasProvenance: true, ProvenanceKnown: true}, evidenceReadPlain},
		{"known absent", passiveColumns{HasProvenance: false, ProvenanceKnown: true}, evidenceReadSkip},
		{"unknown", passiveColumns{HasProvenance: false, ProvenanceKnown: false}, evidenceReadAttemptTolerating},
	} {
		if got := tc.cols.evidenceReadMode(); got != tc.want {
			t.Errorf("%s: got mode %d, want %d — an unknown version must never take the skipping branch", tc.name, got, tc.want)
		}
	}
}
