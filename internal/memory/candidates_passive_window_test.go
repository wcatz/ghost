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
	for trial := 0; trial < 60; trial++ {
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
	n := 20 + rng.Intn(90)
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
// AGED rows of a decaying category holds a window whose last rank has decayed
// far below their importance, every later row is above it, and the scan reads
// the bucket to its end.
//
// The second figure is the real behaviour, not a bound to hold the code to: the
// scan's stop is exact, so the window it returns is the same either way, and
// the price of a window whose rows have aged is a read of the whole bucket.
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
	// A decaying category whose rows have aged: the window's last rank has
	// decayed with them, so the stop cannot hold and the bucket is read whole.
	if got := reads(decayedBucket(t, rows, "gotcha", 400, 0.8)); got != rows {
		t.Errorf("a bucket of %d aged rows of a decaying category cost %d reads; the scan reads such a bucket to its end",
			rows, got)
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

// TestPassiveThresholdScanReadsDoNotGrowWithTheTail pins the READ COUNT, not a
// duration: a bucket whose top rows are high-importance and whose tail is far
// below the window must cost the window's size, so growing the tail from one
// thousand rows to twenty thousand leaves the number of rows the scan read
// unchanged. It is the property the threshold scan exists for, and it is a number
// the plan cannot express.
func TestPassiveThresholdScanReadsDoNotGrowWithTheTail(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	pol := projectPassivePolicy()
	pol.OverFetch = 45

	reads := make(map[int]int64)
	for _, tail := range []int{1000, 5000} {
		st := thresholdScanFixture(t, 45, tail)
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
		reads[tail] = passiveKeyScanReads()
		if len(window) != pol.OverFetch {
			t.Fatalf("tail %d: the window holds %d rows, want %d", tail, len(window), pol.OverFetch)
		}
	}
	if reads[1000] != reads[5000] {
		t.Errorf("the scan read %d rows at a 1,000-row tail and %d at 5,000; the tail is below the window and must not be read",
			reads[1000], reads[5000])
	}
	if reads[5000] > int64(pol.OverFetch)+2 {
		t.Errorf("the scan read %d rows for a %d-row window; it must stop at the window, not walk the bucket",
			reads[5000], pol.OverFetch)
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

// TestPassiveThresholdScanKeepsTheNewestTiedRows pins the tie-break at the
// window's edge, for both orders. Every row of a bucket is a fact at the save
// default for that bucket (0.7 for a project, 0.8 for `_global`), so every rank
// equals every importance and the window is decided by the secondary key alone:
// created_at for `decay`, updated_at for `pinned_importance_updated`. The five
// newest rows are inserted LAST, so the index hands them to the scan last: the
// scan reads a tie in the index's secondary-key order, newest first, so the
// newest rows arrive before the non-strict stop can fire and all five must be
// in the window, in the legacy order.
func TestPassiveThresholdScanKeepsTheNewestTiedRows(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	old := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	// The fixture only tests the tie if the rank IS the importance: a fact on
	// the project tier must not decay.
	if f := DecayFactor("fact", RetentionProject, false, ageDays(old.Format("2006-01-02 15:04:05"), now)); f != 1 {
		t.Fatalf("a project-tier fact decays by %v, so the fixture's rows are not tied on rank", f)
	}
	for _, tc := range []struct {
		pol        SlicePolicy
		importance float64
	}{
		{projectPassivePolicy(), 0.7},
		{globalPassivePolicy(), 0.8},
	} {
		t.Run(tc.pol.Order, func(t *testing.T) {
			db, err := OpenDB(filepath.Join(t.TempDir(), "tied.db"))
			if err != nil {
				t.Fatalf("OpenDB: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			st := NewStore(db, nil)
			ctx := context.Background()
			if err := st.EnsureProject(ctx, tc.pol.Bucket, tc.pol.Bucket, tc.pol.Bucket); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			insert := func(id string, at time.Time) {
				stamp := at.Format("2006-01-02 15:04:05")
				if _, err := db.Exec(
					`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
					 VALUES (?, ?, 'fact', 'tied fixture', 'manual', ?, 0, ?, ?)`,
					id, tc.pol.Bucket, tc.importance, stamp, stamp,
				); err != nil {
					t.Fatalf("insert %s: %v", id, err)
				}
			}
			for i := 0; i < 55; i++ {
				insert(fmt.Sprintf("old%02d", i), old.Add(time.Duration(i)*time.Hour))
			}
			var newest []string
			for i := 0; i < 5; i++ {
				id := fmt.Sprintf("new%02d", i)
				insert(id, fresh.Add(time.Duration(i)*time.Hour))
				newest = append(newest, id)
			}
			cols, err := passiveColumnsFor(st)
			if err != nil {
				t.Fatalf("passiveColumnsFor: %v", err)
			}
			req := passiveRequest(tc.pol.Bucket, tc.pol)
			req.Now = now
			got, err := st.passiveWindow(ctx, req, tc.pol, cols)
			if err != nil {
				t.Fatalf("passiveWindow: %v", err)
			}
			ids := idsOfMemories(got)
			in := make(map[string]bool, len(ids))
			for _, id := range ids {
				in[id] = true
			}
			for _, id := range newest {
				if !in[id] {
					t.Errorf("the window lost the newest tied row %s: %v", id, ids)
				}
			}
			if want := legacyPassiveWindow(t, st, tc.pol, req, cols); !eqStrings(ids, want) {
				t.Errorf("the tied window differs from the legacy window:\n got %v\nwant %v", ids, want)
			}
		})
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

// TestPassiveThresholdScanReadsStayBoundedOnTiedImportance pins the read count
// (passiveKeyScanReads) on the save-defaults case, where importance is tied at
// the edge, for both orders. Every row is a fact at the bucket's save default,
// so all importances are equal and the scan must stop at the window rather than
// walk the bucket.
func TestPassiveThresholdScanReadsStayBoundedOnTiedImportance(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, pol := range []SlicePolicy{projectPassivePolicy(), globalPassivePolicy()} {
		t.Run(pol.Order, func(t *testing.T) {
			db, err := OpenDB(filepath.Join(t.TempDir(), pol.Order+"-tied-reads.db"))
			if err != nil {
				t.Fatalf("OpenDB: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			st := NewStore(db, nil)
			ctx := context.Background()
			bucket := pol.Bucket
			if err := st.EnsureProject(ctx, bucket, bucket, bucket); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			imp := 0.7
			if pol.Order == OrderPinnedImportanceUpdated {
				imp = 0.8
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			for i := 0; i < 1000; i++ {
				created := base.Add(time.Duration(i) * time.Hour).Format("2006-01-02 15:04:05")
				if _, err := tx.Exec(
					`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
					 VALUES (?, ?, 'fact', ?, 'manual', ?, 0, ?, ?)`,
					fmt.Sprintf("t%06d", i), bucket, "tied importance fixture", imp, created, created,
				); err != nil {
					t.Fatalf("insert %d: %v", i, err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			cols, err := passiveColumnsFor(st)
			if err != nil {
				t.Fatalf("passiveColumnsFor: %v", err)
			}
			req := passiveRequest("proj", pol)
			req.Now = now
			resetPassiveKeyScan()
			got, err := st.passiveWindow(ctx, req, pol, cols)
			if err != nil {
				t.Fatalf("passiveWindow: %v", err)
			}
			if len(got) != pol.OverFetch {
				t.Fatalf("the window holds %d rows, want %d", len(got), pol.OverFetch)
			}
			if reads := passiveKeyScanReads(); reads > int64(pol.OverFetch)+2 {
				t.Errorf("the scan read %d rows for a %d-row window on tied importance; it must not walk the bucket",
					reads, pol.OverFetch)
			}
			want := legacyPassiveWindow(t, st, pol, req, cols)
			if !eqStrings(idsOfMemories(got), want) {
				t.Fatalf("the threshold scan differs from the legacy window:\n got %v\nwant %v",
					idsOfMemories(got), want)
			}
		})
	}
}

// TestPassiveThresholdScanBreaksSameTimestampTiesOnID pins the id tie-break when
// importance and the secondary key are both equal. Rows are inserted in
// descending id order so that, without id in the scan's ORDER BY, the smaller-id
// rows arrive after the non-strict stop and never enter the window.
func TestPassiveThresholdScanBreaksSameTimestampTiesOnID(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	stamp := time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	for _, pol := range []SlicePolicy{projectPassivePolicy(), globalPassivePolicy()} {
		t.Run(pol.Order, func(t *testing.T) {
			db, err := OpenDB(filepath.Join(t.TempDir(), pol.Order+"-same-ts.db"))
			if err != nil {
				t.Fatalf("OpenDB: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			st := NewStore(db, nil)
			ctx := context.Background()
			bucket := pol.Bucket
			if err := st.EnsureProject(ctx, bucket, bucket, bucket); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			imp := 0.7
			if pol.Order == OrderPinnedImportanceUpdated {
				imp = 0.8
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			// Insert in descending id order.
			for i := 300; i >= 1; i-- {
				id := fmt.Sprintf("id%04d", i)
				if _, err := tx.Exec(
					`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
					 VALUES (?, ?, 'fact', ?, 'manual', ?, 0, ?, ?)`,
					id, bucket, "same timestamp fixture", imp, stamp, stamp,
				); err != nil {
					t.Fatalf("insert %s: %v", id, err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			cols, err := passiveColumnsFor(st)
			if err != nil {
				t.Fatalf("passiveColumnsFor: %v", err)
			}
			req := passiveRequest("proj", pol)
			req.Now = now
			got, err := st.passiveWindow(ctx, req, pol, cols)
			if err != nil {
				t.Fatalf("passiveWindow: %v", err)
			}
			want := legacyPassiveWindow(t, st, pol, req, cols)
			if !eqStrings(idsOfMemories(got), want) {
				t.Fatalf("the threshold scan differs from the legacy window:\n got %v\nwant %v",
					idsOfMemories(got), want)
			}
		})
	}
}

// TestPassiveKeyScanPlanUsesBothIndexShapes is the plan pin for both scan
// shapes: the decay bucket streams out of idx_memories_project_imp and the
// pinned/importance bucket out of idx_memories_project_imp_updated, with no
// temp b-tree sort in either case.
func TestPassiveKeyScanPlanUsesBothIndexShapes(t *testing.T) {
	st := thresholdScanFixture(t, 60, 60)
	// seed a _global bucket for the updated index shape.
	ctx := context.Background()
	if err := st.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}
	cols, err := passiveColumnsFor(st)
	if err != nil {
		t.Fatalf("passiveColumnsFor: %v", err)
	}
	cases := []struct {
		pol   SlicePolicy
		index string
	}{
		{projectPassivePolicy(), "idx_memories_project_imp"},
		{globalPassivePolicy(), "idx_memories_project_imp_updated"},
	}
	for _, c := range cases {
		t.Run(c.pol.Order, func(t *testing.T) {
			req := passiveRequest("proj", c.pol)
			q, args := passiveKeyScanSQL(c.pol, req, cols, false)
			rows, err := st.db.Query("EXPLAIN QUERY PLAN "+q, args...)
			if err != nil {
				t.Fatalf("explain query plan: %v", err)
			}
			defer rows.Close() //nolint:errcheck
			var details []string
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					t.Fatalf("scan plan row: %v", err)
				}
				details = append(details, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("iterate plan: %v", err)
			}
			plan := strings.Join(details, " | ")
			if len(details) == 0 {
				t.Fatal("the query plan is empty, so nothing is pinned")
			}
			if strings.Contains(plan, "USE TEMP B-TREE") {
				t.Errorf("the passive key scan sorts the bucket:\n%s", plan)
			}
			if !strings.Contains(plan, c.index) {
				t.Errorf("the passive key scan does not stream out of %s:\n%s", c.index, plan)
			}
		})
	}
}
