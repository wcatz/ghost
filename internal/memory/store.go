package memory

// Package memory provides the SQLite-backed persistence layer for Ghost memories,
// including CRUD operations, FTS5 search, hybrid vector/FTS retrieval, time-decay
// scoring with category-aware decay, pin exemptions, project lifecycle management,
// tasks, decisions, memory links (supersedes/contradicts/elaborates/causes).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
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
	ResolvedAt   *string  `json:"resolved_at,omitempty"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`

	// Provenance: which harness wrote this, in which session, against which
	// reference, and how much it was trusted.
	//
	// The three strings treat "" as unknown and carry omitempty, since there
	// is no meaningful difference between "no agent recorded" and "an agent
	// whose name is empty". Confidence is a pointer instead: 0.0 is a valid
	// rating, so nil — not zero — has to mean "no belief was recorded".
	//
	// The database column is NULL for every memory written before schema v10
	// and for every write that passes no Provenance. That is deliberate —
	// Ghost never learned those values, and inventing one would be a claim
	// nobody made.
	Agent      string   `json:"agent,omitempty"`
	SessionID  string   `json:"session_id,omitempty"`
	SourceRef  string   `json:"source_ref,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`

	// Scope names where this memory applies — environment, component, and so
	// on — as machine-readable values rather than words in the sentence.
	//
	// nil means "applies everywhere", which is not the same as "applies
	// nowhere": a memory with no stated scope is the general knowledge worth
	// keeping, and ScopeMatches treats it as eligible for every request. The
	// column is NULL for anything written before schema v12, since inventing
	// a scope for existing memories would claim where they apply.
	Scope map[string]string `json:"scope,omitempty"`
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

	// vectorMinSimilarity is the cosine floor applied to the vector leg of
	// hybrid search (SearchParams.MinSimilarity). Defaults to 0 (historical:
	// only non-positive cosines dropped); config search.min_similarity
	// overrides via SetVectorMinSimilarity.
	vectorMinSimilarity float32
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

// SetVectorMinSimilarity overrides the vector-leg cosine floor hybrid search
// applies before fusion. Call after NewStore once config is loaded; until
// called, the floor is 0 (only non-positive cosines dropped).
func (s *Store) SetVectorMinSimilarity(floor float32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vectorMinSimilarity = floor
}

// vectorMinSimilarityFloor returns the configured floor under the read lock.
func (s *Store) vectorMinSimilarityFloor() float32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.vectorMinSimilarity
}

// NewStore creates a new memory store from an open database.
//
// A nil logger means "log nothing" and is honoured as such: Store has over a
// dozen unguarded s.logger.X call sites, and Go's log/slog panics on a nil
// *Logger receiver (Info, Warn and Debug all dereference l.Handler), so a nil
// passed straight through would turn any logging call into a crash. Call
// sites such as mcpinit's lifecycle lock and marker paths pass nil
// deliberately — they build a Store only to resolve a project identifier and
// have no logger yet — and a discarded handler preserves that silence rather
// than forcing every caller to construct one.
func NewStore(db *sql.DB, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
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

// GetProjectPath returns the filesystem path recorded for a project. Callers
// use it to inspect the working tree (e.g. reflection's git context); a
// missing project is an error, but callers that treat the path as optional
// can ignore it.
func (s *Store) GetProjectPath(ctx context.Context, id string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var path string
	err := s.db.QueryRowContext(ctx, `SELECT path FROM projects WHERE id = ?`, id).Scan(&path)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("project %s not found", id)
	}
	if err != nil {
		return "", fmt.Errorf("get project path: %w", err)
	}
	return path, nil
}

// EnsureProject creates a project record if it doesn't exist.
// When called with an absolute path, it auto-merges any same-name project
// that was created with a non-absolute path (e.g., by MCP using name-as-ID).
// EnsureProject creates or refreshes a project with no repository identity.
// Callers that know the filesystem path should prefer EnsureProjectWithRepo,
// which lets two checkouts of one repository collapse into a single project.
func (s *Store) EnsureProject(ctx context.Context, id, path, name string) error {
	return s.ensureProjectLocked(ctx, id, path, name, "")
}

// EnsureProjectWithRepo is EnsureProject plus the normalized remote URL of the
// repository at path.
//
// When another project already claims that remote, this merges into it instead
// of creating a second project — mirroring the path-based self-heal below.
// That is the case repository identity exists for: ~/src/ghost and
// ~/work/ghost are different paths, and an agent that changes working
// directory would otherwise start a second project and lose everything the
// first one knew.
//
// repoRemote may be empty. An empty value never clears a remote already on
// record, so callers that cannot inspect a path (MCP tools pass path="") do
// not erase identity that a caller which could inspect it had established.
func (s *Store) EnsureProjectWithRepo(ctx context.Context, id, path, name, repoRemote string) error {
	return s.ensureProjectLocked(ctx, id, path, name, repoRemote)
}

// ResolveOrBindRepoRemote returns the project already carrying repoRemote. If
// none exists, it first considers an explicit projectRef (the id or path the
// caller supplied), then attaches the remote to the one project named name when
// that project has not claimed a different repository. An explicit projectRef
// already bound elsewhere is an error: falling through to its same-named
// project would route the save away from the location the caller supplied.
//
// Callers must pass a name derived from the repository itself, not a directory
// basename. This is a write-side bridge for a repository-aware save arriving
// before a named project has recorded its remote; it deliberately does not
// participate in Store.ResolveProject's location-sensitive lookup rules.
func (s *Store) ResolveOrBindRepoRemote(ctx context.Context, projectRef, name, repoRemote string) (id string, matched bool, err error) {
	repoRemote = NormalizeRepoRemote(repoRemote)
	if repoRemote == "" {
		return "", false, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var existingID string
	existingErr := s.db.QueryRowContext(ctx, `
		SELECT id FROM projects
		WHERE repo_remote = ? AND id != '_global'
		LIMIT 1
	`, repoRemote).Scan(&existingID)
	if existingErr == nil {
		return existingID, true, nil
	}
	if existingErr != sql.ErrNoRows {
		return "", false, fmt.Errorf("resolve project by repository: %w", existingErr)
	}

	if id, found, err := s.resolveExplicitProjectRepoLocked(ctx, projectRef, repoRemote); found || err != nil {
		return id, found, err
	}
	if name == "" {
		return "", false, nil
	}
	return s.bindUniqueProjectNameRepoLocked(ctx, name, repoRemote)
}

func (s *Store) resolveExplicitProjectRepoLocked(ctx context.Context, projectRef, repoRemote string) (id string, found bool, err error) {
	if projectRef == "" {
		return "", false, nil
	}

	var count int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM projects
		WHERE (id = ? OR path = ?) AND id != '_global'
	`, projectRef, projectRef).Scan(&count); err != nil {
		return "", false, fmt.Errorf("count explicit projects for repository: %w", err)
	}
	if count > 1 {
		return "", false, fmt.Errorf("project reference %q matches multiple projects", projectRef)
	}
	if count == 0 {
		return "", false, nil
	}

	var existingRemote string
	if err := s.db.QueryRowContext(ctx, `
		SELECT id, COALESCE(repo_remote, '')
		FROM projects
		WHERE (id = ? OR path = ?) AND id != '_global'
	`, projectRef, projectRef).Scan(&id, &existingRemote); err != nil {
		return "", true, fmt.Errorf("read explicit project for repository: %w", err)
	}
	if existingRemote == repoRemote {
		return id, true, nil
	}
	if existingRemote != "" {
		return "", true, fmt.Errorf("project %q belongs to a different repository", id)
	}

	bound, err := s.bindRepoRemoteIfUnsetLocked(ctx, id, repoRemote)
	if err != nil {
		return "", true, fmt.Errorf("bind repository to explicit project: %w", err)
	}
	if !bound {
		return "", true, fmt.Errorf("project %q repository changed concurrently", id)
	}
	return id, true, nil
}

func (s *Store) bindUniqueProjectNameRepoLocked(ctx context.Context, name, repoRemote string) (id string, matched bool, err error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM projects
		WHERE name = ? AND id != '_global'
	`, name).Scan(&count); err != nil {
		return "", false, fmt.Errorf("count projects by name for repository: %w", err)
	}
	if count != 1 {
		return "", false, nil
	}

	var existingRemote string
	if err := s.db.QueryRowContext(ctx, `
		SELECT id, COALESCE(repo_remote, '')
		FROM projects
		WHERE name = ? AND id != '_global'
	`, name).Scan(&id, &existingRemote); err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read project by name for repository: %w", err)
	}
	if existingRemote != "" {
		if existingRemote == repoRemote {
			return id, true, nil
		}
		return "", false, nil
	}

	bound, err := s.bindRepoRemoteIfUnsetLocked(ctx, id, repoRemote)
	if err != nil {
		return "", false, fmt.Errorf("bind repository to named project: %w", err)
	}
	return id, bound, nil
}

func (s *Store) bindRepoRemoteIfUnsetLocked(ctx context.Context, id, repoRemote string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET repo_remote = ?, updated_at = datetime('now')
		WHERE id = ? AND COALESCE(repo_remote, '') = ''
	`, repoRemote, id)
	if err != nil {
		return false, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return updated == 1, nil
}

func (s *Store) ensureProjectLocked(ctx context.Context, id, path, name, repoRemote string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Normalize empty path to id. MCP callers pass path="" because they
	// don't know the filesystem path; using "" as path would collide across
	// projects since projects.path has a UNIQUE constraint. Using id as path
	// preserves the invariant already on disk for name-as-ID projects.
	if path == "" {
		path = id
	}

	// Repository identity: one remote means one project, whatever the local
	// checkout path happens to be. _global is excluded on both sides for the
	// same reason MergeProject refuses it — merging it would move or dump the
	// bucket that global injection reads from.
	repoRemote = NormalizeRepoRemote(repoRemote)
	if repoRemote != "" && id != "_global" {
		var incomingRemote string
		incomingErr := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, id).Scan(&incomingRemote)
		if incomingErr != nil && incomingErr != sql.ErrNoRows {
			return fmt.Errorf("check project repository: %w", incomingErr)
		}
		if incomingRemote != "" && incomingRemote != repoRemote {
			return fmt.Errorf("project %q belongs to a different repository", id)
		}

		var existingID string
		scanErr := s.db.QueryRowContext(ctx,
			`SELECT id FROM projects WHERE repo_remote = ? AND id != ? LIMIT 1`,
			repoRemote, id).Scan(&existingID)
		if scanErr == nil && existingID != "" && existingID != "_global" {
			// mergeProjectLocked does not lock internally (only the public
			// MergeProject wrapper does), and we already hold s.mu.
			if err := s.mergeProjectLocked(ctx, id, existingID); err != nil {
				return fmt.Errorf("auto-merge repository duplicate: %w", err)
			}
			return nil
		}
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
		INSERT INTO projects (id, path, name, repo_remote) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			path = CASE WHEN excluded.path = excluded.id THEN projects.path ELSE excluded.path END,
			repo_remote = CASE
				WHEN excluded.repo_remote = '' THEN projects.repo_remote
				WHEN projects.repo_remote IS NULL OR projects.repo_remote = '' THEN excluded.repo_remote
				ELSE projects.repo_remote
			END,
			updated_at = datetime('now')
	`, id, path, name, repoRemote)
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
// Refuses _global on either side, mirroring DeleteProject: merging away from
// _global would delete its project row and silently stop global injection
// everywhere; merging into it would dump an arbitrary bucket into every
// session's context.
func (s *Store) MergeProject(ctx context.Context, oldID, newID string) error {
	if oldID == "_global" || newID == "_global" {
		return fmt.Errorf("refusing to merge the _global project (old=%q new=%q)", oldID, newID)
	}
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
		`UPDATE tasks SET project_id = ? WHERE project_id = ?`,
		`UPDATE decisions SET project_id = ? WHERE project_id = ?`,
		`UPDATE token_usage SET project_id = ? WHERE project_id = ?`,
		`UPDATE audit_log SET project_id = ? WHERE project_id = ?`,
		// Snapshots too: memory_snapshots.project_id is ON DELETE CASCADE, so
		// omitting this would let the DELETE FROM projects below silently
		// destroy the merged project's entire undo history.
		`UPDATE memory_snapshots SET project_id = ? WHERE project_id = ?`,
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
// tasks, decisions, ghost_state, and memory_snapshots all cascade from the
// projects row via ON DELETE CASCADE (see schema.go). token_usage and
// audit_log carry a project_id column but no foreign key, so they're deleted
// explicitly in the same transaction.
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

	// This log line is the durable record of what was removed. (audit_log is a
	// legacy table with no production writer — it is counted and deleted here
	// for older databases, not because deletions are otherwise recorded there.)
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
// detectRemoteForPath maps a filesystem path to its repository remote.
//
// It is a package variable rather than a call to git because internal/memory
// is deliberately a pure storage layer that never spawns a process. The
// capability is injected instead: the application wires it in once, tests can
// pin it, and a store built without one resolves exactly as it did before
// repository identity existed. The zero value (the default) disables the
// feature rather than half-enabling it.
var detectRemoteForPath = func(string) string { return "" }

// SetDetectRemote wires the repository detector used when resolving a project
// by filesystem path. Passing nil restores the default no-op.
func SetDetectRemote(fn func(dir string) string) {
	if fn == nil {
		detectRemoteForPath = func(string) string { return "" }
		return
	}
	detectRemoteForPath = fn
}

// Lookup order, first hit wins:
//  1. exact id = input
//  2. exact name = input
//  3. if input contains '/' or '\': input = path OR input has literal prefix
//     path + "/" (ordered by LENGTH(path) DESC — longest/most-specific match
//     wins; LENGTH(path) > 10 guards against a short project path matching too
//     broadly, matching the hook's original lookupProject behavior; the
//     prefix check is a literal substr comparison, not LIKE, so '%'/'_' in a
//     stored path can't act as SQL wildcards against input; input and stored
//     path are compared both raw and slash-normalized so native-Windows
//     backslash paths resolve too)
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

	if strings.ContainsAny(input, `/\`) {
		// Literal prefix comparison, not LIKE: a stored path containing '%' or
		// '_' must not be treated as a SQL wildcard against input.
		// The gate works by detecting '\' as well as '/'; REPLACE below
		// normalizes the STORED path so the SQL comparisons match slash or
		// backslash inputs on native Windows. SQLite string literals don't
		// process backslash escapes, so '\' in SQL is one literal backslash.
		// Normalization is done with strings.ReplaceAll, not filepath.ToSlash:
		// ToSlash is a no-op for backslashes off-Windows (it only swaps
		// filepath.Separator), so it can't be exercised or relied on in Linux
		// tests.
		norm := strings.ReplaceAll(input, `\`, "/")
		err = s.db.QueryRowContext(ctx, `
			SELECT id, name FROM projects
			WHERE (path = ?
			    OR REPLACE(path, '\', '/') = ?
			    OR substr(?, 1, LENGTH(REPLACE(path, '\', '/')) + 1) = REPLACE(path, '\', '/') || '/')
			  AND LENGTH(path) > 10
			ORDER BY LENGTH(path) DESC LIMIT 1
		`, input, norm, norm).Scan(&id, &name)
		if err == nil {
			return id, name, nil
		}
		if err != sql.ErrNoRows {
			return "", "", fmt.Errorf("resolve project by path: %w", err)
		}
	}

	// Repository identity, ahead of the basename fallback: a remote is more
	// specific than a bare directory name, and a caller naming a repository
	// (git@host:owner/repo.git, https://host/owner/repo) is asking for that
	// repository, not for whichever project happens to sit in a folder called
	// "repo". Normalization makes every spelling of one remote compare equal.
	//
	// The input may also be a filesystem path — the second checkout of a
	// repository whose first checkout already created a project. A path
	// carries no identity of its own, so it is resolved through the injected
	// detector; without that, saving from ~/work/ghost and then reading from
	// it would disagree about which project it is. Detection runs only for
	// absolute inputs, so resolving by id or name never spawns a process.
	remote := NormalizeRepoRemote(input)
	if remote == "" && filepath.IsAbs(input) {
		remote = NormalizeRepoRemote(detectRemoteForPath(input))
	}
	if remote != "" {
		err = s.db.QueryRowContext(ctx, `SELECT id, name FROM projects WHERE repo_remote = ? LIMIT 1`, remote).Scan(&id, &name)
		if err == nil {
			return id, name, nil
		}
		if err != sql.ErrNoRows {
			return "", "", fmt.Errorf("resolve project by repository: %w", err)
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
		INSERT INTO memories (project_id, category, content, source, importance, tags,
		                      agent, session_id, source_ref, confidence, scope)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id
	`, projectID, m.Category, m.Content, m.Source, m.Importance, string(tags),
		nullIfEmpty(m.Agent), nullIfEmpty(m.SessionID),
		nullIfEmpty(m.SourceRef), m.Confidence, scopeJSON(m.Scope)).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create memory: %w", err)
	}
	if s.onSave != nil {
		s.onSave(projectID)
	}
	return id, nil
}

// Upsert checks for an existing similar memory, probing in two stages:
// same-category first (existing bar: mergeScore — Jaccard >= 0.5 or the
// gated overlap leg), then — only when the same-category probe misses —
// cross-category candidates at a deliberately higher bar: token Jaccard >=
// upsertCrossCategoryThreshold (0.7), dead records excluded. On a hit from
// either probe it strengthens the existing memory's importance/access_count
// and links the new content to it as a 'duplicate' — it never overwrites the
// existing row's content or its category (the fold target keeps whatever
// category it already has, so a save never silently recategorizes a
// pinned/canonical memory; the linked copy carries the incoming category so
// nothing the caller saved is lost), so a genuinely different follow-up save
// under a manual-sourced target (or any target) is never silently discarded.
// If no candidate scores above the applicable bar, it creates a new,
// unlinked row.
// Provenance is the optional write-time context attached to a single save.
// Every field is optional and its zero value means "not known": passing no
// Provenance at all records NULL across the board rather than a guess.
//
// SessionID is deliberately not auto-populated. Ghost has no session identity
// available on the MCP path — no column, no handshake field, no env marker —
// so it stays empty until a caller that genuinely knows supplies it. An
// auto-filled value would be fabricated provenance, which is the exact thing
// these columns exist to avoid.
type Provenance struct {
	// Agent names the harness that produced the memory. The values are the
	// source tokens ai.NewSourceProviderForSource understands — claude-code,
	// opencode, codex, goose — not the binary names. Claude is "claude-code"
	// everywhere it appears as a source (SourceForClientName maps it, and
	// detectSourceFromEnv normalizes CLAUDECODE to it), so storing anything
	// else would split the vocabulary and make a filter on agent miss rows
	// that ghost_resolve and the CLI already treat as claude-code.
	//
	// Distinct from Source, which records *how* the memory arrived (mcp,
	// reflection, manual).
	Agent string
	// SessionID identifies the caller session, when one is known.
	SessionID string
	// SourceRef points at what the memory was read from — a file, path,
	// commit, or URL.
	SourceRef string
	// Confidence is a belief rating in [0,1]. Pointer so that nil means
	// "no belief recorded" while 0.0 remains a real rating.
	Confidence *float64
}

// nullIfEmpty maps an empty provenance string to SQL NULL rather than the
// empty string. The distinction is real: NULL records that Ghost never
// learned an agent or reference, while ” would claim a value that happens
// to be empty. Queries that count recorded provenance rely on IS NOT NULL.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Upsert stores a memory with no provenance, exactly as before.
// UpsertOptions carries the optional facts about one save. Provenance and
// scope are orthogonal — where a memory came from and where it applies — so
// they are separate fields rather than one growing parameter list, and a zero
// UpsertOptions is exactly what plain Upsert passes.
type UpsertOptions struct {
	Provenance Provenance
	Scope      map[string]string
}

func (s *Store) Upsert(ctx context.Context, projectID, category, content, source string, importance float32, tags []string) (id string, duplicateOf string, score float64, err error) {
	return s.UpsertWithOptions(ctx, projectID, category, content, source, importance, tags, UpsertOptions{})
}

// UpsertWithProvenance is Upsert plus optional write-time provenance.
func (s *Store) UpsertWithProvenance(ctx context.Context, projectID, category, content, source string, importance float32, tags []string, prov Provenance) (id string, duplicateOf string, score float64, err error) {
	return s.UpsertWithOptions(ctx, projectID, category, content, source, importance, tags, UpsertOptions{Provenance: prov})
}

// UpsertWithOptions is Upsert plus provenance and/or scope.
func (s *Store) UpsertWithOptions(ctx context.Context, projectID, category, content, source string, importance float32, tags []string, opts UpsertOptions) (id string, duplicateOf string, score float64, err error) {
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
		SELECT m.id, m.importance, m.content, m.scope
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
			var candScope sql.NullString
			if scanErr := rows.Scan(&candID, &candImportance, &candContent, &candScope); scanErr != nil {
				continue
			}
			// A candidate that names a shared key differently is a different
			// fact about a different place, not a second wording of this one.
			// Skipping rather than stopping keeps looking for a compatible
			// candidate among the remaining matches.
			if ScopesConflict(opts.Scope, parseScope(candScope)) {
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

	// Cross-category probe — runs ONLY when the same-category probe above
	// missed (existingID == ""), so a save that folds within its own category
	// pays nothing extra. One FTS query with category != surfaces
	// other-category candidates; precision then requires token Jaccard >=
	// upsertCrossCategoryThreshold (0.7) — stricter than the same-category
	// mergeScore gate, because cross-category vocabulary overlap is common
	// and near-identical wording is the only reliable duplicate signal. The
	// fold target's category is never touched (see the fold path below): the
	// existing memory — possibly pinned/canonical — keeps its category, and
	// the linked copy inserted by the fold carries the incoming category.
	// Resolved records and the targets of active 'supersedes' edges are
	// excluded here so a re-save can never fold into a dead memory; the
	// same-category probe predates those exclusions and keeps its behavior.
	if existingID == "" {
		crossRows, crossErr := s.db.QueryContext(ctx, `
			SELECT m.id, m.importance, m.content, m.scope
			FROM memories m
			JOIN memories_fts f ON f.rowid = m.rowid
			WHERE m.project_id = ?
			  AND m.category != ?
			  AND m.resolved_at IS NULL
			  AND NOT EXISTS (
			      SELECT 1 FROM memory_links l
			      WHERE l.target_id = m.id
			        AND l.relation = 'supersedes'
			        AND l.invalidated_at IS NULL
			  )
			  AND memories_fts MATCH ?
			ORDER BY rank, m.importance DESC
			LIMIT 15
		`, projectID, category, ftsQuery)
		if crossErr == nil {
			// Same empty-token guard as the same-category probe: token-free
			// content can still FTS-match, and jaccard(∅,∅) scores 1.0.
			newTokens := tokenizeContent(content)
			var bestJaccard float64
			for len(newTokens) > 0 && crossRows.Next() {
				var candID, candContent string
				var candImportance float32
				var candScope sql.NullString
				if scanErr := crossRows.Scan(&candID, &candImportance, &candContent, &candScope); scanErr != nil {
					continue
				}
				// The stricter Jaccard gate does not help here: near-identical
				// wording across environments scores high regardless of
				// category, so scope is what separates them.
				if ScopesConflict(opts.Scope, parseScope(candScope)) {
					continue
				}
				j := jaccard(newTokens, tokenizeContent(candContent))
				if j >= upsertCrossCategoryThreshold && j > bestJaccard {
					bestJaccard = j
					existingID = candID
					existingImportance = candImportance
				}
			}
			if rowsErr := crossRows.Err(); rowsErr != nil {
				existingID = "" // treat a broken candidate scan as no match
			}
			_ = crossRows.Close()
			if existingID != "" {
				score = bestJaccard
			}
		}
		// A failed cross probe falls through to the no-match insert below —
		// same failure semantics as the same-category probe (a broken probe
		// must never block a save).
	}

	tagsJSON, _ := json.Marshal(tags)

	if existingID != "" {
		// Found a match (same-category or cross-category probe) — strengthen
		// the existing row, then insert the new content as its own row and
		// link it back as a 'duplicate'. Never overwrite existingContent or
		// the existing row's category: that silently discarded content for
		// source='manual' targets while still reporting a merge, and would
		// recategorize a pinned/canonical fold target. The inserted copy
		// carries the incoming category unchanged.
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
			SET importance = ?, access_count = access_count + 1
			WHERE id = ? AND project_id = ?
		`, newImportance, existingID, projectID); err != nil {
			return "", "", 0, fmt.Errorf("strengthen memory: %w", err)
		}

		if err = tx.QueryRowContext(ctx, `
			INSERT INTO memories (project_id, category, content, source, importance, tags,
			                      agent, session_id, source_ref, confidence, scope)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING id
		`, projectID, category, content, source, importance, string(tagsJSON),
			nullIfEmpty(opts.Provenance.Agent), nullIfEmpty(opts.Provenance.SessionID),
			nullIfEmpty(opts.Provenance.SourceRef), opts.Provenance.Confidence,
			scopeJSON(opts.Scope)).Scan(&id); err != nil {
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
		INSERT INTO memories (project_id, category, content, source, importance, tags,
		                      agent, session_id, source_ref, confidence, scope)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id
	`, projectID, category, content, source, importance, string(tagsJSON),
		nullIfEmpty(opts.Provenance.Agent), nullIfEmpty(opts.Provenance.SessionID),
		nullIfEmpty(opts.Provenance.SourceRef), opts.Provenance.Confidence,
		scopeJSON(opts.Scope)).Scan(&id)
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
// with category-aware time decay and pinned exemption.
func (s *Store) GetTopMemories(ctx context.Context, projectID string, limit int) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, category, content, importance, access_count,
		       last_accessed, source, tags, pinned, resolved_at, created_at, updated_at,
		       agent, session_id, source_ref, confidence, scope
		FROM memories
		WHERE (project_id = ? OR project_id = '_global')
		  AND resolved_at IS NULL
		ORDER BY (`+DecayRankingSQL+`) DESC, importance DESC, created_at DESC, id
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

	// Supersede demote runs whenever the window has a pair to reorder —
	// membership-preserving, same helper the search path uses — so a
	// superseded memory cannot outrank its replacement in injection even
	// when the result fits under limit (order alone still matters).
	if len(results) >= 2 {
		ids := make([]string, len(results))
		for i, m := range results {
			ids[i] = m.ID
		}
		penalty, err := SupersedePenalties(ctx, s.db, ids)
		if err != nil {
			s.logger.Debug("get top memories: supersede demotion lookup failed", "error", err)
		} else if len(penalty) > 0 {
			results = StableDemote(results, func(m Memory) string { return m.ID }, penalty)
		}
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
		       m.last_accessed, m.source, m.tags, m.pinned, m.resolved_at, m.created_at, m.updated_at,
		       m.agent, m.session_id, m.source_ref, m.confidence, m.scope
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
		       m.last_accessed, m.source, m.tags, m.pinned, m.resolved_at, m.created_at, m.updated_at,
		       m.agent, m.session_id, m.source_ref, m.confidence, m.scope
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
		       last_accessed, source, tags, pinned, resolved_at, created_at, updated_at,
		       agent, session_id, source_ref, confidence, scope
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
		       last_accessed, source, tags, pinned, resolved_at, created_at, updated_at,
		       agent, session_id, source_ref, confidence, scope
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
		       last_accessed, source, tags, pinned, resolved_at, created_at, updated_at,
		       agent, session_id, source_ref, confidence, scope
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

// ResolveKeptHashes returns the content hash recorded when resolve last judged
// each memory KEEP, keyed by memory ID. Only rows with a recorded hash appear.
func (s *Store) ResolveKeptHashes(ctx context.Context, projectID string) (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, resolve_kept_hash
		FROM memories
		WHERE project_id = ? AND resolve_kept_hash != ''
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("resolve kept hashes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]string)
	for rows.Next() {
		var id, hash string
		if err := rows.Scan(&id, &hash); err != nil {
			return nil, fmt.Errorf("scan resolve kept hash: %w", err)
		}
		out[id] = hash
	}
	return out, rows.Err()
}

// MarkResolveKept records KEEP verdicts (id -> content hash) for memories
// resolve classified as not-resolved. It deliberately does not touch
// updated_at: the content did not change, and bumping freshness would perturb
// the reflect signature and decay ranking. The update is project-scoped, so a
// stale caller cannot write another project's rows. A no-op on an empty map.
func (s *Store) MarkResolveKept(ctx context.Context, projectID string, hashes map[string]string) error {
	if len(hashes) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mark resolve kept: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for id, hash := range hashes {
		if _, err := tx.ExecContext(ctx,
			`UPDATE memories SET resolve_kept_hash = ? WHERE id = ? AND project_id = ?`,
			hash, id, projectID,
		); err != nil {
			return fmt.Errorf("mark resolve kept %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// SupersedeCheck is a cached NEITHER verdict for an ordered pair: the content
// hashes it was judged on. A pair is only skipped while both hashes still
// match.
type SupersedeCheck struct {
	NewerHash string
	OlderHash string
}

// SupersedeChecked returns the cached NEITHER verdicts for a project, keyed by
// {newerID, olderID}. Pairs never judged NEITHER are absent from the map.
func (s *Store) SupersedeChecked(ctx context.Context, projectID string) (map[[2]string]SupersedeCheck, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT newer_id, older_id, newer_hash, older_hash
		FROM supersede_checked
		WHERE project_id = ?
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("supersede checked: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[[2]string]SupersedeCheck)
	for rows.Next() {
		var newerID, olderID string
		var check SupersedeCheck
		if err := rows.Scan(&newerID, &olderID, &check.NewerHash, &check.OlderHash); err != nil {
			return nil, fmt.Errorf("scan supersede check: %w", err)
		}
		out[[2]string{newerID, olderID}] = check
	}
	return out, rows.Err()
}

// MarkSupersedeNeither records (or refreshes) NEITHER verdicts for a project,
// keyed by {newerID, olderID}. A re-mark updates the stored hashes in place so
// a stale row cannot accumulate beside a refreshed one. A no-op on an empty
// map.
//
// The caller is responsible for projectID matching the pair's memories: unlike
// MarkResolveKept, this does not re-check ownership per row, so a mismatched
// call would label a row with the wrong project. That is safe for the only
// caller today — supersede.Run passes its own projectID and candidate pairs it
// loaded from that project — and memory IDs are globally unique, so a
// mislabeled row still cascades away with its endpoints.
func (s *Store) MarkSupersedeNeither(ctx context.Context, projectID string, checks map[[2]string]SupersedeCheck) error {
	if len(checks) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin mark supersede neither: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for key, check := range checks {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO supersede_checked (newer_id, older_id, project_id, newer_hash, older_hash)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(newer_id, older_id) DO UPDATE SET
				newer_hash = excluded.newer_hash,
				older_hash = excluded.older_hash,
				checked_at = datetime('now')
		`, key[0], key[1], projectID, check.NewerHash, check.OlderHash); err != nil {
			return fmt.Errorf("mark supersede neither %s->%s: %w", key[0], key[1], err)
		}
	}
	return tx.Commit()
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

	// resolved_at is a resolve pass's verdict, not part of the editable fields.
	// Only a content change may clear it — a metadata-only edit (tags,
	// importance, category) must not resurrect resolved evidence into ranked
	// injection. Content edits are treated as the memory being reasserted.
	clearResolved := 0
	if newContent != curContent {
		clearResolved = 1
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE memories
		SET content = ?, category = ?, importance = ?, tags = ?, updated_at = datetime('now'),
		    resolved_at = CASE WHEN ? THEN NULL ELSE resolved_at END
		WHERE id = ?
	`, newContent, newCategory, newImportance, newTags, clearResolved, id); err != nil {
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
// It returns the IDs of memories preserved because they were saved during the
// consolidation round trip (the consolidatedSince race) — callers must not
// treat the post-apply corpus as fully consolidated when this is non-empty.
//
// consolidatedSince should be a timestamp (see CurrentTimestamp) captured
// before the caller fetched the memories it fed to the consolidator. ghost
// reflect runs as a separate process from the long-lived MCP server, so a
// ghost_memory_save landing on the live server during the multi-minute
// consolidation round trip would otherwise be silently deleted here — it was
// durably written but never part of what the consolidator saw. Any non-manual
// memory created at/after that timestamp is preserved through the replace
// instead. Pass "" to skip the check (tests that don't exercise the race).
func (s *Store) ReplaceNonManual(ctx context.Context, projectID string, memories []Memory, consolidatedSince string) (preserved []string, err error) {
	if len(memories) == 0 {
		return nil, fmt.Errorf("refusing to replace memories with empty set — reflection likely malformed")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Snapshot existing non-manual memories before deleting. Pinned memories
	// and resolved memories are excluded throughout this function — like
	// source='manual', a pin is an explicit user override and a resolved_at
	// stamp is a resolve pass's verdict, both of which reflection must never
	// delete, rewrite, or silently drop (it has no way to know a consolidated
	// memory it emits corresponds to a pinned/resolved one it never saw as
	// such, so preservation has to mean "don't touch it" rather than "carry the
	// flag through"). See issue #318.
	snapshotID := fmt.Sprintf("%s-%d", projectID, time.Now().UnixNano())
	_, err = tx.ExecContext(ctx, `
		INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source, tags,
		                              created_at, memory_id, access_count, last_accessed,
		                              agent, session_id, source_ref, confidence,
		                              valid_from, valid_until, verified_at)
		SELECT ?, project_id, category, content, importance, source, tags,
		       created_at, id, access_count, last_accessed,
		       agent, session_id, source_ref, confidence,
		       valid_from, valid_until, verified_at
		FROM memories WHERE project_id = ? AND source != 'manual' AND pinned = 0 AND resolved_at IS NULL
	`, snapshotID, projectID)
	if err != nil {
		return nil, fmt.Errorf("snapshot memories: %w", err)
	}

	// Identify which existing rows this replace may delete, and which emitted
	// memory can reuse one. A row whose content the consolidator re-emits
	// unchanged is updated in place instead of being deleted and re-inserted: a fresh ID
	// would cascade its memory_embeddings and memory_links away (both are ON
	// DELETE CASCADE), so identical content used to mean a re-embedded memory
	// and a lost link graph on every reflection. Rows saved concurrently with
	// the consolidation round trip are kept in place for the same reason.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, content FROM memories
		WHERE project_id = ? AND source != 'manual' AND pinned = 0 AND resolved_at IS NULL
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list replaceable memories: %w", err)
	}
	type replaceCandidate struct {
		id      string
		content string
	}
	var candidates []replaceCandidate
	for rows.Next() {
		var c replaceCandidate
		if err := rows.Scan(&c.id, &c.content); err != nil {
			rows.Close() //nolint:errcheck
			return nil, fmt.Errorf("scan replaceable memory: %w", err)
		}
		candidates = append(candidates, c)
	}
	rowsErr := rows.Err()
	rows.Close() //nolint:errcheck
	if rowsErr != nil {
		return nil, fmt.Errorf("iterate replaceable memories: %w", rowsErr)
	}

	// Memories saved during the round trip never reached the consolidator, so
	// they must survive untouched — same predicate as before, but matched by
	// ID so the row itself is never deleted. >= (not >) errs toward a rare
	// same-second duplicate, which the next reflection merges, rather than
	// toward silent loss.
	concurrent := make(map[string]bool)
	if consolidatedSince != "" {
		crows, err := tx.QueryContext(ctx, `
			SELECT id FROM memories
			WHERE project_id = ? AND source != 'manual' AND pinned = 0 AND resolved_at IS NULL AND created_at >= ?
		`, projectID, consolidatedSince)
		if err != nil {
			return nil, fmt.Errorf("find concurrent memories: %w", err)
		}
		for crows.Next() {
			var id string
			if err := crows.Scan(&id); err != nil {
				crows.Close() //nolint:errcheck
				return nil, fmt.Errorf("scan concurrent memory: %w", err)
			}
			concurrent[id] = true
		}
		crowsErr := crows.Err()
		crows.Close() //nolint:errcheck
		if crowsErr != nil {
			return nil, fmt.Errorf("iterate concurrent memories: %w", crowsErr)
		}
	}

	// Report the survivors so the caller can avoid recording a skip
	// fingerprint that would absorb them into the next no-op gate.
	preserved = make([]string, 0, len(concurrent))
	for id := range concurrent {
		preserved = append(preserved, id)
	}

	// Content -> reusable row IDs. Concurrent rows are excluded: they are kept
	// as they are, never claimed by an emitted memory.
	reusable := make(map[string][]string)
	for _, c := range candidates {
		if concurrent[c.id] {
			continue
		}
		reusable[c.content] = append(reusable[c.content], c.id)
	}
	reuseFor := make(map[int]string, len(memories))
	for i, m := range memories {
		// Matching is exact, not trimmed: reuse preserves the existing
		// embedding, so it is only valid when the stored text is byte-identical
		// to what the consolidator emitted. A whitespace-only difference takes
		// the insert path instead, which leaves the memory to be re-embedded
		// rather than keeping a vector that no longer describes its content.
		if ids := reusable[m.Content]; len(ids) > 0 {
			reuseFor[i] = ids[0]
			reusable[m.Content] = ids[1:]
		}
	}

	var deleteIDs []string
	for _, ids := range reusable {
		deleteIDs = append(deleteIDs, ids...)
	}
	if len(deleteIDs) > 0 {
		stmt, err := tx.PrepareContext(ctx, `DELETE FROM memories WHERE id = ?`)
		if err != nil {
			return nil, fmt.Errorf("prepare delete replaced memory: %w", err)
		}
		for _, id := range deleteIDs {
			if _, err := stmt.ExecContext(ctx, id); err != nil {
				stmt.Close() //nolint:errcheck
				return nil, fmt.Errorf("delete replaced memory: %w", err)
			}
		}
		stmt.Close() //nolint:errcheck
	}

	reused := 0
	for i, m := range memories {
		tags, _ := json.Marshal(m.Tags)
		if id := reuseFor[i]; id != "" {
			// created_at is reset deliberately: consolidated knowledge counts
			// as refreshed (issue #279). The row identity — and with it its
			// embeddings, links, and access stats — survives.
			if _, err := tx.ExecContext(ctx, `
				UPDATE memories
				SET category = ?, content = ?, importance = ?, source = 'reflection', tags = ?,
				    created_at = datetime('now'), updated_at = datetime('now')
				WHERE id = ?
			`, m.Category, m.Content, m.Importance, string(tags), id); err != nil {
				return nil, fmt.Errorf("update reused memory: %w", err)
			}
			reused++
			continue
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO memories (project_id, category, content, source, importance, tags)
			VALUES (?, ?, ?, 'reflection', ?, ?)
		`, projectID, m.Category, m.Content, m.Importance, string(tags))
		if err != nil {
			return nil, fmt.Errorf("insert memory: %w", err)
		}
	}

	if s.logger != nil && (reused > 0 || len(concurrent) > 0) {
		s.logger.Info("preserved memory identity across reflection replace",
			"project_id", projectID, "reused", reused, "concurrent_kept", len(concurrent))
	}

	// Prune old snapshots — keep only the 10 most recent per project. Order by
	// snapshot_id, not created_at: snapshot_id embeds UnixNano, while
	// created_at is only second-precision, so same-second snapshots would
	// otherwise prune in arbitrary order.
	//
	// Widened from 3: with a restore no longer consuming the snapshot it
	// read, three was a handful of reflections of history — one bad round
	// and there was nothing left to roll back to.
	_, err = tx.ExecContext(ctx, `
		DELETE FROM memory_snapshots
		WHERE project_id = ? AND snapshot_id NOT IN (
			SELECT DISTINCT snapshot_id FROM memory_snapshots
			WHERE project_id = ?
			ORDER BY snapshot_id DESC
			LIMIT 10
		)
	`, projectID, projectID)
	if err != nil {
		s.logger.Warn("prune old snapshots", "error", err, "project_id", projectID)
	}

	s.logger.Info("memories snapshotted before replace", "project_id", projectID, "snapshot_id", snapshotID)
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit replace: %w", err)
	}
	return preserved, nil
}

// RestoreSnapshot restores memories from the most recent snapshot for a project.
// Returns the number of memories restored.
func (s *Store) RestoreSnapshot(ctx context.Context, projectID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Find the latest snapshot. Order by snapshot_id (embeds UnixNano), not
	// created_at (second-precision, so same-second snapshots order
	// arbitrarily). Older Unix()-suffixed IDs sort before newer
	// UnixNano()-suffixed ones because the shorter numeric suffix is a prefix
	// of the longer, so mixed-vintage databases still order correctly.
	var snapshotID string
	err := s.db.QueryRowContext(ctx, `
		SELECT snapshot_id FROM memory_snapshots
		WHERE project_id = ?
		ORDER BY snapshot_id DESC
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

	// Remove what the replace created — and only that.
	//
	// "Rows created after the snapshot" cannot be the rule, tempting as it
	// sounds: the replacements the revert exists to undo ARE created after
	// the snapshot, in the same transaction that takes it. Deleting them by
	// age would leave the reflection's output sitting next to the state it
	// replaced, so a restore added rows back without ever undoing anything.
	//
	// Identity gives a cleaner cut. ReplaceNonManual hardcodes
	// source='reflection' for everything it emits, and every reflection row
	// that existed before the snapshot is in the snapshot by id — so a
	// reflection row NOT in this snapshot can only be output of this replace
	// (or a later one, which is equally superseded). A save made afterwards
	// through the MCP server carries source='mcp' and is never touched, which
	// is the data-loss half of this bug: it was never in the snapshot, so
	// deleting it had no way back.
	//
	// Pinned and resolved rows stay excluded throughout (issue #318).
	del, err := tx.ExecContext(ctx, `
		DELETE FROM memories
		WHERE project_id = ? AND source = 'reflection' AND pinned = 0 AND resolved_at IS NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM memory_snapshots s
		      WHERE s.snapshot_id = ?
		        AND ((s.memory_id IS NOT NULL AND s.memory_id = memories.id)
		             OR (s.memory_id IS NULL
		                 AND s.content = memories.content AND s.source = memories.source))
		  )
	`, projectID, snapshotID)
	if err != nil {
		return 0, fmt.Errorf("remove replace output: %w", err)
	}
	removedN, _ := del.RowsAffected()

	// Restore in place instead of delete-then-reinsert.
	//
	// The old shape deleted EVERY non-manual row and re-inserted the
	// snapshot. Because memory_embeddings and memory_links both reference
	// memories(id) ON DELETE CASCADE, re-inserting under a fresh id destroyed
	// the embedding and the link graph of every row the restore "brought
	// back", while the text looked perfectly fine — and the reset
	// created_at/access_count made decay treat an old memory as new.
	//
	// Updating the row that still exists keeps its identity, its embeddings
	// and its links; the FTS triggers refresh the index on content change.
	updated, err := tx.ExecContext(ctx, `
		UPDATE memories SET
		    category = s.category, content = s.content, importance = s.importance,
		    source = s.source, tags = s.tags, created_at = s.created_at,
		    access_count = s.access_count, last_accessed = s.last_accessed,
		    agent = s.agent, session_id = s.session_id, source_ref = s.source_ref,
		    confidence = s.confidence, valid_from = s.valid_from,
		    valid_until = s.valid_until, verified_at = s.verified_at
		FROM memory_snapshots s
		WHERE s.snapshot_id = ? AND s.memory_id = memories.id
		  AND memories.pinned = 0 AND memories.resolved_at IS NULL
	`, snapshotID)
	if err != nil {
		return 0, fmt.Errorf("restore existing rows: %w", err)
	}
	updatedN, _ := updated.RowsAffected()

	// Bring back rows that are genuinely gone, under the id they had, so
	// anything still pointing at them resolves again.
	//
	// memory_id is NULL only for snapshots written before schema v13, whose
	// ids were never recorded. Those match on content instead: that can only
	// add a row that is missing and never overwrites a live one, so a repeated
	// restore stays a no-op rather than duplicating what it just wrote.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO memories (id, project_id, category, content, source, importance, tags,
		                      created_at, updated_at, access_count, last_accessed,
		                      agent, session_id, source_ref, confidence,
		                      valid_from, valid_until, verified_at)
		SELECT COALESCE(memory_id, hex(randomblob(16))), project_id, category, content,
		       source, importance, tags, created_at, created_at, access_count, last_accessed,
		       agent, session_id, source_ref, confidence,
		       valid_from, valid_until, verified_at
		FROM memory_snapshots
		WHERE snapshot_id = ?
		  AND ((memory_id IS NOT NULL AND memory_id NOT IN (SELECT id FROM memories))
		       OR (memory_id IS NULL
		           AND NOT EXISTS (SELECT 1 FROM memories m
		                           WHERE m.project_id = memory_snapshots.project_id
		                             AND m.content = memory_snapshots.content
		                             AND m.source = memory_snapshots.source)))
	`, snapshotID)
	if err != nil {
		return 0, fmt.Errorf("restore snapshot: %w", err)
	}
	insertedN, _ := res.RowsAffected()

	// The snapshot is deliberately kept. Restore is idempotent — re-running
	// it produces the same state — so deleting it only meant a second attempt
	// failed with "no snapshots found" while burning one of the few retained.

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}

	n := int(removedN + updatedN + insertedN)
	s.logger.Info("memories restored from snapshot", "project_id", projectID, "snapshot_id", snapshotID,
		"removed", removedN, "updated", updatedN, "reinserted", insertedN, "count", n)
	return n, nil
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

// GetReflectInputSignature returns the fingerprint of the memory set that
// produced the last applied consolidation for projectID, or "" when none was
// recorded.
func (s *Store) GetReflectInputSignature(ctx context.Context, projectID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var sig string
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(reflect_input_sig, '') FROM ghost_state WHERE project_id = ?`, projectID).Scan(&sig)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get reflect signature: %w", err)
	}
	return sig, nil
}

// SetReflectInputSignature records the fingerprint of the memory set that just
// produced an applied consolidation.
func (s *Store) SetReflectInputSignature(ctx context.Context, projectID, sig string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		UPDATE ghost_state
		SET reflect_input_sig = ?, updated_at = datetime('now')
		WHERE project_id = ?
	`, sig, projectID)
	if err != nil {
		return fmt.Errorf("set reflect signature: %w", err)
	}
	return nil
}

func scanMemories(rows *sql.Rows) ([]Memory, error) {
	var memories []Memory
	for rows.Next() {
		var m Memory
		var lastAccessed sql.NullString
		var resolvedAt sql.NullString
		var tagsJSON string
		var pinned int
		var agent, sessionID, sourceRef sql.NullString
		var confidence sql.NullFloat64
		var scopeRaw sql.NullString

		if err := rows.Scan(
			&m.ID, &m.ProjectID, &m.Category, &m.Content, &m.Importance,
			&m.AccessCount, &lastAccessed, &m.Source, &tagsJSON,
			&pinned, &resolvedAt, &m.CreatedAt, &m.UpdatedAt,
			&agent, &sessionID, &sourceRef, &confidence, &scopeRaw,
		); err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}

		if lastAccessed.Valid {
			m.LastAccessed = &lastAccessed.String
		}
		if resolvedAt.Valid {
			m.ResolvedAt = &resolvedAt.String
		}
		m.Pinned = pinned == 1

		// NULL provenance reads back as the empty string. There is no
		// meaningful empty value for an agent, session or reference, so
		// collapsing NULL to "" costs nothing — while Confidence stays a
		// pointer, because nil ("no belief recorded") and 0.0 ("rated
		// worthless") are different claims.
		if agent.Valid {
			m.Agent = agent.String
		}
		if sessionID.Valid {
			m.SessionID = sessionID.String
		}
		if sourceRef.Valid {
			m.SourceRef = sourceRef.String
		}
		if confidence.Valid {
			m.Confidence = &confidence.Float64
		}
		m.Scope = parseScope(scopeRaw)

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
//
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

// upsertCrossCategoryThreshold is the bar a candidate from ANOTHER category
// must clear before Upsert folds a save into it: token Jaccard >= 0.7 on the
// full content token sets, with no overlap-coefficient leg. The deliberately
// higher bar (same-category keeps its existing mergeScore gate untouched)
// exists because different categories legitimately hold different facts with
// overlapping vocabulary — "deploy staging" vs "deploy production" may both
// read like gotchas without being one rule. What it must catch is the shape
// that produced the 12-copy production-safety incident: the REFLECT
// consolidator re-emitting one rule as paraphrases split across
// preference/gotcha, which then walked past the same-category-only probe on
// every later re-save. Dead records are never candidates regardless of score
// (resolved_at IS NULL, no active 'supersedes' edge): a fold must not
// strengthen a record injection already dropped.
//
// Cost: one extra FTS query, issued only when the same-category probe misses
// (genuinely new facts pay it; a save that folds within its own category does
// not), bounded by the same LIMIT 15 candidate window the same-category probe
// uses.
const upsertCrossCategoryThreshold = 0.7

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

// ftsSearchWordLimit caps how many query terms reach the FTS leg, and it is
// deliberately 10. WHICH terms fill that budget is decided by term
// selection — see selectFTSTERMs/ftsTermValue in fts_terms.go for the
// scoring (identifier-shaped tokens outrank content words, stopwords last,
// ties by original position), which is how a natural-language query's tail
// terms reach FTS without the budget itself growing.
//
// The cap itself must not grow: raising it to 15/20/25/30 broadened the OR
// query enough that FTS-only NDCG@10 rose above the fused hybrid and
// inverted TestBenchRegressionFloors (fusion must beat either single leg),
// measured on the v2 benchmark. Any future change must be re-run against the
// current graded dataset rather than relying on older experiment numbers.
const ftsSearchWordLimit = 10

// sanitizeFTS sanitizes text into an FTS5 OR-query. Used on the search path
// (SearchFTS, SearchFTSAll); capped by ftsSearchWordLimit.
func sanitizeFTS(text string) string {
	return sanitizeFTSN(text, ftsSearchWordLimit)
}

// sanitizeFTSN sanitizes text into an FTS5 OR-query, capped at maxWords
// terms SELECTED by value (ftsTermValue in fts_terms.go) and emitted in
// original query order — a natural-language query's specific tail terms must
// reach FTS, not just its first maxWords words. The cap itself exists to
// keep the OR query narrow (see ftsSearchWordLimit); selection only decides
// which terms fill it. Upsert's duplicate-recall probe uses a wider cap than
// plain search (see sanitizeFTS) because a broader OR-probe over more of the
// content improves candidate recall for the precision pass (mergeScore) that
// follows; widening the cap globally would instead perturb ranked search
// results.
func sanitizeFTSN(text string, maxWords int) string {
	// Remove FTS5 operators and punctuation, keep only words.
	var terms []ftsTerm
	for _, word := range strings.Fields(text) {
		// A trailing '*' is an FTS5 prefix operator. Capture it before the edge
		// trim (which would strip it) and re-attach it outside the quotes as
		// "term"*, so the 'sqlite*' syntax advertised in the tool description
		// actually works instead of degrading to an exact-token match.
		prefix := strings.HasSuffix(word, "*")
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
			term := `"` + escaped + `"`
			if prefix {
				term += "*"
			}
			terms = append(terms, ftsTerm{
				text:  term,
				value: ftsTermValue(clean),
				pos:   len(terms),
			})
		}
	}
	if len(terms) == 0 {
		return `""`
	}
	// Select the maxWords highest-value terms (stopwords only fill the cap
	// when content words can't) instead of truncating the tail positionally.
	if len(terms) > maxWords {
		slog.Warn("fts query truncated",
			"original_terms", len(terms),
			"limit", maxWords)
		terms = selectFTSTERMs(terms, maxWords)
	}
	words := make([]string, len(terms))
	for i, t := range terms {
		words[i] = t.text
	}
	return strings.Join(words, " OR ")
}
