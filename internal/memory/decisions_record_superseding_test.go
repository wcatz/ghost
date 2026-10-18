package memory

import (
	"context"
	"errors"
	"testing"
)

// decisionTableCounts is every table RecordDecisionSuperseding writes, so a
// call that wrote nothing leaves each count where it was.
func decisionTableCounts(t *testing.T, s *Store) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, table := range []string{"decisions", "memories", "memory_history", "memory_provenance"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

func decisionStatus(t *testing.T, s *Store, id string) (status, supersededBy string) {
	t.Helper()
	var by *string
	if err := s.db.QueryRow(`SELECT status, superseded_by FROM decisions WHERE id = ?`, id).Scan(&status, &by); err != nil {
		t.Fatalf("read decision %s: %v", id, err)
	}
	if by != nil {
		supersededBy = *by
	}
	return status, supersededBy
}

// TestRecordDecisionSupersedingCancelledAfterTheInsert cancels the request in
// the window the two separate writes left open: after the new decision and its
// companion are written and before the decision they replace is marked. The
// whole call rolls back, so the store is as it was and A is still the live
// decision; the retry is then the only write, with exactly one live B.
func TestRecordDecisionSupersedingCancelledAfterTheInsert(t *testing.T) {
	s := testStore(t)
	a, aMemory, _, err := s.RecordDecision(context.Background(), testProject, "storage", "Use A", "first", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision (A): %v", err)
	}
	before := decisionTableCounts(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fired := false
	s.afterDecisionInsert = func() { fired = true; cancel() }
	if _, err := s.RecordDecisionSuperseding(ctx, testProject, "storage", "Use B", "A failed", nil, nil, a); err == nil {
		t.Fatal("RecordDecisionSuperseding returned no error on a context cancelled mid-write")
	}
	s.afterDecisionInsert = nil
	if !fired {
		t.Fatal("the hook did not run: the cancel never landed between the insert and the supersede")
	}

	after := decisionTableCounts(t, s)
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s = %d after the cancelled call, want %d: part of the call committed", table, after[table], n)
		}
	}
	if status, _ := decisionStatus(t, s, a); status != "active" {
		t.Errorf("A is %s after the cancelled call, want active", status)
	}

	rec, err := s.RecordDecisionSuperseding(context.Background(), testProject, "storage", "Use B", "A failed", nil, nil, a)
	if err != nil || rec.SupersedeErr != nil {
		t.Fatalf("retry: err %v, supersede err %v", err, rec.SupersedeErr)
	}
	var live int
	if err := s.db.QueryRow(`SELECT count(*) FROM decisions WHERE decision = 'Use B' AND status != 'superseded'`).Scan(&live); err != nil {
		t.Fatalf("count live B: %v", err)
	}
	if live != 1 {
		t.Errorf("live 'Use B' decisions after the retry = %d, want 1", live)
	}
	if status, by := decisionStatus(t, s, a); status != "superseded" || by != rec.DecisionID {
		t.Errorf("A is %s superseded_by %q after the retry, want superseded by %s", status, by, rec.DecisionID)
	}
	if !decisionMemoryResolved(t, s, aMemory) {
		t.Errorf("A's companion %s is still live after the retry", aMemory)
	}
}

// TestRecordDecisionSupersedingRecordsAndSupersedesTogether is the control:
// with nothing cancelled, one call writes the new decision, marks the old one
// and retires its companion, and reports what it retired.
func TestRecordDecisionSupersedingRecordsAndSupersedesTogether(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, aMemory, _, err := s.RecordDecision(ctx, testProject, "storage", "Use A", "first", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision (A): %v", err)
	}
	rec, err := s.RecordDecisionSuperseding(ctx, testProject, "storage", "Use B", "A failed", nil, []string{"db"}, a)
	if err != nil || rec.SupersedeErr != nil {
		t.Fatalf("RecordDecisionSuperseding: err %v, supersede err %v", err, rec.SupersedeErr)
	}
	if rec.DecisionID == "" || rec.MemoryID == "" {
		t.Fatalf("missing ids: %+v", rec)
	}
	if status, by := decisionStatus(t, s, a); status != "superseded" || by != rec.DecisionID {
		t.Errorf("A is %s superseded_by %q, want superseded by %s", status, by, rec.DecisionID)
	}
	if status, _ := decisionStatus(t, s, rec.DecisionID); status != "active" {
		t.Errorf("B is %s, want active", status)
	}
	if len(rec.Retirement.Retired) != 1 || rec.Retirement.Retired[0] != aMemory {
		t.Errorf("Retired = %v, want [%s]", rec.Retirement.Retired, aMemory)
	}
	if decisionMemoryResolved(t, s, rec.MemoryID) {
		t.Errorf("B's own companion %s was retired", rec.MemoryID)
	}
}

// TestRecordDecisionSupersedingRefusalKeepsTheNewDecision is the documented
// partial failure (#1041, #1144): a supersede the store refuses for what the
// rows say leaves the new decision committed and the old one untouched, and
// the refusal comes back in SupersedeErr rather than as the call's error.
func TestRecordDecisionSupersedingRefusalKeepsTheNewDecision(t *testing.T) {
	t.Run("unknown id", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		rec, err := s.RecordDecisionSuperseding(ctx, testProject, "storage", "Use B", "A failed", nil, nil, "NOSUCHDECISION")
		if err != nil {
			t.Fatalf("RecordDecisionSuperseding: %v", err)
		}
		if rec.SupersedeErr == nil {
			t.Fatal("an unknown supersedes id was not reported")
		}
		if status, _ := decisionStatus(t, s, rec.DecisionID); status != "active" {
			t.Errorf("the new decision is %s, want active and committed", status)
		}
	})
	t.Run("live successor", func(t *testing.T) {
		s := testStore(t)
		ctx := context.Background()
		a, _, _, err := s.RecordDecision(ctx, testProject, "storage", "Use A", "first", nil, nil)
		if err != nil {
			t.Fatalf("RecordDecision (A): %v", err)
		}
		b, err := s.RecordDecisionSuperseding(ctx, testProject, "storage", "Use B", "A failed", nil, nil, a)
		if err != nil || b.SupersedeErr != nil {
			t.Fatalf("B: err %v, supersede err %v", err, b.SupersedeErr)
		}
		c, err := s.RecordDecisionSuperseding(ctx, testProject, "storage", "Use C", "B failed", nil, nil, a)
		if err != nil {
			t.Fatalf("C: %v", err)
		}
		var successorErr *DecisionSuccessorError
		if !errors.As(c.SupersedeErr, &successorErr) || successorErr.Successor != b.DecisionID {
			t.Fatalf("SupersedeErr = %v, want a DecisionSuccessorError naming %s", c.SupersedeErr, b.DecisionID)
		}
		if status, by := decisionStatus(t, s, a); status != "superseded" || by != b.DecisionID {
			t.Errorf("A is %s superseded_by %q, want still superseded by %s", status, by, b.DecisionID)
		}
		if status, _ := decisionStatus(t, s, c.DecisionID); status != "active" {
			t.Errorf("C is %s, want active and committed", status)
		}
	})
}

// TestRecordDecisionSupersedingFailureRollsBackBothHalves makes the supersede
// fail on a database error after it has marked the old decision: a trigger
// refuses the companion's retirement. That is a failed write, not a refusal,
// so nothing commits — not the new decision, and not the old one's mark —
// and the call returns the error. A cancel cannot tell the two apart, because
// a commit on a cancelled context fails on its own.
func TestRecordDecisionSupersedingFailureRollsBackBothHalves(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, _, _, err := s.RecordDecision(ctx, testProject, "storage", "Use A", "first", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision (A): %v", err)
	}
	if _, err := s.db.Exec(`
		CREATE TRIGGER fail_companion_retire BEFORE UPDATE OF resolved_at ON memories
		BEGIN SELECT RAISE(ABORT, 'retire refused by test'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	before := decisionTableCounts(t, s)

	rec, err := s.RecordDecisionSuperseding(ctx, testProject, "storage", "Use B", "A failed", nil, nil, a)
	if err == nil {
		t.Fatalf("a failed retirement returned no error: %+v", rec)
	}
	after := decisionTableCounts(t, s)
	for table, n := range before {
		if after[table] != n {
			t.Errorf("%s = %d after the failed call, want %d: part of the call committed", table, after[table], n)
		}
	}
	if status, by := decisionStatus(t, s, a); status != "active" || by != "" {
		t.Errorf("A is %s superseded_by %q after the failed call, want active and unlinked", status, by)
	}
}
