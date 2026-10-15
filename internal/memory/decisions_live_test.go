package memory

import (
	"context"
	"testing"
)

// TestLiveDecisionsShowsActiveBeforeRevisitAndExcludesSuperseded: the injected
// surfaces' read returns a project's non-superseded decisions (active or
// revisit) and orders active ahead of revisit, so a decision flagged for
// reconsideration is shown but never ahead of a live one. Superseded decisions
// stay out entirely.
func TestLiveDecisionsShowsActiveBeforeRevisitAndExcludesSuperseded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	record := func(title, status string) string {
		t.Helper()
		id, _, _, err := s.RecordDecision(ctx, testProject, title, "body "+title, "why", nil, nil)
		if err != nil {
			t.Fatalf("RecordDecision %q: %v", title, err)
		}
		if status != "active" {
			if _, err := s.db.Exec(`UPDATE decisions SET status = ? WHERE id = ?`, status, id); err != nil {
				t.Fatalf("set %q to %s: %v", title, status, err)
			}
		}
		return id
	}

	activeIDs := []string{record("active one", "active"), record("active two", "active"), record("active three", "active")}
	revisitID := record("revisit one", "revisit")
	supersededID := record("superseded one", "superseded")

	all, err := s.LiveDecisions(ctx, testProject, 10)
	if err != nil {
		t.Fatalf("LiveDecisions: %v", err)
	}
	index := map[string]int{}
	for i, d := range all {
		index[d.ID] = i
	}
	if _, ok := index[supersededID]; ok {
		t.Errorf("a superseded decision reached the live read: %v", index)
	}
	if _, ok := index[revisitID]; !ok {
		t.Errorf("a revisit decision is missing from the live read: %v", index)
	}
	for _, id := range activeIDs {
		i, ok := index[id]
		if !ok {
			t.Errorf("active decision %s is missing from the live read: %v", id, index)
			continue
		}
		if i > index[revisitID] {
			t.Errorf("active decision %s sorted below the revisit one: %v", id, index)
		}
	}

	// The window closes AFTER the active-first order: with exactly three active
	// decisions, a fourth-row revisit decision cannot take one of their slots.
	windowed, err := s.LiveDecisions(ctx, testProject, 3)
	if err != nil {
		t.Fatalf("LiveDecisions windowed: %v", err)
	}
	for _, d := range windowed {
		if d.ID == revisitID {
			t.Errorf("a revisit decision pushed an active one out of a full window: %v", windowed)
		}
	}
	if len(windowed) != len(activeIDs) {
		t.Errorf("expected the three active decisions in a three-row window, got %d", len(windowed))
	}
}
