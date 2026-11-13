package memory

import (
	"context"
	"database/sql"
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
	// reservation lifts pp_beh_a (gotcha, weighted 1.2) from third to second among
	// the unpinned rows, and the pinned row is FIRST: a pin is a slot guarantee on
	// the passive surfaces (#924), reserved ahead of ranking, so its low importance
	// of 0.10 no longer decides where it sits. The pin exempts a row from decay and
	// reserves it a slot; the unpinned rows are ordered exactly as before.
	assertIDs(t, passiveIDs(set), []string{"pp_pin", "pp_beh_b", "pp_beh_a", "pp_non_b", "pp_non_a", "pp_beh_c"})
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
	// The pinned row holds the first slot (#924); the behavioural reservation is
	// asserted over the rows behind it.
	if got[0] != "pp_pin" {
		t.Fatalf("the pinned row must lead the set: %v", got)
	}
	got = got[1:]
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
	// The fixture's one pinned row is inside the pool the selection fills, and it
	// leads it (#924).
	const pinned = 1
	pool, tail := got[:2*pol.ItemCap], got[2*pol.ItemCap:]
	if len(tail) == 0 {
		t.Fatalf("precondition: the window is exactly the pool, so there is no tail and this test proves nothing")
	}
	// The reserved gotcha is lifted to the front of the pool even though the
	// window ranks it third, which is the reordering the tail is defined against.
	if pool[pinned] != "pp_beh_a" {
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

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
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

func TestCandidatesPassiveGlobalVetoesNearDuplicateDropOnContradict(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()
	if err := st.CreateLink(ctx, "gp_mid", "gp_high", "duplicate", 1, "manual"); err != nil {
		t.Fatalf("link duplicate: %v", err)
	}
	if err := st.CreateLink(ctx, "gp_mid", "gp_high", "contradicts", 1, "manual"); err != nil {
		t.Fatalf("link contradicts: %v", err)
	}

	global, err := st.Candidates(ctx, passiveRequest("proj", globalPassivePolicy()))
	if err != nil {
		t.Fatalf("global passive: %v", err)
	}
	// The contradicts veto spares the near-duplicate loser.
	if !containsStr(passiveIDs(global), "gp_mid") {
		t.Errorf("with a contradicts edge, the loser must not be removed: %v", passiveIDs(global))
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
	// pp_non_b is the later-ranked end of the pair, so it is the loser and the
	// demotion moves it to the back. The pair is two UNPINNED rows: a pinned row
	// is reserved ahead of ranking (#924), so it can only ever win a pair and
	// cannot show whether the gate ran.
	if err := st.CreateLink(ctx, "pp_beh_b", "pp_non_b", "duplicate", 1, "manual"); err != nil {
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
	if underIDs[len(underIDs)-1] == "pp_non_b" {
		t.Errorf("under the cap the demotion must be skipped, but the loser moved to the back: %v", underIDs)
	}

	// Over the cap: the gate is on, and the same pair reorders.
	over := under
	over.ItemCap = 3
	set, err = st.Candidates(ctx, passiveRequest("proj", over))
	if err != nil {
		t.Fatalf("over the cap: %v", err)
	}
	overIDs := passiveIDs(set)
	if indexOf(overIDs, "pp_beh_b") < 0 || indexOf(overIDs, "pp_non_b") < 0 {
		t.Fatalf("over the cap both endpoints must be in the window: %v", overIDs)
	}
	if overIDs[len(overIDs)-1] != "pp_non_b" {
		t.Errorf("over the cap the demotion must run, but the loser was not moved to the back: %v", overIDs)
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
	// pp_non_b is the later-ranked end of an unpinned pair, so it is the loser and
	// the demotion has something real to move. A pinned row is reserved ahead of
	// ranking (#924) and cannot show whether the gate ran.
	if err := st.CreateLink(ctx, "pp_beh_b", "pp_non_b", "duplicate", 1, "manual"); err != nil {
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
			if indexOf(ids, "pp_beh_b") < 0 || indexOf(ids, "pp_non_b") < 0 {
				t.Fatalf("both endpoints must be in the window: %v", ids)
			}
			if moved := ids[len(ids)-1] == "pp_non_b"; moved != tc.wantMoved {
				t.Errorf("the near-duplicate loser moved = %v, want %v: on a set of %d selected rows against a cap "+
					"of %d the reorder runs only when the set is WIDER than the cap, and %v is the order either way",
					moved, tc.wantMoved, len(ids), tc.itemCap, ids)
			}
		})
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

// TestCandidatesPassiveReadsAStoreBelowTheValidityFloor is the validity sibling of
// the tier-floor case above, and it exists for the same reason: the validity
// predicate runs in the fetch SQL, so a non-migrating handle has to know whether
// the columns are there before it names them. The triple arrived at v10
// (migrateV10, phase1aProvenanceColumns), not at the v15 table rebuild that only
// carried it across, so a v10..v18 store is read WITH the predicate and a store
// below v10 must be read without it.
func TestCandidatesPassiveReadsAStoreBelowTheValidityFloor(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()

	// The vector epoch triggers read memories.scope (v24) and, since v25,
	// memories.valid_from/valid_until, and SQLite refuses to drop a column a
	// trigger names. A pre-v10 store has none of these triggers, so the fixture
	// removes them with the columns they postdate — exactly as the tier-floor case
	// above does for scope.
	dropVectorEpoch(t, st.db)
	// No index reads the validity columns, so they drop cleanly. The steps FAIL
	// LOUDLY rather than skipping: a skip would leave the substitution path with no
	// coverage while the test reported nothing wrong.
	for _, col := range []string{"valid_from", "valid_until", "verified_at"} {
		if _, err := st.db.Exec(`ALTER TABLE memories DROP COLUMN ` + col); err != nil {
			t.Fatalf("drop %s to build a pre-v10 store: %v", col, err)
		}
	}
	if _, err := st.db.Exec(`PRAGMA user_version = 9`); err != nil {
		t.Fatalf("stamp an old schema version: %v", err)
	}

	cols, err := passiveColumnsFor(st)
	if err != nil {
		t.Fatalf("resolve the store's shape: %v", err)
	}
	if cols.HasValidity {
		t.Error("a store stamped below the validity floor must not report the validity columns as present")
	}

	// A pinned-order policy. The only binding is the bucket: the window's size is
	// the Go window, not a SQL LIMIT, so a stray limit (or a validity stamp bound
	// but not consumed) shows up as a placeholder count that does not match the
	// argument list.
	pol := projectPassivePolicy()
	pol.Order = OrderPinnedImportanceUpdated
	q, args := passiveKeyScanSQL(pol, passiveRequest("proj"), cols, false)
	// The PREDICATE is what must be absent. The SELECT list still carries
	// `NULL AS valid_from` — that substitution is what keeps the read working — so
	// asserting on the column NAME would be asserting the wrong thing.
	if strings.Contains(q, "strftime") || strings.Contains(q, validityMatchesSQL()) {
		t.Errorf("the validity predicate is present in a statement for a store with no validity columns; it names columns that are not there, "+
			"which is `no such column: valid_from` at run time: %s", q)
	}
	if len(args) != 1 {
		t.Errorf("args = %d, want 1 (the bucket) with no validity stamp and no limit bound on a pre-validity store", len(args))
	}

	set, err := st.Candidates(ctx, passiveRequest("proj", projectPassivePolicy()))
	if err != nil {
		t.Fatalf("a pre-validity store must still be read, not refused: %v", err)
	}
	if len(set.Rows) == 0 {
		t.Fatal("the pre-validity read returned no rows; the NULL substitutions dropped everything")
	}
	for _, r := range set.Rows {
		if r.ValidFrom != nil || r.ValidUntil != nil || r.VerifiedAt != nil {
			t.Errorf("row %s: validity bounds hydrated from a store that has no validity columns: from=%v until=%v verified=%v",
				r.ID, r.ValidFrom, r.ValidUntil, r.VerifiedAt)
		}
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

// TestCandidatesPassiveReportsTheLosersItRemoved (#894): a loser a
// DropDemotedLosers policy removes is not in Rows, so the set carries it in
// DroppedLosers with the id of the row it lost to. A policy that only reorders
// reports none.
func TestCandidatesPassiveReportsTheLosersItRemoved(t *testing.T) {
	st := passiveFixture(t)
	ctx := context.Background()
	if err := st.CreateLink(ctx, "gp_mid", "gp_high", "duplicate", 1, "manual"); err != nil {
		t.Fatalf("link: %v", err)
	}
	set, err := st.Candidates(ctx, passiveRequest("proj", globalPassivePolicy()))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	if containsStr(passiveIDs(set), "gp_mid") {
		t.Fatalf("the loser is still in Rows: %v", passiveIDs(set))
	}
	if len(set.DroppedLosers) != 1 || set.DroppedLosers[0].ID != "gp_mid" ||
		len(set.DroppedLosers[0].LostTo) != 1 || set.DroppedLosers[0].LostTo[0] != "gp_high" {
		t.Errorf("DroppedLosers = %+v, want gp_mid lost to gp_high", set.DroppedLosers)
	}
	// A reorder-only policy removes nothing and so reports nothing.
	reorder := globalPassivePolicy()
	reorder.DropDemotedLosers = false
	set, err = st.Candidates(ctx, passiveRequest("proj", reorder))
	if err != nil {
		t.Fatalf("passive Candidates: %v", err)
	}
	if len(set.DroppedLosers) != 0 || !containsStr(passiveIDs(set), "gp_mid") {
		t.Errorf("a reorder-only policy reported %+v or lost the row", set.DroppedLosers)
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

// seedPassiveBucket adds n live rows to project `p` in one statement, which is
// what lets a test sit on either side of the count's cap without ten thousand
// round trips.
func seedPassiveBucket(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p','/p','p')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		WITH RECURSIVE seq(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM seq WHERE i < ?)
		INSERT INTO memories (id, project_id, category, content, source, importance)
		SELECT 'bulk' || i, 'p', 'fact', 'c', 'manual', 0.5 FROM seq`, n); err != nil {
		t.Fatal(err)
	}
}

// TestPassiveEligibleCountReportsWhenItStoppedAtTheCap: below the cap the count
// is exact and not capped; at the cap and above it the count is the cap, a lower
// bound, and says so. It used to return -1, which the header read as "unknown"
// and replaced with the window's size (issue #1531).
func TestPassiveEligibleCountReportsWhenItStoppedAtTheCap(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rows       int
		want       int
		wantCapped bool
	}{
		{"below the cap", PassiveEligibleCountCap - 1, PassiveEligibleCountCap - 1, false},
		{"at the cap", PassiveEligibleCountCap, PassiveEligibleCountCap, true},
		{"above the cap", PassiveEligibleCountCap + 1, PassiveEligibleCountCap, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := OpenDB(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close() //nolint:errcheck
			seedPassiveBucket(t, db, tc.rows)
			got, excluded, capped, err := NewStore(db, nil).PassiveEligibleCount(
				context.Background(), SlicePolicy{Bucket: "p"}, time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC), nil)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || excluded != 0 || capped != tc.wantCapped {
				t.Errorf("count = %d/%d capped=%v, want %d/0 capped=%v", got, excluded, capped, tc.want, tc.wantCapped)
			}
		})
	}
}

// TestPassiveEligibleCountCapIsBounded pins the cap's size. The aggregate is
// capped so a session start never pays a table pass for a number a reader does
// not act on; a cap of 1<<40 passes every behavioural test while making the
// count unbounded, so the bound itself is asserted. The floor keeps a header
// below it exact for any project a person plausibly keeps.
func TestPassiveEligibleCountCapIsBounded(t *testing.T) {
	if PassiveEligibleCountCap < 1000 || PassiveEligibleCountCap > 100000 {
		t.Errorf("PassiveEligibleCountCap = %d, want within [1000, 100000]", PassiveEligibleCountCap)
	}
}
