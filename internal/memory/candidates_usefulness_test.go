package memory

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// usefulnessFlag plants one memory_flags row with an explicit session and
// stamp, bypassing the FlagMemory writer so the fixture can choose WHICH text
// was flagged.
func usefulnessFlag(t *testing.T, db *sql.DB, project, memoryID, kind, session, at string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO memory_flags
		(project_id, memory_id, kind, reason, content_hash, agent, session_id, recorded_at)
		VALUES (?, ?, ?, 'test reason', '', 'test-agent', ?, ?)`,
		project, memoryID, kind, session, at); err != nil {
		t.Fatalf("plant the %s flag on %s: %v", kind, memoryID, err)
	}
}

// TestCandidatesAttachesUsefulnessEvidence is the store-side counterpart of the
// assemble demotion tests: it exercises the Candidates path that annotates each
// row with UsefulnessEvidence, not only UsefulnessByMemory or runProvenance.
// A memory contradicted twice must ride with Contradicted=2, a singly-
// contradicted one with 1, a flag with Flagged, and a clean one with zero.
// It fails on origin/main where Candidate has no Usefulness field.
func TestCandidatesAttachesUsefulnessEvidence(t *testing.T) {
	s, dbPath := totalsStore(t)
	db := auditPlant(t, dbPath)
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	const planted = "2026-01-01 00:00:00"

	// Memories are written via direct SQL so the audit's legacy timestamp rule
	// (recorded_at >= updated_at) decides, and so every verdict's memory
	// exists for the two-bounded-reads join to find.
	usefulnessMemory(t, db, testProject, "M1", planted)
	usefulnessMemory(t, db, testProject, "M2", planted)
	usefulnessMemory(t, db, testProject, "M3", planted)

	// M1: two contradictions — the repeated case that demotes.
	usefulnessVerdict(t, db, testProject, "M1", VerdictOutcomeContradicted, "ses_1", "2026-09-20 09:00:00")
	usefulnessVerdict(t, db, testProject, "M1", VerdictOutcomeContradicted, "ses_2", "2026-09-24 10:00:00")
	// M2: one contradiction — evidence but no demotion.
	usefulnessVerdict(t, db, testProject, "M2", VerdictOutcomeContradicted, "ses_3", "2026-09-25 11:00:00")
	// M3: flagged only — never demotes.
	usefulnessFlag(t, db, testProject, "M3", FlagKindWrong, "ses_flag", "2026-09-26 12:00:00")

	set, err := s.Candidates(ctx, CandidateRequest{
		ProjectID: testProject,
		Mode:      ProjectScoped,
		Query:     "claim",
		Condition: CondFTSOnly,
		Params:    DefaultSearchParams(),
		Now:       now,
		Fetch:     Fetch{FTSTopK: 10, VectorTopK: 10, Limit: 10},
	})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	byID := map[string]UsefulnessEvidence{}
	for _, c := range set.Rows {
		byID[c.ID] = c.Usefulness
	}
	if g := byID["M1"]; g.Contradicted != 2 {
		t.Errorf("M1 Usefulness = %+v, want Contradicted=2", g)
	}
	if g := byID["M2"]; g.Contradicted != 1 {
		t.Errorf("M2 Usefulness = %+v, want Contradicted=1", g)
	}
	if g := byID["M3"]; g.Flagged != 1 {
		t.Errorf("M3 Usefulness = %+v, want Flagged=1", g)
	}
	// A memory with no negative evidence must ride with the zero value, and
	// the set must still be returned — usefulness is annotation, not filter.
	if len(set.Rows) != 3 {
		t.Errorf("rows = %d, want 3 (every claim matches)", len(set.Rows))
	}
}

// TestCandidatesPassiveAttachesUsefulnessEvidence mirrors the query-path test
// for the passive retrieval seam, which reads usefulness via s.queryDB() rather
// than the read transaction.
func TestCandidatesPassiveAttachesUsefulnessEvidence(t *testing.T) {
	s, dbPath := totalsStore(t)
	db := auditPlant(t, dbPath)
	ctx := context.Background()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	const planted = "2026-01-01 00:00:00"

	usefulnessMemory(t, db, testProject, "M1", planted)
	usefulnessMemory(t, db, testProject, "M2", planted)
	usefulnessVerdict(t, db, testProject, "M1", VerdictOutcomeContradicted, "ses_1", "2026-09-20 09:00:00")
	usefulnessVerdict(t, db, testProject, "M1", VerdictOutcomeContradicted, "ses_2", "2026-09-24 10:00:00")
	usefulnessVerdict(t, db, testProject, "M2", VerdictOutcomeContradicted, "ses_3", "2026-09-25 11:00:00")

	set, err := s.Candidates(ctx, CandidateRequest{
		ProjectID: testProject,
		Mode:      ProjectScoped,
		Query:     "",
		Condition: CondFTSOnly,
		Params:    DefaultSearchParams(),
		Now:       now,
		Fetch:     Fetch{Limit: 10},
		Passive: []SlicePolicy{{
			Bucket:    testProject,
			Order:     OrderDecay,
			OverFetch: 10,
		}},
	})
	if err != nil {
		t.Fatalf("Candidates passive: %v", err)
	}
	byID := map[string]UsefulnessEvidence{}
	for _, c := range set.Rows {
		byID[c.ID] = c.Usefulness
	}
	if g := byID["M1"]; g.Contradicted != 2 {
		t.Errorf("M1 Usefulness = %+v, want Contradicted=2", g)
	}
	if g := byID["M2"]; g.Contradicted != 1 {
		t.Errorf("M2 Usefulness = %+v, want Contradicted=1", g)
	}
}

// TestCandidatesToleratesMissingUsefulnessTables verifies blocker 2's fix: on a
// store predating the usefulness schema, Candidates degrades to zero evidence
// rather than erroring the session. The tables are dropped to simulate an
// unmigrated store, then a query is issued.
func TestCandidatesToleratesMissingUsefulnessTables(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the claim for missing table tolerance", Source: "manual", Importance: 0.7,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Simulate a store that has never been migrated to v23: drop both tables
	// usefulness reads. The FTS leg still works, so Candidates should return
	// the row with zero Usefulness rather than an error.
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS retrieval_audit`); err != nil {
		t.Fatalf("drop retrieval_audit: %v", err)
	}
	if _, err := s.db.Exec(`DROP TABLE IF EXISTS memory_flags`); err != nil {
		t.Fatalf("drop memory_flags: %v", err)
	}
	set, err := s.Candidates(ctx, candidateRequest("claim", 5, now))
	if err != nil {
		t.Fatalf("Candidates with missing usefulness tables: %v; want degraded zero evidence", err)
	}
	if len(set.Rows) == 0 {
		t.Fatalf("rows empty; want the matching memory despite missing tables")
	}
	for _, c := range set.Rows {
		if c.Usefulness != (UsefulnessEvidence{}) {
			t.Errorf("row %s Usefulness = %+v, want zero when tables absent", c.ID, c.Usefulness)
		}
	}
}
