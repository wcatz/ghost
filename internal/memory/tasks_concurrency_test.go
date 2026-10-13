package memory

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Issue #1083: UpdateTask read the task row on the pool OUTSIDE its write
// transaction and wrote every mutable column back from that stale copy, so two
// handles editing different fields of one task lost one of the two edits and
// both were told the update succeeded. s.mu is per Store and closes nothing
// across processes (see TestConcurrentProcessesMixedReadWrite: OpenDB pins one
// connection per handle, so N handles is the real concurrency N), and two MCP
// servers on one database is a documented, supported mode.
//
// The same code discarded the UPDATE's RowsAffected, so a task deleted between
// the read and the write produced a 0-row update that still reported success —
// CompleteTask did the same.
//
// Both tests below drive the interleaving through SQLite's own write lock
// rather than a timing race, following the shape upsert_concurrency_test.go
// established for #560: a third handle holds an open write transaction, so a
// writer started underneath it cannot proceed until the lock is released, and a
// read that runs BEFORE the lock is taken sees a pre-transaction snapshot while
// one that runs after sees the committed row.

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
// still held. Without this the test could pass for the wrong reason: a writer
// that never reached the lock would run entirely after the commit, see the
// other writer's row, and look correct against a fix that changed nothing.
func requireTaskBlocked(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s returned while another writer held the write lock (err=%v): the interleaving under test never happened", what, err)
	case <-time.After(500 * time.Millisecond):
	}
}

// awaitTaskCall collects the results of the writers released after the hold.
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

func requireNoTaskErrors(t *testing.T, errs []error) {
	t.Helper()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("task write: %v", err)
		}
	}
}

// TestUpdateTaskKeepsAConcurrentEditToAnotherField is the lost-update half of
// #1083: one handle sets status=done and another sets priority=4 on the same
// task, and both edits must survive — including the completion stamp the status
// move writes, which the second merge used to clear.
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

	// The third handle holds an open write transaction, so both editors are
	// kept out until it releases.
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

	requireNoTaskErrors(t, awaitTaskCalls(t, 2, done))

	// Whichever order the two landed in, the second one merges onto the row the
	// first left behind. A merge taken from a copy read before the lock was
	// taken writes back the pre-commit status and priority, so one of the two
	// edits is gone and the completion stamp with it.
	got, err := first.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != "done" || got.Priority != 4 {
		t.Errorf("lost update: status=%q priority=%d, want status=done priority=4", got.Status, got.Priority)
	}
	if got.CompletedAt == "" {
		t.Error("completed_at is empty: the status move that set it was overwritten by a stale merge")
	}
}

// TestCompleteTaskReportsATaskDeletedUnderItsWrite is the RowsAffected half of
// #1083: the completion read the row, the row was then deleted, and the UPDATE
// that matched nothing must be an error rather than a success nobody can detect.
func TestCompleteTaskReportsATaskDeletedUnderItsWrite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tasks-complete-deleted.sqlite")
	completing := taskLockTestStore(t, dbPath)
	holder := taskLockTestStore(t, dbPath)

	ctx := context.Background()
	id, err := completing.CreateTask(ctx, testProject, "Deleted under the caller", "", 2)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	held, release := holdWriteLock(t, holder)

	done := make(chan error, 1)
	go func() { done <- completing.CompleteTask(ctx, id[:8], "closed it") }()
	requireTaskBlocked(t, done, "CompleteTask")

	// The row goes away inside the holder's own still-open transaction, and the
	// same commit that releases the write lock commits the delete, so the
	// caller's UPDATE lands on a row that no longer exists.
	if _, err := held.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id); err != nil {
		t.Fatalf("delete inside the held transaction: %v", err)
	}
	release()

	errs := awaitTaskCalls(t, 1, done)
	if errs[0] == nil {
		t.Error("CompleteTask reported success for a task deleted between its read and its write")
	}
}

// TestUpdateTaskReportsATaskDeletedUnderItsWrite is the same half for the
// update: the merge must not write a row it can no longer see and call it
// saved.
func TestUpdateTaskReportsATaskDeletedUnderItsWrite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "tasks-update-deleted.sqlite")
	updating := taskLockTestStore(t, dbPath)
	holder := taskLockTestStore(t, dbPath)

	ctx := context.Background()
	id, err := updating.CreateTask(ctx, testProject, "Deleted under the updater", "", 2)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	held, release := holdWriteLock(t, holder)

	done := make(chan error, 1)
	go func() {
		status := "done"
		_, err := updating.UpdateTask(ctx, id[:8], &status, nil, nil)
		done <- err
	}()
	requireTaskBlocked(t, done, "UpdateTask")

	if _, err := held.ExecContext(ctx, `DELETE FROM tasks WHERE id = ?`, id); err != nil {
		t.Fatalf("delete inside the held transaction: %v", err)
	}
	release()

	errs := awaitTaskCalls(t, 1, done)
	if errs[0] == nil {
		t.Error("UpdateTask reported success for a task deleted between its read and its write")
	}
}
