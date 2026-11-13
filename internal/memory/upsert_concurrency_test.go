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

// issue #560: Upsert probed for duplicates and read the fold target's importance
// outside the write transaction, so a concurrent save could be overwritten by
// stale arithmetic, and two saves of one fact became two unlinked rows. The tests
// drive the interleaving through SQLite's own write lock: a second handle holds an
// open write transaction so the save under test queues behind it.

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

// holdWriteLock opens a write transaction on handle (BEGIN IMMEDIATE) and returns
// it with the commit that releases the lock.
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

// requireBlocked fails when save finished while the write lock was still held, so
// the test cannot pass without the interleaving happening.
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
// issue #560: neither of two saves overwrites the other's importance.
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

	// 0.9 + 0.5*0.2 capped at 1.0; a stale read lands on 0.6.
	if got := memoryImportance(t, saving, id); math.Abs(got-1.0) > 1e-6 {
		t.Errorf("importance = %v, want 1.0: this save discarded the increment a concurrent save had already applied", got)
	}
}

// TestUpsertFoldsOntoAConcurrentlyCommittedRow is the duplicate half of issue
// #560: a save whose probe missed a freshly committed row must fold onto it
// instead of storing an unlinked second copy.
func TestUpsertFoldsOntoAConcurrentlyCommittedRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upsert-duplicate.sqlite")
	saving := upsertLockTestStore(t, dbPath)
	other := upsertLockTestStore(t, dbPath)

	ctx := context.Background()
	const content = "reflection reuses a row whose content the consolidator re-emitted unchanged"

	held, release := holdWriteLock(t, other)
	var otherID string
	if err := held.QueryRowContext(ctx, `
		INSERT INTO memories (project_id, category, content, source, importance, tags)
		VALUES (?, 'fact', ?, 'mcp', 0.5, '[]')
		RETURNING id
	`, testProject, content).Scan(&otherID); err != nil {
		t.Fatalf("concurrent insert: %v", err)
	}

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

	if imp := memoryImportance(t, saving, otherID); math.Abs(imp-0.6) > 1e-6 {
		t.Errorf("fold target importance = %v, want 0.6: the save did not strengthen the row it folded onto", imp)
	}
}
