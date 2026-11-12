package memory

import (
	"context"
	"testing"
)

// TestDeleteProjectPurgesLegacyRetrievalAuditRows: retrieval_audit is no longer
// read or written, but a store an older build filled still holds rows that name
// a project and its memory ids. The table has no foreign key, so a project
// delete is the only thing that removes them; another project's rows stay.
func TestDeleteProjectPurgesLegacyRetrievalAuditRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const other = "other-project"
	if err := s.EnsureProject(ctx, other, t.TempDir(), other); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for _, p := range []string{testProject, other} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO retrieval_audit (project_id, source, memory_id, outcome) VALUES (?, 'injection', 'M1', 'used')`, p); err != nil {
			t.Fatalf("seed retrieval_audit: %v", err)
		}
	}
	count := func(p string) int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM retrieval_audit WHERE project_id = ?`, p).Scan(&n); err != nil {
			t.Fatalf("count retrieval_audit: %v", err)
		}
		return n
	}

	if _, err := s.DeleteProject(ctx, testProject, true); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if got := count(testProject); got != 0 {
		t.Errorf("retrieval_audit rows for the deleted project = %d, want 0", got)
	}
	if got := count(other); got != 1 {
		t.Errorf("retrieval_audit rows for another project = %d, want 1", got)
	}
}
