package memory

import (
	"context"
	"fmt"
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

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "a consolidated fact scoped to production", Importance: 0.6, Scope: scope},
	}, "", nil); err != nil {
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

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "a fact that reflection will rewrite in place", Importance: 0.6,
			Scope: map[string]string{"environment": "production"}},
	}, "", nil); err != nil {
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
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "a scoped fact the consolidator re-emits verbatim", Importance: 0.6},
	}, "", nil); err != nil {
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

// TestRestoreSnapshotSkipsUnverifiedScopeOnInsert is the other half of the
// legacy-snapshot boundary: the row is missing rather than present, so restore
// reinserts it instead of updating in place.
//
// migrateV14 leaves scope_captured at 0 for a snapshot that predates the column
// and for one an operator backfilled by hand, and calls both unverified. The
// UPDATE path already refuses to believe such a scope; if the INSERT path copied
// it anyway, a hand-backfilled "environment=production" would be restored onto a
// memory that may never have had that scope, with no provenance afterwards to
// tell a restored fact from an invented one. A row restored from an unverified
// snapshot comes back unscoped instead.
func TestRestoreSnapshotSkipsUnverifiedScopeOnInsert(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const content = "a fact whose snapshot scope was never actually recorded"
	id, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: content, Source: "reflection", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "an unrelated consolidated fact", Importance: 0.6},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	for _, m := range all {
		if m.Content == content {
			t.Fatal("precondition: the fact should have been replaced away")
		}
	}

	// Stand in for a hand-migrated snapshot: a scope value present, but with
	// nothing asserting it was ever recorded. migrateV14 stamps exactly this
	// shape with scope_captured = 0.
	if _, err := s.db.ExecContext(ctx, `
		UPDATE memory_snapshots SET scope = ?, scope_captured = 0 WHERE content = ?
	`, `{"environment":"production"}`, content); err != nil {
		t.Fatalf("mark snapshot unverified: %v", err)
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	rows, err := s.GetByIDs(ctx, []string{id})
	if err != nil || len(rows) == 0 {
		t.Fatalf("restore did not bring the fact back: %v", err)
	}
	if len(rows[0].Scope) != 0 {
		t.Errorf("scope = %v, want unscoped — an unverified snapshot value must not be restored as fact", rows[0].Scope)
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
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "an unrelated consolidated fact", Importance: 0.6},
	}, "", nil); err != nil {
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

// TestReplaceNonManualRewriteInheritsScope verifies that a single-source
// rewrite inherits the source's scope. This is the fix for #1391: the fresh
// INSERT branch of ReplaceNonManual must read the source rows' scope rather
// than using the emission's (which is nil from reflection).
func TestReplaceNonManualRewriteInheritsScope(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create a source memory with a specific scope.
	srcID, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "production db port is 5432",
		Source:     "mcp",
		Importance: 0.5,
		Scope:      map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}

	// Rewrite it (single source). The emission has NO scope (as from reflection).
	// The successor should inherit the source's scope.
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{
			Category:    "fact",
			Content:     "production db port is 5433 (rewritten)",
			Importance:  0.6,
			ReplacesIDs: []string{srcID},
			// Scope is nil - exactly what reflection produces
		},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	var rewritten *Memory
	for i := range all {
		if all[i].Content == "production db port is 5433 (rewritten)" {
			rewritten = &all[i]
			break
		}
	}
	if rewritten == nil {
		t.Fatalf("rewritten memory not found: %+v", all)
	}
	if rewritten.Scope["environment"] != "production" {
		t.Errorf("rewrite scope = %v, want environment=production — single-source rewrite must inherit source scope", rewritten.Scope)
	}
}

// TestReplaceNonManualMergeInheritsScope verifies that a merge inherits the
// scope when all sources agree. This is the fix for #1391.
func TestReplaceNonManualMergeInheritsScope(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create two source memories with the SAME scope.
	src1, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "prod port is 5432",
		Source:     "mcp",
		Importance: 0.5,
		Scope:      map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create src1: %v", err)
	}
	src2, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "staging port is 8443",
		Source:     "mcp",
		Importance: 0.5,
		Scope:      map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create src2: %v", err)
	}

	// Merge them. The emission has NO scope.
	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{
			Category:    "fact",
			Content:     "ports: prod 5432; staging 8443",
			Importance:  0.6,
			ReplacesIDs: []string{src1, src2},
			// Scope is nil
		},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	var merged *Memory
	for i := range all {
		if all[i].Content == "ports: prod 5432; staging 8443" {
			merged = &all[i]
			break
		}
	}
	if merged == nil {
		t.Fatalf("merged memory not found: %+v", all)
	}
	if merged.Scope["environment"] != "production" {
		t.Errorf("merge scope = %v, want environment=production — merge with agreeing sources must inherit scope", merged.Scope)
	}
}

// TestReplaceNonManualMergeDisagreeingScope verifies that a merge with
// disagreeing scopes does NOT silently widen the scope. Per #1391, the
// expected behavior is that the merge is not performed (sources stay unmerged)
// or only shared keys are kept. The current implementation will drop the
// emission as a scope refusal (not a corpus-guard drop) when sources disagree
// on scope.
func TestReplaceNonManualMergeDisagreeingScope(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create two source memories with DIFFERENT scopes.
	src1, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "prod port is 5432",
		Source:     "mcp",
		Importance: 0.5,
		Scope:      map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create src1: %v", err)
	}
	src2, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "staging port is 8443",
		Source:     "mcp",
		Importance: 0.5,
		Scope:      map[string]string{"environment": "staging"},
	})
	if err != nil {
		t.Fatalf("create src2: %v", err)
	}

	// Merge them - the emission has NO scope.
	// The replace should refuse this emission because the sources
	// have conflicting scopes and we can't silently widen.
	var dropped EmissionDrops
	if _, dropped, _, _, err = s.ReplaceNonManual(ctx, testProject, []Memory{
		{
			Category:    "fact",
			Content:     "ports: prod 5432; staging 8443",
			Importance:  0.6,
			ReplacesIDs: []string{src1, src2},
			// Scope is nil
		},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	// The emission should be dropped due to scope conflict
	// A scope refusal is counted under its own reason, not as a concurrent edit
	// (#1492): nothing moved, and every later run would refuse it again.
	if dropped.Scope != 1 || dropped.Changed != 0 {
		t.Errorf("dropped = %+v, want the emission counted as one scope refusal and no concurrent-edit drop", dropped)
	}

	// Both sources should still exist
	rows, err := s.GetByIDs(ctx, []string{src1, src2})
	if err != nil || len(rows) != 2 {
		t.Fatalf("GetByIDs: %v, len=%d", err, len(rows))
	}
	foundProd, foundStaging := false, false
	for _, r := range rows {
		if r.Content == "prod port is 5432" {
			foundProd = true
		}
		if r.Content == "staging port is 8443" {
			foundStaging = true
		}
	}
	if !foundProd || !foundStaging {
		t.Errorf("sources were modified: %+v", rows)
	}
}

// TestReplaceNonManualMergeMixedScopeOrderAB verifies that a merge with
// one unscoped and one scoped source is dropped regardless of order.
// This is the fix for the order-dependency bug: sources [unscoped A, scoped B]
// in order [A,B] used to write the merge with B's scope; now it drops in
// both orders.
func TestReplaceNonManualMergeMixedScopeOrderAB(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create an unscoped source and a scoped source.
	unscoped, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "unscoped fact",
		Source:     "mcp",
		Importance: 0.5,
		// No scope = applies everywhere
	})
	if err != nil {
		t.Fatalf("create unscoped: %v", err)
	}
	scoped, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "production fact",
		Source:     "mcp",
		Importance: 0.5,
		Scope:      map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create scoped: %v", err)
	}

	// Order: unscoped first, then scoped (used to succeed with scoped scope)
	var dropped EmissionDrops
	if _, dropped, _, _, err = s.ReplaceNonManual(ctx, testProject, []Memory{
		{
			Category:    "fact",
			Content:     "merged fact",
			Importance:  0.6,
			ReplacesIDs: []string{unscoped, scoped},
			// Scope is nil
		},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	if dropped.Scope != 1 || dropped.Changed != 0 {
		t.Errorf("dropped = %+v, want the mixed-scope emission (order AB) counted as one scope refusal and no concurrent-edit drop", dropped)
	}

	// Both sources should still exist
	rows, err := s.GetByIDs(ctx, []string{unscoped, scoped})
	if err != nil || len(rows) != 2 {
		t.Fatalf("GetByIDs: %v, len=%d", err, len(rows))
	}
}

// TestReplaceNonManualMergeMixedScopeOrderBA verifies that a merge with
// one unscoped and one scoped source is dropped regardless of order.
// This is the fix for the order-dependency bug: sources [scoped B, unscoped A]
// in order [B,A] used to drop; now both orders drop consistently.
func TestReplaceNonManualMergeMixedScopeOrderBA(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create a scoped source and an unscoped source.
	scoped, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "production fact",
		Source:     "mcp",
		Importance: 0.5,
		Scope:      map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create scoped: %v", err)
	}
	unscoped, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "unscoped fact",
		Source:     "mcp",
		Importance: 0.5,
		// No scope = applies everywhere
	})
	if err != nil {
		t.Fatalf("create unscoped: %v", err)
	}

	// Order: scoped first, then unscoped
	var dropped EmissionDrops
	if _, dropped, _, _, err = s.ReplaceNonManual(ctx, testProject, []Memory{
		{
			Category:    "fact",
			Content:     "merged fact",
			Importance:  0.6,
			ReplacesIDs: []string{scoped, unscoped},
			// Scope is nil
		},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	if dropped.Scope != 1 || dropped.Changed != 0 {
		t.Errorf("dropped = %+v, want the mixed-scope emission (order BA) counted as one scope refusal and no concurrent-edit drop", dropped)
	}

	// Both sources should still exist
	rows, err := s.GetByIDs(ctx, []string{scoped, unscoped})
	if err != nil || len(rows) != 2 {
		t.Fatalf("GetByIDs: %v, len=%d", err, len(rows))
	}
}

// TestReplaceNonManualReuseDisagreeingScope verifies that a multi-source
// emission that would reuse an existing row is also dropped when sources
// disagree on scope. The reuse branch now applies the same "never silently
// change scope" rule: if the emission's sources have conflicting scopes,
// the emission is dropped and both sources remain unchanged.
func TestReplaceNonManualReuseDisagreeingScope(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Create a source that will be reused (byte-identical content).
	reuseSrc, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "reused content",
		Source:     "mcp",
		Importance: 0.5,
		Scope:      map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create reuseSrc: %v", err)
	}

	// Create another source with different scope.
	otherSrc, err := s.Create(ctx, testProject, Memory{
		Category:   "fact",
		Content:    "other fact",
		Source:     "mcp",
		Importance: 0.5,
		Scope:      map[string]string{"environment": "staging"},
	})
	if err != nil {
		t.Fatalf("create otherSrc: %v", err)
	}

	// Try to merge them into the reused content (byte-identical to reuseSrc).
	// This would take the reuse branch but sources disagree on scope.
	var dropped EmissionDrops
	if _, dropped, _, _, err = s.ReplaceNonManual(ctx, testProject, []Memory{
		{
			Category:    "fact",
			Content:     "reused content", // byte-identical to reuseSrc
			Importance:  0.6,              // different importance
			ReplacesIDs: []string{reuseSrc, otherSrc},
			// Scope is nil
		},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	// The emission should be dropped due to scope conflict
	if dropped.Scope != 1 || dropped.Changed != 0 {
		t.Errorf("dropped = %+v, want the reuse-branch refusal counted as one scope refusal and no concurrent-edit drop", dropped)
	}

	// Both sources should still exist unchanged
	rows, err := s.GetByIDs(ctx, []string{reuseSrc, otherSrc})
	if err != nil || len(rows) != 2 {
		t.Fatalf("GetByIDs: %v, len=%d", err, len(rows))
	}
	foundReuse, foundOther := false, false
	for _, r := range rows {
		if r.Content == "reused content" {
			foundReuse = true
			if r.Importance != 0.5 {
				t.Errorf("reuseSrc importance changed: got %f, want 0.5", r.Importance)
			}
			if r.Scope["environment"] != "production" {
				t.Errorf("reuseSrc scope changed: got %v, want production", r.Scope)
			}
		}
		if r.Content == "other fact" {
			foundOther = true
			if r.Scope["environment"] != "staging" {
				t.Errorf("otherSrc scope changed: got %v, want staging", r.Scope)
			}
		}
	}
	if !foundReuse || !foundOther {
		t.Errorf("sources were modified or missing: %+v", rows)
	}
}

// TestReplaceNonManualScopeReadErrorAbortsReplace: a scope column that will
// not decode is a READ failure, not a scope conflict. A conflict is a refusal
// the caller can act on — the emission is dropped and its sources kept — but a
// broken value has no such meaning, and counting it as a drop would make one
// corrupt row a silent refusal that recurs on every later run under a reason
// that describes a concurrent edit. The replace must fail the way
// inheritedClaims and inheritedRetention do.
func TestReplaceNonManualScopeReadErrorAbortsReplace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	src, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "a source whose scope column will be corrupted",
		Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create src: %v", err)
	}

	// A value json.Unmarshal rejects, as a hand-edited or half-written row
	// would hold.
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET scope = ? WHERE id = ?`, "{not json", src); err != nil {
		t.Fatalf("corrupt scope: %v", err)
	}

	_, _, _, _, err = s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "a merged successor", Importance: 0.6, ReplacesIDs: []string{src}},
	}, "", nil)
	if err == nil {
		t.Fatal("ReplaceNonManual returned nil for a corrupt scope column — a read failure must fail the replace, not count as a dropped emission")
	}

	// The source is still there: the failed replace rolled back.
	rows, err := s.GetByIDs(ctx, []string{src})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs after failed replace: %v, len=%d", err, len(rows))
	}
}

// TestReplaceNonManualReuseAgreesOnScope is the positive half of the reuse
// check: when the reused row's scope matches the one the emission's sources
// agreed on, the reuse proceeds and the merge lands.
func TestReplaceNonManualReuseAgreesOnScope(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	reused, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the shared fact", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create reused: %v", err)
	}
	src1, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "production fact one", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create src1: %v", err)
	}
	src2, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "production fact two", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create src2: %v", err)
	}

	var dropped EmissionDrops
	if _, dropped, _, _, err = s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "the shared fact", Importance: 0.6, ReplacesIDs: []string{src1, src2}},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if dropped.Total() != 0 {
		t.Fatalf("dropped = %+v, want no refusal when the reused row agrees on scope", dropped)
	}

	rows, err := s.GetByIDs(ctx, []string{reused})
	if err != nil || len(rows) != 1 {
		t.Fatalf("GetByIDs: %v, len=%d", err, len(rows))
	}
	if rows[0].Scope["environment"] != "production" {
		t.Errorf("reused row scope = %v, want production", rows[0].Scope)
	}
	// The two sources were merged into the reused row and removed.
	if _, err := s.GetByIDs(ctx, []string{src1, src2}); err != nil {
		t.Fatalf("GetByIDs sources: %v", err)
	}
}

// TestReplaceNonManualReuseDisagreesOnScope is the reuse-branch counterpart of
// the fresh-INSERT scope rule. takeReusableRow picks the row an emission
// updates in place by CONTENT, and it need not be one of the emission's
// sources: a merge can land on a same-text row consolidation never named. The
// reuse UPDATE writes scope through COALESCE, so the reused row would keep ITS
// OWN scope — here unscoped — while the sources agreed on production, silently
// widening a production fact to every environment. The emission must be
// refused and both rows left exactly as they are.
func TestReplaceNonManualReuseDisagreesOnScope(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// The row the merge will reuse: same text as the emission, and unscoped.
	reused, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the shared fact", Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create reused: %v", err)
	}
	src1, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "production fact one", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create src1: %v", err)
	}
	src2, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "production fact two", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create src2: %v", err)
	}

	var dropped EmissionDrops
	if _, dropped, _, _, err = s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "the shared fact", Importance: 0.6, ReplacesIDs: []string{src1, src2}},
	}, "", nil); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if dropped.Scope != 1 || dropped.Changed != 0 {
		t.Errorf("dropped = %+v, want the reuse emission refused over the scope disagreement, counted as a scope refusal", dropped)
	}

	rows, err := s.GetByIDs(ctx, []string{reused, src1, src2})
	if err != nil || len(rows) != 3 {
		t.Fatalf("GetByIDs: %v, len=%d, want all three rows kept", err, len(rows))
	}
	for _, r := range rows {
		switch r.ID {
		case reused:
			if len(r.Scope) != 0 {
				t.Errorf("reused row scope = %v, want unscoped — the refused merge wrote over it", r.Scope)
			}
			if r.Importance != 0.5 {
				t.Errorf("reused row importance = %v, want 0.5 unchanged", r.Importance)
			}
		case src1, src2:
			if r.Scope["environment"] != "production" {
				t.Errorf("source %s scope = %v, want production unchanged", r.ID, r.Scope)
			}
		}
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

	if _, _, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "a fact whose scope will be edited after the snapshot", Importance: 0.6,
			Scope: map[string]string{"environment": "staging"}},
	}, "", nil); err != nil {
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

// TestReplaceNonManualScopeRefusalIsNotASurvivor is #1492 at the store. A merge
// whose sources' scopes disagree is refused and its sources kept, and that is a
// refusal of its own kind: it is counted in EmissionDrops.Scope rather than in
// Changed, the corpus guard's bucket, and the sources it kept are not reported
// as preserved survivors. Either one would make `ghost reflect` withhold the skip
// fingerprint for a refusal that every later run makes again.
func TestReplaceNonManualScopeRefusalIsNotASurvivor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	prod, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the deploy needs a manual approval gate", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create prod: %v", err)
	}
	staging, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the deploy needs no manual approval gate", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "staging"},
	})
	if err != nil {
		t.Fatalf("create staging: %v", err)
	}
	other, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the consumer restarts in place", Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	setAgesDaysAgo(t, s, prod, 2)
	setAgesDaysAgo(t, s, staging, 2)
	setAgesDaysAgo(t, s, other, 2)
	// The corpus the pass read, unchanged since: nothing moved under this round.
	fed := corpusRows(t, s, prod, staging, other)

	preserved, dropped, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "the deploy approval gate differs by environment", Importance: 0.6, ReplacesIDs: []string{prod, staging}},
		{Category: "fact", Content: "the consumer restarts in place", Importance: 0.5},
	}, "", fed)
	if err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if dropped.Scope != 1 || dropped.Changed != 0 || dropped.Overflow != 0 {
		t.Errorf("dropped = %+v, want exactly one scope refusal: no row changed underneath the pass", dropped)
	}
	if len(preserved) != 0 {
		t.Errorf("preserved = %v, want empty: the rows a scope refusal keeps are not survivors a later run can merge", preserved)
	}
	rows, err := s.GetByIDs(ctx, []string{prod, staging})
	if err != nil || len(rows) != 2 {
		t.Fatalf("GetByIDs: %v, len=%d, want both sources kept", err, len(rows))
	}
}

// TestReplaceNonManualScopeRefusalBesideAMovedRow is the control: a row that
// really changed while the pass ran is still the corpus guard's, counted in
// Changed and reported as preserved, in the same round as a scope refusal.
// Only the rows the scope refusal alone kept leave the preserved list.
func TestReplaceNonManualScopeRefusalBesideAMovedRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	prod, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the deploy needs a manual approval gate", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create prod: %v", err)
	}
	staging, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the deploy needs no manual approval gate", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "staging"},
	})
	if err != nil {
		t.Fatalf("create staging: %v", err)
	}
	edited, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the consumer restarts in place", Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("create edited: %v", err)
	}
	setAgesDaysAgo(t, s, prod, 2)
	setAgesDaysAgo(t, s, staging, 2)
	setAgesDaysAgo(t, s, edited, 2)
	fed := corpusRows(t, s, prod, staging, edited)

	// The edit lands while the harness is answering.
	moved := "the consumer restarts in place after a config change"
	if err := s.UpdateMemory(ctx, testProject, edited, &moved, nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}

	preserved, dropped, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "the deploy approval gate differs by environment", Importance: 0.6, ReplacesIDs: []string{prod, staging}},
		{Category: "fact", Content: "the consumer restarts in place", Importance: 0.5},
	}, "", fed)
	if err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if dropped.Scope != 1 || dropped.Changed != 1 {
		t.Errorf("dropped = %+v, want one scope refusal and one concurrent-edit drop", dropped)
	}
	if len(preserved) != 1 || preserved[0] != edited {
		t.Errorf("preserved = %v, want only the edited row %s: it changed underneath the pass and needs the next run", preserved, edited)
	}
}

// TestFoldSourceScopesIsTheReplaceRule pins the exported fold to the refusal
// the replace makes, case for case: the reflect dry run predicts a scope
// refusal with FoldSourceScopes, so a rule that drifted from inheritedScope's
// would preview a merge the apply refuses, or the reverse. Each case runs
// through both — the pure fold over the scopes, and ReplaceNonManual over rows
// stored with them.
func TestFoldSourceScopesIsTheReplaceRule(t *testing.T) {
	prod := map[string]string{"environment": "production"}
	staging := map[string]string{"environment": "staging"}
	cases := []struct {
		name    string
		scopes  []map[string]string
		refused bool
	}{
		{"agreeing scopes", []map[string]string{prod, prod}, false},
		{"disagreeing scopes", []map[string]string{prod, staging}, true},
		{"scoped then unscoped", []map[string]string{prod, nil}, true},
		{"unscoped then scoped", []map[string]string{nil, prod}, true},
		{"no scope anywhere", []map[string]string{nil, nil}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			var ids []string
			var sources []SourceScope
			for i, sc := range tc.scopes {
				id, err := s.Create(ctx, testProject, Memory{
					Category: "fact", Content: fmt.Sprintf("source fact number %d", i), Source: "mcp", Importance: 0.5, Scope: sc,
				})
				if err != nil {
					t.Fatalf("create: %v", err)
				}
				ids = append(ids, id)
				sources = append(sources, SourceScope{ID: id, Scope: sc})
			}
			_, foldErr := FoldSourceScopes(sources)
			if got := IsScopeConflict(foldErr); got != tc.refused {
				t.Errorf("FoldSourceScopes conflict = %v (%v), want %v", got, foldErr, tc.refused)
			}
			_, dropped, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
				{Category: "fact", Content: "the merged fact", Importance: 0.6, ReplacesIDs: ids},
			}, "", nil)
			if err != nil {
				t.Fatalf("ReplaceNonManual: %v", err)
			}
			if got := dropped.Scope == 1; got != tc.refused {
				t.Errorf("replace scope refusal = %v (%+v), want %v: the fold and the write disagree", got, dropped, tc.refused)
			}
		})
	}
}

// reflectHistoryFor counts the history rows the reflect phase wrote for id.
func reflectHistoryFor(t *testing.T, s *Store, id string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_history WHERE memory_id = ? AND phase = 'reflect'`, id).Scan(&n); err != nil {
		t.Fatalf("count history: %v", err)
	}
	return n
}

// TestReplaceNonManualReAddOfAScopeKeptRowIsAKeep is the guard re-add after a
// scope refusal. The merge over X and Y is refused and both are kept; the drop
// guard also handed X back verbatim, because the merge text did not carry it.
// The kept row left the reuse map, so that re-add used to be INSERTed as a fresh
// row with no scope — a copy of a production-only fact that applied in every
// environment, and, once the refusal stopped holding the project open, one a
// recorded fingerprint would freeze in place. The re-add must land on X as a
// keep: no insert, no write to X, and nothing reported as preserved.
//
// Both shapes of the source pair are covered: two disagreeing scopes, and a
// scoped row beside an unscoped one.
func TestReplaceNonManualReAddOfAScopeKeptRowIsAKeep(t *testing.T) {
	prod := map[string]string{"environment": "production"}
	for _, tc := range []struct {
		name  string
		other map[string]string
	}{
		{"disagreeing scopes", map[string]string{"environment": "staging"}},
		{"scoped beside unscoped", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			x, err := s.Create(ctx, testProject, Memory{
				Category: "fact", Content: "the deploy needs a manual approval gate", Source: "mcp", Importance: 0.5,
				Tags: []string{"deploy"}, Scope: prod,
			})
			if err != nil {
				t.Fatalf("create x: %v", err)
			}
			y, err := s.Create(ctx, testProject, Memory{
				Category: "fact", Content: "the deploy approval gate is skipped", Source: "mcp", Importance: 0.5,
				Tags: []string{"deploy"}, Scope: tc.other,
			})
			if err != nil {
				t.Fatalf("create y: %v", err)
			}
			setAgesDaysAgo(t, s, x, 2)
			setAgesDaysAgo(t, s, y, 2)
			fed := corpusRows(t, s, x, y)
			historyBefore := reflectHistoryFor(t, s, x)

			preserved, dropped, deleted, applied, err := s.ReplaceNonManual(ctx, testProject, []Memory{
				{Category: "fact", Content: "the approval gate depends on the environment", Importance: 0.6, ReplacesIDs: []string{x, y}},
				// RetainGuardedDrops' shape: the stored row's own fields, no sources.
				{Category: "fact", Content: "the deploy needs a manual approval gate", Importance: 0.5, Tags: []string{"deploy"}},
			}, "", fed)
			if err != nil {
				t.Fatalf("ReplaceNonManual: %v", err)
			}
			if dropped.Scope != 1 || dropped.Changed != 0 {
				t.Errorf("dropped = %+v, want one scope refusal and no concurrent-edit drop", dropped)
			}
			if len(preserved) != 0 || deleted != 0 {
				t.Errorf("preserved = %v, deleted = %d, want neither: nothing moved and nothing was removed", preserved, deleted)
			}
			if applied["fact"] != 1 {
				t.Errorf("applied = %v, want the re-add counted as the one keep that landed", applied)
			}
			all, err := s.GetAll(ctx, testProject, -1)
			if err != nil {
				t.Fatalf("GetAll: %v", err)
			}
			if len(all) != 2 {
				t.Fatalf("the project holds %d rows, want exactly X and Y — the re-add inserted a copy: %+v", len(all), all)
			}
			for _, m := range all {
				switch m.ID {
				case x:
					if !ScopeEquals(m.Scope, prod) {
						t.Errorf("X scope = %v, want %v", m.Scope, prod)
					}
				case y:
					if !ScopeEquals(m.Scope, tc.other) {
						t.Errorf("Y scope = %v, want %v", m.Scope, tc.other)
					}
				default:
					t.Errorf("unexpected row %s (scope %v): %q", m.ID, m.Scope, m.Content)
				}
			}
			if got := reflectHistoryFor(t, s, x); got != historyBefore {
				t.Errorf("X has %d reflect history rows, want %d: a keep writes nothing", got, historyBefore)
			}
		})
	}
}

// TestReplaceNonManualReAddOfAMovedRowIsStillRefused is the control: when the
// row the re-add restates really changed while the pass ran, it is the corpus
// guard's drop as before, counted in Changed, and still no copy is inserted.
func TestReplaceNonManualReAddOfAMovedRowIsStillRefused(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	x, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the deploy needs a manual approval gate", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "production"},
	})
	if err != nil {
		t.Fatalf("create x: %v", err)
	}
	y, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "the deploy approval gate is skipped", Source: "mcp", Importance: 0.5,
		Scope: map[string]string{"environment": "staging"},
	})
	if err != nil {
		t.Fatalf("create y: %v", err)
	}
	setAgesDaysAgo(t, s, x, 2)
	setAgesDaysAgo(t, s, y, 2)
	fed := corpusRows(t, s, x, y)
	moved := float32(0.9)
	if err := s.UpdateMemory(ctx, testProject, x, nil, nil, &moved, nil); err != nil {
		t.Fatalf("edit x: %v", err)
	}

	_, dropped, _, _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "the approval gate depends on the environment", Importance: 0.6, ReplacesIDs: []string{x, y}},
		{Category: "fact", Content: "the deploy needs a manual approval gate", Importance: 0.5},
	}, "", fed)
	if err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if dropped.Changed != 2 {
		t.Errorf("dropped = %+v, want both emissions refused as claims on the moved row", dropped)
	}
	if n := countRows(t, s, "memories", testProject); n != 2 {
		t.Errorf("the project holds %d rows, want X and Y only", n)
	}
}
