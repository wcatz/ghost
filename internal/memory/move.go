package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ResolveMoveTarget resolves a move destination to a project id. "_global" and
// "global" name the global bucket; anything else is resolved as a project
// reference and must exist. field names the caller's argument in refusals
// ("to" for the CLI --to flag, "to_project" for the MCP tool argument), so a
// withheld value names the argument the caller sent.
func (s *Store) ResolveMoveTarget(ctx context.Context, field, to string) (string, error) {
	if to == GlobalProjectID || to == "global" {
		return GlobalProjectID, nil
	}
	id, _, err := s.ResolveProject(ctx, to)
	if err != nil {
		return "", fmt.Errorf("resolve target project: %w", err)
	}
	if id == "" {
		return "", fmt.Errorf("target project %s not found", RefusalArg(field, to))
	}
	return id, nil
}

// MoveMemoryOptions controls a move between projects.
type MoveMemoryOptions struct {
	// Confirm allows moving a pinned or persistent memory. Without it, the move is refused.
	Confirm bool
	// DryRun reports what would move without writing.
	DryRun bool
	// Provenance records who performed the move in the memory's history.
	Provenance Provenance
}

// MoveMemoryResult reports a completed or previewed move.
type MoveMemoryResult struct {
	MemoryID      string
	FromProjectID string
	ToProjectID   string
	DryRun        bool
	Moved         bool
}

// MoveMemory moves one memory from fromProjectID to toProjectID, keeping its
// identity, links, embeddings, provenance, flags, evidence and recorded history.
// It refuses unknown source or target projects, and refuses a pinned or
// persistent memory unless opts.Confirm is set. A dry run reports what would
// move without writing. updated_at is not changed.
func (s *Store) MoveMemory(ctx context.Context, fromProjectID, memoryID, toProjectID string, opts MoveMemoryOptions) (MoveMemoryResult, error) {
	if fromProjectID == "" || memoryID == "" || toProjectID == "" {
		return MoveMemoryResult{}, fmt.Errorf("move memory: from_project_id, memory_id and to_project_id are required")
	}
	res := MoveMemoryResult{MemoryID: memoryID, FromProjectID: fromProjectID, ToProjectID: toProjectID, DryRun: opts.DryRun}
	if fromProjectID == toProjectID {
		return MoveMemoryResult{}, fmt.Errorf("move memory: memory %s is already in project %s",
			RefusalArg("memory_id", memoryID), RefusalArg("project_id", toProjectID))
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "move")
	if err != nil {
		return MoveMemoryResult{}, fmt.Errorf("begin move: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var srcExists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, fromProjectID).Scan(&srcExists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MoveMemoryResult{}, fmt.Errorf("move memory: source project %s not found", RefusalArg("project_id", fromProjectID))
		}
		return MoveMemoryResult{}, fmt.Errorf("move memory: lookup source project: %w", err)
	}

	if toProjectID == GlobalProjectID {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')
			ON CONFLICT(id) DO NOTHING
		`); err != nil {
			return MoveMemoryResult{}, fmt.Errorf("ensure _global project: %w", err)
		}
		_, _ = tx.ExecContext(ctx, `INSERT OR IGNORE INTO ghost_state (project_id) VALUES ('_global')`)
	} else {
		var dstExists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, toProjectID).Scan(&dstExists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return MoveMemoryResult{}, fmt.Errorf("move memory: target project %s not found", RefusalArg("project_id", toProjectID))
			}
			return MoveMemoryResult{}, fmt.Errorf("move memory: lookup target project: %w", err)
		}
	}

	var pinned int
	var retention string
	err = tx.QueryRowContext(ctx, `
		SELECT pinned, retention FROM memories WHERE id = ? AND project_id = ?
	`, memoryID, fromProjectID).Scan(&pinned, &retention)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MoveMemoryResult{}, fmt.Errorf("memory %s not found in project %s",
				RefusalArg("memory_id", memoryID), RefusalArg("project_id", fromProjectID))
		}
		return MoveMemoryResult{}, fmt.Errorf("move memory: lookup memory: %w", err)
	}
	if !opts.Confirm {
		if pinned != 0 {
			return MoveMemoryResult{}, fmt.Errorf("move memory: memory %s is pinned; set confirm to move it",
				RefusalArg("memory_id", memoryID))
		}
		norm, _ := NormalizeRetention(retention)
		if norm == RetentionPersistent {
			return MoveMemoryResult{}, fmt.Errorf("move memory: memory %s is persistent; set confirm to move it",
				RefusalArg("memory_id", memoryID))
		}
	}

	if opts.DryRun {
		return res, nil
	}

	moveRes, err := tx.ExecContext(ctx, `
		UPDATE memories SET project_id = ?
		WHERE id = ? AND project_id = ?
	`, toProjectID, memoryID, fromProjectID)
	if err != nil {
		return MoveMemoryResult{}, fmt.Errorf("move memory: %w", err)
	}
	n, err := moveRes.RowsAffected()
	if err != nil {
		return MoveMemoryResult{}, fmt.Errorf("move memory rows: %w", err)
	}
	if n == 0 {
		return MoveMemoryResult{}, fmt.Errorf("memory %s not found in project %s",
			RefusalArg("memory_id", memoryID), RefusalArg("project_id", fromProjectID))
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE memory_history SET project_id = ? WHERE memory_id = ?`, toProjectID, memoryID,
	); err != nil {
		return MoveMemoryResult{}, fmt.Errorf("move memory history: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE memory_flags SET project_id = ? WHERE memory_id = ?`, toProjectID, memoryID,
	); err != nil {
		return MoveMemoryResult{}, fmt.Errorf("move memory flags: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE supersede_checked SET project_id = ? WHERE newer_id = ? OR older_id = ?`,
		toProjectID, memoryID, memoryID,
	); err != nil {
		return MoveMemoryResult{}, fmt.Errorf("move supersede checks: %w", err)
	}
	if err := appendHistoryEventsTx(ctx, tx, []historyEvent{{
		phase:         phaseMove,
		prov:          opts.Provenance,
		fromProjectID: fromProjectID,
		toProjectID:   toProjectID,
	}}, []string{memoryID}); err != nil {
		return MoveMemoryResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return MoveMemoryResult{}, fmt.Errorf("commit move: %w", err)
	}
	res.Moved = true
	return res, nil
}
