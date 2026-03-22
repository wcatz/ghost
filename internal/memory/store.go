package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wcatz/ghost/internal/ai"
)

// Memory represents a single discrete memory.
type Memory struct {
	ID           string   `json:"id"`
	ProjectID    string   `json:"project_id"`
	Category     string   `json:"category"`
	Content      string   `json:"content"`
	Importance   float32  `json:"importance"`
	AccessCount  int      `json:"access_count"`
	LastAccessed *string  `json:"last_accessed,omitempty"`
	Source       string   `json:"source"`
	Tags         []string `json:"tags"`
	Pinned       bool     `json:"pinned"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
}

// Project represents a registered project.
type Project struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Store manages the SQLite memory database.
type Store struct {
	db     *sql.DB
	mu     sync.RWMutex
	logger *slog.Logger
	onSave func(projectID string) // optional callback after memory create/upsert
}

// SetOnSave registers a callback invoked after each successful memory save.
// The callback must be non-blocking (e.g., a non-blocking channel send).
func (s *Store) SetOnSave(fn func(projectID string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onSave = fn
}

// NewStore creates a new memory store from an open database.
func NewStore(db *sql.DB, logger *slog.Logger) *Store {
	return &Store{db: db, logger: logger}
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// ListProjects returns all registered projects.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, path, name, created_at, updated_at FROM projects ORDER BY name ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var projects []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.Path, &p.Name, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// EnsureProject creates a project record if it doesn't exist.
// When called with an absolute path, it auto-merges any same-name project
// that was created with a non-absolute path (e.g., by MCP using name-as-ID).
func (s *Store) EnsureProject(ctx context.Context, id, path, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (id, path, name) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET path = excluded.path, updated_at = datetime('now')
	`, id, path, name)
	if err != nil {
		return fmt.Errorf("ensure project: %w", err)
	}

	// Also ensure ghost_state exists.
	_, err = s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO ghost_state (project_id) VALUES (?)
	`, id)
	if err != nil {
		return err
	}

	// Auto-merge: if this project has a real filesystem path, look for
	// a same-name project created by MCP with a non-absolute path.
	if path != id && filepath.IsAbs(path) {
		var dupID string
		scanErr := s.db.QueryRowContext(ctx,
			`SELECT id FROM projects WHERE name = ? AND id != ? AND path NOT LIKE '/%' LIMIT 1`,
			name, id).Scan(&dupID)
		if scanErr == nil && dupID != "" {
			if mergeErr := s.mergeProjectLocked(ctx, dupID, id); mergeErr != nil {
				s.logger.Error("auto-merge failed", "old_id", dupID, "new_id", id, "error", mergeErr)
			}
		}
	}

	return nil
}

// MergeProject reassigns all child records from oldID to newID, then deletes
// the old project row. Use this to unify duplicate project entries.
func (s *Store) MergeProject(ctx context.Context, oldID, newID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mergeProjectLocked(ctx, oldID, newID)
}

func (s *Store) mergeProjectLocked(ctx context.Context, oldID, newID string) error {
	if oldID == newID {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin merge tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Reassign all child records from old project to new.
	stmts := []string{
		`UPDATE memories SET project_id = ? WHERE project_id = ?`,
		`UPDATE conversations SET project_id = ? WHERE project_id = ?`,
		`UPDATE tasks SET project_id = ? WHERE project_id = ?`,
		`UPDATE decisions SET project_id = ? WHERE project_id = ?`,
		`UPDATE token_usage SET project_id = ? WHERE project_id = ?`,
		`UPDATE audit_log SET project_id = ? WHERE project_id = ?`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt, newID, oldID); err != nil {
			return fmt.Errorf("merge reassign: %w", err)
		}
	}

	// Delete old project's ghost_state (newID already has its own).
	if _, err := tx.ExecContext(ctx, `DELETE FROM ghost_state WHERE project_id = ?`, oldID); err != nil {
		return fmt.Errorf("merge delete ghost_state: %w", err)
	}

	// Delete the old project row.
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, oldID); err != nil {
		return fmt.Errorf("merge delete project: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("merge commit: %w", err)
	}

	s.logger.Info("merged duplicate project", "old_id", oldID, "new_id", newID)
	return nil
}

// ResolveProjectByName looks up a project by name and returns its hash ID.
func (s *Store) ResolveProjectByName(ctx context.Context, name string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM projects WHERE name = ? LIMIT 1`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("resolve project by name: %w", err)
	}
	return id, nil
}

// Create inserts a new memory and returns its ID.
func (s *Store) Create(ctx context.Context, projectID string, m Memory) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tags, _ := json.Marshal(m.Tags)

	var id string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO memories (project_id, category, content, source, importance, tags)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING id
	`, projectID, m.Category, m.Content, m.Source, m.Importance, string(tags)).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create memory: %w", err)
	}
	if s.onSave != nil {
		s.onSave(projectID)
	}
	return id, nil
}

// Upsert checks for an existing similar memory (same category, FTS overlap).
// If found, it strengthens the existing memory. If not, creates a new one.
func (s *Store) Upsert(ctx context.Context, projectID, category, content, source string, importance float32, tags []string) (id string, merged bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var existingID string
	var existingImportance float32
	var existingContent string

	// Extract keywords for FTS match (strip FTS5 operators to prevent injection).
	ftsQuery := sanitizeFTS(content)
	err = s.db.QueryRowContext(ctx, `
		SELECT m.id, m.importance, m.content
		FROM memories m
		JOIN memories_fts f ON f.rowid = m.rowid
		WHERE m.project_id = ?
		  AND m.category = ?
		  AND memories_fts MATCH ?
		ORDER BY rank, m.importance DESC
		LIMIT 1
	`, projectID, category, ftsQuery).Scan(&existingID, &existingImportance, &existingContent)

	if err == nil && existingID != "" {
		// Found a match — strengthen it.
		newImportance := existingImportance + (importance * 0.2)
		if newImportance > 1.0 {
			newImportance = 1.0
		}
		finalContent := existingContent
		if len(content) > len(existingContent) {
			finalContent = content
		}

		_, err = s.db.ExecContext(ctx, `
			UPDATE memories
			SET content = CASE WHEN source = 'manual' THEN content ELSE ? END,
			    importance = ?, access_count = access_count + 1, updated_at = datetime('now')
			WHERE id = ? AND project_id = ?
		`, finalContent, newImportance, existingID, projectID)
		if err != nil {
			return "", false, fmt.Errorf("strengthen memory: %w", err)
		}
		if s.onSave != nil {
			s.onSave(projectID)
		}
		return existingID, true, nil
	}

	// No match — create new.
	tagsJSON, _ := json.Marshal(tags)

	err = s.db.QueryRowContext(ctx, `
		INSERT INTO memories (project_id, category, content, source, importance, tags)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING id
	`, projectID, category, content, source, importance, string(tagsJSON)).Scan(&id)
	if err != nil {
		return "", false, fmt.Errorf("create memory: %w", err)
	}
	if s.onSave != nil {
		s.onSave(projectID)
	}
	return id, false, nil
}

// GetTopMemories returns the top N memories ranked by composite score
// with category-aware time decay and pinned boost.
func (s *Store) GetTopMemories(ctx context.Context, projectID string, limit int) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, category, content, importance, access_count,
		       last_accessed, source, tags, pinned, created_at, updated_at
		FROM memories
		WHERE project_id = ? OR project_id = '_global'
		ORDER BY (
			importance
			* CASE
				WHEN category IN ('preference', 'convention', 'fact') THEN 1.0
				WHEN category IN ('pattern', 'architecture') THEN
					MAX(0.3, 1.0 / (1.0 + (julianday('now') - julianday(created_at)) / 45.0))
				ELSE
					MAX(0.15, 1.0 / (1.0 + (julianday('now') - julianday(created_at)) / 30.0))
			END
			* CASE WHEN pinned = 1 THEN 1.5 ELSE 1.0 END
		) DESC
		LIMIT ?
	`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("get top memories: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// SearchFTS searches memories using full-text search.
func (s *Store) SearchFTS(ctx context.Context, projectID, query string, limit int) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.project_id, m.category, m.content, m.importance, m.access_count,
		       m.last_accessed, m.source, m.tags, m.pinned, m.created_at, m.updated_at
		FROM memories m
		JOIN memories_fts f ON f.rowid = m.rowid
		WHERE (m.project_id = ? OR m.project_id = '_global')
		  AND memories_fts MATCH ?
		ORDER BY rank, m.importance DESC
		LIMIT ?
	`, projectID, sanitizeFTS(query), limit)
	if err != nil {
		return nil, fmt.Errorf("search memories: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// SearchFTSAll searches memories across ALL projects using full-text search.
func (s *Store) SearchFTSAll(ctx context.Context, query string, limit int) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.project_id, m.category, m.content, m.importance, m.access_count,
		       m.last_accessed, m.source, m.tags, m.pinned, m.created_at, m.updated_at
		FROM memories m
		JOIN memories_fts f ON f.rowid = m.rowid
		WHERE memories_fts MATCH ?
		ORDER BY rank, m.importance DESC
		LIMIT ?
	`, sanitizeFTS(query), limit)
	if err != nil {
		return nil, fmt.Errorf("search all memories: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// GetByCategory returns memories of a specific category.
func (s *Store) GetByCategory(ctx context.Context, projectID, category string, limit int) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, category, content, importance, access_count,
		       last_accessed, source, tags, pinned, created_at, updated_at
		FROM memories
		WHERE (project_id = ? OR project_id = '_global') AND category = ?
		ORDER BY importance DESC, created_at DESC
		LIMIT ?
	`, projectID, category, limit)
	if err != nil {
		return nil, fmt.Errorf("get by category: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// GetAll returns all memories for a project.
func (s *Store) GetAll(ctx context.Context, projectID string, limit int) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, category, content, importance, access_count,
		       last_accessed, source, tags, pinned, created_at, updated_at
		FROM memories
		WHERE project_id = ?
		ORDER BY importance DESC, created_at DESC
		LIMIT ?
	`, projectID, limit)
	if err != nil {
		return nil, fmt.Errorf("get all memories: %w", err)
	}
	defer rows.Close()
	return scanMemories(rows)
}

// Touch increments access_count and updates last_accessed.
func (s *Store) Touch(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	placeholders := make([]string, len(ids))
	args := make([]interface{}, len(ids)+1)
	args[0] = time.Now().UTC().Format(time.RFC3339)
	for i, id := range ids {
		placeholders[i] = "?"
		args[i+1] = id
	}

	query := fmt.Sprintf(`
		UPDATE memories
		SET access_count = access_count + 1, last_accessed = ?
		WHERE id IN (%s)
	`, strings.Join(placeholders, ","))

	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

// Delete removes a specific memory.
func (s *Store) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	result, err := s.db.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("memory not found: %s", id)
	}
	return nil
}

// TogglePin sets or clears the pinned flag.
func (s *Store) TogglePin(ctx context.Context, id string, pinned bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	pinnedInt := 0
	if pinned {
		pinnedInt = 1
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE memories SET pinned = ?, updated_at = datetime('now') WHERE id = ?
	`, pinnedInt, id)
	return err
}

// ReplaceNonManual atomically replaces all non-manual memories for a project.
// Manual-sourced memories are preserved. Refuses to replace with an empty set.
func (s *Store) ReplaceNonManual(ctx context.Context, projectID string, memories []Memory) error {
	if len(memories) == 0 {
		return fmt.Errorf("refusing to replace memories with empty set — reflection likely malformed")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `DELETE FROM memories WHERE project_id = ? AND source != 'manual'`, projectID)
	if err != nil {
		return fmt.Errorf("delete old memories: %w", err)
	}

	for _, m := range memories {
		tags, _ := json.Marshal(m.Tags)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO memories (project_id, category, content, source, importance, tags)
			VALUES (?, ?, ?, 'reflection', ?, ?)
		`, projectID, m.Category, m.Content, m.Importance, string(tags))
		if err != nil {
			return fmt.Errorf("insert memory: %w", err)
		}
	}

	return tx.Commit()
}

// CountMemories returns the total number of memories for a project.
func (s *Store) CountMemories(ctx context.Context, projectID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE project_id = ?`, projectID).Scan(&count)
	return count, err
}

// IncrementInteraction increments the interaction count and returns the new value.
func (s *Store) IncrementInteraction(ctx context.Context, projectID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var count int
	err := s.db.QueryRowContext(ctx, `
		UPDATE ghost_state
		SET interaction_count = interaction_count + 1, updated_at = datetime('now')
		WHERE project_id = ?
		RETURNING interaction_count
	`, projectID).Scan(&count)
	return count, err
}

// GetLearnedContext returns the learned context for a project.
func (s *Store) GetLearnedContext(ctx context.Context, projectID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var ctx_ string
	err := s.db.QueryRowContext(ctx, `SELECT learned_context FROM ghost_state WHERE project_id = ?`, projectID).Scan(&ctx_)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return ctx_, err
}

// UpdateLearnedContext updates the learned context and reflection metadata.
func (s *Store) UpdateLearnedContext(ctx context.Context, projectID, learnedContext, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		UPDATE ghost_state
		SET learned_context = ?, reflection_summary = ?,
		    last_reflection_at = datetime('now'), updated_at = datetime('now')
		WHERE project_id = ?
	`, learnedContext, summary, projectID)
	return err
}

// --- Conversation persistence ---

// CreateConversation starts a new conversation.
func (s *Store) CreateConversation(ctx context.Context, projectID, mode string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var id string
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO conversations (project_id, mode)
		VALUES (?, ?)
		RETURNING id
	`, projectID, mode).Scan(&id)
	return id, err
}

// AppendMessage adds a message to a conversation.
func (s *Store) AppendMessage(ctx context.Context, conversationID, role, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (conversation_id, role, content)
		VALUES (?, ?, ?)
	`, conversationID, role, content)
	return err
}

// GetRecentExchanges returns the last N user+assistant pairs for reflection.
func (s *Store) GetRecentExchanges(ctx context.Context, projectID string, limit int) ([][2]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content FROM (
			SELECT m.role, m.content, m.created_at
			FROM messages m
			JOIN conversations c ON c.id = m.conversation_id
			WHERE c.project_id = ? AND m.role IN ('user', 'assistant')
			ORDER BY m.created_at DESC
			LIMIT ?
		) ORDER BY created_at ASC
	`, projectID, limit*2)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pairs [][2]string
	var current [2]string
	for rows.Next() {
		var role, content string
		if err := rows.Scan(&role, &content); err != nil {
			return nil, err
		}
		if role == "user" {
			current[0] = content
		} else {
			current[1] = content
			if current[0] != "" {
				pairs = append(pairs, current)
			}
			current = [2]string{}
		}
	}
	return pairs, rows.Err()
}

// GetLatestConversation returns the most recent conversation ID for a project.
func (s *Store) GetLatestConversation(ctx context.Context, projectID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var id string
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM conversations
		WHERE project_id = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, projectID).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

// ConversationMessage is a stored message with role and JSON content.
type ConversationMessage struct {
	Role    string
	Content string
}

// GetConversationMessages returns all messages in a conversation, ordered.
func (s *Store) GetConversationMessages(ctx context.Context, conversationID string) ([]ConversationMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT role, content FROM messages
		WHERE conversation_id = ?
		ORDER BY created_at ASC
	`, conversationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var msgs []ConversationMessage
	for rows.Next() {
		var m ConversationMessage
		if err := rows.Scan(&m.Role, &m.Content); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// RecordUsage saves token usage for cost tracking.
func (s *Store) RecordUsage(ctx context.Context, projectID, model string, usage TokenUsage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO token_usage (project_id, model, input_tokens, output_tokens, cache_creation, cache_read, cost_usd)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, projectID, model, usage.InputTokens, usage.OutputTokens, usage.CacheCreation, usage.CacheRead, usage.CostUSD)
	return err
}

// TokenUsage for cost tracking.
type TokenUsage struct {
	InputTokens   int
	OutputTokens  int
	CacheCreation int
	CacheRead     int
	CostUSD       float64
}

// ModelCost holds cost for a single model within a monthly summary.
type ModelCost struct {
	Model string  `json:"model"`
	Cost  float64 `json:"cost"`
}

// MonthlyCost holds aggregated cost data for a calendar month.
type MonthlyCost struct {
	Year         int         `json:"year"`
	Month        int         `json:"month"`
	TotalCost    float64     `json:"total_cost"`
	TotalSavings float64     `json:"total_savings"`
	ByModel      []ModelCost `json:"by_model"`
}

// GetMonthlyCost returns aggregated cost data for the given month across all projects,
// including per-model breakdown and cache savings.
func (s *Store) GetMonthlyCost(ctx context.Context, year, month int) (MonthlyCost, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	start := fmt.Sprintf("%04d-%02d-01", year, month)
	var end string
	if month == 12 {
		end = fmt.Sprintf("%04d-01-01", year+1)
	} else {
		end = fmt.Sprintf("%04d-%02d-01", year, month+1)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT model,
		       COALESCE(SUM(cost_usd), 0),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_creation), 0),
		       COALESCE(SUM(cache_read), 0)
		FROM token_usage
		WHERE created_at >= ? AND created_at < ?
		GROUP BY model
	`, start, end)
	if err != nil {
		return MonthlyCost{}, err
	}
	defer func() { _ = rows.Close() }()

	mc := MonthlyCost{Year: year, Month: month}
	for rows.Next() {
		var model string
		var cost float64
		var input, output, cacheWrite, cacheRead int
		if err := rows.Scan(&model, &cost, &input, &output, &cacheWrite, &cacheRead); err != nil {
			return MonthlyCost{}, err
		}
		mc.TotalCost += cost
		mc.ByModel = append(mc.ByModel, ModelCost{Model: model, Cost: cost})

		// Compute what cost would have been without caching for this model.
		noCacheCost := ai.CostWithoutCacheForUsage(input, output, cacheWrite, cacheRead, model)
		mc.TotalSavings += noCacheCost - cost
	}
	return mc, rows.Err()
}

func scanMemories(rows *sql.Rows) ([]Memory, error) {
	var memories []Memory
	for rows.Next() {
		var m Memory
		var lastAccessed sql.NullString
		var tagsJSON string
		var pinned int

		if err := rows.Scan(
			&m.ID, &m.ProjectID, &m.Category, &m.Content, &m.Importance,
			&m.AccessCount, &lastAccessed, &m.Source, &tagsJSON,
			&pinned, &m.CreatedAt, &m.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}

		if lastAccessed.Valid {
			m.LastAccessed = &lastAccessed.String
		}
		m.Pinned = pinned == 1

		if err := json.Unmarshal([]byte(tagsJSON), &m.Tags); err != nil {
			m.Tags = []string{}
		}

		memories = append(memories, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory rows: %w", err)
	}
	return memories, nil
}

// sanitizeFTS strips FTS5 special operators from text to prevent query injection.
// Extracts plain words and quotes each one so they're treated as literals.
func sanitizeFTS(text string) string {
	// Remove FTS5 operators and punctuation, keep only words.
	var words []string
	for _, word := range strings.Fields(text) {
		// Strip non-alphanumeric characters from edges.
		clean := strings.TrimFunc(word, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
		})
		if len(clean) >= 1 {
			// Quote each word to treat as literal.
			words = append(words, `"`+clean+`"`)
		}
	}
	if len(words) == 0 {
		return `""`
	}
	// Limit to first 10 words to keep the query reasonable.
	if len(words) > 10 {
		words = words[:10]
	}
	return strings.Join(words, " OR ")
}
