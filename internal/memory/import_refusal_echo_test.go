package memory

import (
	"context"
	"strings"
	"testing"
)

// TestImportDanglingReferenceRefusalsNeverEchoTheValue covers the two store-level
// refusals that quote a record's pointer to another record. The artifact importer
// clears a dangling pointer before it reaches the store, so these are reached by
// a direct caller; a token-shaped or oversized value must still not come back.
func TestImportDanglingReferenceRefusalsNeverEchoTheValue(t *testing.T) {
	ctx := context.Background()
	token := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
	for name, value := range map[string]string{"token": token, "long": strings.Repeat("Z", 300)} {
		t.Run("blocked_by/"+name, func(t *testing.T) {
			s := testStore(t)
			_, _, err := s.ImportTask(ctx, Task{ID: "t1", ProjectID: testProject, Title: "a task", Status: "pending", BlockedBy: value}, true)
			assertRefusalWithoutValue(t, err, "blocked_by", value)
		})
		t.Run("superseded_by/"+name, func(t *testing.T) {
			s := testStore(t)
			_, _, err := s.ImportDecision(ctx, Decision{ID: "d1", ProjectID: testProject, Title: "a decision", Decision: "do it", Rationale: "because", Status: "superseded", SupersededBy: value}, true)
			assertRefusalWithoutValue(t, err, "superseded_by", value)
		})
	}
}

func assertRefusalWithoutValue(t *testing.T, err error, field, value string) {
	t.Helper()
	if err == nil {
		t.Fatalf("the import was not refused")
	}
	if strings.Contains(err.Error(), value) || strings.Contains(err.Error(), value[:40]) {
		t.Errorf("the refusal echoes the value: %v", err)
	}
	if !strings.Contains(err.Error(), field) {
		t.Errorf("the refusal does not name %q: %v", field, err)
	}
}
