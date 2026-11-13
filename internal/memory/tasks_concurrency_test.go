package memory

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Issue #1083: UpdateTask read the task row outside its write transaction and
// wrote every column back from that stale copy, so two handles editing different
// fields of one task lost one edit, and a task deleted between the read and the
// write produced a 0-row update that still reported success (CompleteTask did
// the same). The tests drive the interleaving through SQLite's own write lock,
// following upsert_concurrency_test.go: a third handle holds an open write
// transaction so the writers under test queue behind it.

// taskLockTestStore opens a handle on one file, standing in for a second ghost
// process holding it.
func taskLockTestStore(t *testing.T, dbPath string) *Store {
	t.Helper()
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB %s: %v", dbPath, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewStore(db, logger)
	if err := s.EnsureProject(context.Background(), testProject, "/tmp/task-lock", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return s
}

// requireTaskBlocked fails when the call finished while the write lock was
// still held, so the test cannot pass without the interleaving happening.
func requireTaskBlocked(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s returned while another writer held the write lock (err=%v)", what, err)
	case <-time.After(500 * time.Millisecond):
	}
}

// awaitTaskCalls collects the results of the writers released after the hold.
func awaitTaskCalls(t *testing.T, n int, done <-chan error) []error {
	t.Helper()
	errs := make([]error, 0, n)
	for len(errs) < n {
		select {
		case err := <-done:
			errs = append(errs, err)
		case <-time.After(60 * time.Second):
			t.Fatalf("only %d of %d task writers returned after the write lock was released", len(errs), n)
		}
	}
	return errs
}

// TestUpdateTaskKeepsAConcurrentEditToAnotherField is the lost-update half of
// #1083: one handle sets status=done and another sets priority=4 on the same
// task, and both edits must survive, including the completion stamp.
func TestUpdateTaskKeepsAConcurrentEditToAnotherField(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tasks-lost-update.sqlite")
	first := taskLockTestStore(t, dbPath)
	second := taskLockTestStore(t, dbPath)
	holder := taskLockTestStore(t, dbPath)

	ctx := context.Background()
	id, err := first.CreateTask(ctx, testProject, "Two editors, two fields", "", 2)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	_, release := holdWriteLock(t, holder)

	done := make(chan error, 2)
	go func() {
		status := "done"
		_, err := first.UpdateTask(ctx, id, &status, nil, nil)
		done <- err
	}()
	go func() {
		priority := 4
		_, err := second.UpdateTask(ctx, id, nil, &priority, nil)
		done <- err
	}()
	requireTaskBlocked(t, done, "an UpdateTask")
	requireTaskBlocked(t, done, "the other UpdateTask")
	release()
	for _, err := range awaitTaskCalls(t, 2, done) {
		if err != nil {
			t.Fatalf("task write: %v", err)
		}
	}

	got, err := first.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "done" || got.Priority != 4 || got.CompletedAt == "" {
		t.Errorf("lost update: status=%q priority=%d completed_at=%q, want done, 4 and a stamp", got.Status, got.Priority, got.CompletedAt)
	}
}

// deletedUnderWrite runs write against a task another handle deletes inside its
// still-open write transaction, and returns the write's error.
func deletedUnderWrite(t *testing.T, write func(s *Store, id string) error) error {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "tasks-deleted.sqlite")
	writing := taskLockTestStore(t, dbPath)
	holder := taskLockTestStore(t, dbPath)

	ctx := context.Background()
	id, err := writing.CreateTask(ctx, testProject, "Deleted under the caller", "", 2)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	held, release := holdWriteLock(t, holder)

	done := make(chan error, 1)
	go func() { done <- write(writing, id[:8]) }()
	requireTaskBlocked(t, done, "the task write")
	if _, err := held.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id); err != nil {
		t.Fatalf("delete inside the held transaction: %v", err)
	}
	release()
	return awaitTaskCalls(t, 1, done)[0]
}

// TestCompleteTaskReportsATaskDeletedUnderItsWrite is the RowsAffected half of
// #1083: an UPDATE that matched nothing must be an error, not a success.
func TestCompleteTaskReportsATaskDeletedUnderItsWrite(t *testing.T) {
	err := deletedUnderWrite(t, func(s *Store, id string) error {
		_, err := s.CompleteTask(context.Background(), id, "closed it")
		return err
	})
	if err == nil {
		t.Error("CompleteTask reported success for a task deleted between its read and its write")
	}
}

// TestUpdateTaskReportsATaskDeletedUnderItsWrite is the same half for the update.
func TestUpdateTaskReportsATaskDeletedUnderItsWrite(t *testing.T) {
	err := deletedUnderWrite(t, func(s *Store, id string) error {
		status := "done"
		_, err := s.UpdateTask(context.Background(), id, &status, nil, nil)
		return err
	})
	if err == nil {
		t.Error("UpdateTask reported success for a task deleted between its read and its write")
	}
}
