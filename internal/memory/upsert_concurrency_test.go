package memory

import (
	"context"
	"database/sql"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// issue #560: Upsert probed for duplicates and read the fold target's
// importance OUTSIDE the write transaction, then wrote a value derived from
// that read. Every other ghost process is a second writer (see
// TestConcurrentProcessesMixedReadWrite: OpenDB pins one connection per
// handle, so N handles is the real concurrency N), so between the probe and
// the write another save could strengthen the very row this save is about to
// strengthen — and this save then overwrote that increment with its own
// stale arithmetic. The same window turned two saves of one fact into two
// unlinked rows, because a probe that reads outside the write lock cannot see
// the row the other writer has not committed yet.
//
// Both tests below drive the interleaving through SQLite's own write lock
// instead of a timing race: the second handle holds an open write
// transaction, so a save started underneath it cannot proceed until the lock
// is released, and a probe that runs BEFORE the lock is taken sees a
// pre-transaction snapshot while one that runs after sees the committed row.

func upsertLockTestStore(t *testing.T, dbPath string) *Store {
	t.Helper()
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB %s: %v", dbPath, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewStore(db, logger)
	if err := s.EnsureProject(context.Background(), testProject, "/tmp/upsert-lock", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s
}

func memoryImportance(t *testing.T, s *Store, id string) float64 {
	t.Helper()
	var importance float64
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT importance FROM memories WHERE id = ?`, id).Scan(&importance); err != nil {
		t.Fatalf("read importance of %s: %v", id, err)
	}
	return importance
}

// holdWriteLock opens a write transaction on handle and returns it with the
// commit that releases the lock. The transaction has already taken SQLite's
// write lock (the DSN asks for BEGIN IMMEDIATE), so nothing else can write
// until it commits.
func holdWriteLock(t *testing.T, handle *Store) (*sql.Tx, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	tx, err := handle.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin write lock: %v", err)
	}
	return tx, func() {
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit write lock: %v", err)
		}
	}
}

// requireBlocked fails when save finished while the write lock was still held.
// Without this the test could pass for the wrong reason: a save that never
// reached the lock would run entirely after the commit, see the other
// writer's row, and look correct against a fix that changed nothing.
func requireBlocked(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("save completed while another writer held the write lock (err=%v): the interleaving under test never happened", err)
	case <-time.After(500 * time.Millisecond):
	}
}

func awaitSave(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Upsert did not return after the write lock was released")
	}
}

// TestUpsertStrengthenKeepsAnotherWritersIncrement is the lost-update half of
// issue #560: two saves of the same fact both count, and neither overwrites
// the other's importance.
func TestUpsertStrengthenKeepsAnotherWritersIncrement(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upsert-increment.sqlite")
	saving := upsertLockTestStore(t, dbPath)
	other := upsertLockTestStore(t, dbPath)

	ctx := context.Background()
	const content = "the embedding worker retires vectors whose model identity no longer matches"
	id, _, _, err := saving.Upsert(ctx, testProject, "fact", content, "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}
	if got := memoryImportance(t, saving, id); math.Abs(got-0.5) > 1e-6 {
		t.Fatalf("seed importance = %v, want 0.5", got)
	}

	// The second handle strengthens the same row inside an open write
	// transaction: the state a concurrent save leaves behind.
	held, release := holdWriteLock(t, other)
	if _, err := held.ExecContext(ctx, `UPDATE memories SET importance = 0.9 WHERE id = ?`, id); err != nil {
		t.Fatalf("concurrent strengthen: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, _, _, err := saving.Upsert(ctx, testProject, "fact", content, "mcp", 0.5, nil)
		done <- err
	}()
	requireBlocked(t, done)
	release()
	awaitSave(t, done)

	// 0.9 (what the other save left) + 0.5*0.2 (this save's increment),
	// capped at 1.0. A strengthen that writes back an importance read before
	// the lock was taken lands on 0.6 and the other save's increment is gone.
	if got := memoryImportance(t, saving, id); math.Abs(got-1.0) > 1e-6 {
		t.Errorf("importance = %v, want 1.0: this save discarded the increment a concurrent save had already applied", got)
	}
}

// TestUpsertFoldsOntoAConcurrentlyCommittedRow is the duplicate half of issue
// #560: a save whose probe missed a row another writer committed
// milliseconds earlier must fold onto it — strengthen it and link the new
// wording to it — instead of storing an unlinked second copy of the fact.
func TestUpsertFoldsOntoAConcurrentlyCommittedRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upsert-duplicate.sqlite")
	saving := upsertLockTestStore(t, dbPath)
	other := upsertLockTestStore(t, dbPath)

	ctx := context.Background()
	const content = "reflection reuses a row whose content the consolidator re-emitted unchanged"

	// The concurrent writer stores the fact inside an open write transaction.
	// A probe that reads outside the write lock cannot see this row; a probe
	// that reads inside the transaction can, once the other writer commits.
	held, release := holdWriteLock(t, other)
	var otherID string
	if err := held.QueryRowContext(ctx, `
		INSERT INTO memories (project_id, category, content, source, importance, tags)
		VALUES (?, 'fact', ?, 'mcp', 0.5, '[]')
		RETURNING id
	`, testProject, content).Scan(&otherID); err != nil {
		t.Fatalf("concurrent insert: %v", err)
	}

	// The outcome is written before the send and read after the receive, so
	// the channel orders the two.
	var outcome struct{ id, duplicateOf string }
	done := make(chan error, 1)
	go func() {
		id, duplicateOf, _, err := saving.Upsert(ctx, testProject, "fact", content, "mcp", 0.5, nil)
		outcome.id, outcome.duplicateOf = id, duplicateOf
		done <- err
	}()
	requireBlocked(t, done)
	release()
	awaitSave(t, done)

	if outcome.duplicateOf != otherID {
		t.Errorf("duplicateOf = %q, want the concurrently committed row %q: the save stored a second, unlinked copy of the fact", outcome.duplicateOf, otherID)
	}

	var links int
	if err := saving.db.QueryRowContext(ctx, `
		SELECT count(*) FROM memory_links
		WHERE source_id = ? AND target_id = ? AND relation = 'duplicate' AND invalidated_at IS NULL
	`, outcome.id, otherID).Scan(&links); err != nil {
		t.Fatalf("read duplicate link: %v", err)
	}
	if links != 1 {
		t.Errorf("duplicate links from %s to %s = %d, want 1", outcome.id, otherID, links)
	}

	// The fold still strengthens the target: the save is evidence the fact
	// keeps recurring, and the concurrent insert's 0.5 is what it must add to.
	if imp := memoryImportance(t, saving, otherID); math.Abs(imp-0.6) > 1e-6 {
		t.Errorf("fold target importance = %v, want 0.6: the save did not strengthen the row it folded onto", imp)
	}
}
