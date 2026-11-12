package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// globalFoldFixture is a _global scope with three near-identical rows, a pinned
// near-duplicate, and two unrelated rows. created_at is stamped directly so the
// survivor choice does not depend on the clock.
type globalFoldFixture struct {
	store                *memory.Store
	db                   *sql.DB
	oldID, midID, newID  string
	pinnedID             string
	distinctA, distinctB string
}

func newGlobalFoldFixture(t *testing.T) globalFoldFixture {
	t.Helper()
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "global.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	mk := func(content, source, agent string, imp float32, created string) string {
		id, err := store.Create(ctx, "_global", memory.Memory{
			Category: "preference", Content: content, Source: source, Importance: imp, Agent: agent,
			Tags: []string{"tag-" + agent},
		})
		if err != nil {
			t.Fatalf("Create %q: %v", content, err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE memories SET created_at = ? WHERE id = ?`, created, id); err != nil {
			t.Fatalf("stamp created_at: %v", err)
		}
		return id
	}
	f := globalFoldFixture{store: store, db: db}
	f.oldID = mk("always run go vet before committing any change", "reflection", "old", 0.9, "2026-01-01 00:00:00")
	f.midID = mk("always run go vet before committing a change", "reflection", "mid", 0.5, "2026-02-01 00:00:00")
	f.newID = mk("always run go vet before committing any change to the code", "reflection", "new", 0.6, "2026-03-01 00:00:00")
	f.pinnedID = mk("always run go vet before committing any change, pinned wording", "mcp", "pin", 0.7, "2026-04-01 00:00:00")
	if err := store.TogglePin(ctx, f.pinnedID, true); err != nil {
		t.Fatalf("TogglePin: %v", err)
	}
	f.distinctA = mk("the grafana dashboards are generated from yaml", "reflection", "da", 0.5, "2026-02-01 00:00:00")
	f.distinctB = mk("cardano preprod uses network magic one", "reflection", "db", 0.5, "2026-02-02 00:00:00")
	return f
}

func globalIDs(t *testing.T, store *memory.Store) map[string]memory.Memory {
	t.Helper()
	all, err := store.GetAll(context.Background(), "_global", -1)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	out := map[string]memory.Memory{}
	for _, m := range all {
		out[m.ID] = m
	}
	return out
}

func TestParseConsolidateGlobalArgs(t *testing.T) {
	if apply, err := parseConsolidateGlobalArgs(nil); err != nil || apply {
		t.Errorf("no args = (%v, %v), want a dry run", apply, err)
	}
	if apply, err := parseConsolidateGlobalArgs([]string{"--apply"}); err != nil || !apply {
		t.Errorf("--apply = (%v, %v)", apply, err)
	}
	if _, err := parseConsolidateGlobalArgs([]string{"--aply"}); err == nil {
		t.Error("a mistyped flag was accepted")
	}
}

// A dry run opens read-only: a store behind this build's schema is reported and
// stays behind, where a read-write open would have migrated it.
func TestConsolidateGlobalDryRunDoesNotMigrateAStaleStore(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatalf("rewind user_version: %v", err)
	}
	_ = db.Close()

	_, _, err = openConsolidateGlobalStore(dir, false)
	if err == nil || !strings.Contains(err.Error(), "schema v3") {
		t.Fatalf("dry run on a stale store = %v, want it to name the schema version and stop", err)
	}
	db, err = memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	if v, err := memory.DBUserVersion(db); err != nil || v != 3 {
		t.Fatalf("user_version = %d (err %v), the dry run migrated the store", v, err)
	}
}

// A store written before the write-boundary guard can hold a credential in a
// _global row. The listing a person reads before approving a fold withholds it.
func TestConsolidateGlobalListingWithholdsACredentialInAPreGuardRow(t *testing.T) {
	s, db, _ := preGuardStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for _, content := range []string{preGuardContent, preGuardContent + " today"} {
		id, err := s.CreateFromCorpus(ctx, "_global", memory.Memory{Category: "gotcha", Content: content, Source: "reflection", Importance: 0.7})
		if err != nil {
			t.Fatalf("CreateFromCorpus: %v", err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE memories SET created_at = '2026-01-01 00:00:00' WHERE id = ?`, id); err != nil {
			t.Fatalf("stamp: %v", err)
		}
	}
	var out bytes.Buffer
	if err := consolidateGlobal(ctx, s, false, &out); err != nil {
		t.Fatalf("consolidateGlobal: %v", err)
	}
	if !strings.Contains(out.String(), "cluster 1") {
		t.Fatalf("the fixture did not cluster, so nothing was printed to withhold:\n%s", out.String())
	}
	assertWithheld(t, "ghost maintenance consolidate-global", out.String())
}

// dumpStore returns every row of every ordinary table, as text, keyed by table.
// Search-index tables and snapshot tables are left out: the first follow the
// memories table, the second are the one place a fold is allowed to add rows.
func dumpStore(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	trs, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
		AND name NOT LIKE '%fts%' AND name NOT LIKE 'memory_snapshot%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var names []string
	for trs.Next() {
		var n string
		if err := trs.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}
	_ = trs.Close()
	out := map[string][]string{}
	for _, n := range names {
		rows, err := db.Query(`SELECT * FROM "` + n + `"`)
		if err != nil {
			t.Fatalf("dump %s: %v", n, err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan %s: %v", n, err)
			}
			var sb strings.Builder
			for i, v := range vals {
				if b, ok := v.([]byte); ok {
					v = fmt.Sprintf("%x", b)
				}
				fmt.Fprintf(&sb, "%s=%v|", cols[i], v)
			}
			out[n] = append(out[n], sb.String())
		}
		_ = rows.Close()
	}
	return out
}

// diffDump returns, per table, the rows only in before (removed) and only in
// after (added).
func diffDump(before, after map[string][]string) (removed, added map[string][]string) {
	removed, added = map[string][]string{}, map[string][]string{}
	for table := range before {
		inAfter := map[string]int{}
		for _, r := range after[table] {
			inAfter[r]++
		}
		for _, r := range before[table] {
			if inAfter[r] > 0 {
				inAfter[r]--
			} else {
				removed[table] = append(removed[table], r)
			}
		}
	}
	for table := range after {
		inBefore := map[string]int{}
		for _, r := range before[table] {
			inBefore[r]++
		}
		for _, r := range after[table] {
			if inBefore[r] > 0 {
				inBefore[r]--
			} else {
				added[table] = append(added[table], r)
			}
		}
	}
	return removed, added
}

func rowMentions(row string, ids ...string) bool {
	for _, id := range ids {
		if strings.Contains(row, id) {
			return true
		}
	}
	return false
}

// A byte-identical agent row beside a reflection cluster survivor must come out
// of --apply unchanged in every table; a whole-set replace matched rows by text
// and let the two trade columns. The full-store diff also proves the fold writes
// nothing outside the cluster.
func TestConsolidateGlobalApplyTouchesOnlyTheCluster(t *testing.T) {
	f := newGlobalFoldFixture(t)
	ctx := context.Background()
	survivorText := "always run go vet before committing any change to the code"
	twin, err := f.store.Create(ctx, "_global", memory.Memory{
		Category: "preference", Content: survivorText, Source: "mcp", Importance: 0.31, Agent: "agent", Tags: []string{"mine"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE memories SET created_at = '2026-01-15 00:00:00' WHERE id = ?`, twin); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	if err := f.store.StoreEmbedding(ctx, twin, []float32{0.1, 0.2, 0.3}, "test"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	if err := f.store.CreateLink(ctx, twin, f.distinctA, "related", 0.5, "manual"); err != nil {
		t.Fatalf("CreateLink: %v", err)
	}
	before := dumpStore(t, f.db)

	if err := consolidateGlobal(ctx, f.store, true, &bytes.Buffer{}); err != nil {
		t.Fatalf("consolidateGlobal: %v", err)
	}
	after := dumpStore(t, f.db)
	removed, added := diffDump(before, after)
	touched := []string{f.newID, f.oldID, f.midID}
	for _, side := range []map[string][]string{removed, added} {
		for table, rows := range side {
			// The vector corpus cache's counter is one row of derived state that
			// every write to a memory or its embedding moves, the cluster's own
			// included. It names no memory, so it cannot say which rows the fold
			// touched; the rows that could are compared above and below.
			if table == "vector_epoch" {
				continue
			}
			for _, r := range rows {
				if !rowMentions(r, touched...) {
					t.Errorf("%s changed a row outside the cluster: %s", table, r)
				}
				if rowMentions(r, twin) {
					t.Errorf("%s changed the agent twin: %s", table, r)
				}
			}
		}
	}
	// And the fold did happen.
	if _, ok := globalIDs(t, f.store)[f.oldID]; ok {
		t.Error("the cluster was not folded")
	}
	if len(added["memory_history"]) == 0 {
		t.Error("the fold recorded no history")
	}
}

// When every row is refused the report says nothing was written, not that
// survivors absorbed anything.
func TestPrintFoldOutcomeStatesWhatHappened(t *testing.T) {
	var none bytes.Buffer
	printFoldOutcome(&none, memory.FoldResult{Skipped: []memory.FoldSkip{{ID: "A", Reason: "pinned"}, {ID: "B", Reason: "pinned"}}})
	if strings.Contains(none.String(), "survivor(s)") || !strings.Contains(none.String(), "nothing written; 2 row(s) skipped") {
		t.Errorf("a run that folded nothing reported:\n%s", none.String())
	}
	var some bytes.Buffer
	printFoldOutcome(&some, memory.FoldResult{Folded: []string{"X", "Y", "Z"}, Clusters: 2})
	if !strings.Contains(some.String(), "folded 3 row(s) into 2 survivor(s)") {
		t.Errorf("a run that folded rows reported:\n%s", some.String())
	}
}
