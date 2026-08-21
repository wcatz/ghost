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

// RecordDecision creates a decision and also saves it as a memory. Both
// writes are atomic — if either fails, neither is committed. It returns both
// the decision's own ID (for ListDecisions/status lookups) and the companion
// memory row's ID (needed for ghost_memory_pin/ghost_memory_update, which
// operate on memories, not decisions) — the two are different rows and
// different IDs.
func (s *Store) RecordDecision(ctx context.Context, projectID, title, decision, rationale string, alternatives, tags []string) (decisionID, memoryID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	altJSON, _ := json.Marshal(alternatives)
	tagJSON, _ := json.Marshal(tags)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", fmt.Errorf("record decision: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // intentional no-op after Commit

	err = tx.QueryRowContext(ctx, `
		INSERT INTO decisions (project_id, title, decision, alternatives, rationale, tags)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING id
	`, projectID, title, decision, string(altJSON), rationale, string(tagJSON)).Scan(&decisionID)
	if err != nil {
		return "", "", fmt.Errorf("record decision: %w", err)
	}

	content := fmt.Sprintf("%s: %s. Rationale: %s", title, decision, rationale)
	err = tx.QueryRowContext(ctx, `
		INSERT INTO memories (project_id, category, content, source, importance, tags)
		VALUES (?, 'decision', ?, 'decision_log', 0.9, ?)
		RETURNING id
	`, projectID, content, string(tagJSON)).Scan(&memoryID)
	if err != nil {
		return "", "", fmt.Errorf("record decision memory: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", "", fmt.Errorf("record decision: commit: %w", err)
	}

	if s.onSave != nil {
		s.onSave(projectID)
	}

	return decisionID, memoryID, nil
}

// SupersedeDecision marks oldID as superseded by newID within one project.
// The decisions table has carried `status` and `superseded_by` columns since
// the schema was written, but nothing ever wrote them — so a reversed decision
// stayed `status: active` forever and ranked alongside the decision that
// replaced it. This is the writer.
//
// Both IDs must belong to projectID and must differ; a decision cannot
// supersede itself. Superseding an already-superseded decision just repoints
// it, so re-running is safe.
func (s *Store) SupersedeDecision(ctx context.Context, projectID, oldID, newID string) error {
	if oldID == newID {
		return fmt.Errorf("supersede decision: a decision cannot supersede itself (%s)", oldID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Verify the superseding decision exists in this project before pointing
	// at it — superseded_by is ON DELETE SET NULL, not enforced on insert.
	var exists int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM decisions WHERE id = ? AND project_id = ?`,
		newID, projectID).Scan(&exists); err != nil {
		return fmt.Errorf("supersede decision: lookup %s: %w", newID, err)
	}
	if exists == 0 {
		return fmt.Errorf("supersede decision: superseding decision %s not found in project %s", newID, projectID)
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE decisions
		SET status = 'superseded', superseded_by = ?, updated_at = datetime('now')
		WHERE id = ? AND project_id = ?
	`, newID, oldID, projectID)
	if err != nil {
		return fmt.Errorf("supersede decision: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("supersede decision: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("supersede decision: decision %s not found in project %s", oldID, projectID)
	}
	return nil
}

// ListDecisions returns decisions for a project.
func (s *Store) ListDecisions(ctx context.Context, projectID, status string, limit int) ([]Decision, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	inner := `SELECT id, project_id, title, decision, alternatives, rationale, status,
	                 COALESCE(superseded_by, '') AS superseded_by, tags, created_at, updated_at
	          FROM decisions WHERE project_id = ?`
	args := []interface{}{projectID}

	if status != "" {
		inner += ` AND status = ?`
		args = append(args, status)
	}
	// The limit picks the window (newest first, as it always has), and only
	// then are superseded decisions sorted below live ones. Reordering before
	// truncating would change *which* decisions come back — it would push the
	// oldest superseded ones out of the window entirely, and a superseded
	// decision is exactly what a caller needs to see to know a prior one was
	// reversed. (This is the same truncate-first invariant the search path's
	// decayRank applies to superseded memories.) The outer sort is a no-op when
	// the caller already filtered by status.
	inner += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	query := `SELECT id, project_id, title, decision, alternatives, rationale, status,
	                 superseded_by, tags, created_at, updated_at
	          FROM (` + inner + `) ORDER BY (status = 'superseded'), created_at DESC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list decisions: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var decisions []Decision
	for rows.Next() {
		var d Decision
		var altJSON, tagJSON string
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Title, &d.Decision, &altJSON,
			&d.Rationale, &d.Status, &d.SupersededBy, &tagJSON, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(altJSON), &d.Alternatives)
		_ = json.Unmarshal([]byte(tagJSON), &d.Tags)
		decisions = append(decisions, d)
	}
	return decisions, rows.Err()
}
