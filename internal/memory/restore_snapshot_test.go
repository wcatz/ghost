package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// TestRestoreKeepsSavesMadeAfterSnapshot is the data-loss defect.
//
// RestoreSnapshot deleted every non-manual row in the project, without the
// consolidatedSince guard ReplaceNonManual has — so an `mcp` save written
// minutes after the snapshot was taken, which the snapshot never saw and
// cannot bring back, was permanently destroyed by a restore. A user saving a
// fact and then restoring would lose it silently, with no undo: the row is
// not in the snapshot, so it is simply gone.
func TestRestoreKeepsSavesMadeAfterSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "old reflection fact to be snapshotted",
		Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create old: %v", err)
	}

	// Takes a snapshot of the project, then replaces it.
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "new consolidated fact", Importance: 0.6},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	// Saved AFTER the snapshot: it is not in it and never can be.
	laterID, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "saved after the snapshot was taken",
		Source: "mcp", Importance: 0.7,
	})
	if err != nil {
		t.Fatalf("create post-snapshot: %v", err)
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	all, err := s.GetAll(ctx, testProject, 100)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	for _, m := range all {
		if m.ID == laterID {
			return // survived
		}
	}
	t.Errorf("a save made after the snapshot was destroyed by the restore; it is not in the snapshot, so it cannot be brought back (id %s)", laterID)
}

// TestRestorePreservesIdentityProvenanceAndCount: the snapshot did not carry
// the original id, created_at, access_count or v10 provenance, so every
// restored row came back as a brand-new memory. That reset decay, and because
// memory_embeddings and memory_links both reference memories(id) ON DELETE
// CASCADE, the delete-then-reinsert destroyed the embedding and the whole
// link graph around the row even though its text came back looking fine.
func TestRestorePreservesIdentityProvenanceAndCount(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	agent := "opencode"
	session := "sess-abc"
	ref := "abc123"
	conf := 0.42
	origID, err := s.Create(ctx, testProject, Memory{
		Category: "gotcha", Content: "a memory with history worth keeping",
		Source: "reflection", Importance: 0.5,
		Agent: agent, SessionID: session, SourceRef: ref, Confidence: &conf,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Give it decay history: access_count and last_accessed both feed decay,
	// and a restore that resets them makes an old memory look freshly written.
	if err := s.Touch(ctx, []string{origID}); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	if err := s.Touch(ctx, []string{origID}); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	before, err := s.GetByIDs(ctx, []string{origID})
	if err != nil || len(before) != 1 {
		t.Fatalf("GetByIDs before: err=%v n=%d", err, len(before))
	}
	orig := before[0]
	if orig.AccessCount < 2 {
		t.Fatalf("precondition: access_count = %d, want >= 2", orig.AccessCount)
	}

	// Emit different content so the original row is really deleted, not
	// reused in place — that is the path that lost identity.
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "gotcha", Content: "entirely different consolidated content", Importance: 0.6},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}
	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}

	restored, err := s.GetByIDs(ctx, []string{origID})
	if err != nil {
		t.Fatalf("GetByIDs after: %v", err)
	}
	if len(restored) != 1 {
		t.Fatalf("restored row is missing its original id %s — a fresh id silently severs embeddings and links (got %d rows)", origID, len(restored))
	}
	got := restored[0]

	if got.CreatedAt != orig.CreatedAt {
		t.Errorf("created_at = %s, want %s — a reset timestamp makes decay treat an old memory as new", got.CreatedAt, orig.CreatedAt)
	}
	if got.AccessCount != orig.AccessCount {
		t.Errorf("access_count = %d, want %d — decay history was discarded", got.AccessCount, orig.AccessCount)
	}
	if got.Agent != agent {
		t.Errorf("agent = %q, want %q — write-time provenance was not carried through the snapshot", got.Agent, agent)
	}
	if got.SessionID != session {
		t.Errorf("session_id = %q, want %q", got.SessionID, session)
	}
	if got.SourceRef != ref {
		t.Errorf("source_ref = %q, want %q", got.SourceRef, ref)
	}
	if got.Confidence == nil || *got.Confidence != conf {
		t.Errorf("confidence = %v, want %v", got.Confidence, conf)
	}
	if got.Content != orig.Content {
		t.Errorf("content = %q, want %q", got.Content, orig.Content)
	}
}

// TestRestoreDoesNotConsumeTheSnapshot: a restore used to delete the snapshot
// it read, so a second attempt failed with "no snapshots found" — and with
// only three kept, a handful of reflections spent the entire history. Because
// the restore is now expressed as update-in-place plus insert-what-is-missing,
// running it again yields the same result instead of a different one.
func TestRestoreDoesNotConsumeTheSnapshot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: "old reflection fact to be snapshotted",
		Source: "reflection", Importance: 0.5,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
		{Category: "fact", Content: "new consolidated fact", Importance: 0.6},
	}, ""); err != nil {
		t.Fatalf("ReplaceNonManual: %v", err)
	}

	first, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("first RestoreSnapshot: %v", err)
	}
	if first == 0 {
		t.Fatalf("first restore reported no work done")
	}
	state := func() string {
		t.Helper()
		all, err := s.GetAll(ctx, testProject, 100)
		if err != nil {
			t.Fatalf("GetAll: %v", err)
		}
		var b strings.Builder
		for _, m := range all {
			b.WriteString(m.ID)
			b.WriteString("|")
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
		return b.String()
	}
	afterFirst := state()

	second, err := s.RestoreSnapshot(ctx, testProject)
	if err != nil {
		t.Fatalf("second RestoreSnapshot failed — the restore consumed the snapshot it read: %v", err)
	}
	// The count is expected to differ: the first run removes the replace's
	// output and re-inserts the old row, the second only rewrites what is
	// already correct. What must not differ is the resulting corpus — that is
	// what "repeatable" has to mean, or a second run could silently drift.
	if got := state(); got != afterFirst {
		t.Errorf("second restore changed the corpus:\nafter first:  %s\nafter second: %s", afterFirst, got)
	}
	if second == 0 {
		t.Errorf("second restore reported no work; it should still find the row to confirm in place")
	}
}

// TestSnapshotRetentionIsWidened: three snapshots meant three reflections of
// history, and each restore burned one. Retention now keeps ten.
func TestSnapshotRetentionIsWidened(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const rounds = 5
	for i := 0; i < rounds; i++ {
		if _, err := s.Create(ctx, testProject, Memory{
			Category: "fact", Content: "round of reflection content",
			Source: "reflection", Importance: 0.5,
		}); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		if _, err := s.ReplaceNonManual(ctx, testProject, []Memory{
			{Category: "fact", Content: "consolidated content", Importance: 0.6},
		}, ""); err != nil {
			t.Fatalf("ReplaceNonManual %d: %v", i, err)
		}
	}

	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT snapshot_id) FROM memory_snapshots WHERE project_id = ?`,
		testProject,
	).Scan(&n); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if n != rounds {
		t.Errorf("distinct snapshots = %d, want %d — retention still trims history (or lost some)", n, rounds)
	}
}

// TestMigrateV13AddsSnapshotColumns: the widened columns must exist on both a
// fresh database and an upgraded one, and an already-captured snapshot must
// keep NULL memory_id rather than a fabricated one — the ids were never
// recorded, so inventing them would attach restored memories to whatever row
// happened to collide.
func TestMigrateV13AddsSnapshotColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE memory_snapshots DROP COLUMN memory_id`); err != nil {
		t.Skipf("this SQLite build cannot drop a column to simulate v12: %v", err)
	}
	seed := []string{
		`INSERT INTO projects (id, path, name) VALUES ('p1', '/tmp/a-very-long-v12-path', 'p1')`,
		`INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source)
		  VALUES ('p1-100', 'p1', 'fact', 'captured before v13', 0.5, 'reflection')`,
		`PRAGMA user_version = 12`,
	}
	for _, q := range seed {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	if err := migrate(db, 12); err != nil {
		t.Fatalf("migrate v12->v13: %v", err)
	}

	for _, col := range []string{"memory_id", "access_count", "last_accessed", "agent",
		"session_id", "source_ref", "confidence", "valid_from", "valid_until", "verified_at"} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pragma_table_info('memory_snapshots') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatalf("inspect %s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("memory_snapshots.%s missing after migrateV13", col)
		}
	}

	var memoryID sql.NullString
	var content string
	if err := db.QueryRow(`SELECT content, memory_id FROM memory_snapshots WHERE snapshot_id = 'p1-100'`).
		Scan(&content, &memoryID); err != nil {
		t.Fatalf("read migrated snapshot: %v", err)
	}
	if content != "captured before v13" {
		t.Errorf("content changed: %q", content)
	}
	if memoryID.Valid {
		t.Errorf("memory_id = %v, want NULL — the original id was never recorded, so it cannot be recovered", memoryID.String)
	}
}

// TestMigrateFreshDBHasSnapshotColumns is the fresh-database twin: initSQL
// must create the widened table directly, or a new install would depend on
// running migrations it never needs.
func TestMigrateFreshDBHasSnapshotColumns(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, col := range []string{"memory_id", "access_count", "agent", "confidence"} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM pragma_table_info('memory_snapshots') WHERE name = ?`, col,
		).Scan(&n); err != nil {
			t.Fatalf("inspect %s: %v", col, err)
		}
		if n != 1 {
			t.Errorf("memory_snapshots.%s missing on a fresh database", col)
		}
	}
}

// TestRestoreLegacySnapshotMatchesByContent covers snapshots written before
// schema v13, whose memory_id is NULL and cannot be recovered.
//
// They restore by content instead: that can only add a row that is missing
// and never overwrites a live one, so the property that matters is that a
// second restore does not duplicate what the first just wrote.
func TestRestoreLegacySnapshotMatchesByContent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A pre-v13 snapshot: everything recorded except the original id.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO memory_snapshots
		    (snapshot_id, project_id, category, content, importance, source, tags, memory_id)
		VALUES (?, ?, 'fact', 'legacy snapshot content', 0.5, 'reflection', '[]', NULL)
	`, testProject+"-legacy", testProject); err != nil {
		t.Fatalf("seed legacy snapshot: %v", err)
	}

	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("first restore: %v", err)
	}
	count := func() int {
		t.Helper()
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM memories WHERE project_id = ? AND content = ?`,
			testProject, "legacy snapshot content").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if count() != 1 {
		t.Fatalf("legacy snapshot content restored %d times, want 1", count())
	}

	// Restoring again must not duplicate it: with no id to key on, an
	// unconditional insert would stack a copy on every run.
	if _, err := s.RestoreSnapshot(ctx, testProject); err != nil {
		t.Fatalf("second restore: %v", err)
	}
	if n := count(); n != 1 {
		t.Errorf("after a second restore the legacy row appears %d times, want 1 — content match is not idempotent", n)
	}
}
