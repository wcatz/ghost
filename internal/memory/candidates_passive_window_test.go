package memory

import (
	"context"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// legacyPassiveWindow is the query the fetch used to run: the policy's order
// applied by the engine and cut by the over-fetch. It is the specification the
// threshold scan has to reproduce, and it is built from the fragments the old
// fetch spliced (decayRankingSQLAt and passivePopulationSQL) rather than
// restated, so it cannot drift from the order it replaces.
func legacyPassiveWindow(t *testing.T, s *Store, pol SlicePolicy, req CandidateRequest, cols passiveColumns) []string {
	t.Helper()
	where, args := passivePopulationSQL(pol, req, cols)
	if cols.HasValidity {
		stamp := req.Now.UTC().Format(stampLayoutForSQL)
		where += " AND " + validityMatchesSQL()
		args = append(args, stamp, stamp)
	}
	orderBy := "pinned DESC, importance DESC, updated_at DESC, id"
	if pol.Order == "" || pol.Order == OrderDecay {
		rank, clock := decayRankingSQLAt(req.Now, cols.HasTier)
		orderBy = fmt.Sprintf("pinned DESC, (%s) DESC, importance DESC, created_at DESC, id", rank)
		args = append(args, clock...)
	}
	q := fmt.Sprintf("SELECT %s FROM memories WHERE %s ORDER BY %s LIMIT ?", cols.list, where, orderBy)
	args = append(args, pol.OverFetch)
	rows, err := s.queryDB().QueryContext(context.Background(), q, args...)
	if err != nil {
		t.Fatalf("legacy window: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	memories, err := scanMemories(rows)
	if err != nil {
		t.Fatalf("scan legacy window: %v", err)
	}
	return idsOfMemories(memories)
}

// TestPassiveThresholdScanMatchesTheLegacyWindow is the differential pin: the
// window the threshold scan keeps is the same rows the SQL ORDER BY ... LIMIT
// returned, in the same order, over random stores that exercise every axis the
// order has — importances a float32 cannot name, pinned rows, every category and
// tier, ages across six years, deliberate ties, resolved and validity-filtered
// rows, and the `_global` bucket.
func TestPassiveThresholdScanMatchesTheLegacyWindow(t *testing.T) {
	rng := rand.New(rand.NewSource(20261015))
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for trial := 0; trial < 30; trial++ {
		st := randomPassiveStore(t, rng, trial)
		cols, err := passiveColumnsFor(st)
		if err != nil {
			t.Fatalf("passiveColumnsFor: %v", err)
		}
		union := projectPassivePolicy()
		union.IncludeGlobal = true
		for _, pol := range []SlicePolicy{projectPassivePolicy(), globalPassivePolicy(), union} {
			req := passiveRequest("proj", pol)
			req.Now = now
			got, err := st.passiveWindow(context.Background(), req, pol, cols)
			if err != nil {
				t.Fatalf("trial %d %s: passiveWindow: %v", trial, pol.Order, err)
			}
			want := legacyPassiveWindow(t, st, pol, req, cols)
			if !eqStrings(idsOfMemories(got), want) {
				t.Fatalf("trial %d %s: the threshold scan differs from the legacy window:\n got %v\nwant %v",
					trial, pol.Order, idsOfMemories(got), want)
			}
		}
	}
}

// randomPassiveStore builds one trial's store. Every value that decides the
// order is drawn independently, and several rows are pinned deliberately, so the
// next trial's shape is a fresh chance to find a divergence rather than a
// variation of one fixture.
func randomPassiveStore(t *testing.T, rng *rand.Rand, trial int) *Store {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), fmt.Sprintf("rand-%d.db", trial)))
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
	cats := []string{"gotcha", "preference", "architecture", "decision", "fact", "pattern", "convention", "dependency"}
	tiers := []string{"project", "session", "persistent"}
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	n := 15 + rng.Intn(25)
	// One transaction for the whole trial: an autocommit per row paid a commit
	// for each of the ~60 trials' rows.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < n; i++ {
		// A double that float32 cannot name, so a comparator reading the float32
		// field would rank a near-tie differently than the engine did.
		imp := rng.Float64()
		if i%7 == 0 {
			imp = 0.5 // an exact tie on importance, for the id tie-break
		}
		pinned := 0
		if rng.Intn(6) == 0 {
			pinned = 1
		}
		created := base.Add(time.Duration(rng.Int63n(int64(6*365*24))) * time.Hour).Format("2006-01-02 15:04:05")
		if i%5 == 0 {
			created = base.Format("2006-01-02 15:04:05") // a tie on created_at
		}
		project := "proj"
		if i%11 == 0 {
			project = "_global"
		}
		resolved := any(nil)
		if i%13 == 0 {
			resolved = "2026-01-01 00:00:00"
		}
		validUntil := any(nil)
		if i%17 == 0 {
			validUntil = "2025-01-01 00:00:00" // expired: the SQL predicate must drop it
		}
		validFrom := any(nil)
		if i%19 == 0 {
			validFrom = "2030-01-01 00:00:00" // not yet open
		}
		if _, err := tx.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, resolved_at,
			                       created_at, updated_at, retention, valid_from, valid_until)
			 VALUES (?, ?, ?, ?, 'manual', ?, ?, ?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("t%02d-m%04d", trial, i), project, cats[rng.Intn(len(cats))],
			"random fixture "+fmt.Sprint(i), imp, pinned, resolved, created, created,
			tiers[rng.Intn(len(tiers))], validFrom, validUntil,
		); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return st
}

// thresholdScanFixture seeds a bucket with `high` fresh high-importance rows
// above `tail` old low-importance ones, none pinned, so the threshold scan should
// stop before the tail whatever the tail's size is.
func thresholdScanFixture(t *testing.T, high, tail int) *Store {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "threshold.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db, nil)
	ctx := context.Background()
	if err := st.EnsureProject(ctx, "proj", "proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	fresh := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	insert := func(prefix string, n int, imp float64, created string) {
		for i := 0; i < n; i++ {
			if _, err := tx.Exec(
				`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
				 VALUES (?, 'proj', 'gotcha', ?, 'manual', ?, 0, ?, ?)`,
				fmt.Sprintf("%s%06d", prefix, i), "threshold fixture", imp, created, created,
			); err != nil {
				t.Fatalf("insert %s %d: %v", prefix, i, err)
			}
		}
	}
	insert("hi", high, 0.9, fresh)
	insert("lo", tail, 0.2, old)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return st
}

// decayedBucket seeds one project bucket whose `n` rows are all of `cat` and
// `ageDays` old, at one importance.
//
// The age is what decides the read: a category that does not decay keeps its
// rank whatever its age, so the window's last rank stays at its importance and
// the scan ends at the window's edge, while a decaying category's rank has
// decayed with the rows and the scan has to read the bucket to its end.
func decayedBucket(t *testing.T, n int, cat string, ageDays int, importance float64) *Store {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "decayed.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db, nil)
	ctx := context.Background()
	if err := st.EnsureProject(ctx, "proj", "proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	created := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -ageDays).Format("2006-01-02 15:04:05")
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
			 VALUES (?, 'proj', ?, 'decay fixture', 'manual', ?, 0, ?, ?)`,
			fmt.Sprintf("%s%06d", cat[:1], i), cat, importance, created, created,
		); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return st
}

// TestPassiveThresholdScanReadsWhatTheStopBounds documents the READ COUNT of
// the threshold scan on both sides of the stop, because the read it bounds is
// the read and the two buckets below sit on either side of it.
//
// A bucket of rows that DO NOT DECAY holds a window whose last rank is its
// importance, so the stop fires at the window's edge and the scan reads the
// window plus the row that proves it, whatever the bucket's size. A bucket of
// AGED rows of a decaying category with TIED importance holds a window whose
// last rank has decayed far below their importance, every later row's
// importance is still above it, and the scan reads the bucket to its end.
func TestPassiveThresholdScanReadsWhatTheStopBounds(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	pol := projectPassivePolicy()
	pol.OverFetch = 45
	const rows = 1000

	reads := func(st *Store) int64 {
		t.Helper()
		cols, err := passiveColumnsFor(st)
		if err != nil {
			t.Fatalf("passiveColumnsFor: %v", err)
		}
		req := passiveRequest("proj", pol)
		req.Now = now
		resetPassiveKeyScan()
		if _, err := st.passiveWindow(context.Background(), req, pol, cols); err != nil {
			t.Fatalf("passiveWindow: %v", err)
		}
		return passiveKeyScanReads()
	}

	// A category that never decays: the window's last rank is its importance.
	if got := reads(decayedBucket(t, rows, "fact", 400, 0.8)); got != int64(pol.OverFetch)+1 {
		t.Errorf("a bucket of %d rows that do not decay cost %d reads; the stop ends the scan at the window's edge, so it costs the window plus one row",
			rows, got)
	}
	// A uniform decaying bucket with importance cycling 0.4-0.8: every row
	// shares the same decay factor, rank equals importance scaled, so the
	// tight rank bound fires at window+1.
	if got := reads(cyclingDecayedBucket(t, rows, "gotcha", 400)); got != int64(pol.OverFetch)+1 {
		t.Errorf("a uniform %d-row 400-day-old gotcha bucket with cycling importance cost %d reads; want %d", rows, got, pol.OverFetch+1)
	}
}

// cyclingDecayedBucket seeds one project bucket whose n rows are all of cat and
// ageDays old, with importance cycling 0.4 to 0.8 in steps of 0.05. Every row
// shares the same decay factor, so rank order equals importance order and the
// tight rank bound fires at window+1.
func cyclingDecayedBucket(t *testing.T, n int, cat string, ageDays int) *Store {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "cycling.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db, nil)
	ctx := context.Background()
	if err := st.EnsureProject(ctx, "proj", "proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	created := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC).AddDate(0, 0, -ageDays).Format("2006-01-02 15:04:05")
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < n; i++ {
		imp := 0.4 + float64(i%9)*0.05 // 0.4, 0.45 ... 0.8
		if _, err := tx.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
			 VALUES (?, 'proj', ?, 'cycling fixture', 'manual', ?, 0, ?, ?)`,
			fmt.Sprintf("%s%06d", cat[:1], i), cat, imp, created, created,
		); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return st
}

// TestPassiveThresholdScanCyclingOldDecayingIsBounded pins the fix for #1705:
// a uniform 400-day-old decaying bucket with importance cycling 0.4-0.8 and
// OverFetch 45 must cost about window+1 reads whatever the bucket size, and
// the window must equal the brute-force legacy ranking.
func TestPassiveThresholdScanCyclingOldDecayingIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	pol := projectPassivePolicy()
	pol.OverFetch = 45
	for _, n := range []int{1000, 50000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			st := cyclingDecayedBucket(t, n, "gotcha", 400)
			cols, err := passiveColumnsFor(st)
			if err != nil {
				t.Fatalf("passiveColumnsFor: %v", err)
			}
			req := passiveRequest("proj", pol)
			req.Now = now
			resetPassiveKeyScan()
			window, err := st.passiveWindow(context.Background(), req, pol, cols)
			if err != nil {
				t.Fatalf("passiveWindow: %v", err)
			}
			if got := passiveKeyScanReads(); got != int64(pol.OverFetch)+1 {
				t.Errorf("a uniform %d-row 400-day-old gotcha bucket with cycling importance cost %d reads; want %d", n, got, pol.OverFetch+1)
			}
			if want := legacyPassiveWindow(t, st, pol, req, cols); !eqStrings(idsOfMemories(window), want) {
				t.Errorf("cycling window differs from legacy:\n got %v\nwant %v", idsOfMemories(window), want)
			}
		})
	}
}

// agedAndFreshBucket seeds one project bucket with two groups of gotchas: `aged`
// of them are `agedDays` old at importance 0.9, and `fresh` of them are one day
// old at importance 0.5.
//
// The groups are the case the decay order exists for: the aged rows carry the
// higher importance and their rank has decayed with them, so the fresh rows
// rank above them however the scan reaches them.
func agedAndFreshBucket(t *testing.T, aged, fresh int) *Store {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "aged.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db, nil)
	ctx := context.Background()
	if err := st.EnsureProject(ctx, "proj", "proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	insert := func(prefix string, n int, imp float64, ageDays int) {
		created := now.AddDate(0, 0, -ageDays).Format("2006-01-02 15:04:05")
		for i := 0; i < n; i++ {
			if _, err := tx.Exec(
				`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
				 VALUES (?, 'proj', 'gotcha', ?, 'manual', ?, 0, ?, ?)`,
				fmt.Sprintf("%s%06d", prefix, i), "aged fixture", imp, created, created,
			); err != nil {
				t.Fatalf("insert %s %d: %v", prefix, i, err)
			}
		}
	}
	insert("aged", aged, 0.9, 400)
	insert("fresh", fresh, 0.5, 1)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return st
}

// TestPassiveThresholdScanRanksTheFreshRowsAboveTheAgedOnes is the case a
// read-bound cannot be allowed to break: the window's best rows by importance
// are the aged ones, and their rank has decayed with them, so the rows the
// block should show are the fresh ones.
//
// The scan reads in importance order, so it meets the aged rows first and
// fills the window with them. The fresh rows follow, each of them ranks above
// the aged ones, and they take the window's places — which is what the full
// ranking of the bucket returns, and what the block must show.
func TestPassiveThresholdScanRanksTheFreshRowsAboveTheAgedOnes(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	pol := projectPassivePolicy()
	pol.OverFetch = 45
	st := agedAndFreshBucket(t, 100, 100)
	cols, err := passiveColumnsFor(st)
	if err != nil {
		t.Fatalf("passiveColumnsFor: %v", err)
	}
	req := passiveRequest("proj", pol)
	req.Now = now
	window, err := st.passiveWindow(context.Background(), req, pol, cols)
	if err != nil {
		t.Fatalf("passiveWindow: %v", err)
	}
	ids := idsOfMemories(window)
	if len(ids) != pol.OverFetch {
		t.Fatalf("the window holds %d rows, want %d", len(ids), pol.OverFetch)
	}
	for _, id := range ids {
		if !strings.HasPrefix(id, "fresh") {
			t.Fatalf("the window holds the aged row %s; the fresh rows outrank it: %v", id, ids)
		}
	}
	// The full ranking of the bucket is the oracle: it reads every row, ranks
	// them, and cuts the window.
	if want := legacyPassiveWindow(t, st, pol, req, cols); !eqStrings(ids, want) {
		t.Errorf("the window differs from the full ranking:\n got %v\nwant %v", ids, want)
	}
}

// TestPassiveKeyScanPlanDoesNotSortTheBucket is the plan pin: the key scan must
// stream out of idx_memories_project_imp and must not spill to a temp b-tree,
// which is what the expression ORDER BY did over the whole bucket. It covers
// the policy that admits `_global` too: an OR over the two buckets lost the
// index order and sorted both, and every arm of the union has to stream.
func TestPassiveKeyScanPlanDoesNotSortTheBucket(t *testing.T) {
	st := thresholdScanFixture(t, 60, 60)
	cols, err := passiveColumnsFor(st)
	if err != nil {
		t.Fatalf("passiveColumnsFor: %v", err)
	}
	union := projectPassivePolicy()
	union.IncludeGlobal = true
	for _, pol := range []SlicePolicy{projectPassivePolicy(), union} {
		req := passiveRequest("proj", pol)
		plan := passiveKeyScanPlan(t, st, pol, req, cols, false)
		if strings.Contains(plan, "USE TEMP B-TREE") {
			t.Errorf("include_global=%v: the passive key scan still sorts the bucket:\n%s", pol.IncludeGlobal, plan)
		}
		arms := 1
		if pol.IncludeGlobal {
			arms = 2
		}
		if n := strings.Count(plan, "idx_memories_project_imp"); n < arms {
			t.Errorf("include_global=%v: %d arm(s) stream out of idx_memories_project_imp, want %d:\n%s",
				pol.IncludeGlobal, n, arms, plan)
		}
	}
}

// idsOfMemories is the id projection the window tests compare on.
func idsOfMemories(memories []Memory) []string {
	out := make([]string, len(memories))
	for i, m := range memories {
		out[i] = m.ID
	}
	return out
}

// TestPassiveThresholdScanRanksDatedFactsOnTheDecayCurve holds the one place the
// SQL-derived dated flag feeds the Go rank. An old dated fact and an old timeless
// fact of equal importance are the same row to every column the scan reads except
// the flag, so a scan that took the flag as always false would rank them equal and
// keep the dated one. With the flag read, the dated fact decays (0.5 × 0.3 = 0.15)
// below a younger gotcha (0.3 × ~1.0), the window of two keeps the timeless fact
// and the gotcha, and it is the window the SQL ORDER BY keeps.
func TestPassiveThresholdScanRanksDatedFactsOnTheDecayCurve(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "dated.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db, nil)
	if err := st.EnsureProject(context.Background(), "proj", "proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	old := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	fresh := time.Date(2026, 1, 30, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	for _, r := range []struct {
		id, cat, content string
		imp              float64
		created          string
	}{
		{"timeless", "fact", "the ledger uses double entry bookkeeping", 0.5, old},
		{"dated", "fact", "the ledger cutover finished on 2023-01-01", 0.5, old},
		{"younger", "gotcha", "a younger row of lower importance", 0.3, fresh},
	} {
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
			 VALUES (?, 'proj', ?, ?, 'manual', ?, 0, ?, ?)`,
			r.id, r.cat, r.content, r.imp, r.created, r.created,
		); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	cols, err := passiveColumnsFor(st)
	if err != nil {
		t.Fatalf("passiveColumnsFor: %v", err)
	}
	pol := projectPassivePolicy()
	pol.OverFetch = 2
	req := passiveRequest("proj", pol)
	got, err := st.passiveWindow(context.Background(), req, pol, cols)
	if err != nil {
		t.Fatalf("passiveWindow: %v", err)
	}
	if want := []string{"timeless", "younger"}; !eqStrings(idsOfMemories(got), want) {
		t.Fatalf("window = %v, want %v: the dated fact must rank on the decay curve below the younger row", idsOfMemories(got), want)
	}
	if want := legacyPassiveWindow(t, st, pol, req, cols); !eqStrings(idsOfMemories(got), want) {
		t.Fatalf("window %v differs from the SQL order %v", idsOfMemories(got), want)
	}
}
