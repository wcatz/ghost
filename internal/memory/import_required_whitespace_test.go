package memory

import (
	"context"
	"strings"
	"testing"
)

func TestCheckImportedTaskRefusesWhitespaceOnlyTitle(t *testing.T) {
	err := CheckImportedTask(Task{
		ID:        "t1",
		ProjectID: "p1",
		Title:     "   ",
		Status:    "pending",
	})
	if err == nil {
		t.Fatal("CheckImportedTask accepted a whitespace-only title")
	}
	if !strings.Contains(err.Error(), "title") {
		t.Errorf("refusal does not name title: %v", err)
	}
}

func TestCheckImportedDecisionRefusesWhitespaceOnlyRequiredFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Decision
		want string
	}{
		{
			name: "title",
			d: Decision{
				ID: "d1", ProjectID: "p1", Title: "   ",
				Decision: "a decision", Rationale: "a rationale", Status: "active",
			},
			want: "title",
		},
		{
			name: "decision",
			d: Decision{
				ID: "d1", ProjectID: "p1", Title: "a title",
				Decision: "\t\n", Rationale: "a rationale", Status: "active",
			},
			want: "decision",
		},
		{
			name: "rationale",
			d: Decision{
				ID: "d1", ProjectID: "p1", Title: "a title",
				Decision: "a decision", Rationale: "  \t  ", Status: "active",
			},
			want: "rationale",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckImportedDecision(tc.d)
			if err == nil {
				t.Fatalf("CheckImportedDecision accepted a whitespace-only %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal does not name %s: %v", tc.want, err)
			}
		})
	}
}

func TestImportTaskRefusesWhitespaceOnlyTitle(t *testing.T) {
	s := portableTestStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	created, _, err := s.ImportTask(ctx, Task{
		ID:        "t-ws",
		ProjectID: "p1",
		Title:     "   ",
		Status:    "pending",
	}, true)
	if err == nil {
		t.Fatal("ImportTask accepted a whitespace-only title")
	}
	if created {
		t.Error("ImportTask reported created for a refused task")
	}

	if _, getErr := s.GetTask(ctx, "t-ws"); getErr == nil {
		t.Error("whitespace-only task was stored")
	}
}

func TestImportDecisionRefusesWhitespaceOnlyRequiredFields(t *testing.T) {
	s := portableTestStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	created, _, err := s.ImportDecision(ctx, Decision{
		ID:        "d-ws",
		ProjectID: "p1",
		Title:     "   ",
		Decision:  "   ",
		Rationale: "   ",
		Status:    "active",
	}, true)
	if err == nil {
		t.Fatal("ImportDecision accepted whitespace-only required fields")
	}
	if created {
		t.Error("ImportDecision reported created for a refused decision")
	}

	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM decisions WHERE id = 'd-ws'`).Scan(&n); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	if n != 0 {
		t.Errorf("whitespace-only decision was stored: %d rows", n)
	}
}
