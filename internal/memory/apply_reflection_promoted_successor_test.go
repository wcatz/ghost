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
