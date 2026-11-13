package memory

import (
	"context"
	"strings"
	"testing"
)

// A whitespace-only required field (task title; decision title, decision,
// rationale) is refused by the checks and by the importers, which store nothing.
func TestImportRefusesWhitespaceOnlyRequiredFields(t *testing.T) {
	task := func(title string) Task { return Task{ID: "t1", ProjectID: "p1", Title: title, Status: "pending"} }
	dec := func(title, decision, rationale string) Decision {
		return Decision{ID: "d1", ProjectID: "p1", Title: title, Decision: decision, Rationale: rationale, Status: "active"}
	}
	if err := CheckImportedTask(task("   ")); err == nil || !strings.Contains(err.Error(), "title") {
		t.Errorf("CheckImportedTask(whitespace title) = %v, want a refusal naming title", err)
	}
	for field, d := range map[string]Decision{
		"title":     dec("   ", "a decision", "a rationale"),
		"decision":  dec("a title", "\t\n", "a rationale"),
		"rationale": dec("a title", "a decision", "  \t  "),
	} {
		if err := CheckImportedDecision(d); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("CheckImportedDecision(whitespace %s) = %v, want a refusal naming it", field, err)
		}
	}

	s := portableTestStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if created, _, err := s.ImportTask(ctx, task("   "), true); err == nil || created {
		t.Errorf("ImportTask(whitespace title) = created %v, %v, want a refusal", created, err)
	}
	if _, err := s.GetTask(ctx, "t1"); err == nil {
		t.Error("whitespace-only task was stored")
	}
	if created, _, err := s.ImportDecision(ctx, dec("   ", "   ", "   "), true); err == nil || created {
		t.Errorf("ImportDecision(whitespace fields) = created %v, %v, want a refusal", created, err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM decisions WHERE id = 'd1'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("whitespace-only decision was stored: %d rows (%v)", n, err)
	}
}
