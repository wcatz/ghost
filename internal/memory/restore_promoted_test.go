package memory

import (
	"context"
	"testing"
)

// TestRestoreSnapshotSkipsPromotedMemory is #1044. A project restore must only
// touch memories the project still holds. The restore's in-place UPDATE and the
// changedIDs read both matched a snapshot row to a live memory by id alone, so a
// memory that had since been promoted to `_global` was rewritten back to its
// pre-promotion text by a project-scoped command — overwriting a row every
// session reads with a state the project no longer owns.
//
// The sibling row proves the filter is scoped rather than a disabled restore:
// a memory that stayed in the project is still reverted.
func TestRestoreSnapshotSkipsPromotedMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const stayText = "a fact that stays in the project"
	const moveText = "a fact that will be promoted"

	stayID, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: stayText, Source: "reflection", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create stay: %v", err)
	}
	moveID, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: moveText, Source: "reflection", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create move: %v", err)
	}

	// Snapshot both by re-emitting their text, which reuses each row in place.
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: stayText, Importance: 0.5},
		{Category: "fact", Content: moveText, Importance: 0.5},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	const stayEdited = "a local edit that a project restore should undo"
	stayEdit := stayEdited
	if err := s.UpdateMemory(ctx, testProject, stayID, &stayEdit, nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory stay: %v", err)
	}

	if err := s.PromoteToGlobal(ctx, testProject, moveID); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	const moveEdited = "a global edit a project restore must not undo"
	moveEdit := moveEdited
	if err := s.UpdateMemory(ctx, GlobalProjectID, moveID, &moveEdit, nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory promoted: %v", err)
	}

	n, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if n != 1 {
		t.Errorf("restore reported %d rows, want 1 — only the row the project still owns is restored", n)
	}

	rows, err := s.GetByIDs(ctx, []string{stayID, moveID})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	byID := make(map[string]Memory, len(rows))
	for _, m := range rows {
		byID[m.ID] = m
	}
	if got := byID[stayID].Content; got != stayText {
		t.Errorf("stay memory content = %q, want %q — a project restore must still revert a row the project owns", got, stayText)
	}
	if got := byID[moveID].Content; got != moveEdited {
		t.Errorf("promoted memory content = %q, want %q — a project restore must not rewrite a global memory", got, moveEdited)
	}
	if got := byID[moveID].ProjectID; got != GlobalProjectID {
		t.Errorf("promoted memory project = %q, want %q", got, GlobalProjectID)
	}

	// The history append is the half the issue names second, and it is a
	// separate read: changedIDs decides which rows the restore records. A
	// project restore that skipped the promoted row in the UPDATE but still
	// read it in changedIDs would append a restore-phase event to the global
	// memory — a changelog entry claiming the project reverted a row it never
	// touched, recording content the global row never held. The sibling proves
	// the record still happens for a row the project does own.
	if n := restorePhaseRows(t, s, moveID); n != 0 {
		t.Errorf("promoted memory has %d restore-phase history rows, want 0 — changedIDs must not claim a row the project no longer owns", n)
	}
	if n := restorePhaseRows(t, s, stayID); n != 1 {
		t.Errorf("stay memory has %d restore-phase history rows, want 1 — a row the project still owns is recorded", n)
	}
}

// restorePhaseRows counts the restore-phase history a memory carries, read from
// the table rather than through MemoryHistory so a retention cap cannot hide a
// row the assertion is about.
func restorePhaseRows(t *testing.T, s *Store, memoryID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history WHERE memory_id = ? AND phase = ?`,
		memoryID, phaseRestore,
	).Scan(&n); err != nil {
		t.Fatalf("count restore-phase history for %s: %v", memoryID, err)
	}
	return n
}
