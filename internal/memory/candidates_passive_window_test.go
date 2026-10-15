package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// passiveWindowFixture seeds one project with n rows spread across importances,
// categories, ages and pin states, so the window's tie-breaks are all reachable,
// plus a `_global` row and a resolved row the population predicate must exclude.
//
// Importance is a multiple of 1/32 and created_at a whole second, so both are
// exactly representable as float32 and as SQLite REAL: the SQL reference and the
// Go window then rank on the same numbers rather than on a rounding of one.
func passiveWindowFixture(t *testing.T, n int) *Store {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "passive-window.db"))
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
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		created := base.Add(time.Duration(i) * 13 * time.Hour).Format("2006-01-02 15:04:05")
		// A 0.5 floor and a 1/32 grid: importance never reaches 0, so a row can
		// always be beaten by a fresher high-importance twin, which is the shape
		// the score reorder (not the importance order) is about.
		imp := 0.5 + float64((i*7)%16)/32.0
		pinned := 0
		if i%23 == 0 {
			pinned = 1
		}
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
			 VALUES (?, 'proj', ?, ?, 'manual', ?, ?, ?, ?)`,
			fmt.Sprintf("wmem%04d", i), cats[i%len(cats)], "window fixture "+fmt.Sprint(i), imp, pinned, created, created,
		); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	for i := 0; i < 40; i++ {
		created := base.Add(time.Duration(i) * 11 * time.Hour).Format("2006-01-02 15:04:05")
		imp := 0.25 + float64((i*5)%24)/32.0
		pinned := 0
		if i%7 == 0 {
			pinned = 1
		}
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
			 VALUES (?, '_global', 'preference', ?, 'manual', ?, ?, ?, ?)`,
			fmt.Sprintf("gwmem%04d", i), "global fixture "+fmt.Sprint(i), imp, pinned, created, created,
		); err != nil {
			t.Fatalf("insert global %d: %v", i, err)
		}
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, importance, resolved_at, created_at, updated_at)
		 VALUES ('wres', 'proj', 'gotcha', 'a resolved row', 'manual', 1.0, ?, ?, ?)`,
		base.Format("2006-01-02 15:04:05"), base.Format("2006-01-02 15:04:05"), base.Format("2006-01-02 15:04:05"),
	); err != nil {
		t.Fatalf("insert resolved: %v", err)
	}
	return st
}

// TestPassiveWindowMatchesTheFullSort is the equivalence pin: the window the
// streaming fetch keeps is the same rows the SQL ORDER BY ... LIMIT would have
// returned, because the rank the engine used to sort the whole bucket is the
// rank passiveWindowLess applies in Go. It compares against a query built from
// the SAME fragments the fetch used to splice (decayRankingSQLAt and
// passivePopulationSQL), so it cannot pass against a comparator that merely
// re-implements the order by hand.
func TestPassiveWindowMatchesTheFullSort(t *testing.T) {
	st := passiveWindowFixture(t, 400)
	ctx := context.Background()
	now := time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)
	cols, err := passiveColumnsFor(st)
	if err != nil {
		t.Fatalf("passiveColumnsFor: %v", err)
	}

	for _, pol := range []SlicePolicy{projectPassivePolicy(), globalPassivePolicy()} {
		t.Run(pol.Order, func(t *testing.T) {
			req := passiveRequest("proj", pol)
			req.Now = now
			q, args := passiveFetchSQL(pol, req, cols)
			rows, err := st.db.QueryContext(ctx, q, args...)
			if err != nil {
				t.Fatalf("stream: %v", err)
			}
			got, err := scanPassiveWindow(rows, pol, now)
			if err != nil {
				rows.Close() //nolint:errcheck
				t.Fatalf("scanPassiveWindow: %v", err)
			}
			rows.Close() //nolint:errcheck

			want := fullSortWindow(t, st, pol, now, cols)
			if len(got) != len(want) {
				t.Fatalf("the streaming window holds %d rows; the full sort holds %d", len(got), len(want))
			}
			if !eqStrings(idsOfMemories(got), want) {
				t.Errorf("the streaming window differs from the full sort it replaces:\n got %v\nwant %v", idsOfMemories(got), want)
			}
		})
	}
}

// fullSortWindow is the query the fetch used to run: the policy's order applied
// by the engine and cut by the over-fetch. It is the specification the Go window
// has to reproduce, and it is deliberately built from the same fragments rather
// than restated, so it cannot drift from the order it deletes.
func fullSortWindow(t *testing.T, st *Store, pol SlicePolicy, now time.Time, cols passiveColumns) []string {
	t.Helper()
	where, args := passivePopulationSQL(pol, CandidateRequest{Now: now}, cols)
	if cols.HasValidity {
		stamp := now.UTC().Format(stampLayoutForSQL)
		where += " AND " + validityMatchesSQL()
		args = append(args, stamp, stamp)
	}
	orderBy := "pinned DESC, importance DESC, updated_at DESC, id"
	if pol.Order == "" || pol.Order == OrderDecay {
		rank, clock := decayRankingSQLAt(now, cols.HasTier)
		orderBy = fmt.Sprintf("pinned DESC, (%s) DESC, importance DESC, created_at DESC, id", rank)
		args = append(args, clock...)
	}
	q := fmt.Sprintf("SELECT %s FROM memories WHERE %s ORDER BY %s LIMIT ?", cols.list, where, orderBy)
	args = append(args, pol.OverFetch)
	rows, err := st.db.QueryContext(context.Background(), q, args...)
	if err != nil {
		t.Fatalf("full sort: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	memories, err := scanMemories(rows)
	if err != nil {
		t.Fatalf("scan full sort: %v", err)
	}
	return idsOfMemories(memories)
}

// TestPassiveWindowKeepsTheFullSortsTop feeds a window in a deliberately hostile
// order (reverse) so an insertion bug cannot hide behind a lucky input, and
// compares it to a plain sort of the same rows cut at the cap.
func TestPassiveWindowKeepsTheFullSortsTop(t *testing.T) {
	const rows, cap = 120, 45
	all := make([]passiveRow, 0, rows)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < rows; i++ {
		m := Memory{
			ID:         fmt.Sprintf("r%03d", i),
			Category:   []string{"gotcha", "fact", "architecture", "decision"}[i%4],
			Importance: float32(0.5 + float64(i%16)/32.0),
			Pinned:     i%23 == 0,
			CreatedAt:  base.Add(time.Duration(i) * 13 * time.Hour).Format("2006-01-02 15:04:05"),
			UpdatedAt:  base.Add(time.Duration(i) * 13 * time.Hour).Format("2006-01-02 15:04:05"),
		}
		age := float64(i)
		all = append(all, passiveRow{
			mem: m, age: age,
			decay: DecayFactor(m.Category, m.Retention, m.Pinned, age),
			score: float64(m.Importance) * DecayFactor(m.Category, m.Retention, m.Pinned, age),
		})
	}

	var got []passiveRow
	for i := len(all) - 1; i >= 0; i-- {
		got = insertPassiveWindow(got, all[i], OrderDecay, cap)
	}

	want := append([]passiveRow(nil), all...)
	sort.SliceStable(want, func(i, j int) bool { return passiveWindowLess(want[i], want[j], OrderDecay) })
	want = want[:cap]

	var gotIDs, wantIDs []string
	for _, r := range got {
		gotIDs = append(gotIDs, r.mem.ID)
	}
	for _, r := range want {
		wantIDs = append(wantIDs, r.mem.ID)
	}
	if !eqStrings(gotIDs, wantIDs) {
		t.Errorf("the bounded window is not the full sort's top %d:\n got %v\nwant %v", cap, gotIDs, wantIDs)
	}
}

// TestPassiveFetchPlanDoesNotSortTheBucket is the plan pin the issue asks for:
// the read must use the index and must not spill to a temp b-tree, which is what
// the expression ORDER BY did over the whole bucket. It is a plan assertion and
// not a duration, so it holds on any machine.
func TestPassiveFetchPlanDoesNotSortTheBucket(t *testing.T) {
	st := passiveWindowFixture(t, 60)
	cols, err := passiveColumnsFor(st)
	if err != nil {
		t.Fatalf("passiveColumnsFor: %v", err)
	}
	pol := projectPassivePolicy()
	q, args := passiveFetchSQL(pol, passiveRequest("proj"), cols)
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
		t.Errorf("the passive fetch still sorts the bucket:\n%s", plan)
	}
	if !strings.Contains(plan, "idx_memories_project_imp") {
		t.Errorf("the passive fetch does not stream out of idx_memories_project_imp:\n%s", plan)
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
