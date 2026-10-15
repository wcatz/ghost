package memory

import (
	"context"
	"strings"
	"testing"
)

// TestDecisionRevisitMarkerLabelsOnlyRevisit: three surfaces print this label
// and the docs promise all three print the same one, so the rule lives in one
// place — internal/memory, beside the status vocabulary it reads — and a surface
// that already knows its rows carry the label cannot tell this function what to
// render. Both renderers delegate to it, so a second copy cannot exist to drift,
// and this test pins the delegation's behaviour rather than each caller's copy.
func TestDecisionRevisitMarkerLabelsOnlyRevisit(t *testing.T) {
	for _, tc := range []struct {
		status string
		want   string
	}{
		{"revisit", " (revisit)"},
		{"active", ""},
		{"", ""},
		{"superseded", ""},
		{"nonsense", ""},
	} {
		if got := DecisionRevisitMarker(tc.status); got != tc.want {
			t.Errorf("DecisionRevisitMarker(%q) = %q, want %q", tc.status, got, tc.want)
		}
	}
	// The cap and order are the reason a marker exists at all: a live decision
	// that is not active is shown, and it must read as the one that needs
	// reconsidering rather than as an ordinary active row.
	if got := DecisionRevisitMarker("revisit"); !strings.Contains(got, "revisit") {
		t.Errorf("the marker never names the status it marks: %q", got)
	}
}

// TestLiveDecisionsShowsActiveBeforeRevisitAndExcludesSuperseded: the injected
// surfaces' read returns a project's non-superseded decisions (active or
// revisit) and orders active ahead of revisit, so a decision flagged for
// reconsideration is shown but never ahead of a live one. Superseded decisions
// stay out entirely.
//
// The stamps are explicit and the REVISIT row is the newest of the lot, because
// that is the only shape that makes the order discriminating: `RecordDecision`
// writes every created_at from datetime('now'), so un-stamped rows share a
// wall-clock second and an answer ordered by created_at alone would coincidentally
// agree with active-first. Making the revisit row newest means dropping the
// order term sorts it to the FRONT, where it displaces an active one.
func TestLiveDecisionsShowsActiveBeforeRevisitAndExcludesSuperseded(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	record := func(title, status, createdMod string) string {
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
		// Stamp rather than leave the column default: the newest row is the
		// revisit one, so its POSITION is what the order term decides.
		if _, err := s.db.Exec(`UPDATE decisions SET created_at = datetime('now', ?) WHERE id = ?`, createdMod, id); err != nil {
			t.Fatalf("stamp created_at on %q: %v", title, err)
		}
		return id
	}

	activeIDs := []string{
		record("active one", "active", "-1 day"),
		record("active two", "active", "-1 day"),
		record("active three", "active", "-1 day"),
	}
	// The revisit row is the NEWEST row in the project, so only the active-first
	// order keeps it out of a window the active rows should fill.
	revisitID := record("revisit one", "revisit", "-1 minute")
	supersededID := record("superseded one", "superseded", "-1 day")

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
