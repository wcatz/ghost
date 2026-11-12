package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
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

// Caller must hold s.mu (read or write).
func (s *Store) resolveTaskID(ctx context.Context, idOrPrefix string) (string, error) {
	if idOrPrefix == "" {
		return "", fmt.Errorf("task id is required")
	}

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

// CompleteTask marks a task as done. It returns whether notes were clamped.
func (s *Store) CompleteTask(ctx context.Context, taskID, notes string) (bool, error) {
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

	if strings.TrimSpace(notes) == "" {
		res, err := s.execGuardedWrite(ctx, "complete-task", `
			UPDATE tasks SET status = 'done',
				completed_at = CASE WHEN status = 'done' AND completed_at IS NOT NULL THEN completed_at ELSE datetime('now') END,
				updated_at = datetime('now')
			WHERE id = ?
		`, id)
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

	res, err := s.execGuardedWrite(ctx, "complete-task", `
		UPDATE tasks SET status = 'done',
			notes = ?,
			completed_at = CASE WHEN status = 'done' AND completed_at IS NOT NULL THEN completed_at ELSE datetime('now') END,
			updated_at = datetime('now')
		WHERE id = ?
	`, notes, id)
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

// GetTask returns a single task by ID or unique prefix.
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

// UpdateTask updates a task's mutable fields and returns the updated task.
func (s *Store) UpdateTask(ctx context.Context, taskID string, status *string, priority *int, description *string) (Task, error) {
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
