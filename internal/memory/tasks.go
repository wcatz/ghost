package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Task represents a work item for a project.
type Task struct {
	ID          string `json:"id"`
	ProjectID   string `json:"project_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	Priority    int    `json:"priority"`
	BlockedBy   string `json:"blocked_by,omitempty"`
	Branch      string `json:"branch,omitempty"`
	PRNumber    int    `json:"pr_number,omitempty"`
	Notes       string `json:"notes,omitempty"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	CompletedAt string `json:"completed_at,omitempty"`
}

// CreateTask inserts a new task.
func (s *Store) CreateTask(ctx context.Context, projectID, title, description string, priority int) (string, error) {
	if err := rejectSecret("title", title); err != nil {
		return "", err
	}
	if err := rejectSecret("description", description); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// A guarded TRANSACTION rather than execGuardedWrite: `RETURNING id` is a
	// read, and the row must come back from the insert that produced it, so the
	// scan has to run inside the same transaction — which is also where the
	// newer-store check runs (#746). Routing this through QueryRowContext on the
	// pool instead, as it did, is how a write reaches a store a newer Ghost owns
	// with nothing checking: `ghost_task_create` is the live tool behind it.
	tx, lock, err := s.beginWrite(ctx, "create-task")
	if err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}

	var id string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO tasks (project_id, title, description, priority)
		VALUES (?, ?, ?, ?)
		RETURNING id
	`, projectID, title, description, priority).Scan(&id); err != nil {
		_ = tx.Rollback()
		return "", fmt.Errorf("create task: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}
	lock.reportHold("create-task", time.Now())
	return id, nil
}

// ListTasks returns tasks for a project, optionally filtered by status.
func (s *Store) ListTasks(ctx context.Context, projectID, status string, limit int) ([]Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, project_id, title, description, status, priority,
	                  COALESCE(blocked_by, ''), COALESCE(branch, ''), COALESCE(pr_number, 0),
	                  COALESCE(notes, ''), created_at, updated_at, COALESCE(completed_at, '')
	           FROM tasks WHERE project_id = ?`
	args := []interface{}{projectID}

	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY priority ASC, created_at DESC, rowid DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var tasks []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.Title, &t.Description, &t.Status,
			&t.Priority, &t.BlockedBy, &t.Branch, &t.PRNumber, &t.Notes,
			&t.CreatedAt, &t.UpdatedAt, &t.CompletedAt); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// resolveTaskID expands a task ID or unique prefix to the full stored ID,
// like git short SHAs. Session-hook and task-list output truncate IDs to 8
// characters, so callers routinely hold a prefix rather than the full 32-char
// ID. An exact id match is tried first and wins at any length; only an input
// that names no task exactly is expanded as a prefix. Caller must hold s.mu
// (read or write).
func (s *Store) resolveTaskID(ctx context.Context, idOrPrefix string) (string, error) {
	if idOrPrefix == "" {
		return "", fmt.Errorf("task id is required")
	}

	// A byte-exact match wins at any length, the rule memref follows for a
	// memory reference: the id is the primary key, so the spelling the caller
	// typed is decisive. Without this arm a task whose id prefixes another's
	// (`ghost import` writes an artifact's ids verbatim, so T1 beside T10 is
	// possible) is ambiguous under every spelling and can never be reached.
	var exact string
	switch err := s.db.QueryRowContext(ctx, `SELECT id FROM tasks WHERE id = ?`, idOrPrefix).Scan(&exact); {
	case err == nil:
		return exact, nil
	case !errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("resolve task id: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM tasks WHERE substr(id, 1, length(?1)) = ?1 LIMIT 2
	`, idOrPrefix)
	if err != nil {
		return "", fmt.Errorf("resolve task id: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	switch len(ids) {
	case 0:
		return "", fmt.Errorf("task not found: %s", RefusalArg("task_id", idOrPrefix))
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("ambiguous task id prefix %s matches multiple tasks — use more characters", RefusalArg("task_id", idOrPrefix))
	}
}

// CompleteTask marks a task as done. Accepts a full task ID or a unique prefix.
//
// Completing is idempotent (#1143): a task that is already done keeps the
// completed_at it got when it became done (the rule UpdateTask follows, #1040),
// and a blank (empty or whitespace-only) notes argument never overwrites stored
// notes; a non-blank one replaces them. Both are decided inside the single
// UPDATE, against the row as it stands when the write lands, so a concurrent
// edit cannot interleave.
//
// The note is guarded on its UNCLAMPED text and THEN clamped to MaxContentLen
// (#1238). The guard is first so a credential-shaped value past the cap is
// refused rather than quietly cut out of sight. The clamp is here because this
// is a task note's writer, and ImportTask clamps the same field — the two must
// store the same bytes for the same input, or the documented backup and restore
// path loses what a save kept. The returned bool reports that ClampContent cut
// something, which is what ghost_task_complete warns the caller about;
// ClampContent's marker is already in the stored value, so a reader of the row
// alone sees the loss too.
//
// The RowsAffected of the UPDATE is read and a 0-row update is an error rather
// than a success (#1083): the row is resolved above and a task deleted in
// between leaves nothing for the write to land on, which is a completion the
// caller believes happened.
func (s *Store) CompleteTask(ctx context.Context, taskID, notes string) (bool, error) {
	// Checked before resolveTaskID so the refusal is the same error whether or
	// not the id resolves — and a completion note is where "here is the value
	// that fixed it" gets written, so it is rendered straight into the next
	// session's context.
	if err := rejectSecret("notes", notes); err != nil {
		return false, err
	}
	notes, clamped := ClampContent(notes)
	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := s.resolveTaskID(ctx, taskID)
	if err != nil {
		return false, err
	}

	res, err := s.execGuardedWrite(ctx, "complete-task", `
		UPDATE tasks SET status = 'done',
			notes = CASE WHEN ? = '' OR trim(?) = '' THEN notes ELSE ? END,
			completed_at = CASE WHEN status = 'done' AND completed_at IS NOT NULL THEN completed_at ELSE datetime('now') END,
			updated_at = datetime('now')
		WHERE id = ?
	`, notes, notes, notes, id)
	if err != nil {
		return false, fmt.Errorf("complete task: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("complete task: rows affected: %w", err)
	}
	if n == 0 {
		return false, fmt.Errorf("complete task: task %s was deleted before the write landed", RefusalArg("task_id", id))
	}
	return clamped, nil
}

// GetTask returns a single task by ID. Accepts a full task ID or a unique prefix.
func (s *Store) GetTask(ctx context.Context, taskID string) (Task, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	id, err := s.resolveTaskID(ctx, taskID)
	if err != nil {
		return Task{}, err
	}

	var t Task
	err = s.db.QueryRowContext(ctx, `
		SELECT id, project_id, title, description, status, priority,
		       COALESCE(blocked_by, ''), COALESCE(branch, ''), COALESCE(pr_number, 0),
		       COALESCE(notes, ''), created_at, updated_at, COALESCE(completed_at, '')
		FROM tasks WHERE id = ?
	`, id).Scan(
		&t.ID, &t.ProjectID, &t.Title, &t.Description, &t.Status,
		&t.Priority, &t.BlockedBy, &t.Branch, &t.PRNumber, &t.Notes,
		&t.CreatedAt, &t.UpdatedAt, &t.CompletedAt,
	)
	if err != nil {
		return Task{}, fmt.Errorf("get task: %w", err)
	}
	return t, nil
}

// UpdateTask merges the caller's optional fields onto the current row and
// writes the result. The row is read INSIDE the write transaction that then
// writes it (#1083): read on the pool outside it, as this did, two handles
// editing different fields of one task each merged onto a copy taken before the
// other's edit landed, so one of the two was lost and both calls reported
// success. s.mu is per Store and closes nothing across processes, and two MCP
// servers on one database is a supported mode. The transaction is opened before
// the read and _txlock=immediate holds the write lock across it, which is the
// arrangement the Boundaries note in docs/architecture.md requires and the one
// UpdateMemoryWithOptions already followed. status, priority, and description
// are nil to preserve the current value.
// completed_at follows the status like CompleteTask sets it: stamped when the
// status moves INTO done and cleared when it moves out. Accepts a full task ID
// or a unique prefix. Returns the task as it stood after the merge, so callers
// don't need a separate read to get ProjectID or the resolved field values.
func (s *Store) UpdateTask(ctx context.Context, taskID string, status *string, priority *int, description *string) (Task, error) {
	// The incoming description only, for the reason UpdateMemory gives: a row
	// that already holds a credential must stay editable field by field.
	if description != nil {
		if err := rejectSecret("description", *description); err != nil {
			return Task{}, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := s.resolveTaskID(ctx, taskID)
	if err != nil {
		return Task{}, err
	}

	// The read that decides this write, in the transaction that makes it. The
	// prefix is resolved above on the pool because it names a row and decides no
	// value; a task that disappears between there and here is caught below, by
	// the read returning no row or by the UPDATE matching nothing.
	tx, lock, err := s.beginWrite(ctx, "update-task")
	if err != nil {
		return Task{}, fmt.Errorf("update task: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var t Task
	err = tx.QueryRowContext(ctx, `
		SELECT id, project_id, title, description, status, priority,
		       COALESCE(blocked_by, ''), COALESCE(branch, ''), COALESCE(pr_number, 0),
		       COALESCE(notes, ''), created_at, updated_at, COALESCE(completed_at, '')
		FROM tasks WHERE id = ?
	`, id).Scan(
		&t.ID, &t.ProjectID, &t.Title, &t.Description, &t.Status,
		&t.Priority, &t.BlockedBy, &t.Branch, &t.PRNumber, &t.Notes,
		&t.CreatedAt, &t.UpdatedAt, &t.CompletedAt,
	)
	if err != nil {
		return Task{}, fmt.Errorf("get task: %w", err)
	}

	// completed_at tracks the status exactly as CompleteTask writes it (#1040):
	// stamped on the transition INTO done and cleared on any transition out.
	// The transition is what matters, so an already-done task edited again
	// keeps the stamp it got when it became done rather than being re-stamped.
	previousStatus := t.Status
	if status != nil {
		t.Status = *status
	}
	if priority != nil {
		t.Priority = *priority
	}
	if description != nil {
		t.Description = *description
	}
	switch {
	case t.Status != "done":
		t.CompletedAt = ""
	case previousStatus != "done":
		t.CompletedAt = time.Now().UTC().Format(StoredStampLayout)
	}

	// Bind NULL rather than "" when clearing, so a cleared stamp is the
	// column's own "no completion" rather than an empty value a reader has to
	// interpret.
	var completedAt any
	if t.CompletedAt != "" {
		completedAt = t.CompletedAt
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE tasks SET status = ?, priority = ?, description = ?, completed_at = ?, updated_at = datetime('now')
		WHERE id = ?
	`, t.Status, t.Priority, t.Description, completedAt, id)
	if err != nil {
		return Task{}, fmt.Errorf("update task: %w", err)
	}
	// A 0-row update is an error, never a success nobody can detect (#1083).
	// The read above is inside this transaction, so the row could only have
	// gone away under it; the count is what says so rather than assuming.
	n, err := res.RowsAffected()
	if err != nil {
		return Task{}, fmt.Errorf("update task: rows affected: %w", err)
	}
	if n == 0 {
		return Task{}, fmt.Errorf("update task: task %s was deleted before the write landed", RefusalArg("task_id", id))
	}
	if err := tx.Commit(); err != nil {
		return Task{}, fmt.Errorf("update task: %w", err)
	}
	lock.reportHold("update-task", time.Now())
	return t, nil
}
