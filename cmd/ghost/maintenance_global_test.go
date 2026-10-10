package main

import (
	"bytes"
	"context"
	"database/sql"
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
	pinnedBefore := globalIDs(t, f.store)[f.pinnedID]
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
		if _, ok := after[id]; !ok {
			t.Errorf("unrelated row %s was lost", id)
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
