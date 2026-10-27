package memory

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
)

// unionPassivePolicy is the project-context policy shape: a project bucket that
// admits `_global` into the same window.
func unionPassivePolicy(order string, overFetch int) SlicePolicy {
	return SlicePolicy{Bucket: "proj", Order: order, IncludeGlobal: true, OverFetch: overFetch}
}

// unionScanFixture seeds a store shaped like the one the union read is slow on:
// `others` unrelated projects of `perOther` rows each, a project bucket of
// `projHigh` fresh high rows above a 500-row old tail, and a `_global` bucket of
// `globalHigh` fresh rows above a `globalTail`-row old tail. It is seeded with
// raw SQL in bulk and never analysed, because Ghost never runs ANALYZE and the
// plan without statistics is the one a store runs.
func unionScanFixture(t *testing.T, others, perOther, projHigh, globalHigh, globalTail int) *Store {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "union.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db, nil)
	ctx := context.Background()
	projects := []string{"proj", GlobalProjectID}
	for i := 0; i < others; i++ {
		projects = append(projects, fmt.Sprintf("other%02d", i))
	}
	for _, p := range projects {
		if err := st.EnsureProject(ctx, p, p, p); err != nil {
			t.Fatalf("EnsureProject %s: %v", p, err)
		}
	}
	fresh := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	bulk := func(prefix, project string, n int, imp float64, created string) {
		if n == 0 {
			return
		}
		if _, err := tx.Exec(`
			INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
			WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i + 1 < ?)
			SELECT printf('%s%06d', ?, i), ?, 'gotcha', 'union fixture', 'manual', ?, 0, ?, ? FROM n`,
			n, prefix, project, imp, created, created,
		); err != nil {
			t.Fatalf("bulk insert %s: %v", prefix, err)
		}
	}
	for i := 0; i < others; i++ {
		bulk(fmt.Sprintf("o%02d-", i), fmt.Sprintf("other%02d", i), perOther, 0.9, fresh)
	}
	bulk("ph", "proj", projHigh, 0.9, fresh)
	bulk("pt", "proj", 500, 0.2, old)
	bulk("gh", GlobalProjectID, globalHigh, 0.85, fresh)
	bulk("gt", GlobalProjectID, globalTail, 0.2, old)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return st
}

// pagerReads is the connection's page reads so far, cache hits plus misses: the
// engine's own count of the b-tree pages a statement touched. It is the read
// count a sort cannot hide. The Go-side counter only sees the rows handed to
// the scan, and a plan that reads and sorts every row first still hands them
// over in importance order, so the scan stops early either way.
//
// The pool is pinned to one connection, so the conn is taken and given back
// around each read; holding it across the window would block the store's own
// queries on the same pool.
func pagerReads(t *testing.T, db *sql.DB, reset bool) int {
	t.Helper()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	var n int
	if err := conn.Raw(func(dc any) error {
		status, ok := dc.(sqlite.DBStatus)
		if !ok {
			return fmt.Errorf("driver conn %T does not expose DBStatus", dc)
		}
		for _, op := range []sqlite.DBStatusOp{sqlite.DBStatusCacheHit, sqlite.DBStatusCacheMiss} {
			cur, _, err := status.Status(op, reset)
			if err != nil {
				return err
			}
			n += cur
		}
		return nil
	}); err != nil {
		t.Fatalf("db status: %v", err)
	}
	return n
}

// windowPagerReads runs one passive window and returns its page reads and the
// rows the threshold scan was handed.
func windowPagerReads(t *testing.T, st *Store, pol SlicePolicy, now time.Time) (pages int, rows int64, ids []string) {
	t.Helper()
	cols, err := passiveColumnsFor(st)
	if err != nil {
		t.Fatalf("passiveColumnsFor: %v", err)
	}
	req := passiveRequest("proj", pol)
	req.Now = now
	pagerReads(t, st.db, true)
	resetPassiveKeyScan()
	window, err := st.passiveWindow(context.Background(), req, pol, cols)
	if err != nil {
		t.Fatalf("passiveWindow: %v", err)
	}
	return pagerReads(t, st.db, false), passiveKeyScanReads(), idsOfMemories(window)
}

// unpinnedScanPagerReads runs the unpinned threshold scan alone and returns its
// page reads, the rows it was handed and the window it kept.
func unpinnedScanPagerReads(t *testing.T, st *Store, pol SlicePolicy, now time.Time) (pages int, rows int64, kept int) {
	t.Helper()
	cols, err := passiveColumnsFor(st)
	if err != nil {
		t.Fatalf("passiveColumnsFor: %v", err)
	}
	req := passiveRequest("proj", pol)
	req.Now = now
	pagerReads(t, st.db, true)
	resetPassiveKeyScan()
	window, err := st.scanPassiveKeys(context.Background(), req, pol, cols, false, nil)
	if err != nil {
		t.Fatalf("scanPassiveKeys: %v", err)
	}
	return pagerReads(t, st.db, false), passiveKeyScanReads(), len(window)
}

// TestPassiveUnionKeyScanReadsDoNotGrowWithTheGlobalBucket pins what a passive
// read that includes globals costs. A plan that reads the union through
// `project_id = ? OR project_id = '_global'` sorts both buckets in a temp
// b-tree before the threshold scan sees a row, so the unpinned scan's page
// reads grow with a global tail the window never reaches; streaming both
// buckets in importance order lets it stop at the window.
//
// The unpinned scan is the one pinned here because it is the one with a stop.
// The pinned population has none until the window is full of pinned rows, and
// no index leads with `pinned`, so that scan walks its buckets for one bucket
// and for two alike. The whole window is held to the two buckets read on their
// own instead: a read that includes globals costs about what reading them
// separately does.
func TestPassiveUnionKeyScanReadsDoNotGrowWithTheGlobalBucket(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	const projHigh, globalHigh = 30, 30
	pol := unionPassivePolicy(OrderDecay, 45)

	pages := map[int]int{}
	for _, tail := range []int{1000, 5000} {
		st := unionScanFixture(t, 20, 500, projHigh, globalHigh, tail)
		p, rows, kept := unpinnedScanPagerReads(t, st, pol, now)
		pages[tail] = p
		if kept != pol.OverFetch {
			t.Fatalf("tail %d: the scan kept %d rows, want %d", tail, kept, pol.OverFetch)
		}
		if rows > projHigh+globalHigh+2 {
			t.Errorf("tail %d: the scan was handed %d rows for a %d-row window over %d+%d high rows; it must stop at the window",
				tail, rows, pol.OverFetch, projHigh, globalHigh)
		}
		if tail != 5000 {
			continue
		}
		// The two buckets read on their own, under the same order and window.
		project := pol
		project.IncludeGlobal = false
		global := project
		global.Bucket = GlobalProjectID
		up, _, _ := windowPagerReads(t, st, pol, now)
		pp, _, _ := windowPagerReads(t, st, project, now)
		gp, _, _ := windowPagerReads(t, st, global, now)
		t.Logf("window pages: union %d, project alone %d, global alone %d", up, pp, gp)
		if up > pp+gp+32 {
			t.Errorf("the union window read %d pages; the project bucket alone reads %d and `_global` alone %d. "+
				"A read that includes globals must cost about the two buckets, not a sort of both", up, pp, gp)
		}
	}
	t.Logf("unpinned scan pages: 1,000-row global tail %d, 5,000-row global tail %d", pages[1000], pages[5000])
	if pages[5000] > pages[1000]*3/2+32 {
		t.Errorf("the unpinned union scan read %d pages at a 5,000-row global tail and %d at 1,000; "+
			"the tail is below the window and must not be read", pages[5000], pages[1000])
	}
}

// unionRow is one row of a hand-built union store.
type unionRow struct {
	id      string
	project string
	imp     float64
	pinned  bool
	created string
}

// unionStore builds a store from explicit rows in both buckets.
func unionStore(t *testing.T, rows []unionRow) *Store {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "union-shape.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := NewStore(db, nil)
	for _, p := range []string{"proj", GlobalProjectID} {
		if err := st.EnsureProject(context.Background(), p, p, p); err != nil {
			t.Fatalf("EnsureProject %s: %v", p, err)
		}
	}
	for _, r := range rows {
		pinned := 0
		if r.pinned {
			pinned = 1
		}
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
			 VALUES (?, ?, 'gotcha', 'union shape', 'manual', ?, ?, ?, ?)`,
			r.id, r.project, r.imp, pinned, r.created, r.created,
		); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	return st
}

// TestPassiveUnionWindowMatchesTheLegacyWindow is the identity pin for a read
// that includes globals: the window is the rows, in the order, that the SQL
// ORDER BY ... LIMIT over `project_id = ? OR project_id = '_global'` returned,
// on the shapes where reading the buckets as two streams could go wrong —
// exact ties across the buckets at the cut (the id decides), spread ranks,
// pinned rows in both buckets with the window under- and over-full of them, a
// window whose last place is a project row with globals just below it, one
// whose last place is a global row, and a project bucket whose tail falls away
// steeply while globals rank just inside the cut.
func TestPassiveUnionWindowMatchesTheLegacyWindow(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	bucketOf := func(i int) string {
		if i%2 == 0 {
			return GlobalProjectID
		}
		return "proj"
	}
	rng := rand.New(rand.NewSource(1528))
	shapes := map[string][]unionRow{}

	var tied []unionRow
	for i := 0; i < 24; i++ {
		tied = append(tied, unionRow{fmt.Sprintf("tie%03d", i), bucketOf(i), 0.5, false, fresh})
	}
	shapes["tied across the buckets"] = tied

	var spread []unionRow
	for i := 0; i < 40; i++ {
		created := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC).
			Add(time.Duration(rng.Int63n(int64(5*365*24))) * time.Hour).Format("2006-01-02 15:04:05")
		spread = append(spread, unionRow{fmt.Sprintf("spr%03d", i), bucketOf(i), rng.Float64(), false, created})
	}
	shapes["spread"] = spread

	for _, pinnedN := range []int{6, 14} {
		var rows []unionRow
		for i := 0; i < 30; i++ {
			rows = append(rows, unionRow{fmt.Sprintf("pin%03d", i), bucketOf(i), rng.Float64(), i < pinnedN, fresh})
		}
		shapes[fmt.Sprintf("%d pinned across both buckets", pinnedN)] = rows
	}

	var projectEdge, globalEdge []unionRow
	for i := 0; i < 12; i++ {
		projectEdge = append(projectEdge,
			unionRow{fmt.Sprintf("pe-p%03d", i), "proj", 0.9 - float64(i)*0.01, false, fresh},
			unionRow{fmt.Sprintf("pe-g%03d", i), GlobalProjectID, 0.7 - float64(i)*0.01, false, fresh})
		globalEdge = append(globalEdge,
			unionRow{fmt.Sprintf("ge-g%03d", i), GlobalProjectID, 0.9 - float64(i)*0.01, false, fresh},
			unionRow{fmt.Sprintf("ge-p%03d", i), "proj", 0.7 - float64(i)*0.01, false, fresh})
	}
	shapes["project-only edge"] = projectEdge
	shapes["global-only edge"] = globalEdge

	var steep []unionRow
	for i := 0; i < 8; i++ {
		steep = append(steep, unionRow{fmt.Sprintf("st-p%03d", i), "proj", 0.9, false, fresh})
	}
	for i := 0; i < 20; i++ {
		steep = append(steep, unionRow{fmt.Sprintf("st-t%03d", i), "proj", 0.3 - float64(i)*0.01, false, fresh})
	}
	for i := 0; i < 4; i++ {
		steep = append(steep, unionRow{fmt.Sprintf("st-g%03d", i), GlobalProjectID, 0.6, false, fresh})
	}
	shapes["steep project tail, globals inside the cut"] = steep

	for name, rows := range shapes {
		st := unionStore(t, rows)
		cols, err := passiveColumnsFor(st)
		if err != nil {
			t.Fatalf("passiveColumnsFor: %v", err)
		}
		for _, order := range []string{OrderDecay, OrderPinnedImportanceUpdated} {
			pol := unionPassivePolicy(order, 10)
			req := passiveRequest("proj", pol)
			req.Now = now
			got, err := st.passiveWindow(context.Background(), req, pol, cols)
			if err != nil {
				t.Fatalf("%s %s: passiveWindow: %v", name, order, err)
			}
			want := legacyPassiveWindow(t, st, pol, req, cols)
			if len(want) != pol.OverFetch {
				t.Fatalf("%s %s: the legacy window holds %d rows, want %d; the shape does not fill it", name, order, len(want), pol.OverFetch)
			}
			if !eqStrings(idsOfMemories(got), want) {
				t.Errorf("%s %s: the union window differs from the legacy window:\n got %v\nwant %v",
					name, order, idsOfMemories(got), want)
			}
		}
	}
}

// TestPassiveUnionKeyScanPlanStreamsBothBuckets is the plan pin for a read
// that includes globals: every arm of the key scan streams out of
// idx_memories_project_imp and nothing spills to a temp b-tree, for the pinned
// and unpinned populations, with and without a scope filter.
func TestPassiveUnionKeyScanPlanStreamsBothBuckets(t *testing.T) {
	st := unionScanFixture(t, 2, 20, 20, 20, 20)
	cols, err := passiveColumnsFor(st)
	if err != nil {
		t.Fatalf("passiveColumnsFor: %v", err)
	}
	if !cols.HasValidity {
		t.Fatal("the fixture store has no validity columns, so the plan pinned is not the one a store runs")
	}
	for _, order := range []string{OrderDecay, OrderPinnedImportanceUpdated} {
		for _, scope := range []map[string]string{nil, {"environment": "production"}} {
			for _, pinned := range []bool{false, true} {
				pol := unionPassivePolicy(order, 15)
				req := passiveRequest("proj", pol)
				req.Scope = scope
				plan := passiveKeyScanPlan(t, st, pol, req, cols, pinned)
				label := fmt.Sprintf("%s scope=%v pinned=%v", order, scope, pinned)
				if strings.Contains(plan, "USE TEMP B-TREE") {
					t.Errorf("%s: the union key scan sorts the buckets:\n%s", label, plan)
				}
				if n := strings.Count(plan, "idx_memories_project_imp"); n < 2 {
					t.Errorf("%s: %d arm(s) stream out of idx_memories_project_imp, want both:\n%s", label, n, plan)
				}
			}
		}
	}
}

// TestPassiveGlobalBucketWithIncludeGlobalReadsOnce pins the degenerate union:
// a `_global` bucket that also admits `_global` is one bucket, and reading it
// as two arms would hand every row to the window twice.
func TestPassiveGlobalBucketWithIncludeGlobalReadsOnce(t *testing.T) {
	now := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	st := unionScanFixture(t, 0, 0, 0, 12, 0)
	pol := SlicePolicy{Bucket: GlobalProjectID, Order: OrderPinnedImportanceUpdated, IncludeGlobal: true, OverFetch: 20}
	_, rows, ids := windowPagerReads(t, st, pol, now)
	if rows != 12 || len(ids) != 12 {
		t.Errorf("the scan was handed %d rows and kept %d for a 12-row `_global` bucket, want 12 and 12", rows, len(ids))
	}
}

// passiveKeyScanPlan is EXPLAIN QUERY PLAN of the real key-scan statement.
func passiveKeyScanPlan(t *testing.T, st *Store, pol SlicePolicy, req CandidateRequest, cols passiveColumns, pinned bool) string {
	t.Helper()
	q, args := passiveKeyScanSQL(pol, req, cols, pinned)
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
	if len(details) == 0 {
		t.Fatal("the query plan is empty, so nothing is pinned")
	}
	return strings.Join(details, " | ")
}
