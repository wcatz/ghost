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
		for _, pol := range []SlicePolicy{projectPassivePolicy(), globalPassivePolicy()} {
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
		if _, err := db.Exec(
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
	for _, tail := range []int{1000, 20000} {
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
	if reads[1000] != reads[20000] {
		t.Errorf("the scan read %d rows at a 1,000-row tail and %d at 20,000; the tail is below the window and must not be read",
			reads[1000], reads[20000])
	}
	if reads[20000] > int64(pol.OverFetch)+2 {
		t.Errorf("the scan read %d rows for a %d-row window; it must stop at the window, not walk the bucket",
			reads[20000], pol.OverFetch)
	}
}

// TestPassiveKeyScanPlanDoesNotSortTheBucket is the plan pin: the key scan must
// stream out of idx_memories_project_imp and must not spill to a temp b-tree,
// which is what the expression ORDER BY did over the whole bucket.
func TestPassiveKeyScanPlanDoesNotSortTheBucket(t *testing.T) {
	st := thresholdScanFixture(t, 60, 60)
	cols, err := passiveColumnsFor(st)
	if err != nil {
		t.Fatalf("passiveColumnsFor: %v", err)
	}
	pol := projectPassivePolicy()
	req := passiveRequest("proj", pol)
	q, args := passiveKeyScanSQL(pol, req, cols, false)
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
		t.Errorf("the passive key scan still sorts the bucket:\n%s", plan)
	}
	if !strings.Contains(plan, "idx_memories_project_imp") {
		t.Errorf("the passive key scan does not stream out of idx_memories_project_imp:\n%s", plan)
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
