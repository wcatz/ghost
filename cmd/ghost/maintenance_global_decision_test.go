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

// A promoted merge that carries a decision link and is then folded by
// consolidate-global hands the link, and the validity stamps it inherited, to
// the survivor, so a later supersede of that decision still retires the row
// stating it. Both _global rows come from real promotions: project B promotes a
// longer text, and project P promotes a merge of a decision companion and an
// agent fact that the promotion itself does not fold (the promoted text is a
// subset), but the global planner clusters.
func TestConsolidateGlobalFoldCarriesAPromotedMergesDecisionLink(t *testing.T) {
	db, err := memory.OpenDB(filepath.Join(t.TempDir(), "g.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := memory.NewStore(db, nil)
	ctx := context.Background()
	const proj, projB = "fold-proj", "fold-other"
	if err := store.EnsureProject(ctx, proj, "/srv/fold-proj", "fold-proj"); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureProject(ctx, projB, "/srv/fold-other", "fold-other"); err != nil {
		t.Fatal(err)
	}

	g0Text := "every repository ships its events over nats deployed by the arc runners from the infra repository"
	xb, err := store.Create(ctx, projB, memory.Memory{Category: "decision", Content: "repos ship events over nats; the arc runners deploy them from infra", Source: "mcp", Importance: 0.6})
	if err != nil {
		t.Fatal(err)
	}
	ob, err := store.Create(ctx, projB, memory.Memory{Category: "fact", Content: "an unrelated fact in the other project", Source: "reflection", Importance: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if _, pb, _, _, _, _, _, err := store.ApplyReflection(ctx, projB,
		[]memory.Memory{{Category: "fact", Source: "reflection", Importance: 0.5, Content: "an unrelated fact in the other project", ReplacesIDs: []string{ob}}},
		[]memory.Memory{{Category: "decision", Source: "reflection", Importance: 0.6, Content: g0Text, ReplacesIDs: []string{xb}}}, "", true, nil); err != nil || pb != 1 {
		t.Fatalf("project B promotion: promoted=%d err=%v", pb, err)
	}

	decID, comp, _, err := store.RecordDecision(ctx, proj, "Event queue", "we use nats for the event queue", "cheapest", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	const verified, validFrom = "2026-02-01 00:00:00", "2026-01-01 00:00:00"
	if _, err := db.Exec(`UPDATE memories SET verified_at = ?, valid_from = ? WHERE id = ?`, verified, validFrom, comp); err != nil {
		t.Fatal(err)
	}
	save, _, _, err := store.UpsertWithOptions(ctx, proj, "fact", "the arc runners deploy every chart from the infra repository", "mcp", 0.5, nil, memory.UpsertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.Create(ctx, proj, memory.Memory{Category: "fact", Content: "an unrelated project fact the replace keeps", Source: "reflection", Importance: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	mergeText := "every repository ships its events over nats deployed by the arc runners"
	if _, promoted, _, _, _, _, _, err := store.ApplyReflection(ctx, proj,
		[]memory.Memory{{Category: "fact", Source: "reflection", Importance: 0.5, Content: "an unrelated project fact the replace keeps", ReplacesIDs: []string{other}}},
		[]memory.Memory{{Category: "decision", Source: "reflection", Importance: 0.7, Content: mergeText, ReplacesIDs: []string{comp, save}}}, "", true, nil); err != nil || promoted != 1 {
		t.Fatalf("project P promotion: promoted=%d err=%v", promoted, err)
	}

	var g0, g1, g1ref string
	if err := db.QueryRow(`SELECT id FROM memories WHERE content = ? AND project_id = '_global'`, g0Text).Scan(&g0); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT id, COALESCE(source_ref, '') FROM memories WHERE content = ? AND project_id = '_global'`, mergeText).Scan(&g1, &g1ref); err != nil {
		t.Fatal(err)
	}
	link := "decision:" + decID
	if g1ref != link {
		t.Fatalf("fixture: the promoted merge carries source_ref %q, want %q", g1ref, link)
	}
	if _, err := db.Exec(`UPDATE memories SET created_at = datetime('now', '-1 hour') WHERE id IN (?, ?)`, g0, g1); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := consolidateGlobal(ctx, store, true, &out); err != nil {
		t.Fatalf("consolidateGlobal: %v", err)
	}
	if !strings.Contains(out.String(), "folded 1 row(s) into 1 survivor(s)") {
		t.Fatalf("the fixture did not fold G1 into G0:\n%s", out.String())
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM memories WHERE id = ?`, g1).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the promoted merge is still live (n=%d err=%v); the survivor was not G0", n, err)
	}

	var ref, gotVerified, gotFrom sql.NullString
	if err := db.QueryRow(`SELECT source_ref, verified_at, valid_from FROM memories WHERE id = ?`, g0).Scan(&ref, &gotVerified, &gotFrom); err != nil {
		t.Fatal(err)
	}
	if ref.String != link {
		t.Errorf("survivor source_ref = %q, want the folded row's link %q", ref.String, link)
	}
	// The merge path's rule for a row that already exists (fillReusedClaimsTx):
	// a stamp the survivor does not state is taken from a consumed source.
	if gotVerified.String != verified || gotFrom.String != validFrom {
		t.Errorf("survivor verified_at=%q valid_from=%q, want %q and %q from the folded row", gotVerified.String, gotFrom.String, verified, validFrom)
	}

	repl, _, _, err := store.RecordDecision(ctx, proj, "Event queue v2", "we use kafka for the event queue", "scales", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := store.SupersedeDecisionReport(ctx, proj, decID, repl)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Retired) != 1 || rep.Retired[0] != g0 || len(rep.Declined) != 0 || len(rep.Ambiguous) != 0 {
		t.Errorf("supersede after the fold: retired=%v declined=%v ambiguous=%v, want only the survivor %s retired", rep.Retired, rep.Declined, rep.Ambiguous, g0)
	}
	var resolved sql.NullString
	if err := db.QueryRow(`SELECT resolved_at FROM memories WHERE id = ?`, g0).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if !resolved.Valid {
		t.Error("the survivor stating the reversed decision is still live")
	}
}
