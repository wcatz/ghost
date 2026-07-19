package memory

import (
	"context"
	"fmt"
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
	s.mu.Lock()
	defer s.mu.Unlock()

	var id string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO tasks (project_id, title, description, priority)
		VALUES (?, ?, ?, ?)
		RETURNING id
	`, projectID, title, description, priority).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}
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
	query += ` ORDER BY priority ASC, created_at DESC LIMIT ?`
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
// ID. Caller must hold s.mu (read or write).
func (s *Store) resolveTaskID(ctx context.Context, idOrPrefix string) (string, error) {
	if idOrPrefix == "" {
		return "", fmt.Errorf("task id is required")
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
		return "", fmt.Errorf("task not found: %s", idOrPrefix)
	case 1:
		return ids[0], nil
	default:
		return "", fmt.Errorf("ambiguous task id prefix %q matches multiple tasks — use more characters", idOrPrefix)
	}
}

// CompleteTask marks a task as done. Accepts a full task ID or a unique prefix.
func (s *Store) CompleteTask(ctx context.Context, taskID, notes string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := s.resolveTaskID(ctx, taskID)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE tasks SET status = 'done', notes = ?, completed_at = datetime('now'), updated_at = datetime('now')
		WHERE id = ?
	`, notes, id)
	if err != nil {
		return fmt.Errorf("complete task: %w", err)
	}
	return nil
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

// UpdateTask updates a task's status, priority, or description. Accepts a
// full task ID or a unique prefix.
func (s *Store) UpdateTask(ctx context.Context, taskID, status string, priority int, description string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := s.resolveTaskID(ctx, taskID)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `
		UPDATE tasks SET status = ?, priority = ?, description = ?, updated_at = datetime('now')
		WHERE id = ?
	`, status, priority, description, id)
	if err != nil {
		return fmt.Errorf("update task: %w", err)
	}
	return nil
}
