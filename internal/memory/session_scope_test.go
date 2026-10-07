package memory

// The session scope of the retrieval audit's reads.
//
// A verdict is a claim "the agent's session did / did not use this memory", so a
// verdict is only meaningful against the one session whose text it was compared
// with. Two readers need that to hold in the store, where a caller cannot forget it:
// the read that hands a run its calls, and the reader that turns negative verdicts
// into evidence for resolve and reflect.

import (
	"context"
	"testing"
)

func recordSessionCall(t *testing.T, s *Store, project, session, memoryID string) {
	t.Helper()
	if err := s.RecordRetrieval(context.Background(), RetrievalRecord{
		ProjectID: project,
		SessionID: session,
		Source:    "search",
		Outcome:   "answerable",
		Verdicts:  []RowVerdict{{ID: memoryID, Kept: true, Stage: "fit", Reason: "fit_response"}},
	}); err != nil {
		t.Fatalf("RecordRetrieval(%q): %v", session, err)
	}
}

// TestRetrievalRecordsForSessionFiltersBeforeTheLimit: the session predicate has to be
// in the SQL. Filtered in Go after a project-wide LIMIT, newer calls from other
// sessions push this session's calls out of the window and the audit judges nothing.
func TestRetrievalRecordsForSessionFiltersBeforeTheLimit(t *testing.T) {
	store, _, ctx := auditStore(t) // seeds one call with NO session
	recordSessionCall(t, store, "p1", "sess-A", "A1")
	for i := 0; i < 5; i++ {
		recordSessionCall(t, store, "p1", "sess-B", "B1")
	}
	recordSessionCall(t, store, "p1", "", "N1")

	got, err := store.RetrievalRecordsForSession(ctx, "p1", "sess-A", 3)
	if err != nil {
		t.Fatalf("RetrievalRecordsForSession: %v", err)
	}
	if len(got) != 1 || got[0].SessionID != "sess-A" {
		t.Fatalf("got %d record(s) %+v, want exactly sess-A's one call even though 6 newer calls belong to others", len(got), got)
	}
}

// TestRetrievalRecordsForSessionNeverMatchesAnEmptySession: "" is what every legacy row
// and every host that cannot name its session holds, so matching it literally would
// hand the audit every unattributable call.
func TestRetrievalRecordsForSessionNeverMatchesAnEmptySession(t *testing.T) {
	store, _, ctx := auditStore(t)
	recordSessionCall(t, store, "p1", "", "N1")
	got, err := store.RetrievalRecordsForSession(ctx, "p1", "", 50)
	if err != nil {
		t.Fatalf("RetrievalRecordsForSession: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("an empty session matched %d unscoped call(s); a call with no session must be left unjudged, never guessed", len(got))
	}
}

// TestUsefulnessByMemorySkipsUnscopedVerdicts: before this change every verdict was
// filed with an empty session and judged against whichever session the stop hook had
// just scanned, so none of them says anything about the call it names. They stay in
// the table (nothing is deleted from a user's store) and stop being evidence.
func TestUsefulnessByMemorySkipsUnscopedVerdicts(t *testing.T) {
	s, dbPath := totalsStore(t)
	db := auditPlant(t, dbPath)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/tmp/usefulness-p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	usefulnessMemory(t, db, "p1", "M1", "2026-01-01 00:00:00")
	usefulnessMemory(t, db, "p1", "M2", "2026-01-01 00:00:00")
	usefulnessVerdict(t, db, "p1", "M1", VerdictOutcomeContradicted, "", "2026-09-24 10:00:00")
	usefulnessVerdict(t, db, "p1", "M2", VerdictOutcomeContradicted, "", "2026-09-24 10:00:00")
	usefulnessVerdict(t, db, "p1", "M2", VerdictOutcomeSuperseded, "ses_1", "2026-09-25 10:00:00")

	got, err := s.UsefulnessByMemory(ctx, "p1")
	if err != nil {
		t.Fatalf("UsefulnessByMemory: %v", err)
	}
	if ev, ok := got["M1"]; ok {
		t.Errorf("a verdict with no session is evidence %+v; it was judged against a session that is not its own", ev)
	}
	want := UsefulnessEvidence{SupersededInSession: 1, LastSession: "ses_1", LastAt: "2026-09-25 10:00:00"}
	if g := got["M2"]; g != want {
		t.Errorf("M2 evidence = %+v, want %+v (only the scoped verdict counts)", g, want)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM retrieval_audit WHERE session_id = ''`).Scan(&n); err != nil || n != 2 {
		t.Errorf("unscoped rows left in the table = %d (%v), want 2: the fix must never delete rows", n, err)
	}
}
