package memory

import (
	"context"
	"testing"
)

// TestRestoreSnapshotSkipsPromotedMemory is #1044.
func TestRestoreSnapshotSkipsPromotedMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const stayText = "a fact that stays in the project"
	const moveText = "a fact that will be promoted"

	stayID, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: stayText, Source: "reflection", Importance: 0.5})
	if err != nil {
		t.Fatalf("create stay: %v", err)
	}
	moveID, err := s.Create(ctx, testProject, Memory{Category: "fact", Content: moveText, Source: "reflection", Importance: 0.5})
	if err != nil {
		t.Fatalf("create move: %v", err)
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: stayText, Importance: 0.5},
		{Category: "fact", Content: moveText, Importance: 0.5},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	stayEdit := "a local edit that a project restore should undo"
	if err := s.UpdateMemory(ctx, testProject, stayID, &stayEdit, nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory stay: %v", err)
	}
	if err := s.PromoteToGlobal(ctx, testProject, moveID); err != nil {
		t.Fatalf("PromoteToGlobal: %v", err)
	}
	moveEdit := "a global edit a project restore must not undo"
	if err := s.UpdateMemory(ctx, GlobalProjectID, moveID, &moveEdit, nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory promoted: %v", err)
	}

	res, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	if res.Restored != 1 {
		t.Errorf("restore reported %d rows, want 1", res.Restored)
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
		t.Errorf("stay content = %q, want %q", got, stayText)
	}
	if got := byID[moveID].Content; got != moveEdit {
		t.Errorf("promoted content = %q, want %q", got, moveEdit)
	}
	if got := byID[moveID].ProjectID; got != GlobalProjectID {
		t.Errorf("promoted project = %q, want %q", got, GlobalProjectID)
	}
	if n := restorePhaseRows(t, s, moveID); n != 0 {
		t.Errorf("promoted restore history = %d, want 0", n)
	}
	if n := restorePhaseRows(t, s, stayID); n != 1 {
		t.Errorf("stay restore history = %d, want 1", n)
	}
}

func restorePhaseRows(t *testing.T, s *Store, memoryID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_history WHERE memory_id = ? AND phase = ?`, memoryID, phaseRestore).Scan(&n); err != nil {
		t.Fatalf("count restore-phase history for %s: %v", memoryID, err)
	}
	return n
}
