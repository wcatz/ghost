package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// These tests hold a promoted merge's decision link to one rule: it ends on a
// live row that a supersede of the decision finds and retires, as on the
// project path, in the two shapes where the promotion cannot write it onto
// the _global row it folds into.

// readPromotedRowByID is readPromotedRow for a text more than one row holds.
func readPromotedRowByID(t *testing.T, s *Store, id string) promotedRow {
	t.Helper()
	var r promotedRow
	if err := s.db.QueryRow(`
		SELECT id, project_id, source, COALESCE(retention, 'project'), expires_at, source_ref, valid_from, verified_at
		FROM memories WHERE id = ?`, id).Scan(
		&r.id, &r.project, &r.source, &r.retention, &r.expires, &r.sourceRef, &r.validFrom, &r.verifiedAt,
	); err != nil {
		t.Fatalf("read row %s: %v", id, err)
	}
	return r
}

// idsHolding lists the ids of every row in project holding content.
func idsHolding(t *testing.T, s *Store, project, content string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT id FROM memories WHERE project_id = ? AND content = ? ORDER BY rowid`, project, content)
	if err != nil {
		t.Fatalf("read rows holding %q: %v", content, err)
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read rows: %v", err)
	}
	return out
}

// A replaceable _global row already holding twelve decision links, which the
// promoted merge folds into: the companion's link would be the thirteenth, so
// the write overflows although the sources' own links fit. The merge is
// refused as the pre-check refuses it, as an overflow drop of thirteen links
// with every source kept live and nothing written back into the project, so
// the companion still carries its link and a supersede retires it.
func TestAPromotionThatOverflowsItsFoldTargetKeepsItsSources(t *testing.T) {
	f := newIssueFixture(t)
	ctx := context.Background()
	if err := f.s.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
		t.Fatal(err)
	}
	target, err := f.s.Create(ctx, GlobalProjectID, Memory{Category: "decision", Source: "mcp", Importance: 0.6,
		Content: issueMergeText})
	if err != nil {
		t.Fatal(err)
	}
	var links []string
	for i := 0; i < 12; i++ {
		fake := fmt.Sprintf("%0*d", len(f.decisionID), i)
		links = append(links, decisionRefPrefix+fake)
	}
	ownRef := strings.Join(links, decisionRefSeparator)
	if len(ownRef) > MaxSourceRefLen || len(ownRef+decisionRefSeparator+decisionRefPrefix+f.decisionID) <= MaxSourceRefLen {
		t.Fatalf("fixture: twelve links take %d bytes, want them to fit and a thirteenth not to", len(ownRef))
	}
	if _, err := f.s.db.Exec(`UPDATE memories SET source_ref = ? WHERE id = ?`, ownRef, target); err != nil {
		t.Fatal(err)
	}

	preserved, promoted, kept, _, globalDropped, deleted, _, err := f.s.ApplyReflection(ctx, testProject,
		[]Memory{f.unrelatedProjectMm}, []Memory{f.candidate}, "", true, nil)
	if err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	if promoted != 0 || len(kept) != 0 {
		t.Errorf("promoted=%d kept=%d, want the merge written nowhere", promoted, len(kept))
	}
	if globalDropped.Overflow != 1 || globalDropped.OverflowLinks != 13 || globalDropped.Total() != 1 {
		t.Errorf("globalDropped = %+v, want one overflow of 13 links", globalDropped)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want no source removed", deleted)
	}
	for _, id := range []string{f.comp, f.save} {
		if !rowExists(t, f.s, id) {
			t.Errorf("source %s was deleted by a refused promotion", id)
		}
		for _, p := range preserved {
			if p == id {
				t.Errorf("source %s reported as preserved", id)
			}
		}
	}
	if got := idsHolding(t, f.s, testProject, issueMergeText); len(got) != 0 {
		t.Errorf("the refused merge was written back into the project as %v", got)
	}
	if after := readPromotedRowByID(t, f.s, target); after.sourceRef.String != ownRef || after.source != "mcp" {
		t.Errorf("the fold target changed: %+v", after)
	}
	if report := f.supersede(t); len(report.Retired) != 1 || report.Retired[0] != f.comp {
		t.Errorf("supersede report = %+v, want the kept companion %s retired", report, f.comp)
	}
}

// A promoted merge whose text a manual or a pinned _global row already holds:
// that row is one the replace never touches, so the promotion inserts a fresh
// _global row beside it, as the project path inserts one beside a manual row.
// The fresh row carries the decision link, the consumed sources' records and
// their history successor, and a supersede retires it and nothing else.
func TestAPromotionBesideAManualOrPinnedGlobalRowCarriesTheLink(t *testing.T) {
	for _, shape := range []string{"manual", "pinned"} {
		t.Run(shape, func(t *testing.T) {
			f := newIssueFixture(t)
			ctx := context.Background()
			if err := f.s.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
				t.Fatal(err)
			}
			m := Memory{Category: "decision", Source: "mcp", Importance: 0.6, Content: issueMergeText}
			if shape == "manual" {
				m.Source = "manual"
			}
			existing, err := f.s.Create(ctx, GlobalProjectID, m)
			if err != nil {
				t.Fatal(err)
			}
			if shape == "pinned" {
				if err := f.s.TogglePin(ctx, existing, true); err != nil {
					t.Fatal(err)
				}
			}
			if promoted, _ := f.apply(t, []Memory{f.unrelatedProjectMm}, true); promoted != 1 {
				t.Fatalf("promoted = %d, want 1", promoted)
			}
			holders := idsHolding(t, f.s, GlobalProjectID, issueMergeText)
			if len(holders) != 2 || holders[0] != existing {
				t.Fatalf("_global rows holding the merge = %v, want %s and a fresh row", holders, existing)
			}
			fresh := holders[1]
			row := readPromotedRowByID(t, f.s, fresh)
			if row.source != "reflection" || !sourceRefNamesDecision(row.sourceRef.String, f.decisionID) {
				t.Errorf("fresh row = %+v, want a reflection row carrying the decision link", row)
			}
			if got := carriedCounts(t, f.s, fresh); got[f.comp] != 3 || got[f.save] != 2 {
				t.Errorf("fresh row's carried evidence = %v, want 3 and 2", got)
			}
			for _, id := range []string{f.comp, f.save} {
				if rowExists(t, f.s, id) {
					t.Errorf("source %s was kept, want it consumed into the fresh row", id)
				}
				if hist := successorsOf(t, f.s, id); len(hist) != 1 || hist[0] != fresh {
					t.Errorf("delete history of %s names %v, want %s", id, hist, fresh)
				}
			}
			if sourceRefNamesDecision(readPromotedRowByID(t, f.s, existing).sourceRef.String, f.decisionID) {
				t.Errorf("the %s row was given the decision link", shape)
			}
			if report := f.supersede(t); len(report.Retired) != 1 || report.Retired[0] != fresh {
				t.Errorf("supersede report = %+v, want the fresh row %s retired", report, fresh)
			}
			if decisionMemoryResolved(t, f.s, existing) {
				t.Errorf("the %s row was retired", shape)
			}
		})
	}
}

// The control: a promotion whose consumed sources carry no decision link keeps
// the FoldOnly dedup against a manual or pinned _global row. It folds into
// that row, so one _global row holds the text, and the row takes nothing from
// the sources.
func TestAPromotionConsumingNoDecisionLinkStillFoldsIntoAManualOrPinnedRow(t *testing.T) {
	for _, shape := range []string{"manual", "pinned"} {
		t.Run(shape, func(t *testing.T) {
			f := newIssueFixture(t)
			ctx := context.Background()
			f.candidate.ReplacesIDs = []string{f.save}
			if err := f.s.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
				t.Fatal(err)
			}
			m := Memory{Category: "decision", Source: "mcp", Importance: 0.6, Content: issueMergeText}
			if shape == "manual" {
				m.Source = "manual"
			}
			existing, err := f.s.Create(ctx, GlobalProjectID, m)
			if err != nil {
				t.Fatal(err)
			}
			if shape == "pinned" {
				if err := f.s.TogglePin(ctx, existing, true); err != nil {
					t.Fatal(err)
				}
			}
			before := readPromotedRowByID(t, f.s, existing)
			if promoted, _ := f.apply(t, []Memory{f.unrelatedProjectMm}, true); promoted != 1 {
				t.Fatalf("promoted = %d, want 1", promoted)
			}
			if holders := idsHolding(t, f.s, GlobalProjectID, issueMergeText); len(holders) != 1 || holders[0] != existing {
				t.Fatalf("_global rows holding the merge = %v, want the fold into %s alone", holders, existing)
			}
			after := readPromotedRowByID(t, f.s, existing)
			if after.source != before.source || after.sourceRef != before.sourceRef || after.retention != before.retention {
				t.Errorf("the %s row changed: before %+v, after %+v", shape, before, after)
			}
			if got := carriedCounts(t, f.s, existing); len(got) != 1 {
				t.Errorf("the %s row holds carried records %v", shape, got)
			}
		})
	}
}

// overflowingGlobalTarget gives _global a replaceable row holding the merge
// text and twelve decision links, so the fixture's merge overflows at the
// write when it folds into it.
func overflowingGlobalTarget(t *testing.T, f issueFixture) string {
	t.Helper()
	ctx := context.Background()
	if err := f.s.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
		t.Fatal(err)
	}
	target, err := f.s.Create(ctx, GlobalProjectID, Memory{Category: "decision", Source: "mcp", Importance: 0.6,
		Content: issueMergeText})
	if err != nil {
		t.Fatal(err)
	}
	var links []string
	for i := 0; i < 12; i++ {
		links = append(links, decisionRefPrefix+fmt.Sprintf("%0*d", len(f.decisionID), i))
	}
	if _, err := f.s.db.Exec(`UPDATE memories SET source_ref = ? WHERE id = ?`, strings.Join(links, decisionRefSeparator), target); err != nil {
		t.Fatal(err)
	}
	return target
}

// A source the overflowing promotion shares with a project emission that
// already took it stays that emission's: it is deleted into the emission's
// row as before, and only the sources nothing else consumed are kept.
func TestAnOverflowingPromotionLeavesASourceAnEarlierEmissionTook(t *testing.T) {
	f := newIssueFixture(t)
	ctx := context.Background()
	overflowingGlobalTarget(t, f)
	const projectText = "the arc runners deploy every chart, merged in the project"
	projectMerge := reflectionMemory("fact", projectText)
	projectMerge.ReplacesIDs = []string{f.save}
	_, promoted, kept, _, globalDropped, deleted, _, err := f.s.ApplyReflection(ctx, testProject,
		[]Memory{projectMerge}, []Memory{f.candidate}, "", true, nil)
	if err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	if promoted != 0 || len(kept) != 0 || globalDropped.Overflow != 1 {
		t.Fatalf("promoted=%d kept=%d globalDropped=%+v, want one overflow drop", promoted, len(kept), globalDropped)
	}
	merged := idsHolding(t, f.s, testProject, projectText)
	if len(merged) != 1 {
		t.Fatalf("project rows holding the project merge = %v, want one", merged)
	}
	if rowExists(t, f.s, f.save) {
		t.Errorf("source %s was kept live, want it consumed by the project merge", f.save)
	}
	if hist := successorsOf(t, f.s, f.save); len(hist) != 1 || hist[0] != merged[0] {
		t.Errorf("delete history of %s names %v, want %s", f.save, hist, merged[0])
	}
	if !rowExists(t, f.s, f.comp) {
		t.Errorf("companion %s was deleted by a refused promotion", f.comp)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want the one source the project merge took", deleted)
	}
}

// A project row holding the overflowing merge's text, though the merge does
// not name it, is kept with the sources, as the pre-check keeps it, so the
// refused merge's text is still held by a live row.
func TestAnOverflowingPromotionKeepsAnUnnamedRowHoldingItsText(t *testing.T) {
	f := newIssueFixture(t)
	ctx := context.Background()
	overflowingGlobalTarget(t, f)
	holder, err := f.s.Create(ctx, testProject, Memory{Category: "fact", Source: "mcp", Importance: 0.5, Content: issueMergeText})
	if err != nil {
		t.Fatal(err)
	}
	_, promoted, kept, _, globalDropped, deleted, _, err := f.s.ApplyReflection(ctx, testProject,
		[]Memory{f.unrelatedProjectMm}, []Memory{f.candidate}, "", true, nil)
	if err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	if promoted != 0 || len(kept) != 0 || globalDropped.Overflow != 1 {
		t.Fatalf("promoted=%d kept=%d globalDropped=%+v, want one overflow drop", promoted, len(kept), globalDropped)
	}
	for _, id := range []string{f.comp, f.save, holder} {
		if !rowExists(t, f.s, id) {
			t.Errorf("row %s was deleted by a refused promotion", id)
		}
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want nothing removed", deleted)
	}
}

// Two rounds promoting the same text beside a manual _global row that
// outranks everything: the first inserts the carrier, and the second folds
// into that carrier rather than inserting another, so two rows hold the text,
// and the carrier holds both decisions' links until the last is superseded.
func TestASecondPromotionBesideAManualRowFoldsIntoTheFirstCarrier(t *testing.T) {
	f := newIssueFixture(t)
	ctx := context.Background()
	if err := f.s.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
		t.Fatal(err)
	}
	manual, err := f.s.Create(ctx, GlobalProjectID, Memory{Category: "decision", Source: "manual", Importance: 0.95,
		Content: issueMergeText})
	if err != nil {
		t.Fatal(err)
	}
	if promoted, _ := f.apply(t, []Memory{f.unrelatedProjectMm}, true); promoted != 1 {
		t.Fatalf("round 1: promoted = %d, want 1", promoted)
	}
	holders := idsHolding(t, f.s, GlobalProjectID, issueMergeText)
	if len(holders) != 2 || holders[0] != manual {
		t.Fatalf("round 1: _global rows holding the merge = %v, want %s and a carrier", holders, manual)
	}
	carrier := holders[1]

	decision2, comp2, _, err := f.s.RecordDecision(ctx, testProject, "Event bus", "we run nats in every cluster", "one transport", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	save2, _, _, err := f.s.UpsertWithOptions(ctx, testProject, "fact", "the arc runners roll the nats chart out first", "mcp", 0.5, nil, UpsertOptions{})
	if err != nil {
		t.Fatalf("seed the second save: %v", err)
	}
	second := f.candidate
	second.ReplacesIDs = []string{comp2, save2}
	_, promoted, kept, _, _, _, _, err := f.s.ApplyReflection(ctx, testProject, []Memory{f.unrelatedProjectMm}, []Memory{second}, "", true, nil)
	if err != nil {
		t.Fatalf("round 2: ApplyReflection: %v", err)
	}
	if promoted != 1 || len(kept) != 0 {
		t.Fatalf("round 2: promoted=%d kept=%d, want 1 and none", promoted, len(kept))
	}
	if got := idsHolding(t, f.s, GlobalProjectID, issueMergeText); len(got) != 2 || got[0] != manual || got[1] != carrier {
		t.Fatalf("round 2: _global rows holding the merge = %v, want %s and %s", got, manual, carrier)
	}
	ref := readPromotedRowByID(t, f.s, carrier).sourceRef.String
	if !sourceRefNamesDecision(ref, f.decisionID) || !sourceRefNamesDecision(ref, decision2) {
		t.Errorf("carrier source_ref = %q, want both decisions' links", ref)
	}
	if sourceRefNamesDecision(readPromotedRowByID(t, f.s, manual).sourceRef.String, decision2) {
		t.Error("the manual row was given the second decision's link")
	}

	if report := f.supersede(t); len(report.Shared) != 1 || report.Shared[0] != carrier || len(report.Retired) != 0 {
		t.Errorf("first supersede report = %+v, want the carrier shared with the live second decision", report)
	}
	replacement, _, _, err := f.s.RecordDecision(ctx, testProject, "Event bus v2", "we run kafka in every cluster", "it scales", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision replacement: %v", err)
	}
	report, err := f.s.SupersedeDecisionReport(ctx, testProject, decision2, replacement)
	if err != nil {
		t.Fatalf("SupersedeDecisionReport: %v", err)
	}
	if len(report.Retired) != 1 || report.Retired[0] != carrier {
		t.Errorf("second supersede report = %+v, want the carrier %s retired", report, carrier)
	}
	if decisionMemoryResolved(t, f.s, manual) {
		t.Error("the manual row was retired")
	}
}
