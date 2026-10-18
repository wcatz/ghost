package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// foldRowsLinks joins n distinct decision links starting at base.
func foldRowsLinks(base, n int) string {
	refs := make([]string, n)
	for i := range refs {
		refs[i] = fmt.Sprintf("decision:%032X", base+i)
	}
	return strings.Join(refs, decisionRefSeparator)
}

// TestFoldRowsRefusesAClusterWhoseDecisionLinksOverflow: the fold carries the
// decision links of the rows it consumes onto the survivor, and when their union
// with the survivor's own would not fit in one source_ref the fold refuses that
// cluster rather than dropping links — every row of it is skipped and left as it
// was, while another cluster in the same call still folds and carries its link.
func TestFoldRowsRefusesAClusterWhoseDecisionLinksOverflow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	mk := func(content, ref string) FoldRow {
		t.Helper()
		id, err := s.Create(ctx, testProject, Memory{Category: "decision", Content: content, Source: "reflection", Importance: 0.5})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE memories SET source_ref = NULLIF(?, ''), created_at = '2026-01-01 00:00:00' WHERE id = ?`, ref, id); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		r := FoldRow{ID: id}
		if err := s.db.QueryRowContext(ctx, `SELECT content, updated_at FROM memories WHERE id = ?`, id).Scan(&r.Content, &r.UpdatedAt); err != nil {
			t.Fatalf("read: %v", err)
		}
		return r
	}
	// Seven links each (293 bytes), fourteen together (587): each row is within
	// the bound, the merge is not.
	survA := mk("the overflow cluster survivor text that is the longer one", foldRowsLinks(0, 7))
	foldA := mk("the overflow cluster survivor text", foldRowsLinks(100, 7))
	if len(foldRowsLinks(0, 14)) <= MaxSourceRefLen || len(foldRowsLinks(0, 7)) > MaxSourceRefLen {
		t.Fatal("fixture: the link lists do not straddle the bound")
	}
	cleanLink := foldRowsLinks(500, 1)
	survB := mk("the clean cluster survivor text that is the longer one", "")
	foldB := mk("the clean cluster survivor text", cleanLink)

	refOf := func(id string) (string, bool) {
		var ref sql.NullString
		err := s.db.QueryRowContext(ctx, `SELECT source_ref FROM memories WHERE id = ?`, id).Scan(&ref)
		if err == sql.ErrNoRows {
			return "", false
		}
		if err != nil {
			t.Fatalf("read source_ref: %v", err)
		}
		return ref.String, true
	}
	beforeA, _ := refOf(survA.ID)
	beforeFoldA, _ := refOf(foldA.ID)

	res, err := s.FoldRows(ctx, testProject, []FoldCluster{
		{Survivor: survA, Folded: []FoldRow{foldA}},
		{Survivor: survB, Folded: []FoldRow{foldB}},
	}, "")
	if err != nil {
		t.Fatalf("FoldRows: %v", err)
	}

	skipped := map[string]string{}
	for _, sk := range res.Skipped {
		skipped[sk.ID] = sk.Reason
	}
	for _, id := range []string{survA.ID, foldA.ID} {
		if why := skipped[id]; !strings.Contains(why, "decision links") {
			t.Errorf("overflow cluster row %s: skip reason %q, want a decision-link overflow", id, why)
		}
	}
	if got, ok := refOf(survA.ID); !ok || got != beforeA {
		t.Errorf("the refused survivor changed: live=%v source_ref=%q", ok, got)
	}
	if got, ok := refOf(foldA.ID); !ok || got != beforeFoldA {
		t.Errorf("the refused folded row changed or went: live=%v source_ref=%q", ok, got)
	}

	if len(res.Folded) != 1 || res.Folded[0] != foldB.ID || res.Clusters != 1 {
		t.Fatalf("folded=%v clusters=%d, want only the clean cluster's %s", res.Folded, res.Clusters, foldB.ID)
	}
	if got, _ := refOf(survB.ID); got != cleanLink {
		t.Errorf("the clean survivor's source_ref = %q, want the folded row's link %q", got, cleanLink)
	}
}
