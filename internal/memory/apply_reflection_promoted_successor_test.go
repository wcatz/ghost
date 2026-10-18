package memory

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// These tests hold a cross-project merge promoted to _global to the rule the
// project replace follows for the same merge: the promoted row is the
// consolidation successor of the project rows the replace consumes, so it takes
// their tier, claims, evidence and history successor by the same code, and it
// takes from a source the replace keeps live only what a project merge would
// (its decision links). Most fixtures run both ways and compare.

type promotedRow struct {
	id, project, source, retention string
	expires, sourceRef             sql.NullString
	validFrom, verifiedAt          sql.NullString
}

func readPromotedRow(t *testing.T, s *Store, content string) promotedRow {
	t.Helper()
	var r promotedRow
	if err := s.db.QueryRow(`
		SELECT id, project_id, source, COALESCE(retention, 'project'), expires_at, source_ref, valid_from, verified_at
		FROM memories WHERE content = ?`, content).Scan(
		&r.id, &r.project, &r.source, &r.retention, &r.expires, &r.sourceRef, &r.validFrom, &r.verifiedAt,
	); err != nil {
		t.Fatalf("read the row holding %q: %v", content, err)
	}
	return r
}

// carriedCounts counts the records a row holds by the row they were carried
// from; "" is the row's own.
func carriedCounts(t *testing.T, s *Store, id string) map[string]int {
	t.Helper()
	rows, err := s.db.Query(`SELECT COALESCE(carried_from, '') FROM memory_provenance WHERE memory_id = ?`, id)
	if err != nil {
		t.Fatalf("read evidence of %s: %v", id, err)
	}
	defer rows.Close() //nolint:errcheck
	out := map[string]int{}
	for rows.Next() {
		var from string
		if err := rows.Scan(&from); err != nil {
			t.Fatalf("scan evidence: %v", err)
		}
		out[from]++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read evidence: %v", err)
	}
	return out
}

func ownEvidence(t *testing.T, s *Store, id string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_provenance WHERE memory_id = ? AND carried_from IS NULL`, id).Scan(&n); err != nil {
		t.Fatalf("count evidence of %s: %v", id, err)
	}
	return n
}

// padEvidence gives a row exactly n records of its own.
func padEvidence(t *testing.T, s *Store, id string, n int) {
	t.Helper()
	for ownEvidence(t, s, id) < n {
		if _, err := s.db.Exec(`INSERT INTO memory_provenance (memory_id, kind, agent, observed_at) VALUES (?, 'observed', 'agent-pad', datetime('now'))`, id); err != nil {
			t.Fatalf("pad evidence of %s: %v", id, err)
		}
	}
	if got := ownEvidence(t, s, id); got != n {
		t.Fatalf("fixture: %s holds %d records, want %d", id, got, n)
	}
}

// successorsOf lists the related ids every delete row of id names.
func successorsOf(t *testing.T, s *Store, id string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT COALESCE(related_id, '') FROM memory_history WHERE memory_id = ? AND phase = ? ORDER BY rowid`, id, phaseDelete)
	if err != nil {
		t.Fatalf("read delete history of %s: %v", id, err)
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatalf("scan history: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read history: %v", err)
	}
	return out
}

// issueFixture is the issue's first fixture: a decision companion with a
// validity start, a verification and three records, and a session-tier agent
// save with two records, about to be merged by one cross-project candidate.
type issueFixture struct {
	s                  *Store
	decisionID         string
	comp, save         string
	compText           string
	candidate          Memory
	unrelatedProjectMm Memory
}

const issueMergeText = "every repository ships its events over nats, deployed by the arc runners"

func newIssueFixture(t *testing.T) issueFixture {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	decisionID, comp, _, err := s.RecordDecision(ctx, testProject, "Event queue", "we use nats for the event queue", "it was the cheapest", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE memories SET valid_from = '2026-01-01 00:00:00', verified_at = '2026-02-01 00:00:00' WHERE id = ?`, comp); err != nil {
		t.Fatalf("stamp the companion: %v", err)
	}
	save, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", "the arc runners deploy every chart from the infra repository", "mcp", 0.5, nil, UpsertOptions{
		Retention: RetentionSession,
	})
	if err != nil {
		t.Fatalf("seed the session save: %v", err)
	}
	padEvidence(t, s, comp, 3)
	padEvidence(t, s, save, 2)
	var compText string
	if err := s.db.QueryRow(`SELECT content FROM memories WHERE id = ?`, comp).Scan(&compText); err != nil {
		t.Fatalf("read the companion: %v", err)
	}
	return issueFixture{
		s: s, decisionID: decisionID, comp: comp, save: save, compText: compText,
		candidate: Memory{Category: "decision", Source: "reflection", Importance: 0.7,
			Content: issueMergeText, ReplacesIDs: []string{comp, save}},
		unrelatedProjectMm: reflectionMemory("fact", "an unrelated project fact the replace keeps"),
	}
}

func (f issueFixture) apply(t *testing.T, projectMems []Memory, promote bool) (promoted int, globalDropped EmissionDrops) {
	t.Helper()
	_, promoted, kept, _, globalDropped, _, _, err := f.s.ApplyReflection(context.Background(), testProject, projectMems, []Memory{f.candidate}, "", promote, nil)
	if err != nil {
		t.Fatalf("ApplyReflection(promote=%v): %v", promote, err)
	}
	if len(kept) != 0 {
		t.Fatalf("promote=%v: %d candidate(s) written back to the project, want none", promote, len(kept))
	}
	return promoted, globalDropped
}

func (f issueFixture) supersede(t *testing.T) DecisionRetirement {
	t.Helper()
	ctx := context.Background()
	replacement, _, _, err := f.s.RecordDecision(ctx, testProject, "Event queue v2", "we use kafka for the event queue", "it scales", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision replacement: %v", err)
	}
	report, err := f.s.SupersedeDecisionReport(ctx, testProject, f.decisionID, replacement)
	if err != nil {
		t.Fatalf("SupersedeDecisionReport: %v", err)
	}
	return report
}

// The issue's first fixture, run with promotion off and on: the promoted row
// carries the decision link, the window start, the verification, the union of
// both sources' records and each source's history successor, exactly as the
// project row does, and a supersede of the decision retires it.
func TestPromotedMergeInheritsWhatTheProjectMergeDoes(t *testing.T) {
	for _, promote := range []bool{false, true} {
		t.Run(fmt.Sprintf("promote=%v", promote), func(t *testing.T) {
			f := newIssueFixture(t)
			promoted, dropped := f.apply(t, []Memory{f.unrelatedProjectMm}, promote)
			wantProject, wantPromoted := testProject, 0
			if promote {
				wantProject, wantPromoted = GlobalProjectID, 1
			}
			if promoted != wantPromoted || dropped.Total() != 0 {
				t.Fatalf("promoted=%d dropped=%+v, want %d and none", promoted, dropped, wantPromoted)
			}
			row := readPromotedRow(t, f.s, issueMergeText)
			if row.project != wantProject {
				t.Fatalf("successor is in %q, want %q", row.project, wantProject)
			}
			if rowExists(t, f.s, f.comp) || rowExists(t, f.s, f.save) {
				t.Fatal("a source survived the merge")
			}
			if row.source != "reflection" {
				t.Errorf("source = %q, want reflection", row.source)
			}
			if !sourceRefNamesDecision(row.sourceRef.String, f.decisionID) {
				t.Errorf("source_ref = %q, want the companion's decision link", row.sourceRef.String)
			}
			if row.validFrom.String != "2026-01-01 00:00:00" || row.verifiedAt.String != "2026-02-01 00:00:00" {
				t.Errorf("valid_from=%q verified_at=%q, want the companion's", row.validFrom.String, row.verifiedAt.String)
			}
			got := carriedCounts(t, f.s, row.id)
			if got[f.comp] != 3 || got[f.save] != 2 {
				t.Errorf("carried evidence = %v, want 3 from the companion and 2 from the save", got)
			}
			for _, id := range []string{f.comp, f.save} {
				if hist := successorsOf(t, f.s, id); len(hist) != 1 || hist[0] != row.id {
					t.Errorf("delete history of %s names %v, want the successor %s", id, hist, row.id)
				}
			}
			report := f.supersede(t)
			if len(report.Retired) != 1 || report.Retired[0] != row.id || len(report.Ambiguous) != 0 {
				t.Errorf("supersede report = %+v, want the successor %s retired", report, row.id)
			}
			if !decisionMemoryResolved(t, f.s, row.id) {
				t.Errorf("successor %s is still live after its decision was superseded", row.id)
			}
		})
	}
}

// The issue's second fixture: two session-tier sources promote to a session
// row with an expiry, as they merge in the project, rather than to a project
// row nothing prunes.
func TestPromotedMergeOfSessionRowsStaysSession(t *testing.T) {
	for _, promote := range []bool{false, true} {
		t.Run(fmt.Sprintf("promote=%v", promote), func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			var ids []string
			for _, text := range []string{"the bastion host fronts every ssh session", "ssh keys rotate on the first of the month"} {
				id, _, _, err := s.UpsertWithOptions(ctx, testProject, "convention", text, "mcp", 0.5, nil, UpsertOptions{Retention: RetentionSession})
				if err != nil {
					t.Fatalf("seed: %v", err)
				}
				ids = append(ids, id)
			}
			const text = "every ssh session goes through the bastion, with keys rotated monthly"
			candidate := Memory{Category: "convention", Source: "reflection", Importance: 0.6, Content: text, ReplacesIDs: ids}
			if _, _, _, _, _, _, _, err := s.ApplyReflection(ctx, testProject, []Memory{reflectionMemory("fact", "kept")}, []Memory{candidate}, "", promote, nil); err != nil {
				t.Fatalf("ApplyReflection: %v", err)
			}
			row := readPromotedRow(t, s, text)
			if row.retention != RetentionSession || !row.expires.Valid {
				t.Errorf("successor in %s: retention=%s expires=%v, want session with an expiry", row.project, row.retention, row.expires)
			}
		})
	}
}

// R2, the drop guard's re-add shape: a project emission restates the companion
// verbatim, so the replace reuses it in place and consumes only the save. The
// promoted row takes the companion's decision link and nothing else of it —
// not its records, its window, its verification or its tier, and the companion
// is not given a successor — exactly as the project merge does. Two live rows
// then carry the link, and the supersede reports both instead of retiring one.
func TestPromotedMergeTakesOnlyTheLinkFromAKeptLiveSource(t *testing.T) {
	for _, promote := range []bool{false, true} {
		t.Run(fmt.Sprintf("promote=%v", promote), func(t *testing.T) {
			f := newIssueFixture(t)
			readd := reflectionMemory("decision", f.compText)
			f.apply(t, []Memory{readd, f.unrelatedProjectMm}, promote)
			if !rowExists(t, f.s, f.comp) {
				t.Fatal("fixture: the re-added companion did not keep its row")
			}
			if rowExists(t, f.s, f.save) {
				t.Fatal("fixture: the save was not consumed")
			}
			row := readPromotedRow(t, f.s, issueMergeText)
			if !sourceRefNamesDecision(row.sourceRef.String, f.decisionID) {
				t.Errorf("source_ref = %q, want the kept companion's decision link", row.sourceRef.String)
			}
			if row.validFrom.Valid || row.verifiedAt.Valid {
				t.Errorf("valid_from=%v verified_at=%v, want none: the companion keeps its claims", row.validFrom, row.verifiedAt)
			}
			if row.retention != RetentionSession {
				t.Errorf("retention = %s, want the consumed save's session tier", row.retention)
			}
			got := carriedCounts(t, f.s, row.id)
			if got[f.comp] != 0 || got[f.save] != 2 {
				t.Errorf("carried evidence = %v, want none from the companion and 2 from the save", got)
			}
			if hist := successorsOf(t, f.s, f.save); len(hist) != 1 || hist[0] != row.id {
				t.Errorf("delete history of the save names %v, want %s", hist, row.id)
			}
			var pointers int
			if err := f.s.db.QueryRow(`SELECT count(*) FROM memory_history WHERE memory_id = ? AND related_id = ?`, f.comp, row.id).Scan(&pointers); err != nil {
				t.Fatal(err)
			}
			if pointers != 0 {
				t.Errorf("the live companion's history names the merge as its successor")
			}
			report := f.supersede(t)
			if len(report.Retired) != 0 || len(report.Ambiguous) != 2 {
				t.Errorf("supersede report = %+v, want nothing retired and both carriers ambiguous", report)
			}
		})
	}
}

// R3, a global-only apply: no project replace runs, so nothing is consumed and
// the promoted row is nobody's successor. It inherits nothing from the rows
// that stay live — no link, no claims, no records, no tier, no history pointer.
func TestGlobalOnlyPromotionInheritsNothingFromLiveSources(t *testing.T) {
	f := newIssueFixture(t)
	promoted, dropped := f.apply(t, nil, true)
	if promoted != 1 || dropped.Total() != 0 {
		t.Fatalf("promoted=%d dropped=%+v, want 1 and none", promoted, dropped)
	}
	if !rowExists(t, f.s, f.comp) || !rowExists(t, f.s, f.save) {
		t.Fatal("a global-only apply deleted a source")
	}
	row := readPromotedRow(t, f.s, issueMergeText)
	if row.project != GlobalProjectID {
		t.Fatalf("row is in %q, want _global", row.project)
	}
	if row.sourceRef.Valid || row.validFrom.Valid || row.verifiedAt.Valid {
		t.Errorf("row = %+v, want no link, window or verification", row)
	}
	if row.retention != RetentionProject || row.expires.Valid {
		t.Errorf("retention=%s expires=%v, want the emission's own project tier", row.retention, row.expires)
	}
	if got := carriedCounts(t, f.s, row.id); len(got) != 1 || got[""] != 1 {
		t.Errorf("evidence = %v, want only its own observation", got)
	}
	for _, id := range []string{f.comp, f.save} {
		if hist := successorsOf(t, f.s, id); len(hist) != 0 {
			t.Errorf("live source %s has delete history %v", id, hist)
		}
	}
}

// R9/R10: the same apply run twice carries nothing the second time. A
// consumed source is gone by then, and a kept-live one never gave records, so
// the carried set of the promoted row is what the first run left.
func TestRerunningAPromotedApplyCarriesNothingTwice(t *testing.T) {
	shapes := map[string]func(f issueFixture) []Memory{
		"merge": func(f issueFixture) []Memory { return []Memory{f.unrelatedProjectMm} },
		"kept-live": func(f issueFixture) []Memory {
			return []Memory{reflectionMemory("decision", f.compText), f.unrelatedProjectMm}
		},
		"global-only": func(issueFixture) []Memory { return nil },
	}
	for name, projectMems := range shapes {
		t.Run(name, func(t *testing.T) {
			f := newIssueFixture(t)
			f.apply(t, projectMems(f), true)
			row := readPromotedRow(t, f.s, issueMergeText)
			first := carriedCounts(t, f.s, row.id)
			f.apply(t, projectMems(f), true)
			again := readPromotedRow(t, f.s, issueMergeText)
			if again.id != row.id {
				t.Fatalf("the rerun wrote a second row %s beside %s", again.id, row.id)
			}
			second := carriedCounts(t, f.s, row.id)
			for from, n := range second {
				if from == "" {
					continue // a fold records one observation per report, as it always has
				}
				if n != first[from] {
					t.Errorf("records carried from %s: %d after the first run, %d after the second", from, first[from], n)
				}
			}
		})
	}
}

// R4: a promoted merge whose decision links do not fit in one source_ref is
// refused as the project path refuses it (#1228): its sources are kept live,
// it is counted as an overflow of the global half, and the kept sources are not
// reported as survivors a later run has to merge.
func TestAnOverflowingPromotedMergeIsRefusedAndItsSourcesKept(t *testing.T) {
	for _, withProject := range []bool{true, false} {
		t.Run(fmt.Sprintf("project-half=%v", withProject), func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			var mems []string
			for i := 0; i < 13; i++ {
				_, mem, _, err := s.RecordDecision(ctx, testProject, fmt.Sprintf("Decision %d", i), fmt.Sprintf("choice %d", i), "because", nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				mems = append(mems, mem)
			}
			const text = "thirteen decisions in one cross-project merge"
			candidate := Memory{Category: "decision", Source: "reflection", Importance: 0.9, Content: text, ReplacesIDs: mems}
			var projectMems []Memory
			if withProject {
				projectMems = []Memory{reflectionMemory("fact", "an unrelated project fact")}
			}
			preserved, promoted, kept, _, globalDropped, _, _, err := s.ApplyReflection(ctx, testProject, projectMems, []Memory{candidate}, "", true, nil)
			if err != nil {
				t.Fatalf("ApplyReflection: %v", err)
			}
			if promoted != 0 || len(kept) != 0 {
				t.Errorf("promoted=%d kept=%d, want the merge written nowhere", promoted, len(kept))
			}
			if globalDropped.Overflow != 1 || globalDropped.OverflowLinks != 13 || globalDropped.Total() != 1 {
				t.Errorf("globalDropped = %+v, want one overflow of 13 links", globalDropped)
			}
			for _, mem := range mems {
				if !rowExists(t, s, mem) {
					t.Errorf("companion %s was deleted by a refused merge", mem)
				}
				for _, id := range preserved {
					if id == mem {
						t.Errorf("companion %s reported as preserved", mem)
					}
				}
			}
			var n int
			if err := s.db.QueryRow(`SELECT count(*) FROM memories WHERE content = ?`, text).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Errorf("%d row(s) hold the refused merge", n)
			}
		})
	}
}

// A merge FoldOnly folds into a replaceable _global row makes that row the
// successor, as a project reuse does: it keeps its own reference and gains the
// decision link, the consumed sources' records, their history successor and
// the reflection provenance the decision lookup trusts, so a supersede retires
// it.
func TestPromotedMergeFoldedIntoAReplaceableGlobalRowExtendsIt(t *testing.T) {
	f := newIssueFixture(t)
	ctx := context.Background()
	f.candidate.Category = "fact"
	if err := f.s.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
		t.Fatal(err)
	}
	existing, err := f.s.Create(ctx, GlobalProjectID, Memory{Category: "fact", Source: "mcp", Importance: 0.6,
		Content: issueMergeText, SourceRef: "issue:7"})
	if err != nil {
		t.Fatal(err)
	}
	if promoted, _ := f.apply(t, []Memory{f.unrelatedProjectMm}, true); promoted != 1 {
		t.Fatalf("promoted = %d, want 1", promoted)
	}
	row := readPromotedRow(t, f.s, issueMergeText)
	if row.id != existing {
		t.Fatalf("successor = %s, want the existing _global row %s", row.id, existing)
	}
	if !sourceRefNamesDecision(row.sourceRef.String, f.decisionID) || len(row.sourceRef.String) < 7 || row.sourceRef.String[:7] != "issue:7" {
		t.Errorf("source_ref = %q, want its own reference extended with the decision link", row.sourceRef.String)
	}
	if row.source != "reflection" {
		t.Errorf("source = %q, want reflection once it absorbed a consumed source", row.source)
	}
	got := carriedCounts(t, f.s, existing)
	if got[f.comp] != 3 || got[f.save] != 2 {
		t.Errorf("carried evidence = %v, want 3 and 2", got)
	}
	for _, id := range []string{f.comp, f.save} {
		if hist := successorsOf(t, f.s, id); len(hist) != 1 || hist[0] != existing {
			t.Errorf("delete history of %s names %v, want %s", id, hist, existing)
		}
	}
	if report := f.supersede(t); len(report.Retired) != 1 || report.Retired[0] != existing {
		t.Errorf("supersede report = %+v, want %s retired", report, existing)
	}
}

// The open question: FoldOnly can fold a promoted merge into a manual or a
// pinned _global row. Those are rows the project replace never touches, and the
// promotion holds them to the same rule: the fold strengthens the row as any
// fold does, but no claim, link, record or provenance of the consumed sources
// is written onto it, so a supersede of the decision cannot retire a row the
// user made or protected.
func TestPromotedMergeLeavesAManualOrPinnedGlobalRowAlone(t *testing.T) {
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
			before := readPromotedRow(t, f.s, issueMergeText)
			f.apply(t, []Memory{f.unrelatedProjectMm}, true)
			after := readPromotedRow(t, f.s, issueMergeText)
			if after.id != existing {
				t.Fatalf("successor = %s, want the fold into %s", after.id, existing)
			}
			if after.source != before.source || after.sourceRef != before.sourceRef || after.validFrom != before.validFrom || after.verifiedAt != before.verifiedAt {
				t.Errorf("the %s row changed: before %+v, after %+v", shape, before, after)
			}
			if got := carriedCounts(t, f.s, existing); len(got) != 1 {
				t.Errorf("the %s row holds carried records %v", shape, got)
			}
			if report := f.supersede(t); len(report.Retired) != 0 {
				t.Errorf("supersede report = %+v, want the %s row left alone", report, shape)
			}
			if decisionMemoryResolved(t, f.s, existing) {
				t.Errorf("the %s row was retired", shape)
			}
		})
	}
}

// --restore after a promoted merge: the project comes back exactly, and the
// promoted row stays in _global with the link and the records it carried. Two
// live rows then carry the decision, and the supersede reports both rather
// than retiring the restored companion alone.
func TestRestoringAPromotedApplyLeavesTheGlobalRowsCarriedLinkAndRecords(t *testing.T) {
	f := newIssueFixture(t)
	ctx := context.Background()
	f.apply(t, []Memory{f.unrelatedProjectMm}, true)
	row := readPromotedRow(t, f.s, issueMergeText)
	if _, err := f.s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if !rowExists(t, f.s, f.comp) || !rowExists(t, f.s, f.save) {
		t.Fatal("the restore did not bring the sources back")
	}
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM memories WHERE content = ?`, f.unrelatedProjectMm.Content).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("the restore kept the replace's own project write")
	}
	if !rowExists(t, f.s, row.id) {
		t.Fatal("the restore removed the promoted row")
	}
	after := readPromotedRow(t, f.s, issueMergeText)
	if !sourceRefNamesDecision(after.sourceRef.String, f.decisionID) {
		t.Errorf("promoted source_ref = %q, want the link it carried", after.sourceRef.String)
	}
	if got := carriedCounts(t, f.s, row.id); got[f.comp] != 3 || got[f.save] != 2 {
		t.Errorf("promoted evidence = %v, want the carried copies kept", got)
	}
	if report := f.supersede(t); len(report.Retired) != 0 || len(report.Ambiguous) != 2 {
		t.Errorf("supersede report = %+v, want both carriers ambiguous", report)
	}
}

// A reflect of _global has nowhere to promote to: its cross-project candidates
// take the replace path in place, so the merge is written once, inherits its
// sources and is not deleted by the replace over the same project.
func TestApplyReflectionOnGlobalKeepsCrossProjectCandidatesInPlace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, text := range []string{"the bastion host fronts every ssh session", "ssh keys rotate on the first of the month"} {
		id, _, _, err := s.UpsertWithOptions(ctx, GlobalProjectID, "convention", text, "reflection", 0.5, nil, UpsertOptions{Retention: RetentionSession})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	const text = "every ssh session goes through the bastion, with keys rotated monthly"
	candidate := Memory{Category: "convention", Source: "reflection", Importance: 0.6, Content: text, ReplacesIDs: ids}
	if _, _, kept, _, dropped, _, _, err := s.ApplyReflection(ctx, GlobalProjectID, []Memory{reflectionMemory("fact", "kept")}, []Memory{candidate}, "", true, nil); err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	} else if len(kept) != 0 || dropped.Total() != 0 {
		t.Fatalf("kept=%d dropped=%+v, want the candidate written in place", len(kept), dropped)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memories WHERE content = ?`, text).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d rows hold the merge, want one", n)
	}
	row := readPromotedRow(t, s, text)
	if row.project != GlobalProjectID || row.retention != RetentionSession {
		t.Errorf("merge = %+v, want a session row in _global", row)
	}
	for _, id := range ids {
		if hist := successorsOf(t, s, id); len(hist) != 1 || hist[0] != row.id {
			t.Errorf("delete history of %s names %v, want %s", id, hist, row.id)
		}
	}
}

// The control: a candidate with no sources is promoted exactly as before — a
// project-tier reflection row with no reference, window or carried record.
func TestPromotingACandidateWithNoSourcesIsUnchanged(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, testProject, reflectionMemory("fact", "old project memory")); err != nil {
		t.Fatal(err)
	}
	const text = "prefer conventional commit subjects in every repository"
	_, promoted, kept, _, _, deleted, applied, err := s.ApplyReflection(ctx, testProject, []Memory{reflectionMemory("fact", "new project memory")}, []Memory{reflectionMemory("preference", text)}, "", true, nil)
	if err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	if promoted != 1 || len(kept) != 0 || deleted != 1 || applied["fact"] != 1 || applied["preference"] != 0 {
		t.Fatalf("promoted=%d kept=%d deleted=%d applied=%v", promoted, len(kept), deleted, applied)
	}
	row := readPromotedRow(t, s, text)
	if row.project != GlobalProjectID || row.source != "reflection" || row.retention != RetentionProject || row.expires.Valid {
		t.Errorf("row = %+v, want a project-tier reflection row in _global", row)
	}
	if row.sourceRef.Valid || row.validFrom.Valid || row.verifiedAt.Valid {
		t.Errorf("row = %+v, want no reference, window or verification", row)
	}
	if got := carriedCounts(t, s, row.id); len(got) != 1 || got[""] != 1 {
		t.Errorf("evidence = %v, want its own observation only", got)
	}
}

// The reason a reflect of _global takes the replace path: promoted inside the
// replace over _global, a merge whose text _global already holds on a row no
// emission claims would be folded into that row, and the same replace would
// then delete it with the merge inside.
func TestApplyReflectionOnGlobalDoesNotFoldAMergeIntoARowItDeletes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
		t.Fatal(err)
	}
	const text = "every ssh session goes through the bastion, with keys rotated monthly"
	var ids []string
	for _, c := range []string{"the bastion host fronts every ssh session", "ssh keys rotate on the first of the month"} {
		id, err := s.Create(ctx, GlobalProjectID, Memory{Category: "convention", Source: "reflection", Importance: 0.5, Content: c})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := s.Create(ctx, GlobalProjectID, Memory{Category: "convention", Source: "reflection", Importance: 0.5, Content: text}); err != nil {
		t.Fatal(err)
	}
	candidate := Memory{Category: "convention", Source: "reflection", Importance: 0.6, Content: text, ReplacesIDs: ids}
	if _, _, _, _, _, _, _, err := s.ApplyReflection(ctx, GlobalProjectID, []Memory{reflectionMemory("fact", "kept")}, []Memory{candidate}, "", true, nil); err != nil {
		t.Fatalf("ApplyReflection: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memories WHERE content = ?`, text).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d rows hold the merge, want one", n)
	}
}
