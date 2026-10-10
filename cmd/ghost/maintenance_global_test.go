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

func TestConsolidateGlobalDryRunListsClustersAndWritesNothing(t *testing.T) {
	f := newGlobalFoldFixture(t)
	before := globalIDs(t, f.store)
	var out bytes.Buffer
	if err := consolidateGlobal(context.Background(), f.store, false, &out); err != nil {
		t.Fatalf("consolidateGlobal: %v", err)
	}
	got := out.String()
	for _, want := range []string{"DRY RUN", "1 near-duplicate cluster(s) covering 3 rows", "keep " + f.newID, "fold " + f.oldID, "fold " + f.midID, "nothing written"} {
		if !strings.Contains(got, want) {
			t.Errorf("dry-run output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, f.pinnedID) || strings.Contains(got, f.distinctA) {
		t.Errorf("dry run names a pinned or unrelated row:\n%s", got)
	}
	after := globalIDs(t, f.store)
	if len(after) != len(before) {
		t.Fatalf("dry run changed the row count: %d -> %d", len(before), len(after))
	}
	for id, m := range before {
		if after[id].Content != m.Content || after[id].Importance != m.Importance || after[id].UpdatedAt != m.UpdatedAt {
			t.Errorf("dry run changed row %s: %+v -> %+v", id, m, after[id])
		}
	}
	var snaps int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM memory_snapshots WHERE project_id = '_global'`).Scan(&snaps); err != nil || snaps != 0 {
		t.Errorf("dry run wrote %d snapshot rows (err %v), want 0", snaps, err)
	}
}

func TestConsolidateGlobalApplyFoldsClusterWithHistoryAndEvidence(t *testing.T) {
	f := newGlobalFoldFixture(t)
	ctx := context.Background()
	beforeAll := globalIDs(t, f.store)
	pinnedBefore := beforeAll[f.pinnedID]
	histBefore := map[string]int{}
	for _, id := range []string{f.distinctA, f.distinctB} {
		h, err := f.store.MemoryHistory(ctx, id, 50)
		if err != nil {
			t.Fatalf("MemoryHistory(%s): %v", id, err)
		}
		histBefore[id] = len(h)
	}
	var out bytes.Buffer
	if err := consolidateGlobal(ctx, f.store, true, &out); err != nil {
		t.Fatalf("consolidateGlobal: %v", err)
	}
	after := globalIDs(t, f.store)
	if len(after) != 4 {
		t.Fatalf("_global holds %d rows after the fold, want survivor + pinned + 2 distinct: %+v", len(after), after)
	}
	survivor, ok := after[f.newID]
	if !ok {
		t.Fatalf("the newest row %s did not survive: %+v", f.newID, after)
	}
	if survivor.Content != "always run go vet before committing any change to the code" {
		t.Errorf("survivor text was rewritten: %q", survivor.Content)
	}
	if survivor.Importance != 0.9 {
		t.Errorf("survivor importance = %v, want the cluster maximum 0.9", survivor.Importance)
	}
	wantTags := map[string]bool{"tag-old": true, "tag-mid": true, "tag-new": true}
	if len(survivor.Tags) != 3 {
		t.Errorf("survivor tags = %v, want the union of the cluster", survivor.Tags)
	}
	for _, tg := range survivor.Tags {
		delete(wantTags, tg)
	}
	if len(wantTags) != 0 {
		t.Errorf("survivor tags %v miss %v", survivor.Tags, wantTags)
	}
	for _, gone := range []string{f.oldID, f.midID} {
		if _, still := after[gone]; still {
			t.Errorf("folded row %s is still live", gone)
		}
		hist, err := f.store.MemoryHistory(ctx, gone, 50)
		if err != nil {
			t.Fatalf("MemoryHistory(%s): %v", gone, err)
		}
		pointed := false
		for _, h := range hist {
			if h.Phase == "delete" && h.RelatedID == f.newID {
				pointed = true
			}
		}
		if !pointed {
			t.Errorf("history of %s has no delete naming the survivor: %+v", gone, hist)
		}
	}
	ev, err := f.store.MemoryProvenance(ctx, f.newID)
	if err != nil {
		t.Fatalf("MemoryProvenance: %v", err)
	}
	carried := map[string]bool{}
	for _, e := range ev {
		if e.CarriedFrom != "" {
			carried[e.Agent] = true
		}
	}
	if !carried["old"] || !carried["mid"] {
		t.Errorf("survivor evidence does not carry the folded rows' agents: %+v", ev)
	}
	// The pinned row is untouched, text and weight alike, even though it is a
	// near-duplicate of the cluster.
	pinnedAfter := after[f.pinnedID]
	if !pinnedAfter.Pinned || pinnedAfter.Content != pinnedBefore.Content || pinnedAfter.Importance != pinnedBefore.Importance {
		t.Errorf("pinned row changed: %+v -> %+v", pinnedBefore, pinnedAfter)
	}
	for _, id := range []string{f.distinctA, f.distinctB} {
		got, ok := after[id]
		if !ok {
			t.Errorf("unrelated row %s was lost", id)
			continue
		}
		was := beforeAll[id]
		if got.Content != was.Content || got.Category != was.Category || got.Importance != was.Importance ||
			strings.Join(got.Tags, ",") != strings.Join(was.Tags, ",") || got.CreatedAt != was.CreatedAt {
			t.Errorf("unrelated row %s was rewritten: %+v -> %+v", id, was, got)
		}
		hist, err := f.store.MemoryHistory(ctx, id, 50)
		if err != nil {
			t.Fatalf("MemoryHistory(%s): %v", id, err)
		}
		if histBefore[id] != len(hist) {
			t.Errorf("unrelated row %s gained history: %d -> %d entries", id, histBefore[id], len(hist))
		}
	}
	var snaps int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM memory_snapshots WHERE project_id = '_global'`).Scan(&snaps); err != nil || snaps == 0 {
		t.Errorf("the fold left no snapshot (count %d, err %v)", snaps, err)
	}
	// A second run finds nothing: the fold is idempotent.
	var again bytes.Buffer
	if err := consolidateGlobal(ctx, f.store, true, &again); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if !strings.Contains(again.String(), "nothing to fold") {
		t.Errorf("second run did not report nothing to fold:\n%s", again.String())
	}
}

// A row saved at or after the run's start is not in the plan: ReplaceNonManual
// keeps it in place and would not let an emission claim it, so planning it would
// leave two copies of its text.
func TestConsolidateGlobalLeavesRowsSavedDuringTheRunAlone(t *testing.T) {
	f := newGlobalFoldFixture(t)
	ctx := context.Background()
	var fresh []string
	for _, c := range []string{"cardano mainnet uses network magic 764824073", "cardano mainnet uses the network magic 764824073"} {
		id, err := f.store.Create(ctx, "_global", memory.Memory{Category: "fact", Content: c, Source: "reflection"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Stamped in the future so they are at or after the run's start however
		// the clock ticks between here and the run.
		if _, err := f.db.ExecContext(ctx, `UPDATE memories SET created_at = '2099-01-01 00:00:00' WHERE id = ?`, id); err != nil {
			t.Fatalf("stamp created_at: %v", err)
		}
		fresh = append(fresh, id)
	}
	var out bytes.Buffer
	if err := consolidateGlobal(ctx, f.store, true, &out); err != nil {
		t.Fatalf("consolidateGlobal: %v", err)
	}
	after := globalIDs(t, f.store)
	for _, id := range fresh {
		if _, ok := after[id]; !ok {
			t.Errorf("row %s saved during the run was removed", id)
		}
	}
	if len(after) != 6 {
		t.Fatalf("_global holds %d rows, want the 4 left by the fold plus the 2 fresh rows: %+v", len(after), after)
	}
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

// Only reflection-written rows are folded: an agent's own near-twin stays, and
// so does a row of opposite polarity.
func TestConsolidateGlobalOnlyFoldsReflectionRowsOfTheSamePolarity(t *testing.T) {
	f := newGlobalFoldFixture(t)
	ctx := context.Background()
	mk := func(content, source string) string {
		id, err := f.store.Create(ctx, "_global", memory.Memory{Category: "preference", Content: content, Source: source})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := f.db.ExecContext(ctx, `UPDATE memories SET created_at = '2026-05-01 00:00:00' WHERE id = ?`, id); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		return id
	}
	always := mk("always run the full test suite before committing", "reflection")
	never := mk("never run the full test suite before committing", "reflection")
	agentA := mk("prefer small commits with one concern each", "mcp")
	agentB := mk("prefer small commits with one concern each, always", "mcp")
	var out bytes.Buffer
	if err := consolidateGlobal(ctx, f.store, true, &out); err != nil {
		t.Fatalf("consolidateGlobal: %v", err)
	}
	after := globalIDs(t, f.store)
	for name, id := range map[string]string{"always": always, "never": never, "agent A": agentA, "agent B": agentB} {
		if _, ok := after[id]; !ok {
			t.Errorf("%s row %s was folded", name, id)
		}
	}
	if !strings.Contains(out.String(), "source reflection") {
		t.Errorf("the listing does not show each row's source:\n%s", out.String())
	}
}

// The long older row carries specifics the newer subset lacks.
func TestConsolidateGlobalKeepsTheContainingRow(t *testing.T) {
	f := newGlobalFoldFixture(t)
	ctx := context.Background()
	mk := func(content, created string) string {
		id, err := f.store.Create(ctx, "_global", memory.Memory{Category: "convention", Content: content, Source: "reflection"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := f.db.ExecContext(ctx, `UPDATE memories SET created_at = ? WHERE id = ?`, created, id); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		return id
	}
	long := mk("deploy requires helmfile diff then apply, and the sops age key must be exported first, and only from the dev machine", "2026-05-01 00:00:00")
	short := mk("deploy requires helmfile diff then apply", "2026-06-01 00:00:00")
	var dry bytes.Buffer
	if err := consolidateGlobal(ctx, f.store, false, &dry); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if !strings.Contains(dry.String(), "keep "+long) {
		t.Errorf("dry run does not keep the containing row:\n%s", dry.String())
	}
	if err := consolidateGlobal(ctx, f.store, true, &bytes.Buffer{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after := globalIDs(t, f.store)
	if _, ok := after[long]; !ok {
		t.Errorf("the long row did not survive")
	}
	if _, ok := after[short]; ok {
		t.Errorf("the short row survived the fold")
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

// A row edited between the plan and the apply is skipped, and its text is not
// reverted.
func TestFoldRowsSkipsARowEditedSinceThePlan(t *testing.T) {
	f := newGlobalFoldFixture(t)
	ctx := context.Background()
	all := globalIDs(t, f.store)
	plan := func(id string) memory.FoldRow {
		m := all[id]
		return memory.FoldRow{ID: m.ID, Content: m.Content, UpdatedAt: m.UpdatedAt}
	}
	if _, err := f.db.ExecContext(ctx, `UPDATE memories SET content = 'always run go vet, edited by a person', updated_at = '2030-01-01 00:00:00' WHERE id = ?`, f.midID); err != nil {
		t.Fatalf("edit: %v", err)
	}
	res, err := f.store.FoldRows(ctx, "_global", []memory.FoldCluster{{Survivor: plan(f.newID), Folded: []memory.FoldRow{plan(f.oldID), plan(f.midID)}}}, "2999-01-01 00:00:00")
	if err != nil {
		t.Fatalf("FoldRows: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0].ID != f.midID {
		t.Fatalf("skipped = %+v, want only the edited row", res.Skipped)
	}
	after := globalIDs(t, f.store)
	if got := after[f.midID].Content; got != "always run go vet, edited by a person" {
		t.Errorf("the edited row was reverted or rewritten: %q", got)
	}
	if _, ok := after[f.oldID]; ok {
		t.Error("the unedited folded row was not folded")
	}
	// A survivor edited since the plan skips the whole cluster.
	if _, err := f.db.ExecContext(ctx, `UPDATE memories SET updated_at = '2031-01-01 00:00:00' WHERE id = ?`, f.newID); err != nil {
		t.Fatalf("edit: %v", err)
	}
	res, err = f.store.FoldRows(ctx, "_global", []memory.FoldCluster{{Survivor: plan(f.newID), Folded: []memory.FoldRow{plan(f.midID)}}}, "2999-01-01 00:00:00")
	if err != nil || len(res.Folded) != 0 || len(res.Skipped) != 2 {
		t.Fatalf("a changed survivor must skip everything: %+v (err %v)", res, err)
	}
}

// The snapshot a fold takes is one a restore can use: it brings the folded rows
// back and removes nothing else.
func TestConsolidateGlobalFoldCanBeRestored(t *testing.T) {
	f := newGlobalFoldFixture(t)
	ctx := context.Background()
	before := globalIDs(t, f.store)
	if err := consolidateGlobal(ctx, f.store, true, &bytes.Buffer{}); err != nil {
		t.Fatalf("consolidateGlobal: %v", err)
	}
	if _, err := f.store.RestoreSnapshot(ctx, "_global"); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	after := globalIDs(t, f.store)
	for id, m := range before {
		got, ok := after[id]
		if !ok {
			t.Errorf("row %s (%q) is missing after the restore", id, m.Content)
			continue
		}
		if got.Content != m.Content || got.Importance != m.Importance {
			t.Errorf("row %s is not as it was: %+v -> %+v", id, m, got)
		}
	}
	if len(after) != len(before) {
		t.Errorf("restore left %d rows, want %d", len(after), len(before))
	}
}

// A row that is not reflection-written, or is pinned, is refused by id even if a
// caller names it.
func TestFoldRowsRefusesRowsItMayNotTouch(t *testing.T) {
	f := newGlobalFoldFixture(t)
	ctx := context.Background()
	all := globalIDs(t, f.store)
	plan := func(id string) memory.FoldRow {
		m := all[id]
		return memory.FoldRow{ID: m.ID, Content: m.Content, UpdatedAt: m.UpdatedAt}
	}
	// An unpinned agent row, and a pinned reflection row: each is refused for one
	// reason only.
	agent, err := f.store.Create(ctx, "_global", memory.Memory{Category: "preference", Content: "always run go vet before committing any change", Source: "mcp"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	pinnedRefl, err := f.store.Create(ctx, "_global", memory.Memory{Category: "preference", Content: "always run go vet before committing a change", Source: "reflection"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := f.store.TogglePin(ctx, pinnedRefl, true); err != nil {
		t.Fatalf("TogglePin: %v", err)
	}
	all = globalIDs(t, f.store)
	for name, id := range map[string]string{"pinned agent row": f.pinnedID, "unpinned agent row": agent, "pinned reflection row": pinnedRefl} {
		res, err := f.store.FoldRows(ctx, "_global", []memory.FoldCluster{{Survivor: plan(f.newID), Folded: []memory.FoldRow{plan(id)}}}, "2999-01-01 00:00:00")
		if err != nil {
			t.Fatalf("FoldRows: %v", err)
		}
		if len(res.Folded) != 0 || len(res.Skipped) != 1 {
			t.Fatalf("the %s was not refused: %+v", name, res)
		}
		if _, ok := globalIDs(t, f.store)[id]; !ok {
			t.Fatalf("the %s was deleted", name)
		}
	}
	var res memory.FoldResult
	// At or after `since`: refused.
	res, err = f.store.FoldRows(ctx, "_global", []memory.FoldCluster{{Survivor: plan(f.newID), Folded: []memory.FoldRow{plan(f.oldID)}}}, "2026-01-01 00:00:00")
	if err != nil || len(res.Folded) != 0 {
		t.Fatalf("a row created at or after since was folded: %+v (err %v)", res, err)
	}
}

// One run, one snapshot taken before the first delete: a single restore of the
// latest snapshot undoes every cluster, and repeated runs do not pile snapshots up.
func TestConsolidateGlobalTakesOneSnapshotPerRunAndPrunes(t *testing.T) {
	f := newGlobalFoldFixture(t)
	ctx := context.Background()
	mk := func(content string) {
		id, err := f.store.Create(ctx, "_global", memory.Memory{Category: "fact", Content: content, Source: "reflection"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := f.db.ExecContext(ctx, `UPDATE memories SET created_at = '2026-01-01 00:00:00' WHERE id = ?`, id); err != nil {
			t.Fatalf("stamp: %v", err)
		}
	}
	// A second cluster beside the fixture's.
	mk("the relay restarts after kernel updates on friday")
	mk("the relay restarts after kernel updates on friday evening")
	before := globalIDs(t, f.store)
	var out bytes.Buffer
	if err := consolidateGlobal(ctx, f.store, true, &out); err != nil {
		t.Fatalf("consolidateGlobal: %v", err)
	}
	if !strings.Contains(out.String(), "into 2 survivor(s)") {
		t.Fatalf("want two clusters folded:\n%s", out.String())
	}
	var snaps int
	if err := f.db.QueryRow(`SELECT COUNT(DISTINCT snapshot_id) FROM memory_snapshots WHERE project_id = '_global'`).Scan(&snaps); err != nil || snaps != 1 {
		t.Fatalf("snapshots after a two-cluster run = %d (err %v), want 1", snaps, err)
	}
	if _, err := f.store.RestoreSnapshot(ctx, "_global"); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	after := globalIDs(t, f.store)
	if len(after) != len(before) {
		t.Errorf("one restore left %d rows, want all %d back", len(after), len(before))
	}
	for id := range before {
		if _, ok := after[id]; !ok {
			t.Errorf("row %s was not restored", id)
		}
	}
	// Snapshots are bounded: the fold prunes as the replace does.
	for i := 0; i < 14; i++ {
		a, err := f.store.Create(ctx, "_global", memory.Memory{Category: "fact", Content: fmt.Sprintf("round %d alpha uses the shared cache on host one", i), Source: "reflection"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		b, err := f.store.Create(ctx, "_global", memory.Memory{Category: "fact", Content: fmt.Sprintf("round %d alpha uses the shared cache on host one today", i), Source: "reflection"})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		for _, id := range []string{a, b} {
			if _, err := f.db.ExecContext(ctx, `UPDATE memories SET created_at = '2026-01-01 00:00:00' WHERE id = ?`, id); err != nil {
				t.Fatalf("stamp: %v", err)
			}
		}
		if err := consolidateGlobal(ctx, f.store, true, &bytes.Buffer{}); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}
	}
	if err := f.db.QueryRow(`SELECT COUNT(DISTINCT snapshot_id) FROM memory_snapshots WHERE project_id = '_global'`).Scan(&snaps); err != nil || snaps > 10 {
		t.Fatalf("snapshots after many runs = %d (err %v), want at most 10", snaps, err)
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

// A cluster of untagged rows changes its survivor in no way, so the fold writes
// nothing to it: no column, no updated_at bump, no history row.
func TestConsolidateGlobalLeavesAnUntaggedSurvivorAlone(t *testing.T) {
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "untagged.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	var ids []string
	for _, c := range []string{"the relay restarts after kernel updates on friday", "the relay restarts after kernel updates on friday evening"} {
		id, err := store.Create(ctx, "_global", memory.Memory{Category: "fact", Content: c, Source: "reflection", Importance: 0.5})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE memories SET created_at = '2026-01-01 00:00:00' WHERE id = ?`, id); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		ids = append(ids, id)
	}
	survivor := ids[1] // the longer row contains the other
	before := globalIDs(t, store)[survivor]
	var tagsBefore string
	if err := db.QueryRow(`SELECT tags FROM memories WHERE id = ?`, survivor).Scan(&tagsBefore); err != nil {
		t.Fatalf("read tags: %v", err)
	}
	histBefore, _ := store.MemoryHistory(ctx, survivor, 50)
	if err := consolidateGlobal(ctx, store, true, &bytes.Buffer{}); err != nil {
		t.Fatalf("consolidateGlobal: %v", err)
	}
	after, ok := globalIDs(t, store)[survivor]
	if !ok {
		t.Fatal("the survivor is gone")
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("updated_at moved: %s -> %s", before.UpdatedAt, after.UpdatedAt)
	}
	var tags string
	if err := db.QueryRow(`SELECT tags FROM memories WHERE id = ?`, survivor).Scan(&tags); err != nil {
		t.Fatalf("read tags: %v", err)
	}
	if tags != tagsBefore {
		t.Errorf("tags column was rewritten: %q -> %q", tagsBefore, tags)
	}
	histAfter, _ := store.MemoryHistory(ctx, survivor, 50)
	if len(histAfter) != len(histBefore) {
		t.Errorf("survivor history grew: %d -> %d", len(histBefore), len(histAfter))
	}
}
