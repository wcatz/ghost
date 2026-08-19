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
	"unicode"

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

	// demotionThreshold gates near-duplicate demotion in GetTopMemories (see
	// DemotionPenalties). Defaults to DefaultDemotionThreshold; callers that
	// have loaded config override it via SetDemotionThreshold.
	demotionThreshold float64
}

// SetOnSave registers a callback invoked after each successful memory save.
// The callback must be non-blocking (e.g., a non-blocking channel send).
func (s *Store) SetOnSave(fn func(projectID string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onSave = fn
}

// SetDemotionThreshold overrides the near-duplicate demotion cutoff
// GetTopMemories uses. Call after NewStore once config is loaded; until
// called, GetTopMemories uses DefaultDemotionThreshold.
func (s *Store) SetDemotionThreshold(threshold float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.demotionThreshold = threshold
}

// NewStore creates a new memory store from an open database.
func NewStore(db *sql.DB, logger *slog.Logger) *Store {
	return &Store{db: db, logger: logger, demotionThreshold: DefaultDemotionThreshold}
}

// seedGlobalMemory defines a memory that Ghost ships with out of the box.
// These are inserted as source='manual', pinned=1, so consolidation never
// touches them.
type seedGlobalMemory struct {
	Category   string
	Content    string
	Importance float32
	Tags       []string
}

// defaultSeedMemories are baked into every Ghost installation.
var defaultSeedMemories = []seedGlobalMemory{
	{
		Category:   "preference",
		Content:    "NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user.",
		Importance: 1.0,
		Tags:       []string{"git", "commits", "non-negotiable"},
	},
}

// SeedGlobalMemories ensures the _global project exists and inserts any
// missing seed memories. Seeds are pinned and source='manual' so they
// survive consolidation. Idempotent — skips memories whose content already
// exists.
func (s *Store) SeedGlobalMemories(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Ensure _global project.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')
		ON CONFLICT(id) DO NOTHING
	`)
	if err != nil {
		return fmt.Errorf("ensure _global project: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO ghost_state (project_id) VALUES ('_global')`); err != nil {
		s.logger.Warn("seed global ghost_state insert failed", "error", err)
	}

	for _, seed := range defaultSeedMemories {
		// Skip if content already exists in _global (exact match).
		var exists int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memories WHERE project_id = '_global' AND content = ?`,
			seed.Content).Scan(&exists)
		if err == nil && exists > 0 {
			continue
		}

		tags, _ := json.Marshal(seed.Tags)
		_, err = s.db.ExecContext(ctx, `
			INSERT INTO memories (project_id, category, content, source, importance, tags, pinned)
			VALUES ('_global', ?, ?, 'manual', ?, ?, 1)
		`, seed.Category, seed.Content, seed.Importance, string(tags))
		if err != nil {
			s.logger.Warn("seed memory insert failed", "content", seed.Content, "error", err)
		}
	}

	return nil
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
	defer func() { _ = rows.Close() }()

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

	// Normalize empty path to id. MCP callers pass path="" because they
	// don't know the filesystem path; using "" as path would collide across
	// projects since projects.path has a UNIQUE constraint. Using id as path
	// preserves the invariant already on disk for name-as-ID projects.
	if path == "" {
		path = id
	}

	// Check if another project already owns this path. If so, merge
	// any orphaned child records into the canonical project and skip
	// creating a duplicate. This self-heals duplicates caused by MCP
	// passing raw filesystem paths as project IDs.
	if filepath.IsAbs(path) && path != id {
		var existingID string
		scanErr := s.db.QueryRowContext(ctx,
			`SELECT id FROM projects WHERE path = ? AND id != ? LIMIT 1`,
			path, id).Scan(&existingID)
		if scanErr == nil && existingID != "" {
			// Merge any child records from the incoming ID into the canonical
			// project. mergeProjectLocked does not lock internally (only the
			// public MergeProject wrapper does), so it's safe to call directly
			// here while we already hold s.mu.
			if err := s.mergeProjectLocked(ctx, id, existingID); err != nil {
				return fmt.Errorf("auto-merge path duplicate: %w", err)
			}
			return nil
		}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (id, path, name) VALUES (?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			path = CASE WHEN excluded.path = excluded.id THEN projects.path ELSE excluded.path END,
			updated_at = datetime('now')
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
			// Call mergeProjectLocked directly to avoid deadlock (we already
			// hold s.mu; only the public MergeProject wrapper locks).
			if err := s.mergeProjectLocked(ctx, dupID, id); err != nil {
				return fmt.Errorf("auto-merge duplicate project: %w", err)
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

// DeleteProjectSummary reports what DeleteProject found (dry-run) or removed
// (apply) for one project, across every table that references it.
type DeleteProjectSummary struct {
	ProjectID   string
	ProjectName string
	Memories    int
	MemoryLinks int
	Tasks       int
	Decisions   int
	TokenUsage  int
	AuditLog    int
}

// DeleteProject permanently removes a project and everything under it.
// memories (with their FTS index entries, embeddings, links, and link_scans),
// conversations (with their messages), tasks, decisions, ghost_state, and
// memory_snapshots all cascade from the projects row via ON DELETE CASCADE
// (see schema.go). token_usage and audit_log carry a project_id column but no
// foreign key, so they're deleted explicitly in the same transaction.
//
// input is resolved exactly like every other command resolves a project (see
// ResolveProject): id, name, path-prefix, or basename all work.
//
// apply=false computes and returns the summary without writing anything.
// apply=true performs the same computation, then actually deletes everything
// in one transaction, returning the summary of what was removed. _global can
// never be deleted, in either mode — it's shared across every project's
// context injection.
//
// ResolveProject takes its own RLock internally, so it's called here before
// this method takes s.mu itself — taking s.mu first and then calling
// ResolveProject would deadlock against sync.RWMutex's non-reentrant lock.
func (s *Store) DeleteProject(ctx context.Context, input string, apply bool) (DeleteProjectSummary, error) {
	id, name, err := s.ResolveProject(ctx, input)
	if err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("resolve project: %w", err)
	}
	if id == "" {
		return DeleteProjectSummary{}, fmt.Errorf("project %q not found", input)
	}
	if id == "_global" {
		return DeleteProjectSummary{}, fmt.Errorf("refusing to delete the _global project")
	}

	if !apply {
		s.mu.RLock()
		defer s.mu.RUnlock()
		summary, err := countProjectRows(ctx, s.db, id)
		if err != nil {
			return DeleteProjectSummary{}, err
		}
		summary.ProjectID, summary.ProjectName = id, name
		return summary, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("begin delete tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// database/sql's BeginTx opens a deferred transaction on this driver: it
	// only acquires SQLite's write lock lazily, on its first write statement.
	// Without forcing that now, a separate ghost process (CLI and MCP server
	// each open their own *Store against the same SQLite file) could write
	// to this project in the gap between the counts below and the deletes —
	// and the returned/logged summary, the only durable record of what was
	// removed once the project's own audit_log rows are gone, would silently
	// undercount what actually got deleted. This no-op write forces the
	// upgrade to the write lock immediately, before any counting happens;
	// verified empirically that a concurrent writer on a separate handle
	// blocks (and eventually times out via busy_timeout) until this
	// transaction commits or rolls back.
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET id = id WHERE id = ?`, id); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("acquire write lock: %w", err)
	}

	summary, err := countProjectRows(ctx, tx, id)
	if err != nil {
		return DeleteProjectSummary{}, err
	}
	summary.ProjectID, summary.ProjectName = id, name

	if _, err := tx.ExecContext(ctx, `DELETE FROM token_usage WHERE project_id = ?`, id); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("delete token_usage: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM audit_log WHERE project_id = ?`, id); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("delete audit_log: %w", err)
	}
	if err := deleteProjectRowTx(ctx, tx, id); err != nil {
		return DeleteProjectSummary{}, err
	}

	if err := tx.Commit(); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("commit delete: %w", err)
	}

	// This log line is the only durable record of what was removed once the
	// project's own audit_log rows are gone, so it carries the full summary.
	s.logger.Info("deleted project", "project_id", id, "project_name", name,
		"memories", summary.Memories, "memory_links", summary.MemoryLinks,
		"tasks", summary.Tasks, "decisions", summary.Decisions,
		"token_usage", summary.TokenUsage, "audit_log", summary.AuditLog)
	return summary, nil
}

// queryRower is satisfied by both *sql.DB and *sql.Tx, letting
// countProjectRows run identically as a plain autocommit read (dry-run) or
// inside an already-open write transaction (apply — see DeleteProject).
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// countProjectRows computes the per-table counts for DeleteProjectSummary.
// It does not set ProjectID/ProjectName — the caller already has both from
// ResolveProject and assigns them itself.
func countProjectRows(ctx context.Context, q queryRower, id string) (DeleteProjectSummary, error) {
	var summary DeleteProjectSummary
	if err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM memories WHERE project_id = ?`, id,
	).Scan(&summary.Memories); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("count memories: %w", err)
	}
	if err := q.QueryRowContext(ctx, `
		SELECT count(*) FROM memory_links
		WHERE source_id IN (SELECT id FROM memories WHERE project_id = ?)
		   OR target_id IN (SELECT id FROM memories WHERE project_id = ?)
	`, id, id).Scan(&summary.MemoryLinks); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("count memory_links: %w", err)
	}
	if err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM tasks WHERE project_id = ?`, id,
	).Scan(&summary.Tasks); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("count tasks: %w", err)
	}
	if err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM decisions WHERE project_id = ?`, id,
	).Scan(&summary.Decisions); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("count decisions: %w", err)
	}
	if err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM token_usage WHERE project_id = ?`, id,
	).Scan(&summary.TokenUsage); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("count token_usage: %w", err)
	}
	if err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log WHERE project_id = ?`, id,
	).Scan(&summary.AuditLog); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("count audit_log: %w", err)
	}
	return summary, nil
}

// deleteProjectRowTx deletes the projects row for id inside tx and guards
// against the row already being gone: ResolveProject released its RLock
// before DeleteProject acquired s.mu, so a concurrent DeleteProject/
// MergeProject in this process — or a separate ghost process (CLI and MCP
// server each open their own *Store against the same SQLite file) — could
// have already deleted this project in that gap. Without this guard the
// transaction would still commit and DeleteProject would return a
// normal-looking success summary for a project that's already gone.
func deleteProjectRowTx(ctx context.Context, tx *sql.Tx, id string) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete project rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("project %q was already deleted", id)
	}
	return nil
}

// ResolveProject resolves an identifier — a project name, hash ID, or
// filesystem path — to that project's (id, name). Returns ("", "", nil)
// on no match; a non-nil error only indicates a real DB failure.
//
// Lookup order, first hit wins:
//  1. exact id = input
//  2. exact name = input
//  3. if input contains '/': input = path OR input has literal prefix path + "/"
//     (ordered by LENGTH(path) DESC — longest/most-specific match wins;
//     LENGTH(path) > 10 guards against a short project path matching too
//     broadly, matching the hook's original lookupProject behavior; the
//     prefix check is a literal substr comparison, not LIKE, so '%'/'_' in a
//     stored path can't act as SQL wildcards against input)
//  4. name = basename(input)
func (s *Store) ResolveProject(ctx context.Context, input string) (id, name string, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	err = s.db.QueryRowContext(ctx, `SELECT id, name FROM projects WHERE id = ? LIMIT 1`, input).Scan(&id, &name)
	if err == nil {
		return id, name, nil
	}
	if err != sql.ErrNoRows {
		return "", "", fmt.Errorf("resolve project by id: %w", err)
	}

	err = s.db.QueryRowContext(ctx, `SELECT id, name FROM projects WHERE name = ? LIMIT 1`, input).Scan(&id, &name)
	if err == nil {
		return id, name, nil
	}
	if err != sql.ErrNoRows {
		return "", "", fmt.Errorf("resolve project by name: %w", err)
	}

	if strings.Contains(input, "/") {
		// Literal prefix comparison, not LIKE: a stored path containing '%' or
		// '_' must not be treated as a SQL wildcard against input.
		err = s.db.QueryRowContext(ctx, `
			SELECT id, name FROM projects
			WHERE (path = ? OR substr(?, 1, LENGTH(path) + 1) = path || '/') AND LENGTH(path) > 10
			ORDER BY LENGTH(path) DESC LIMIT 1
		`, input, input).Scan(&id, &name)
		if err == nil {
			return id, name, nil
		}
		if err != sql.ErrNoRows {
			return "", "", fmt.Errorf("resolve project by path: %w", err)
		}
	}

	base := filepath.Base(input)
	err = s.db.QueryRowContext(ctx, `SELECT id, name FROM projects WHERE name = ? LIMIT 1`, base).Scan(&id, &name)
	if err == nil {
		return id, name, nil
	}
	if err != sql.ErrNoRows {
		return "", "", fmt.Errorf("resolve project by basename: %w", err)
	}

	return "", "", nil
}

// ListProjectNames returns all known project names, ordered the same way as
// ListProjects (name ASC) — used to format an actionable CLI error listing
// known projects on a resolution miss.
func (s *Store) ListProjectNames(ctx context.Context) ([]string, error) {
	projects, err := s.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = p.Name
	}
	return names, nil
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
// If found, it strengthens the existing memory's importance/access_count and
// links the new content to it as a 'duplicate' — it never overwrites the
// existing row's content, so a genuinely different follow-up save under a
// manual-sourced target (or any target) is never silently discarded. If no
// candidate scores above threshold, it creates a new, unlinked row.
func (s *Store) Upsert(ctx context.Context, projectID, category, content, source string, importance float32, tags []string) (id string, duplicateOf string, score float64, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var existingID string
	var existingImportance float32

	// Two-stage duplicate detection. Stage 1 (recall): the FTS OR-probe over
	// the first 30 words retrieves merge candidates cheaply. Stage 2
	// (precision): mergeScore over the full token sets confirms a true
	// duplicate — the OR-probe alone treats a single shared word as a match,
	// which silently swallowed unrelated saves.
	ftsQuery := sanitizeFTSN(content, 30)
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id, m.importance, m.content
		FROM memories m
		JOIN memories_fts f ON f.rowid = m.rowid
		WHERE m.project_id = ?
		  AND m.category = ?
		  AND memories_fts MATCH ?
		ORDER BY rank, m.importance DESC
		LIMIT 15
	`, projectID, category, ftsQuery)
	if err == nil {
		// Token-free content (punctuation/single-char words only) can still
		// FTS-match — sanitizeFTS keeps single-char words that tokenizeContent
		// drops — and jaccard(∅,∅) scores 1.0. Never merge on empty tokens.
		newTokens := tokenizeContent(content)
		var bestSim float64
		for len(newTokens) > 0 && rows.Next() {
			var candID, candContent string
			var candImportance float32
			if scanErr := rows.Scan(&candID, &candImportance, &candContent); scanErr != nil {
				continue
			}
			sim := mergeScore(newTokens, tokenizeContent(candContent))
			if sim > 0 && sim > bestSim {
				bestSim = sim
				existingID = candID
				existingImportance = candImportance
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			existingID = "" // treat a broken candidate scan as no match
		}
		_ = rows.Close()
		score = bestSim
	}

	tagsJSON, _ := json.Marshal(tags)

	if existingID != "" {
		// Found a match — strengthen the existing row, then insert the new
		// content as its own row and link it back as a 'duplicate'. Never
		// overwrite existingContent: that silently discarded content for
		// source='manual' targets while still reporting a merge.
		newImportance := existingImportance + (importance * 0.2)
		if newImportance > 1.0 {
			newImportance = 1.0
		}

		// The UPDATE (strengthen), INSERT (new row), and INSERT (link) must
		// all succeed or none should — otherwise a failure partway through
		// leaves the new memory row orphaned: unlinked, un-embedded (onSave
		// never fires), and invisible. Same tx pattern as mergeProjectLocked.
		tx, txErr := s.db.BeginTx(ctx, nil)
		if txErr != nil {
			return "", "", 0, fmt.Errorf("begin upsert tx: %w", txErr)
		}
		defer tx.Rollback() //nolint:errcheck

		if _, err = tx.ExecContext(ctx, `
			UPDATE memories
			SET importance = ?, access_count = access_count + 1, updated_at = datetime('now'),
			    resolved_at = NULL
			WHERE id = ? AND project_id = ?
		`, newImportance, existingID, projectID); err != nil {
			return "", "", 0, fmt.Errorf("strengthen memory: %w", err)
		}

		if err = tx.QueryRowContext(ctx, `
			INSERT INTO memories (project_id, category, content, source, importance, tags)
			VALUES (?, ?, ?, ?, ?, ?)
			RETURNING id
		`, projectID, category, content, source, importance, string(tagsJSON)).Scan(&id); err != nil {
			return "", "", 0, fmt.Errorf("create memory: %w", err)
		}

		// CreateLink takes s.mu itself — calling it here would deadlock, since
		// Upsert already holds the lock for its entire body. Inline the same
		// upsert-link SQL directly instead. Direction is fixed (source_id=new,
		// target_id=existing), not normalized like symmetric relations.
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO memory_links (source_id, target_id, relation, strength, source)
			VALUES (?, ?, 'duplicate', ?, 'auto')
			ON CONFLICT(source_id, target_id, relation) DO UPDATE SET
				strength = MAX(strength, excluded.strength),
				invalidated_at = NULL
		`, id, existingID, score); err != nil {
			return "", "", 0, fmt.Errorf("link duplicate: %w", err)
		}

		if err = tx.Commit(); err != nil {
			return "", "", 0, fmt.Errorf("commit upsert tx: %w", err)
		}

		if s.onSave != nil {
			s.onSave(projectID)
		}
		return id, existingID, score, nil
	}

	// No match — create new.
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO memories (project_id, category, content, source, importance, tags)
		VALUES (?, ?, ?, ?, ?, ?)
		RETURNING id
	`, projectID, category, content, source, importance, string(tagsJSON)).Scan(&id)
	if err != nil {
		return "", "", 0, fmt.Errorf("create memory: %w", err)
	}
	if s.onSave != nil {
		s.onSave(projectID)
	}
	return id, "", 0, nil
}

// DecayRankingSQL is the composite-score ranking expression shared by
// GetTopMemories below and the session-start hook's own read-only query
// (internal/mcpinit/hook.go's loadSessionContext) — a single source of truth
// for the category-aware time-decay + pinned-exemption formula so the two
// callers can never drift apart. It is a fragment, not a full query: callers
// interpolate it into their own "ORDER BY (...) DESC" clause.
// The 0.15 and 0.3 floors create a dead zone at the low end of each decaying
// category: a brand-new memory saved at importance 0.15 (or 0.3) scores
// identically to a maximally-important, fully-decayed one of the same
// category (1.0 * 0.15 == 0.15 * 1.0). Accepted — importance that low is rare
// in practice (defaults are 0.5+) — but if ranking ever looks off for a
// low-importance category member, this tie is why.
// Pinned is a full decay exemption (factor 1.0), not a multiplier on top of
// the decayed/floored score — a pinned memory always scores at its raw
// importance regardless of age or category. It's a no-op for
// preference/convention/fact, which already never decay.
const DecayRankingSQL = `
	importance
	* CASE
		WHEN pinned = 1 THEN 1.0
		WHEN category IN ('preference', 'convention', 'fact') THEN 1.0
		WHEN category IN ('pattern', 'architecture') THEN
			MAX(0.3, 1.0 / (1.0 + (julianday('now') - julianday(created_at)) / 45.0))
		ELSE
			MAX(0.15, 1.0 / (1.0 + (julianday('now') - julianday(created_at)) / 30.0))
	END
`

// GetTopMemories returns the top N memories ranked by composite score
// with category-aware time decay and pinned boost.
func (s *Store) GetTopMemories(ctx context.Context, projectID string, limit int) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, category, content, importance, access_count,
		       last_accessed, source, tags, pinned, created_at, updated_at
		FROM memories
		WHERE (project_id = ? OR project_id = '_global')
		  AND resolved_at IS NULL
		ORDER BY (`+DecayRankingSQL+`) DESC
		LIMIT ?
	`, projectID, limit*2)
	if err != nil {
		return nil, fmt.Errorf("get top memories: %w", err)
	}
	defer func() { _ = rows.Close() }()
	results, err := scanMemories(rows)
	if err != nil {
		return nil, err
	}

	if len(results) > limit {
		ids := make([]string, len(results))
		pinned := make(map[string]bool, len(results))
		for i, m := range results {
			ids[i] = m.ID
			pinned[m.ID] = m.Pinned
		}
		penalty, err := DemotionPenalties(ctx, s.db, ids, pinned, s.demotionThreshold)
		if err != nil {
			s.logger.Debug("get top memories: demotion lookup failed", "error", err)
		} else {
			results = StableDemote(results, func(m Memory) string { return m.ID }, penalty)
		}
		results = results[:min(limit, len(results))]
	}
	return results, nil
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
	defer func() { _ = rows.Close() }()
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
	defer func() { _ = rows.Close() }()
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
	defer func() { _ = rows.Close() }()
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
	defer func() { _ = rows.Close() }()
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

// ResolveCandidates returns the project's memories that are eligible for
// resolution classification: not yet resolved, not pinned, and not in a
// standing-preference category (convention/preference are never evictable —
// see the guardrail in the resolution-classifier spec §4). Globals are
// excluded by the project_id filter. Newest first, so a batch reviews the most
// recent work first.
func (s *Store) ResolveCandidates(ctx context.Context, projectID string) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, category, content, importance, access_count,
		       last_accessed, source, tags, pinned, created_at, updated_at
		FROM memories
		WHERE project_id = ?
		  AND resolved_at IS NULL
		  AND pinned = 0
		  AND category NOT IN ('convention', 'preference')
		ORDER BY created_at DESC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("resolve candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMemories(rows)
}

// setResolvedBatchSize bounds how many IDs go into a single IN (...) clause
// per SetResolved call, well under SQLite's SQLITE_MAX_VARIABLE_NUMBER
// (32766 on modern builds) so an unusually large batch can't hit that limit.
const setResolvedBatchSize = 500

// SetResolved stamps resolved_at = now on the given memory IDs, dropping them
// from the ranked injection/browse surface while leaving them searchable. The
// WHERE clause re-checks the same eligibility guard as ResolveCandidates
// (unresolved, unpinned, non-exempt category) at write time, not just at read
// time — a candidate pinned or recategorized during the classify loop is
// excluded rather than stamped anyway. Returns the count actually stamped,
// which callers should report instead of len(ids). A no-op on an empty slice.
func (s *Store) SetResolved(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	total := 0
	for len(ids) > 0 {
		batch := ids
		if len(batch) > setResolvedBatchSize {
			batch = ids[:setResolvedBatchSize]
		}
		ids = ids[len(batch):]

		placeholders := make([]string, len(batch))
		args := make([]interface{}, len(batch))
		for i, id := range batch {
			placeholders[i] = "?"
			args[i] = id
		}
		q := `UPDATE memories SET resolved_at = datetime('now')
		      WHERE id IN (` + strings.Join(placeholders, ",") + `)
		        AND resolved_at IS NULL
		        AND pinned = 0
		        AND category NOT IN ('convention', 'preference')`
		result, err := s.db.ExecContext(ctx, q, args...)
		if err != nil {
			return total, fmt.Errorf("set resolved: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return total, fmt.Errorf("set resolved rows affected: %w", err)
		}
		total += int(n)
	}
	return total, nil
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

// UpdateMemory applies a partial update to a memory. Nil content/category/
// importance preserve current values; a non-nil tags slice replaces the tag
// list (pass an empty slice to clear). Source and pinned are never touched.
// When the content changes, the embedding and link-scan rows are deleted in
// the same transaction so the background workers re-embed and re-link the
// memory; FTS needs nothing — the memories_au trigger re-syncs it.
//
// The write is project-scoped: the in-transaction ownership lookup is keyed
// by (id, project_id), so ownership is verified and the update applied
// atomically inside a single transaction — closing the check-then-write race
// where a separate lookup-then-write could target a memory that moved to a
// different project (e.g. via PromoteToGlobal) between the check and the write.
func (s *Store) UpdateMemory(ctx context.Context, projectID, id string, content, category *string, importance *float32, tags []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var curContent, curCategory, curTags string
	var curImportance float32
	err = tx.QueryRowContext(ctx,
		`SELECT content, category, importance, tags FROM memories WHERE id = ? AND project_id = ?`, id, projectID,
	).Scan(&curContent, &curCategory, &curImportance, &curTags)
	if err == sql.ErrNoRows {
		return fmt.Errorf("memory %s not found in project %s", id, projectID)
	}
	if err != nil {
		return fmt.Errorf("lookup memory: %w", err)
	}

	newContent, newCategory, newImportance, newTags := curContent, curCategory, curImportance, curTags
	if content != nil {
		newContent = *content
	}
	if category != nil {
		newCategory = *category
	}
	if importance != nil {
		newImportance = *importance
	}
	if tags != nil {
		b, mErr := json.Marshal(tags)
		if mErr != nil {
			return fmt.Errorf("marshal tags: %w", mErr)
		}
		newTags = string(b)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE memories
		SET content = ?, category = ?, importance = ?, tags = ?, updated_at = datetime('now'),
		    resolved_at = NULL
		WHERE id = ?
	`, newContent, newCategory, newImportance, newTags, id); err != nil {
		return fmt.Errorf("update memory: %w", err)
	}

	if newContent != curContent {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM memory_embeddings WHERE memory_id = ?`, id); err != nil {
			return fmt.Errorf("invalidate embedding: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM link_scans WHERE memory_id = ?`, id); err != nil {
			return fmt.Errorf("invalidate link scan: %w", err)
		}
	}
	return tx.Commit()
}

// PromoteToGlobal moves a memory into the shared _global project. The memory
// keeps its ID, so its embedding and graph links survive; pinned state and
// importance are preserved. The _global project row is ensured inline (the
// FK on memories.project_id requires it) — EnsureProject can't be called
// here because s.mu is already held.
//
// The write is project-scoped: the UPDATE's WHERE clause binds both id and
// projectID, so ownership is verified and the move applied atomically in a
// single statement — closing the check-then-write race where a separate
// lookup-then-write could promote a memory that moved to a different
// project between the check and the write. As a side effect, promoting an
// already-global memory (project_id = '_global') also fails this ownership
// match, which is desired: re-promotion is rejected with a
// not-found-in-project error, not a silently accepted no-op.
func (s *Store) PromoteToGlobal(ctx context.Context, projectID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')
		ON CONFLICT(id) DO NOTHING
	`); err != nil {
		return fmt.Errorf("ensure _global project: %w", err)
	}
	_, _ = s.db.ExecContext(ctx, `INSERT OR IGNORE INTO ghost_state (project_id) VALUES ('_global')`)

	res, err := s.db.ExecContext(ctx, `
		UPDATE memories SET project_id = '_global', updated_at = datetime('now')
		WHERE id = ? AND project_id = ?
	`, id, projectID)
	if err != nil {
		return fmt.Errorf("promote memory: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("promote memory rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("memory %s not found in project %s", id, projectID)
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
	res, err := s.db.ExecContext(ctx, `
		UPDATE memories SET pinned = ?, updated_at = datetime('now') WHERE id = ?
	`, pinnedInt, id)
	if err != nil {
		return fmt.Errorf("toggle pin: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("toggle pin rows: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("memory %s not found", id)
	}
	return nil
}

// CurrentTimestamp returns the database's own notion of "now", formatted
// exactly like the created_at columns (UTC, via SQLite's datetime('now')).
// Callers can later filter rows by created_at against this value with a plain
// string comparison, with no clock-skew risk between the Go process and SQLite.
func (s *Store) CurrentTimestamp(ctx context.Context) (string, error) {
	var ts string
	if err := s.db.QueryRowContext(ctx, `SELECT datetime('now')`).Scan(&ts); err != nil {
		return "", fmt.Errorf("current timestamp: %w", err)
	}
	return ts, nil
}

// ReplaceNonManual atomically replaces all non-manual memories for a project.
// Manual-sourced memories are preserved. Refuses to replace with an empty set.
//
// consolidatedSince should be a timestamp (see CurrentTimestamp) captured
// before the caller fetched the memories it fed to the consolidator. ghost
// reflect runs as a separate process from the long-lived MCP server, so a
// ghost_memory_save landing on the live server during the multi-minute
// consolidation round trip would otherwise be silently deleted here — it was
// durably written but never part of what the consolidator saw. Any non-manual
// memory created at/after that timestamp is preserved through the replace
// instead. Pass "" to skip the check (tests that don't exercise the race).
func (s *Store) ReplaceNonManual(ctx context.Context, projectID string, memories []Memory, consolidatedSince string) error {
	if len(memories) == 0 {
		return fmt.Errorf("refusing to replace memories with empty set — reflection likely malformed")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Snapshot existing non-manual memories before deleting. Pinned memories
	// are excluded throughout this function — like source='manual', a pin is
	// an explicit user override that reflection must never delete, rewrite,
	// or silently drop (it has no way to know a consolidated memory it emits
	// corresponds to a pinned one it never saw as such, so preservation has
	// to mean "don't touch it" rather than "carry the flag through").
	snapshotID := fmt.Sprintf("%s-%d", projectID, time.Now().Unix())
	_, err = tx.ExecContext(ctx, `
		INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source, tags)
		SELECT ?, project_id, category, content, importance, source, tags
		FROM memories WHERE project_id = ? AND source != 'manual' AND pinned = 0
	`, snapshotID, projectID)
	if err != nil {
		return fmt.Errorf("snapshot memories: %w", err)
	}

	// Collect memories saved concurrently with the consolidation round trip —
	// the consolidator never saw them, so the delete below must not be their
	// end. >= (not >) errs toward a rare same-second duplicate, which the next
	// reflection merges, rather than toward silent loss.
	var preserved []Memory
	if consolidatedSince != "" {
		rows, err := tx.QueryContext(ctx, `
			SELECT category, content, importance, source, tags
			FROM memories WHERE project_id = ? AND source != 'manual' AND pinned = 0 AND created_at >= ?
		`, projectID, consolidatedSince)
		if err != nil {
			return fmt.Errorf("find concurrent memories: %w", err)
		}
		for rows.Next() {
			var m Memory
			var tags string
			if err := rows.Scan(&m.Category, &m.Content, &m.Importance, &m.Source, &tags); err != nil {
				rows.Close() //nolint:errcheck
				return fmt.Errorf("scan concurrent memory: %w", err)
			}
			_ = json.Unmarshal([]byte(tags), &m.Tags)
			preserved = append(preserved, m)
		}
		rowsErr := rows.Err()
		rows.Close() //nolint:errcheck
		if rowsErr != nil {
			return fmt.Errorf("iterate concurrent memories: %w", rowsErr)
		}
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM memories WHERE project_id = ? AND source != 'manual' AND pinned = 0`, projectID)
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

	// Re-insert the concurrently-saved memories with their original source, so
	// the next reflection sees them as ordinary consolidation input.
	for _, m := range preserved {
		tags, _ := json.Marshal(m.Tags)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO memories (project_id, category, content, source, importance, tags)
			VALUES (?, ?, ?, ?, ?, ?)
		`, projectID, m.Category, m.Content, m.Source, m.Importance, string(tags))
		if err != nil {
			return fmt.Errorf("insert preserved memory: %w", err)
		}
	}
	if len(preserved) > 0 {
		s.logger.Info("preserved concurrently-saved memories across reflection replace",
			"project_id", projectID, "count", len(preserved))
	}

	// Prune old snapshots — keep only the 3 most recent per project.
	_, err = tx.ExecContext(ctx, `
		DELETE FROM memory_snapshots
		WHERE project_id = ? AND snapshot_id NOT IN (
			SELECT DISTINCT snapshot_id FROM memory_snapshots
			WHERE project_id = ?
			ORDER BY created_at DESC
			LIMIT 3
		)
	`, projectID, projectID)
	if err != nil {
		s.logger.Warn("prune old snapshots", "error", err, "project_id", projectID)
	}

	s.logger.Info("memories snapshotted before replace", "project_id", projectID, "snapshot_id", snapshotID)
	return tx.Commit()
}

// RestoreSnapshot restores memories from the most recent snapshot for a project.
// Returns the number of memories restored.
func (s *Store) RestoreSnapshot(ctx context.Context, projectID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Find the latest snapshot.
	var snapshotID string
	err := s.db.QueryRowContext(ctx, `
		SELECT snapshot_id FROM memory_snapshots
		WHERE project_id = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, projectID).Scan(&snapshotID)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("no snapshots found for project %s", projectID)
	}
	if err != nil {
		return 0, fmt.Errorf("find snapshot: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Delete current non-manual memories. Pinned memories are excluded, same
	// as ReplaceNonManual: reflection never touched them, so restore must not
	// delete them either — they aren't in the snapshot to bring back.
	_, err = tx.ExecContext(ctx, `DELETE FROM memories WHERE project_id = ? AND source != 'manual' AND pinned = 0`, projectID)
	if err != nil {
		return 0, fmt.Errorf("delete current: %w", err)
	}

	// Restore from snapshot.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO memories (project_id, category, content, source, importance, tags)
		SELECT project_id, category, content, source, importance, tags
		FROM memory_snapshots WHERE snapshot_id = ?
	`, snapshotID)
	if err != nil {
		return 0, fmt.Errorf("restore snapshot: %w", err)
	}

	// Clean up the used snapshot.
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_snapshots WHERE snapshot_id = ?`, snapshotID); err != nil {
		s.logger.Warn("failed to delete used snapshot", "snapshot_id", snapshotID, "error", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}

	n, _ := res.RowsAffected()
	s.logger.Info("memories restored from snapshot", "project_id", projectID, "snapshot_id", snapshotID, "count", n)
	return int(n), nil
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
	defer func() { _ = rows.Close() }()

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
	if err != nil {
		return fmt.Errorf("record usage: %w", err)
	}
	return nil
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
// upsertMergeThreshold is the minimum Jaccard similarity between full token
// sets for Upsert to treat two memories as duplicates. Matches the 0.5 gate
// reflection's SQLite tier uses (internal/reflection/tier_sqlite.go), so
// save-time and reflection-time dedup agree on what "duplicate" means for
// same-length restatements.
//
// Save-time dedup deliberately goes *further* than reflection's tier via the
// overlap leg below: Upsert sees one candidate against a live corpus and its
// misses accumulate in the ranked window until they bury distinct facts,
// whereas reflection re-scores the whole set offline and has an LLM tier
// behind it. The divergence is intentional, not drift.
const upsertMergeThreshold = 0.5

// The overlap leg's gates. The overlap coefficient's one known failure mode is
// subset-of-a-much-larger-text: a short save whose few tokens all happen to
// appear in a long unrelated memory scores 1.0. The min-token and min-ratio
// gates — not the threshold — are what exclude that shape, which is why the
// threshold can sit as low as it does.
//
//   - upsertOverlapThreshold: minimum |A∩B| / min(|A|,|B|) — at least half of
//     the shorter memory's distinct vocabulary must also appear in the other.
//     0.5 is empirical, not inherited from the Jaccard gate it happens to
//     equal: the same-fact restatements in
//     TestStoreUpsertMergesLengthAsymmetricParaphrases score 0.857, 0.600 and
//     0.545 against the accumulated memory, so any threshold above 0.545
//     leaves saves that plainly restate one fact unmerged.
//   - upsertOverlapMinTokens: minimum token count on the shorter side. Below
//     it, containment is too easy to hit by accident.
//   - upsertOverlapMinRatio: minimum len(shorter)/len(longer). Genuine
//     restatements of one fact are comparably sized (~0.9); the accidental
//     containment case is lopsided (~0.2). A terse fact and a much wordier
//     version of it are therefore NOT merged — deliberately, because nothing
//     lexical distinguishes that from accidental containment, and a false
//     merge destroys information while a false split is recoverable by
//     reflection.
const (
	upsertOverlapThreshold = 0.5
	upsertOverlapMinTokens = 5
	upsertOverlapMinRatio  = 0.6
)

// tokenizeContent lowercases s and splits it into a set of alphanumeric
// tokens longer than one rune. Mirrors reflection's tokenize so both dedup
// layers score similarity identically.
func tokenizeContent(s string) map[string]bool {
	tokens := make(map[string]bool)
	for _, word := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(word) > 1 {
			tokens[word] = true
		}
	}
	return tokens
}

// jaccard computes the Jaccard similarity coefficient between two token sets.
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1.0
	}
	intersection := 0
	for token := range a {
		if b[token] {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// overlapCoefficient (Szymkiewicz–Simpson) is |A∩B| / min(|A|,|B|). Unlike
// Jaccard it does not penalize length asymmetry, so a terse fact and a verbose
// restatement of the same fact score high instead of being pulled apart by the
// longer side's extra filler tokens.
func overlapCoefficient(a, b map[string]bool) float64 {
	smaller := len(a)
	if len(b) < smaller {
		smaller = len(b)
	}
	if smaller == 0 {
		return 0
	}
	intersection := 0
	for token := range a {
		if b[token] {
			intersection++
		}
	}
	return float64(intersection) / float64(smaller)
}

// mergeScore reports whether two candidate contents' token sets are
// near-duplicates for Upsert purposes.
//
// Jaccard alone under-merges on the dominant real-world case: the same fact
// restated at a different length. "cache TTL is 300s" vs "the cache TTL is set
// to 300 seconds" is Jaccard 0.22 — far below any threshold that isn't itself
// catastrophically over-merging — because the union grows with the wordier
// side while the intersection cannot. The overlap coefficient scores that pair
// 0.75 and is the standard fix for exactly this asymmetry.
//
// Overlap alone over-merges though: a 2-token save whose tokens both happen to
// appear in a long unrelated memory scores 1.0. So the overlap leg is gated on
// token count and length ratio (see the constants) to exclude that shape.
// Anything the gates reject keeps the original Jaccard-only behavior.
// It returns 0 when the pair must not merge, and otherwise a positive score
// used only to pick the best of several candidates.
func mergeScore(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if j := jaccard(a, b); j >= upsertMergeThreshold {
		return j
	}
	smaller, larger := len(a), len(b)
	if larger < smaller {
		smaller, larger = larger, smaller
	}
	if smaller < upsertOverlapMinTokens {
		return 0
	}
	if float64(smaller)/float64(larger) < upsertOverlapMinRatio {
		return 0
	}
	if o := overlapCoefficient(a, b); o >= upsertOverlapThreshold {
		return o
	}
	return 0
}

// sanitizeFTS sanitizes text into an FTS5 OR-query, capped at 10 words. Used
// on the search path (SearchFTS, SearchFTSAll) — kept at the original cap so
// retrieval-quality behavior (see TestBenchRegressionFloors) is unaffected.
func sanitizeFTS(text string) string {
	return sanitizeFTSN(text, 10)
}

// sanitizeFTSN sanitizes text into an FTS5 OR-query, capped at maxWords.
// Upsert's duplicate-recall probe uses a wider cap than plain search
// (see sanitizeFTS) because a broader OR-probe over more of the content
// improves candidate recall for the precision pass (mergeScore) that follows;
// widening the cap globally would instead perturb ranked search results.
func sanitizeFTSN(text string, maxWords int) string {
	// Remove FTS5 operators and punctuation, keep only words.
	var words []string
	for _, word := range strings.Fields(text) {
		// Strip non-alphanumeric characters from edges only — interior
		// punctuation (192.168.9.150, sealed-secrets) is preserved so FTS5's
		// tokenizer can still split and match exact identifiers.
		clean := strings.TrimFunc(word, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
		})
		if len(clean) >= 1 {
			// Quote each word to treat as literal. An interior quote survives
			// the edge trim and must be doubled per FTS5's escaping rule —
			// otherwise it unbalances the "..." wrapper and lets the token
			// re-enter raw FTS5 query grammar instead of staying a literal.
			escaped := strings.ReplaceAll(clean, `"`, `""`)
			words = append(words, `"`+escaped+`"`)
		}
	}
	if len(words) == 0 {
		return `""`
	}
	// Limit to first maxWords words to keep the query reasonable.
	if len(words) > maxWords {
		slog.Warn("fts query truncated",
			"original_terms", len(words),
			"limit", maxWords)
		words = words[:maxWords]
	}
	return strings.Join(words, " OR ")
}
