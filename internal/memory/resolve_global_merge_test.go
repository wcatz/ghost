package memory

import (
	"context"
	"strings"
	"testing"
)

// TestMergeProjectTxRefusesGlobal pins the _global invariant inside the shared
// merge primitive rather than in one wrapper.
//
// MergeProject refuses to touch _global, but migrateV14 and the
// unique-constraint recovery in ensureProjectLocked call mergeProjectTx
// directly. A valid v13 database can carry a repo_remote on _global —
// EnsureProjectWithRepo has always written the supplied remote for that ID — so
// both of those paths can be handed a merge that would move every project
// memory into the global bucket, or delete _global outright. A guard that only
// the public wrapper applies is a guard the other two callers do not have.
func TestMergeProjectTxRefusesGlobal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("ensure _global: %v", err)
	}
	if err := s.EnsureProject(ctx, "regular", "/tmp/regular", "regular"); err != nil {
		t.Fatalf("ensure regular: %v", err)
	}
	// One memory in the regular project, so a merge that ran would visibly move
	// it rather than only deleting a row.
	if _, err := s.Create(ctx, "regular", Memory{Category: "fact", Content: "project knowledge", Source: "manual"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	for _, tc := range []struct{ name, oldID, newID string }{
		{"global as the old side", "_global", "regular"},
		{"global as the new side", "regular", "_global"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// mergeProjectTx directly, not through MergeProject: the point of
			// this test is that the guard lives in the shared primitive, so a
			// test that only exercises a wrapper with its own check would pass
			// whether or not the primitive is guarded at all.
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin tx: %v", err)
			}
			defer tx.Rollback() //nolint:errcheck

			err = mergeProjectTx(ctx, tx, tc.oldID, tc.newID)
			if err == nil {
				t.Fatal("mergeProjectTx succeeded, want a refusal naming _global")
			}
			if !strings.Contains(err.Error(), "_global") {
				t.Errorf("error = %v, want it to name _global so the refusal is diagnosable", err)
			}
		})
	}

	// The refusal is transactional: nothing moved, and _global still exists.
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memories WHERE project_id = 'regular'`).Scan(&n); err != nil {
		t.Fatalf("count regular memories: %v", err)
	}
	if n != 1 {
		t.Errorf("regular project holds %d memories, want 1 — a refused merge moved data", n)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM projects WHERE id = '_global'`).Scan(&n); err != nil {
		t.Fatalf("count _global: %v", err)
	}
	if n != 1 {
		t.Error("_global was deleted by a refused merge")
	}
}

// TestMergeProjectRefusesGlobalThroughWrapper checks the public surface still
// refuses now that MergeProject inherits the guard instead of repeating it.
func TestMergeProjectRefusesGlobalThroughWrapper(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("ensure _global: %v", err)
	}
	if err := s.EnsureProject(ctx, "regular", "/tmp/regular", "regular"); err != nil {
		t.Fatalf("ensure regular: %v", err)
	}
	if err := s.MergeProject(ctx, "regular", "_global"); err == nil {
		t.Fatal("MergeProject into _global succeeded, want a refusal")
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM projects WHERE id = '_global'`).Scan(&n); err != nil {
		t.Fatalf("count _global: %v", err)
	}
	if n != 1 {
		t.Error("MergeProject consumed _global")
	}
}
