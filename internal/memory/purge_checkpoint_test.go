package memory

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAPurgeReportsAnIncompleteCheckpointWhenAReaderHoldsTheLog: the other half
// of the redaction promise. wal_checkpoint(TRUNCATE) does not raise an error
// when it cannot finish — it returns (busy, log, checkpointed) — so a purge that
// discarded the result row reported success while the text stayed in the
// write-ahead log. A second handle holding a read snapshot from before the purge
// is what makes the checkpoint unable to fold or truncate the frames the purge
// writes, and the purge must say so rather than claim the erase is done (#1043).
func TestAPurgeReportsAnIncompleteCheckpointWhenAReaderHoldsTheLog(t *testing.T) {
	const canary = "zqxjkvbrmp"
	const original = "a fact about " + canary + " that a purge must erase"

	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if err := s.EnsureProject(ctx, testProject, "/tmp/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, _, _, err := s.Upsert(ctx, testProject, "fact", original, "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	// A read-only handle takes a snapshot BEFORE the purge writes anything, so
	// the checkpoint cannot fold or truncate the purge's frames while it holds
	// the snapshot. A read transaction only holds one once it has read, hence
	// the count.
	reader, err := OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	readTx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin reader transaction: %v", err)
	}
	var held int
	if err := readTx.QueryRowContext(ctx, `SELECT count(*) FROM memories`).Scan(&held); err != nil {
		_ = readTx.Rollback()
		t.Fatalf("take the reader's snapshot: %v", err)
	}

	outcome, err := s.DeletePurge(ctx, id)
	_ = readTx.Rollback()
	if err != nil {
		t.Fatalf("DeletePurge: %v", err)
	}
	if outcome.CheckpointComplete {
		t.Fatal("the purge reported a complete checkpoint while a reader held the log")
	}
	// The incomplete report is only honest if the log really still holds the
	// text: the canary in the -wal is what the report is about.
	wal, err := os.ReadFile(dbPath + "-wal")
	if err != nil {
		t.Fatalf("read -wal: %v", err)
	}
	if !bytes.Contains(wal, []byte(canary)) {
		t.Fatalf("the -wal no longer holds the canary, so the incomplete report is not explained by the held reader")
	}

	// The state is transient, and the report says exactly that: once the reader
	// is gone a later checkpoint completes, folds the frames in and truncates
	// the log, and the canary is gone from it.
	complete, err := s.checkpointAfterPurge(ctx)
	if err != nil {
		t.Fatalf("checkpoint after the reader left: %v", err)
	}
	if !complete {
		t.Fatal("a checkpoint with no held reader still reported itself incomplete")
	}
	if wal, err := os.ReadFile(dbPath + "-wal"); err != nil {
		t.Fatalf("read -wal after the later checkpoint: %v", err)
	} else if bytes.Contains(wal, []byte(canary)) {
		t.Fatal("the later checkpoint truncated the log but the canary is still in it")
	}
}

// TestAPurgeDoesNotFailWhenTheCheckpointCannotFinish: the delete commits before
// the checkpoint runs, so a checkpoint that cannot truncate the log — or that a
// caller's deadline would have cut short — must not be reported as a failed
// delete. The row is gone whatever the log holds, and the outcome carries the
// warning. The deadline here expires well before the busy timeout, which is the
// case that used to surface as "checkpoint after a purge: context deadline
// exceeded" on a delete that had already happened.
func TestAPurgeDoesNotFailWhenTheCheckpointCannotFinish(t *testing.T) {
	const canary = "zqxjkvbrmp"
	const original = "a fact about " + canary + " that a purge must erase"

	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if err := s.EnsureProject(ctx, testProject, "/tmp/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, _, _, err := s.Upsert(ctx, testProject, "fact", original, "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	reader, err := OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	readTx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin reader transaction: %v", err)
	}
	var held int
	if err := readTx.QueryRowContext(ctx, `SELECT count(*) FROM memories`).Scan(&held); err != nil {
		_ = readTx.Rollback()
		t.Fatalf("take the reader's snapshot: %v", err)
	}

	// The deadline is for the purge, and it expires during the checkpoint's busy
	// wait. The delete must still report success.
	deadlined, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	outcome, err := s.DeletePurge(deadlined, id)
	cancel()
	_ = readTx.Rollback()
	if err != nil {
		t.Fatalf("a committed purge was reported as failed: %v", err)
	}
	if outcome.CheckpointComplete {
		t.Fatal("the checkpoint reported itself complete while a reader held the log")
	}
	if rows, err := s.GetByIDs(ctx, []string{id}); err != nil {
		t.Fatalf("GetByIDs: %v", err)
	} else if len(rows) != 0 {
		t.Fatal("the row survived a purge that reported itself as an incomplete checkpoint")
	}
}
