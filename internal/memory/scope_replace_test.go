package memory

import (
	"context"
	"path/filepath"
	"testing"
)

// TestReplaceNonManualCarriesScope is issue #572.
//
// Schema v12 added memories.scope, but the reflection replace's INSERT never
// named the column, so a consolidated memory came out of a replace with its
// scope silently NULL. A scope is the difference between "true in production"
// and "true anywhere", and reflection is exactly the pass that decides which
// knowledge generalises — so the one writer that most needs to record scope
// was the one dropping it.
func TestReplaceNonManualCarriesScope(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	scope := map[string]string{"environment": "production"}

	// A memory the replace will delete outright (nothing to reuse it onto).
	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "an old fact with no scope at all",
		Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create old: %v", err)
	}

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "a consolidated fact scoped to production", Importance: 0.6, Scope: scope},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("project holds %d memories, want 1", len(all))
	}
	if all[0].Scope["environment"] != "production" {
		t.Errorf("scope = %v, want environment=production — the replace INSERT never named the scope column", all[0].Scope)
	}
}

// TestReplaceNonManualRewritesScopeOnReusedRow covers the other branch. The
// reuse path updates a row in place, and its UPDATE does not mention scope —
// so the row keeps whatever scope it had before, which is only correct by
// accident, when the old and new scope happen to agree. When reflection
// narrows or widens a scope, the replace must record what it decided.
func TestReplaceNonManualRewritesScopeOnReusedRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	old, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "a fact that reflection will rewrite in place",
		Source: "reflection", Importance: 0.5, Scope: map[string]string{"environment": "staging"},
	})
	if err != nil {
		t.Fatalf("create old: %v", err)
	}

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "a fact that reflection will rewrite in place", Importance: 0.6,
			Scope: map[string]string{"environment": "production"}},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	rows, err := s.GetByIDs(ctx, []string{old})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs: %v", err)
	}
	if rows[0].Scope["environment"] != "production" {
		t.Errorf("scope = %v, want environment=production — the reuse UPDATE leaves the old scope behind", rows[0].Scope)
	}
}

// TestReplaceNonManualKeepsScopeWhenReflectionOmitsIt is the production shape
// of the reuse path. The only caller of ReplaceNonManual is `ghost reflect
// --apply`, and it builds its memory.Memory values from reflection.ReflectMemory
// — a contract that carries no machine-readable scope at all. So every emitted
// memory arrives with Scope nil, and the reuse UPDATE, which assigns
// scope = ?, wrote NULL over the live row's scope on every reflection that
// re-emitted a scoped fact verbatim. Silently: the text, the id, the embedding
// and the links all survive, so nothing looks wrong.
//
// The store cannot invent a scope the consolidator never produced, and scope
// has no "clear it" operation in the API, so the reuse path must treat a
// missing scope as "not stated" rather than "stated as nothing" — the same
// NULL-means-unstated rule scopeJSON already encodes for every other writer.
func TestReplaceNonManualKeepsScopeWhenReflectionOmitsIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "a scoped fact the consolidator re-emits verbatim",
		Source: "reflection", Importance: 0.5, Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// No Scope: exactly what cmd/ghost/lifecycle.go builds from a
	// reflection.ReflectMemory, which has no scope field.
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "a scoped fact the consolidator re-emits verbatim", Importance: 0.6},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	rows, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs: %v", err)
	}
	if rows[0].Scope["environment"] != "production" {
		t.Errorf("scope = %v, want environment=production — a reflection that stated no scope must not clear the live one",
			rows[0].Scope)
	}
}

// TestRestoreSnapshotKeepsLiveScopeForLegacySnapshot pins the v13-to-v14
// boundary. migrateV14 adds memory_snapshots.scope as a nullable column, and for
// a snapshot written before v14 that NULL means "this build never recorded it",
// not "the memory had no scope". Restore assigned scope = s.scope
// unconditionally, so rolling back to a v13-era snapshot cleared the scope of a
// live row that a later reflection had correctly re-scoped — a restore making
// the corpus less scoped than it was before the replace it is undoing.
//
// A legacy row is therefore marked explicitly as not carrying scope, and
// restore overwrites the live scope only when the snapshot is known to hold one.
func TestRestoreSnapshotKeepsLiveScopeForLegacySnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-snapshot.sqlite")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB seed: %v", err)
	}
	s := NewStore(db, nil)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, testProject, "/tmp/legacy", "legacy"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// The live row, re-scoped by a later reflection. A v13-era snapshot
	// recorded the memory id but had nowhere to record this scope.
	const content = "a fact scoped before v14 learned to snapshot scope"
	live, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: content, Source: "reflection", Importance: 0.5,
		Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create live row: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source, memory_id)
		VALUES ('legacy-1', ?, 'fact', ?, 0.5, 'reflection', ?)
	`, testProject, content, live); err != nil {
		t.Fatalf("insert legacy snapshot: %v", err)
	}

	// Roll the table back to its v13 shape and re-stamp, so migrateV14 runs
	// over a row that predates the column rather than over a fresh table.
	for _, ddl := range []string{
		`ALTER TABLE memory_snapshots DROP COLUMN scope_captured`,
		`ALTER TABLE memory_snapshots DROP COLUMN scope`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("roll back v14 snapshot columns (%s): %v", ddl, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 13`); err != nil {
		t.Fatalf("stamp v13: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}

	migrated, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB migrated: %v", err)
	}
	defer migrated.Close() //nolint:errcheck
	ms := NewStore(migrated, nil)

	var captured int
	if err := migrated.QueryRowContext(ctx,
		`SELECT scope_captured FROM memory_snapshots WHERE snapshot_id = 'legacy-1'`).Scan(&captured); err != nil {
		t.Fatalf("read migrated scope_captured: %v", err)
	}
	if captured != 0 {
		t.Errorf("migrated legacy snapshot scope_captured = %d, want 0 — a pre-v14 snapshot carries no scope value", captured)
	}

	if _, err := ms.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	rows, err := ms.GetByIDs(ctx, []string{live})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs: %v", err)
	}
	if rows[0].Scope["environment"] != "production" {
		t.Errorf("scope = %v, want environment=production — a legacy snapshot has no scope to restore and must not clear the live one",
			rows[0].Scope)
	}
}

// TestRestoreSnapshotBringsScopeBack is the half that made the first defect
// unrecoverable. memory_snapshots had no scope column at all, so the only undo
// history for a reflection replace could not carry the thing being restored.
// A restore that cannot bring scope back turns a lost scope into a permanent
// loss, which is why this is one issue and not two.
func TestRestoreSnapshotBringsScopeBack(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "a scoped fact that will be snapshotted and replaced away",
		Source: "reflection", Importance: 0.5, Scope: map[string]string{"environment": "production"},
	}); err != nil {
		t.Fatalf("create scoped: %v", err)
	}

	// ReplaceNonManual snapshots first, then destroys the row.
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "an unrelated consolidated fact", Importance: 0.6},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll after replace: %v", err)
	}
	for _, m := range all {
		if m.Content == "a scoped fact that will be snapshotted and replaced away" {
			t.Fatal("precondition: the scoped memory should have been replaced away")
		}
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	all, err = s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll after restore: %v", err)
	}
	var restored *Memory
	for i := range all {
		if all[i].Content == "a scoped fact that will be snapshotted and replaced away" {
			restored = &all[i]
		}
	}
	if restored == nil {
		t.Fatalf("restore did not bring the fact back: %+v", all)
	}
	if restored.Scope["environment"] != "production" {
		t.Errorf("restored scope = %v, want environment=production — memory_snapshots cannot record scope", restored.Scope)
	}
}

// TestRestoreSnapshotRewritesScopeOnExistingRow: restore updates rows that
// still exist by matching snapshot to memory id. That UPDATE has to carry
// scope too, or a restore into a row whose scope was edited in the meantime
// leaves the edit in place and restores nothing about scope.
func TestRestoreSnapshotRewritesScopeOnExistingRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "a fact whose scope will be edited after the snapshot",
		Source: "reflection", Importance: 0.5, Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "a fact whose scope will be edited after the snapshot", Importance: 0.6,
			Scope: map[string]string{"environment": "staging"}},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	rows, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs: %v", err)
	}
	if rows[0].Scope["environment"] != "staging" {
		t.Fatalf("precondition: scope = %v, want staging after the replace", rows[0].Scope)
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	rows, err = s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) == 0 {
		t.Fatalf("GetByIDs after restore: %v", err)
	}
	if rows[0].Scope["environment"] != "production" {
		t.Errorf("restored scope = %v, want environment=production — the restore UPDATE does not carry scope",
			rows[0].Scope)
	}
}
