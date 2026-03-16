package memory

import (
	"context"
	"encoding/json"
	"fmt"
)

// Decision represents an architectural or design decision.
type Decision struct {
	ID           string   `json:"id"`
	ProjectID    string   `json:"project_id"`
	Title        string   `json:"title"`
	Decision     string   `json:"decision"`
	Alternatives []string `json:"alternatives"`
	Rationale    string   `json:"rationale"`
	Status       string   `json:"status"`
	SupersededBy string   `json:"superseded_by,omitempty"`
	Tags         []string `json:"tags"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

// RecordDecision creates a decision and also saves it as a memory.
func (s *Store) RecordDecision(ctx context.Context, projectID, title, decision, rationale string, alternatives, tags []string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	altJSON, _ := json.Marshal(alternatives)
	tagJSON, _ := json.Marshal(tags)

	var id string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO decisions (project_id, title, decision, alternatives, rationale, tags)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING id
	`, projectID, title, decision, string(altJSON), rationale, string(tagJSON)).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("record decision: %w", err)
	}

	// Also save as a memory so it shows up in search and prompt context.
	content := fmt.Sprintf("%s: %s. Rationale: %s", title, decision, rationale)
	s.db.ExecContext(ctx, `
		INSERT INTO memories (project_id, category, content, source, importance, tags)
		VALUES (?, 'decision', ?, 'decision_log', 0.9, ?)
	`, projectID, content, string(tagJSON))

	if s.onSave != nil {
		s.onSave(projectID)
	}

	return id, nil
}

// ListDecisions returns decisions for a project.
func (s *Store) ListDecisions(ctx context.Context, projectID, status string, limit int) ([]Decision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT id, project_id, title, decision, alternatives, rationale, status,
	                  COALESCE(superseded_by, ''), tags, created_at, updated_at
	           FROM decisions WHERE project_id = ?`
	args := []interface{}{projectID}

	if status != "" {
		query += ` AND status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	defer rows.Close()

	var decisions []Decision
	for rows.Next() {
		var d Decision
		var altJSON, tagJSON string
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Title, &d.Decision, &altJSON,
			&d.Rationale, &d.Status, &d.SupersededBy, &tagJSON, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(altJSON), &d.Alternatives)
		json.Unmarshal([]byte(tagJSON), &d.Tags)
		decisions = append(decisions, d)
	}
	return decisions, rows.Err()
}
