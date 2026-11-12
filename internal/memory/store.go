package memory

// Package memory provides the SQLite-backed persistence layer for Ghost memories,
// including CRUD operations, FTS5 search, hybrid vector/FTS retrieval, time-decay
// scoring with category-aware decay, pin exemptions, project lifecycle management,

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

// GlobalProjectID is the reserved project id Ghost stores its own shipped
const GlobalProjectID = "_global"

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

	Agent      string   `json:"agent,omitempty"`
	SessionID  string   `json:"session_id,omitempty"`
	SourceRef  string   `json:"source_ref,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`

	Scope map[string]string `json:"scope,omitempty"`

	ValidFrom  *string `json:"valid_from,omitempty"`
	ValidUntil *string `json:"valid_until,omitempty"`
	VerifiedAt *string `json:"verified_at,omitempty"`

	Retention string  `json:"retention"`
	ExpiresAt *string `json:"expires_at,omitempty"`

	ReplacesIDs []string `json:"-"`
}

// Project represents a registered project.
type Project struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Queryer is the read surface shared by *sql.DB and *sql.Tx. Search
type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Store manages the SQLite memory database.
type Store struct {
	db       *sql.DB
	snapshot Queryer
	readDB   *sql.DB
	mu       sync.RWMutex
	warnOnce sync.Once
	logger   *slog.Logger
	onSave   func(projectID string) // optional callback after memory create/upsert

	afterDecisionInsert func()

	canon canonMemo

	demotionThreshold float64

	vectorMinSimilarity float32

	embeddingIdentity atomic.Pointer[string]

	foreignWarned *foreignWarnGate

	scratch *vectorScratch

	vcache *vectorCache
}

type foreignWarnGate struct {
	mu     sync.Mutex
	warned map[string]bool
}

// SetOnSave registers a callback invoked after each successful memory save.
func (s *Store) SetOnSave(fn func(projectID string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onSave = fn
}

// SetDemotionThreshold overrides the near-duplicate demotion cutoff
func (s *Store) SetDemotionThreshold(threshold float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.demotionThreshold = threshold
}

// SetVectorMinSimilarity overrides the vector-leg cosine floor hybrid search
func (s *Store) SetVectorMinSimilarity(floor float32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vectorMinSimilarity = floor
}

func (s *Store) vectorMinSimilarityFloor() float32 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.vectorMinSimilarity
}

// SetEmbeddingIdentity declares which vector space this process embeds into —
func (s *Store) SetEmbeddingIdentity(identity string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.embeddingIdentity.Store(&identity)
}

func (s *Store) embeddingIdentityValue() string {
	if p := s.embeddingIdentity.Load(); p != nil {
		return *p
	}
	return ""
}

func identityPtr(identity string) *string {
	return &identity
}

func (s *Store) configuredEmbeddingIdentity() string {
	return s.embeddingIdentityValue()
}

// NewStore creates a new memory store from an open database.
func NewStore(db *sql.DB, logger *slog.Logger) *Store {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Store{db: db, logger: logger, demotionThreshold: DefaultDemotionThreshold, foreignWarned: &foreignWarnGate{warned: make(map[string]bool)}}
	s.scratch = newVectorScratch()
	s.vcache = newVectorCache()
	return s
}

// NewStoreWithRead is NewStore with a read-only handle for snapshot reads.
func NewStoreWithRead(db, readDB *sql.DB, logger *slog.Logger) *Store {
	s := NewStore(db, logger)
	s.readDB = readDB
	return s
}

func (s *Store) readHandle() *sql.DB {
	if s.readDB != nil {
		return s.readDB
	}
	return s.db
}

func (s *Store) warnNoReadHandle() {
	if s.db == nil {
		return
	}
	s.warnOnce.Do(func() {
		s.logger.Warn("store has no read-only handle: snapshot reads (candidate retrieval, flag evidence) take their transaction on the primary connection, whose DSN issues BEGIN IMMEDIATE, so the transaction holds the write lock; build the store with NewStoreWithRead and memory.OpenReadDB for a file-backed database")
	})
}

func (s *Store) queryDB() Queryer {
	if s.snapshot != nil {
		return s.snapshot
	}
	return s.db
}

type seedGlobalMemory struct {
	Category   string
	Content    string
	Importance float32
	Tags       []string
}

var defaultSeedMemories = []seedGlobalMemory{
	{
		Category:   "preference",
		Content:    builtinSeedContent,
		Importance: 1.0,
		Tags:       []string{"git", "commits", "non-negotiable"},
	},
}

func shippedSeedContents() []string {
	out := make([]string, 0, len(defaultSeedMemories))
	for _, seed := range defaultSeedMemories {
		out = append(out, seed.Content)
	}
	return out
}

// SeedGlobalMemories ensures the _global project exists and inserts any
func (s *Store) SeedGlobalMemories(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.execGuardedWrite(ctx, "ensure-global-project", `
		INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')
		ON CONFLICT(id) DO NOTHING
	`)
	if err != nil {
		return fmt.Errorf("ensure _global project: %w", err)
	}
	if _, err := s.execGuardedWrite(ctx, "ensure-global-state",
		`INSERT OR IGNORE INTO ghost_state (project_id) VALUES ('_global')`); err != nil {
		s.logger.Warn("seed global ghost_state insert failed", "error", err)
	}

	for _, seed := range defaultSeedMemories {
		tags, _ := json.Marshal(seed.Tags)
		if err := s.seedMemoryTx(ctx, seed, string(tags)); err != nil {
			s.logger.Warn("seed memory insert failed", "content", seed.Content, "error", err)
		}
	}

	return nil
}

func (s *Store) seedMemoryTx(ctx context.Context, seed seedGlobalMemory, tagsJSON string) error {
	tx, _, err := s.beginWrite(ctx, "seed")
	if err != nil {
		return fmt.Errorf("begin seed tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	var exists int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE project_id = '_global' AND content = ?`,
		seed.Content).Scan(&exists); err != nil {
		return fmt.Errorf("probe existing seed: %w", err)
	}
	if exists > 0 {
		return nil
	}
	var id string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO memories (project_id, category, content, source, importance, tags, pinned)
		VALUES ('_global', ?, ?, 'builtin', ?, ?, 1)
		RETURNING id
	`, seed.Category, seed.Content, seed.Importance, tagsJSON).Scan(&id); err != nil {
		return err
	}
	if err := appendHistoryTx(ctx, tx, id, phaseSave, Provenance{}); err != nil {
		return err
	}
	return tx.Commit()
}

// Close closes the underlying database.
func (s *Store) Close() error {
	err := s.db.Close()
	if s.readDB != nil {
		if readErr := s.readDB.Close(); err == nil {
			err = readErr
		}
	}
	return err
}

// ListProjects returns all registered projects.
func (s *Store) ListProjects(ctx context.Context) ([]Project, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.queryDB().QueryContext(ctx, `
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
func (s *Store) GetProjectPath(ctx context.Context, id string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var path string
	err := s.db.QueryRowContext(ctx, `SELECT path FROM projects WHERE id = ?`, id).Scan(&path)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("project %s not found", RefusalArg(ProjectIdentifierLabel, id))
	}
	if err != nil {
		return "", fmt.Errorf("get project path: %w", err)
	}
	return path, nil
}

// EnsureProject creates a project record if it doesn't exist.
func (s *Store) EnsureProject(ctx context.Context, id, path, name string) error {
	return s.ensureProjectLocked(ctx, id, path, name, "")
}

// EnsureProjectWithRepo is EnsureProject plus the normalized remote URL of the
func (s *Store) EnsureProjectWithRepo(ctx context.Context, id, path, name, repoRemote string) error {
	return s.ensureProjectLocked(ctx, id, path, name, repoRemote)
}

// BindNewProjectToCheckout opens project id at the checkout dir, recording the
func (s *Store) BindNewProjectToCheckout(ctx context.Context, id, dir, name, repoRemote string) (bool, error) {
	repoRemote = NormalizeRepoRemote(repoRemote)
	if id == "" || id == "_global" || dir == id || !storedPathIsUsable(dir) {
		return false, nil
	}
	physical, err := canonicalPath(dir)
	if err != nil || physical == id || !storedPathIsUsable(physical) {
		return false, nil
	}
	if info, statErr := os.Stat(physical); statErr != nil || !info.IsDir() {
		return false, nil
	}
	dir = physical

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "bind-checkout")
	if err != nil {
		return false, fmt.Errorf("begin bind checkout tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var claimed int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM projects
		WHERE id = ? OR path = ? OR (? != '' AND repo_remote = ?)
	`, id, dir, repoRemote, repoRemote).Scan(&claimed); err != nil {
		return false, fmt.Errorf("check checkout claim: %w", err)
	}
	if claimed > 0 {
		return false, nil
	}
	if err := checkBindPathConflicts(ctx, tx, id, dir); err != nil {
		if errors.Is(err, ErrBindPathClaimed) || errors.Is(err, ErrBindPathContainsOther) || errors.Is(err, ErrBindPathInsideOther) {
			return false, nil
		}
		return false, err
	}
	if err := CheckImportedProject(createdProject(id, dir, name, repoRemote)); err != nil {
		return false, fmt.Errorf("create project: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO projects (id, path, name, repo_remote) VALUES (?, ?, ?, ?)`,
		id, dir, name, repoRemote); err != nil {
		return false, fmt.Errorf("bind project to checkout: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ghost_state (project_id) VALUES (?)`, id); err != nil {
		return false, err
	}
	if err := checkPathResolvable(ctx, tx, id, dir, repoRemote); err != nil {
		if errors.Is(err, ErrBindPathUnmatchable) {
			return false, nil
		}
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit bind checkout tx: %w", err)
	}
	return true, nil
}

// BindingRefusalKind names the rule that stopped a repository from claiming
type BindingRefusalKind string

const (
	RefusedAmbiguousName   BindingRefusalKind = "ambiguous-name"
	RefusedDifferentRemote BindingRefusalKind = "different-remote"
	RefusedPathMismatch    BindingRefusalKind = "path-mismatch"
)

// BindingRefusal reports that a repository was not allowed to claim the
type BindingRefusal struct {
	Kind           BindingRefusalKind
	Name           string
	ProjectIDs     []string
	CandidateCount int
	RecordedPath   string
	RecordedRemote string
	SavedTo        string
}

// Notice renders the refusal for whoever asked for the save: what kept the
func (r *BindingRefusal) Notice() string {
	if r == nil {
		return ""
	}
	saved := r.SavedTo
	if saved == "" {
		saved = "a project of its own"
	}
	held := ""
	if len(r.ProjectIDs) > 0 {
		held = r.ProjectIDs[0]
	}
	matched := r.CandidateCount
	if matched < len(r.ProjectIDs) {
		matched = len(r.ProjectIDs)
	}
	switch r.Kind {
	case RefusedAmbiguousName:
		return fmt.Sprintf("%d projects are named %q (%s), so the repository could not be bound to any of them; saved to %s instead, which records this checkout and its repository, so the next save from here lands there — save under a project id to choose one instead; if two of the same-named projects are duplicates of each other, fold them with: ghost project merge <duplicate-id> <survivor-id> (the survivor keeps its own recorded checkout and repository, so pick the one whose is right)",
			matched, r.Name, namedCandidates(r.ProjectIDs, matched), saved)
	case RefusedDifferentRemote:
		return fmt.Sprintf("project %q already belongs to a different repository (%s); saved to %s instead — those are two different repositories, so they are two different projects, and a merge would leave one of them without the repository it was verified against. Nothing needs repairing here: %s records this checkout, so the next save from it lands there. But a save under the name goes to the other repository's project, so address the one you meant by id (%s) or by path (%s)",
			r.Name, r.RecordedRemote, saved, saved, held, saved)
	case RefusedPathMismatch:
		return fmt.Sprintf("project %q already exists at %s; saved to %s instead — to write into that project rather than this one, save under %q by name or id. To make this checkout part of it instead, run both, in this order: ghost project merge %s %s (moves what was just saved here into it) then ghost project bind %s %s (records this checkout and its repository on it, so the next save from here lands there instead of splitting again)",
			r.Name, r.RecordedPath, saved, r.Name, shellQuote(saved), shellQuote(held), shellQuote(held), shellQuote(saved))
	default:
		return fmt.Sprintf("project %q was not bound to this repository (%s); saved to %s instead — save under the project you meant, by name, id or path",
			r.Name, r.Kind, saved)
	}
}

const maxNamedCandidates = 5

func namedCandidates(ids []string, matched int) string {
	listed := ids
	if len(listed) > maxNamedCandidates {
		listed = listed[:maxNamedCandidates]
	}
	text := strings.Join(listed, ", ")
	if rest := matched - len(listed); rest > 0 {
		return fmt.Sprintf("%s and %d more", text, rest)
	}
	return text
}

// ResolveOrCreateRepoProject resolves a repository-aware save to one project
func (s *Store) ResolveOrCreateRepoProject(ctx context.Context, projectRef, repoName, id, path, name, repoRemote string) (string, *BindingRefusal, error) {
	repoRemote = NormalizeRepoRemote(repoRemote)
	if repoRemote == "" {
		return "", nil, fmt.Errorf("resolve or create repository project: empty remote")
	}
	if id == "" {
		return "", nil, fmt.Errorf("resolve or create repository project: empty project id")
	}
	if id == "_global" {
		return "", nil, fmt.Errorf("refusing to assign a repository to the _global project")
	}
	if path == "" {
		path = id
	}

	saving := s.savingRepository(ctx, projectRef, repoRemote)

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "repo-project")
	if err != nil {
		return "", nil, fmt.Errorf("begin repository project tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	canonical, refused, err := s.resolveRepoProjectTx(ctx, tx, projectRef, path, repoName, repoRemote, saving)
	if err != nil {
		return "", nil, err
	}
	if canonical == "" {
		if err := CheckImportedProject(createdProject(id, path, name, repoRemote)); err != nil {
			return "", nil, fmt.Errorf("create project: %w", err)
		}
		canonical, err = s.createRepoProjectTx(ctx, tx, id, path, name, repoRemote)
		if err != nil {
			return "", nil, err
		}
	}
	if refused != nil {
		refused.SavedTo = canonical
	}
	if err := tx.Commit(); err != nil {
		return "", nil, fmt.Errorf("commit repository project tx: %w", err)
	}
	return canonical, refused, nil
}

func (s *Store) resolveRepoProjectTx(ctx context.Context, tx *sql.Tx, projectRef, savingPath, repoName, repoRemote string, saving savingRepository) (string, *BindingRefusal, error) {
	if id, found, err := s.resolveExplicitProjectRepoTx(ctx, tx, projectRef, repoRemote, saving); found || err != nil {
		return id, nil, err
	}
	if id, err := s.findProjectByRepoRemoteTx(ctx, tx, repoRemote, ""); err != nil {
		return "", nil, err
	} else if id != "" {
		return id, nil, nil
	}
	if repoName == "" {
		return "", nil, nil
	}
	return s.bindUniqueProjectNameRepoTx(ctx, tx, repoName, savingPath, repoRemote)
}

func (s *Store) resolveExplicitProjectRepoTx(ctx context.Context, tx *sql.Tx, projectRef, repoRemote string, saving savingRepository) (id string, found bool, err error) {
	if projectRef == "" {
		return "", false, nil
	}

	var count int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM projects
		WHERE (id = ? OR path = ?) AND id != '_global'
	`, projectRef, projectRef).Scan(&count); err != nil {
		return "", false, fmt.Errorf("count explicit projects for repository: %w", err)
	}
	if count > 1 {
		return "", false, fmt.Errorf("project reference %s matches multiple projects", RefusalArg("project_id", projectRef))
	}
	var existingRemote, matchedPath string
	norm := strings.ReplaceAll(projectRef, `\`, "/")
	prefixMatch := false
	if count == 0 {
		prefixMatch = true
		prefixErr := tx.QueryRowContext(ctx, `
			SELECT id, COALESCE(repo_remote, ''), path
			FROM projects
			WHERE (
				path = ?
				OR REPLACE(path, '\', '/') = ?
				OR substr(?, 1, LENGTH(REPLACE(path, '\', '/')) + 1) = REPLACE(path, '\', '/') || '/'
			)
			  AND LENGTH(path) > 10
			  AND id != '_global'
			ORDER BY LENGTH(path) DESC
			LIMIT 1
		`, projectRef, norm, norm).Scan(&id, &existingRemote, &matchedPath)
		if prefixErr == sql.ErrNoRows {
			return "", false, nil
		}
		if prefixErr != nil {
			return "", false, fmt.Errorf("find project by path prefix for repository: %w", prefixErr)
		}
	} else {
		if err := tx.QueryRowContext(ctx, `
			SELECT id, COALESCE(repo_remote, '')
			FROM projects
			WHERE (id = ? OR path = ?) AND id != '_global'
		`, projectRef, projectRef).Scan(&id, &existingRemote); err != nil {
			return "", true, fmt.Errorf("read explicit project for repository: %w", err)
		}
	}
	if existingRemote == repoRemote {
		return id, true, nil
	}
	if existingRemote != "" {
		return "", true, fmt.Errorf("project %s belongs to a different repository", RefusalArg(ProjectIdentifierLabel, id))
	}

	// line have to be tellable apart, because each needs a different fix: a
	if prefixMatch && (saving.projectID != id || saving.recordedPath != matchedPath || !saving.speaksForProject) {
		s.logger.Warn("refused to bind a repository to a project whose checkout contains this save: could not establish that the saving directory belongs to that project's own repository",
			"project", id,
			"recorded_path", matchedPath,
			"saving_path", projectRef, "saving_path_remote", repoRemote,
			"evidence_project", saving.projectID, "evidence_path", saving.recordedPath,
			"evidence_recorded_remote", saving.recordedRemote)
		return id, true, nil
	}

	ownerID, err := s.findProjectByRepoRemoteTx(ctx, tx, repoRemote, id)
	if err != nil {
		return "", true, err
	}
	if ownerID != "" {
		if err := s.mergeProjectTx(ctx, tx, id, ownerID); err != nil {
			return "", true, fmt.Errorf("merge explicit project into repository owner: %w", err)
		}
		return ownerID, true, nil
	}
	if err := s.bindRepoRemoteIfUnsetTx(ctx, tx, id, repoRemote); err != nil {
		return "", true, fmt.Errorf("bind repository to explicit project: %w", err)
	}
	return id, true, nil
}

type savingRepository struct {
	projectID        string
	recordedPath     string
	recordedRemote   string
	speaksForProject bool
}

func (s *Store) savingRepository(ctx context.Context, projectRef, repoRemote string) savingRepository {
	norm := absoluteSessionPath(projectRef)
	hasRepoRemote, err := s.hasRepoRemoteColumn(ctx)
	if err != nil {
		return savingRepository{}
	}
	candidates, err := s.pathCandidates(ctx, norm, hasRepoRemote)
	if err != nil {
		return savingRepository{}
	}
	var winner basenameCandidate
	length := -1
	for _, candidate := range candidates {
		if candidate.id == "_global" {
			continue // not a checkout, and never a prefix match for one
		}
		if l := pathRankLength(candidate.path); l > length {
			winner, length = candidate, l
		}
	}
	if winner.id == "" {
		return savingRepository{}
	}
	saving := savingRepository{
		projectID:        winner.id,
		recordedPath:     winner.path,
		recordedRemote:   winner.remote,
		speaksForProject: true,
	}
	if winner.remote != "" || savingPathIsProjectRoot(winner.path, norm) {
		return saving
	}
	recordedRemote := s.inputRemote(winner.path)
	saving.recordedRemote = recordedRemote
	saving.speaksForProject = recordedRemote != "" && recordedRemote == repoRemote
	return saving
}

func savingPathIsProjectRoot(recorded, saving string) bool {
	physicalRecorded, err := canonicalPath(recorded)
	if err != nil {
		return false
	}
	physicalSaving, err := canonicalPath(saving)
	if err != nil {
		return false
	}
	return physicalSaving == physicalRecorded
}

func (s *Store) bindUniqueProjectNameRepoTx(ctx context.Context, tx *sql.Tx, name, savingPath, repoRemote string) (string, *BindingRefusal, error) {
	candidates, matching, err := s.projectsNamedTx(ctx, tx, name, maxNamedCandidates)
	if err != nil {
		return "", nil, err
	}
	if matching == 0 {
		return "", nil, nil
	}
	if matching > 1 {
		ids := make([]string, 0, len(candidates))
		for _, c := range candidates {
			ids = append(ids, c.id)
		}
		s.logger.Warn("refused to bind a repository to an ambiguous project name: no candidate can be evidence for another",
			"name", name, "candidates", ids, "matching", matching,
			"saving_path", savingPath, "remote", repoRemote)
		return "", &BindingRefusal{
			Kind:           RefusedAmbiguousName,
			Name:           name,
			ProjectIDs:     ids,
			CandidateCount: matching,
		}, nil
	}

	candidate := candidates[0]
	id, existingRemote, storedPath := candidate.id, candidate.remote, candidate.path
	if existingRemote != "" {
		if existingRemote == repoRemote {
			return id, nil, nil
		}
		s.logger.Warn("refused to bind a repository to a project of the same name: it already belongs to another repository",
			"project", id, "name", name, "recorded_remote", existingRemote,
			"saving_path", savingPath, "remote", repoRemote)
		return "", &BindingRefusal{
			Kind:           RefusedDifferentRemote,
			Name:           name,
			ProjectIDs:     []string{id},
			CandidateCount: 1,
			RecordedPath:   recordedCheckout(storedPath),
			RecordedRemote: existingRemote,
		}, nil
	}
	if storedPathIsUsable(storedPath) && !pathsAgree(absoluteSessionPath(savingPath), storedPath) {
		_, resolveErr := canonicalPath(storedPath)
		s.logger.Warn("refused to bind a repository to a project of the same name: its recorded path does not contain the saving directory",
			"project", name, "recorded_path", storedPath, "recorded_path_resolves", resolveErr == nil,
			"saving_path", savingPath, "remote", repoRemote)
		return "", &BindingRefusal{
			Kind:           RefusedPathMismatch,
			Name:           name,
			ProjectIDs:     []string{id},
			CandidateCount: 1,
			RecordedPath:   recordedCheckout(storedPath),
		}, nil
	}
	if err := s.bindRepoRemoteIfUnsetTx(ctx, tx, id, repoRemote); err != nil {
		return "", nil, fmt.Errorf("bind repository to named project: %w", err)
	}
	return id, nil, nil
}

func recordedCheckout(stored string) string {
	if !storedPathIsUsable(stored) {
		return ""
	}
	return stored
}

type nameCandidate struct {
	id     string
	remote string
	path   string
}

func (s *Store) projectsNamedTx(ctx context.Context, tx *sql.Tx, name string, limit int) ([]nameCandidate, int, error) {
	if limit < 1 {
		limit = 1
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id, COALESCE(repo_remote, ''), path, COUNT(*) OVER () AS matching
		FROM projects
		WHERE name = ? AND id != '_global'
		ORDER BY id
		LIMIT ?
	`, name, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("read projects by name for repository: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var candidates []nameCandidate
	matching := 0
	for rows.Next() {
		var c nameCandidate
		if err := rows.Scan(&c.id, &c.remote, &c.path, &matching); err != nil {
			return nil, 0, fmt.Errorf("read project by name for repository: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read projects by name for repository: %w", err)
	}
	if len(candidates) == 0 {
		return nil, 0, nil
	}
	return candidates, matching, nil
}

func (s *Store) findProjectByRepoRemoteTx(ctx context.Context, tx *sql.Tx, repoRemote, excludeID string) (string, error) {
	var id string
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM projects
		WHERE repo_remote = ? AND id NOT IN ('_global', ?)
		LIMIT 1
	`, repoRemote, excludeID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find project by repository: %w", err)
	}
	return id, nil
}

func (s *Store) bindRepoRemoteIfUnsetTx(ctx context.Context, tx *sql.Tx, id, repoRemote string) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE projects
		SET repo_remote = ?, updated_at = datetime('now')
		WHERE id = ? AND COALESCE(repo_remote, '') = ''
	`, repoRemote, id)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return fmt.Errorf("project %s repository changed concurrently", RefusalArg(ProjectIdentifierLabel, id))
	}
	return nil
}

func (s *Store) createRepoProjectTx(ctx context.Context, tx *sql.Tx, id, path, name, repoRemote string) (string, error) {
	var existingRemote string
	existingErr := tx.QueryRowContext(ctx,
		`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, id).Scan(&existingRemote)
	if existingErr != nil && existingErr != sql.ErrNoRows {
		return "", fmt.Errorf("query fallback project for repository: %w", existingErr)
	}
	if existingErr == nil {
		if existingRemote != "" && existingRemote != repoRemote {
			return "", fmt.Errorf("project %s belongs to a different repository", RefusalArg(ProjectIdentifierLabel, id))
		}
		if existingRemote == "" {
			if err := s.bindRepoRemoteIfUnsetTx(ctx, tx, id, repoRemote); err != nil {
				return "", fmt.Errorf("bind repository to fallback project: %w", err)
			}
		}
		return id, nil
	}

	if path != id {
		var ownerID, ownerRemote string
		err := tx.QueryRowContext(ctx, `
			SELECT id, COALESCE(repo_remote, '')
			FROM projects
			WHERE path = ? AND id != ?
			LIMIT 1
		`, path, id).Scan(&ownerID, &ownerRemote)
		if err != nil && err != sql.ErrNoRows {
			return "", fmt.Errorf("query path owner for repository: %w", err)
		}
		if err == nil {
			if ownerID == "_global" {
				return "", fmt.Errorf("refusing to merge the _global project (old=%s new=%s)", RefusalArg(ProjectIdentifierLabel, id), RefusalArg(ProjectIdentifierLabel, ownerID))
			}
			if ownerRemote != "" && ownerRemote != repoRemote {
				return "", fmt.Errorf("project %s belongs to a different repository", RefusalArg(ProjectIdentifierLabel, ownerID))
			}
			if ownerRemote == "" {
				if err := s.bindRepoRemoteIfUnsetTx(ctx, tx, ownerID, repoRemote); err != nil {
					return "", fmt.Errorf("bind repository to path owner: %w", err)
				}
			}
			if err := s.mergeProjectTx(ctx, tx, id, ownerID); err != nil {
				return "", fmt.Errorf("merge project into path owner: %w", err)
			}
			return ownerID, nil
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO projects (id, path, name, repo_remote) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			path = CASE WHEN excluded.path = excluded.id THEN projects.path ELSE excluded.path END,
			repo_remote = CASE
				WHEN excluded.repo_remote = '' THEN projects.repo_remote
				WHEN projects.repo_remote IS NULL OR projects.repo_remote = '' THEN excluded.repo_remote
				ELSE projects.repo_remote
			END,
			updated_at = datetime('now')
	`, id, path, name, repoRemote); err != nil {
		return "", fmt.Errorf("create repository project: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ghost_state (project_id) VALUES (?)`, id); err != nil {
		return "", fmt.Errorf("create repository project state: %w", err)
	}
	return id, nil
}

func reconcileMergeRepoTx(ctx context.Context, tx *sql.Tx, existingID, repoRemote string, excluded ...string) error {
	if repoRemote == "" {
		return nil
	}
	var existingRemote string
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, existingID).Scan(&existingRemote); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("read merge target repository: %w", err)
	}
	if existingRemote != "" {
		if existingRemote != repoRemote {
			return fmt.Errorf("project %s belongs to a different repository", RefusalArg(ProjectIdentifierLabel, existingID))
		}
		return nil
	}
	args := []any{repoRemote, existingID}
	clause := ""
	for _, id := range excluded {
		clause += " AND id != ?"
		args = append(args, id)
	}
	var holder string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM projects WHERE repo_remote = ? AND id != ?`+clause+` ORDER BY id`, args...).Scan(&holder); err == nil {
		return fmt.Errorf("%w: repository %q is already recorded on project %s, so it cannot also be bound to %s",
			ErrAmbiguousProject, repoRemote, RefusalArg(ProjectIdentifierLabel, holder), RefusalArg(ProjectIdentifierLabel, existingID))
	} else if err != sql.ErrNoRows {
		return fmt.Errorf("check repository identity before binding: %w", err)
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE projects
		SET repo_remote = ?, updated_at = datetime('now')
		WHERE id = ? AND COALESCE(repo_remote, '') = ''
	`, repoRemote, existingID)
	if err != nil {
		return fmt.Errorf("bind repository to merge target: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check merge target repository: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("project %s repository changed concurrently", RefusalArg(ProjectIdentifierLabel, existingID))
	}
	return nil
}

func (s *Store) ensureProjectLocked(ctx context.Context, id, path, name, repoRemote string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if path == "" {
		path = id
	}
	repoRemote = NormalizeRepoRemote(repoRemote)
	if id == "_global" && repoRemote != "" {
		return fmt.Errorf("refusing to assign a repository to the _global project")
	}

	tx, _, err := s.beginWrite(ctx, "ensure-project")
	if err != nil {
		return fmt.Errorf("begin ensure project tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var mergedOld, mergedNew string
	commit := func() error {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit ensure project tx: %w", err)
		}
		if mergedOld != "" {
			s.logger.Info("merged duplicate project", "old_id", RefusalArg("project_id", mergedOld), "new_id", RefusalArg("project_id", mergedNew))
		}
		return nil
	}

	if repoRemote != "" && id != "_global" {
		var incomingRemote string
		incomingErr := tx.QueryRowContext(ctx,
			`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, id).Scan(&incomingRemote)
		if incomingErr != nil && incomingErr != sql.ErrNoRows {
			return fmt.Errorf("check project repository: %w", incomingErr)
		}
		if incomingRemote != "" && incomingRemote != repoRemote {
			return fmt.Errorf("project %s belongs to a different repository", RefusalArg(ProjectIdentifierLabel, id))
		}

		var existingID string
		scanErr := tx.QueryRowContext(ctx,
			`SELECT id FROM projects WHERE repo_remote = ? AND id NOT IN ('_global', ?) LIMIT 1`,
			repoRemote, id).Scan(&existingID)
		if scanErr != nil && scanErr != sql.ErrNoRows {
			return fmt.Errorf("find repository project: %w", scanErr)
		}
		if existingID != "" {
			if err := s.mergeProjectWithRepoTx(ctx, tx, id, existingID, repoRemote); err != nil {
				return fmt.Errorf("auto-merge repository duplicate: %w", err)
			}
			mergedOld, mergedNew = id, existingID
			return commit()
		}
	}

	if filepath.IsAbs(path) && path != id {
		var existingID string
		scanErr := tx.QueryRowContext(ctx,
			`SELECT id FROM projects WHERE path = ? AND id != ? LIMIT 1`,
			path, id).Scan(&existingID)
		if scanErr != nil && scanErr != sql.ErrNoRows {
			return fmt.Errorf("find path project: %w", scanErr)
		}
		if existingID != "" {
			if err := s.mergeProjectWithRepoTx(ctx, tx, id, existingID, repoRemote); err != nil {
				return fmt.Errorf("auto-merge path duplicate: %w", err)
			}
			mergedOld, mergedNew = id, existingID
			return commit()
		}
	}

	var held int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id = ?`, id).Scan(&held); err != nil {
		return fmt.Errorf("check project before create: %w", err)
	}
	if held == 0 {
		if err := CheckImportedProject(createdProject(id, path, name, repoRemote)); err != nil {
			return fmt.Errorf("create project: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO projects (id, path, name, repo_remote) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			path = CASE WHEN excluded.path = excluded.id THEN projects.path ELSE excluded.path END,
			repo_remote = CASE
				WHEN excluded.repo_remote = '' THEN projects.repo_remote
				WHEN projects.repo_remote IS NULL OR projects.repo_remote = '' THEN excluded.repo_remote
				ELSE projects.repo_remote
			END,
			updated_at = datetime('now')
	`, id, path, name, repoRemote); err != nil {
		return fmt.Errorf("ensure project: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO ghost_state (project_id) VALUES (?)`, id); err != nil {
		return err
	}

	if path != id && filepath.IsAbs(path) {
		var candidateCount int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM projects
			WHERE name = ? AND id NOT IN (?, '_global')
			  AND instr(path, '/') = 0 AND instr(path, '\') = 0
		`, name, id).Scan(&candidateCount); err != nil {
			return fmt.Errorf("count same-name projects: %w", err)
		}
		if candidateCount == 1 {
			var dupID string
			if err := tx.QueryRowContext(ctx, `
				SELECT id FROM projects
				WHERE name = ? AND id NOT IN (?, '_global')
				  AND instr(path, '/') = 0 AND instr(path, '\') = 0
			`, name, id).Scan(&dupID); err != nil {
				return fmt.Errorf("read same-name project: %w", err)
			}
			if err := s.mergeProjectWithRepoTx(ctx, tx, dupID, id, repoRemote); err != nil {
				return fmt.Errorf("auto-merge duplicate project: %w", err)
			}
			mergedOld, mergedNew = dupID, id
		}
	}

	return commit()
}

// MergeProject reassigns all child records from oldID to newID, then deletes
func (s *Store) MergeProject(ctx context.Context, oldID, newID string) error {
	if oldID == "_global" || newID == "_global" {
		return fmt.Errorf("refusing to merge the _global project (old=%s new=%s)", RefusalArg(ProjectIdentifierLabel, oldID), RefusalArg(ProjectIdentifierLabel, newID))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mergeProjectLocked(ctx, oldID, newID)
}

var projectMergeStatements = []string{
	`UPDATE memories SET project_id = ? WHERE project_id = ?`,
	`UPDATE tasks SET project_id = ? WHERE project_id = ?`,
	`UPDATE decisions SET project_id = ? WHERE project_id = ?`,
	`UPDATE token_usage SET project_id = ? WHERE project_id = ?`,
	`UPDATE audit_log SET project_id = ? WHERE project_id = ?`,
	`UPDATE memory_snapshots SET project_id = ? WHERE project_id = ?`,
	`UPDATE memory_history SET project_id = ? WHERE project_id = ?`,
	`UPDATE supersede_checked SET project_id = ? WHERE project_id = ?`,
	`UPDATE retrieval_record SET project_id = ? WHERE project_id = ?`,
	`UPDATE retrieval_audit SET project_id = ? WHERE project_id = ?`,
	`UPDATE memory_flags SET project_id = ? WHERE project_id = ?`,
}

func mergeProjectTx(ctx context.Context, tx *sql.Tx, oldID, newID string) error {
	if oldID == newID {
		return nil
	}
	if oldID == "_global" || newID == "_global" {
		return fmt.Errorf("%w: refusing to merge the _global project (old=%s new=%s)", ErrAmbiguousProject, RefusalArg(ProjectIdentifierLabel, oldID), RefusalArg(ProjectIdentifierLabel, newID))
	}
	for _, stmt := range projectMergeStatements {
		if _, err := tx.ExecContext(ctx, stmt, newID, oldID); err != nil {
			return fmt.Errorf("merge reassign: %w", err)
		}
	}

	if err := mergeGhostStateTx(ctx, tx, oldID, newID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ghost_state WHERE project_id = ?`, oldID); err != nil {
		return fmt.Errorf("merge delete ghost_state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, oldID); err != nil {
		return fmt.Errorf("merge delete project: %w", err)
	}
	return nil
}

type ghostStateRow struct {
	interactionCount  int
	learnedContext    string
	lastReflectionAt  sql.NullString
	reflectionSummary string
	reflectInputSig   string
}

func mergeGhostStateTx(ctx context.Context, tx *sql.Tx, oldID, newID string) error {
	var surv ghostStateRow
	err := tx.QueryRowContext(ctx, `SELECT interaction_count, COALESCE(learned_context,''), last_reflection_at, COALESCE(reflection_summary,''), COALESCE(reflect_input_sig,'') FROM ghost_state WHERE project_id = ?`, newID).
		Scan(&surv.interactionCount, &surv.learnedContext, &surv.lastReflectionAt, &surv.reflectionSummary, &surv.reflectInputSig)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("merge read survivor ghost_state: %w", err)
	}
	var oldg ghostStateRow
	err = tx.QueryRowContext(ctx, `SELECT interaction_count, COALESCE(learned_context,''), last_reflection_at, COALESCE(reflection_summary,''), COALESCE(reflect_input_sig,'') FROM ghost_state WHERE project_id = ?`, oldID).
		Scan(&oldg.interactionCount, &oldg.learnedContext, &oldg.lastReflectionAt, &oldg.reflectionSummary, &oldg.reflectInputSig)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("merge read outgoing ghost_state: %w", err)
	}

	merged := mergeGhostStateRows(surv, oldg)
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ghost_state (project_id) VALUES (?)`, newID); err != nil {
		return fmt.Errorf("merge ensure survivor ghost_state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ghost_state SET interaction_count = ?, learned_context = ?, last_reflection_at = ?, reflection_summary = ?, reflect_input_sig = ?, updated_at = datetime('now') WHERE project_id = ?`,
		merged.interactionCount, merged.learnedContext, merged.lastReflectionAt, merged.reflectionSummary, merged.reflectInputSig, newID); err != nil {
		return fmt.Errorf("merge update survivor ghost_state: %w", err)
	}
	return nil
}

func mergeGhostStateRows(surv, oldg ghostStateRow) ghostStateRow {
	merged := surv
	if oldg.interactionCount > merged.interactionCount {
		merged.interactionCount = oldg.interactionCount
	}

	survivorHasReflection := surv.lastReflectionAt.Valid || surv.learnedContext != ""
	outgoingHasReflection := oldg.lastReflectionAt.Valid || oldg.learnedContext != ""

	takeOutgoing := false
	if !survivorHasReflection && outgoingHasReflection {
		takeOutgoing = true
	} else if surv.lastReflectionAt.Valid && oldg.lastReflectionAt.Valid &&
		oldg.lastReflectionAt.String > surv.lastReflectionAt.String {
		takeOutgoing = true
	}

	if takeOutgoing {
		merged.learnedContext = oldg.learnedContext
		merged.reflectionSummary = oldg.reflectionSummary
		merged.reflectInputSig = oldg.reflectInputSig
		merged.lastReflectionAt = oldg.lastReflectionAt
	}
	return merged
}

func (s *Store) mergeProjectLocked(ctx context.Context, oldID, newID string) error {
	return s.mergeProjectWithRepoLocked(ctx, oldID, newID, "")
}

func (s *Store) mergeProjectWithRepoLocked(ctx context.Context, oldID, newID, repoRemote string) error {
	tx, _, err := s.beginWrite(ctx, "merge-project")
	if err != nil {
		return fmt.Errorf("begin merge tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := s.mergeProjectWithRepoTx(ctx, tx, oldID, newID, repoRemote); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("merge commit: %w", err)
	}

	s.logger.Info("merged duplicate project", "old_id", RefusalArg("project_id", oldID), "new_id", RefusalArg("project_id", newID))
	return nil
}

func (s *Store) mergeProjectWithRepoTx(ctx context.Context, tx *sql.Tx, oldID, newID, repoRemote string) error {
	if oldID == newID {
		return nil
	}
	if oldID == "_global" || newID == "_global" {
		return fmt.Errorf("refusing to merge the _global project (old=%s new=%s)", RefusalArg(ProjectIdentifierLabel, oldID), RefusalArg(ProjectIdentifierLabel, newID))
	}
	if err := validateMergeRepoTx(ctx, tx, oldID, repoRemote); err != nil {
		return err
	}
	if err := mergeProjectTx(ctx, tx, oldID, newID); err != nil {
		return err
	}
	return reconcileMergeRepoTx(ctx, tx, newID, repoRemote, oldID)
}

func validateMergeRepoTx(ctx context.Context, tx *sql.Tx, oldID, repoRemote string) error {
	if repoRemote == "" {
		return nil
	}
	var existingRemote string
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, oldID).Scan(&existingRemote); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("read merge source repository: %w", err)
	}
	if existingRemote != "" && existingRemote != repoRemote {
		return fmt.Errorf("project %s belongs to a different repository", RefusalArg(ProjectIdentifierLabel, oldID))
	}
	return nil
}

func (s *Store) mergeProjectTx(ctx context.Context, tx *sql.Tx, oldID, newID string) error {
	if oldID == newID {
		return nil
	}
	if oldID == "_global" || newID == "_global" {
		return fmt.Errorf("refusing to merge the _global project (old=%s new=%s)", RefusalArg(ProjectIdentifierLabel, oldID), RefusalArg(ProjectIdentifierLabel, newID))
	}

	stmts := []string{
		`UPDATE memories SET project_id = ? WHERE project_id = ?`,
		`UPDATE tasks SET project_id = ? WHERE project_id = ?`,
		`UPDATE decisions SET project_id = ? WHERE project_id = ?`,
		`UPDATE token_usage SET project_id = ? WHERE project_id = ?`,
		`UPDATE audit_log SET project_id = ? WHERE project_id = ?`,
		`UPDATE memory_snapshots SET project_id = ? WHERE project_id = ?`,
		`UPDATE memory_history SET project_id = ? WHERE project_id = ?`,
		`UPDATE supersede_checked SET project_id = ? WHERE project_id = ?`,
		`UPDATE retrieval_record SET project_id = ? WHERE project_id = ?`,
		`UPDATE retrieval_audit SET project_id = ? WHERE project_id = ?`,
		// line would orphan every flag of the outgoing project — under an id that
		`UPDATE memory_flags SET project_id = ? WHERE project_id = ?`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt, newID, oldID); err != nil {
			return fmt.Errorf("merge reassign: %w", err)
		}
	}

	if err := mergeGhostStateTx(ctx, tx, oldID, newID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ghost_state WHERE project_id = ?`, oldID); err != nil {
		return fmt.Errorf("merge delete ghost_state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, oldID); err != nil {
		return fmt.Errorf("merge delete project: %w", err)
	}
	return nil
}

// DeleteProjectSummary reports what DeleteProject found (dry-run) or removed
type DeleteProjectSummary struct {
	ProjectID        string
	ProjectName      string
	Memories         int
	MemoryLinks      int
	Tasks            int
	Decisions        int
	TokenUsage       int
	AuditLog         int
	RetrievalRecords int
	MemoryFlags      int
}

// DeleteProject permanently removes a project and everything under it.
func (s *Store) DeleteProject(ctx context.Context, input string, apply bool) (DeleteProjectSummary, error) {
	id, name, err := s.ResolveProject(ctx, input)
	if err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("resolve project: %w", err)
	}
	if id == "" {
		return DeleteProjectSummary{}, fmt.Errorf("project %s not found", RefusalArg(ProjectIdentifierLabel, input))
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

	tx, _, err := s.beginWrite(ctx, "delete-project")
	if err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("begin delete tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

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
	if _, err := tx.ExecContext(ctx, `DELETE FROM retrieval_record WHERE project_id = ?`, id); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("delete retrieval_record: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM retrieval_audit WHERE project_id = ?`, id); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("delete retrieval_audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memory_flags WHERE `+deleteProjectFlagRowsSQL, id); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("delete memory_flags: %w", err)
	}
	if err := deleteProjectRowTx(ctx, tx, id); err != nil {
		return DeleteProjectSummary{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM memory_snapshot_evidence
		WHERE snapshot_id NOT IN (SELECT DISTINCT snapshot_id FROM memory_snapshots)
	`); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("delete snapshot evidence: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("commit delete: %w", err)
	}

	s.logger.Info("deleted project", "project_id", RefusalArg("project_id", id), "project_name", name,
		"memories", summary.Memories, "memory_links", summary.MemoryLinks,
		"tasks", summary.Tasks, "decisions", summary.Decisions,
		"token_usage", summary.TokenUsage, "audit_log", summary.AuditLog,
		"retrieval_records", summary.RetrievalRecords,
		"memory_flags", summary.MemoryFlags)
	return summary, nil
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const deleteProjectFlagRowsSQL = `memory_id IN (SELECT id FROM memories WHERE project_id = ?)`

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
		`SELECT count(*) FROM retrieval_record WHERE project_id = ?`, id,
	).Scan(&summary.RetrievalRecords); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("count retrieval_record: %w", err)
	}
	if err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM memory_flags WHERE `+deleteProjectFlagRowsSQL, id,
	).Scan(&summary.MemoryFlags); err != nil {
		return DeleteProjectSummary{}, fmt.Errorf("count memory_flags: %w", err)
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
		return fmt.Errorf("project %s was already deleted", RefusalArg(ProjectIdentifierLabel, id))
	}
	return nil
}

var detectRemoteForPath = func(string) string { return "" }

// SetDetectRemote wires the repository detector used when resolving a project
func SetDetectRemote(fn func(dir string) string) {
	if fn == nil {
		detectRemoteForPath = func(string) string { return "" }
		return
	}
	detectRemoteForPath = fn
}

// ErrAmbiguousProject means an identifier matched more than one project and
var ErrAmbiguousProject = errors.New("project identifier is ambiguous")

// ResolveProject resolves an exact ID/name, a canonical path prefix, an exact
func (s *Store) ResolveExactProjectID(ctx context.Context, id string) (string, bool, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT id, name FROM projects WHERE id = ? LIMIT 1`, id).Scan(&id, &name)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve project by exact id: %w", err)
	}
	return id, true, nil
}

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

	hasRepoRemote, schemaErr := s.hasRepoRemoteColumn(ctx)
	if schemaErr != nil {
		return "", "", fmt.Errorf("inspect project schema: %w", schemaErr)
	}

	nameMatches, qErr := s.basenameCandidates(ctx, input, hasRepoRemote)
	if qErr != nil {
		return "", "", fmt.Errorf("resolve project by name: %w", qErr)
	}
	if len(nameMatches) == 1 {
		return nameMatches[0].id, nameMatches[0].name, nil
	}
	if len(nameMatches) > 1 {
		return "", "", ambiguousProject(input, "matches multiple projects")
	}

	remote := ""
	remoteDerived := false
	remoteOnce := func() string {
		if !remoteDerived {
			remote = s.inputRemote(input)
			remoteDerived = true
		}
		return remote
	}

	if IsPathShaped(input) {
		best, tied, found, pErr := s.resolveByPath(ctx, input, hasRepoRemote, remoteOnce)
		if pErr != nil {
			return "", "", fmt.Errorf("resolve project by path: %w", pErr)
		}
		if tied {
			return "", "", ambiguousProject(input, "has tied path-prefix matches")
		}
		if found {
			return best.id, best.name, nil
		}
	}

	remote = remoteOnce()
	if remote != "" && hasRepoRemote {
		rows, queryErr := s.db.QueryContext(ctx, `
			SELECT id, name FROM projects
			WHERE repo_remote = ? AND id != '_global'
			ORDER BY id
		`, remote)
		if queryErr != nil {
			return "", "", fmt.Errorf("resolve project by repository: %w", queryErr)
		}
		var matches []struct{ id, name string }
		for rows.Next() {
			var match struct{ id, name string }
			if scanErr := rows.Scan(&match.id, &match.name); scanErr != nil {
				_ = rows.Close()
				return "", "", fmt.Errorf("resolve project by repository: scan: %w", scanErr)
			}
			matches = append(matches, match)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			_ = rows.Close()
			return "", "", fmt.Errorf("resolve project by repository: %w", rowsErr)
		}
		_ = rows.Close()
		if len(matches) > 1 {
			return "", "", fmt.Errorf("%w: repository %q matches multiple projects", ErrAmbiguousProject, remote)
		}
		if len(matches) == 1 {
			return matches[0].id, matches[0].name, nil
		}
	}

	base := filepath.Base(input)
	candidates, qErr := s.basenameCandidates(ctx, base, hasRepoRemote)
	if qErr != nil {
		return "", "", fmt.Errorf("resolve project by basename: %w", qErr)
	}

	survivors := make([]basenameCandidate, 0, 1)
	for _, cand := range candidates {
		if cand.agreesWithSession(input, remote) {
			survivors = append(survivors, cand)
		}
	}
	if len(survivors) == 0 {
		return "", "", nil
	}
	if len(survivors) > 1 {
		return "", "", ambiguousProject(input, "matches multiple projects")
	}
	return survivors[0].id, survivors[0].name, nil
}

func ambiguousProject(input, reason string) error {
	return fmt.Errorf("%w: %s %s", ErrAmbiguousProject, RefusalArg(ProjectIdentifierLabel, input), reason)
}

type basenameCandidate struct{ id, name, path, remote string }

func (s *Store) hasRepoRemoteColumn(ctx context.Context) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM pragma_table_info('projects') WHERE name = 'repo_remote'
	`).Scan(&exists)
	return exists > 0, err
}

func (s *Store) inputRemote(input string) string {
	remote := NormalizeRepoRemote(input)
	if remote == "" && IsPathShaped(input) {
		remote = NormalizeRepoRemote(detectRemoteForPath(input))
	}
	return remote
}

func scanProjectCandidates(rows *sql.Rows) ([]basenameCandidate, error) {
	defer rows.Close() //nolint:errcheck
	var candidates []basenameCandidate
	for rows.Next() {
		var candidate basenameCandidate
		if err := rows.Scan(&candidate.id, &candidate.name, &candidate.path, &candidate.remote); err != nil {
			return nil, fmt.Errorf("scan candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidates: %w", err)
	}
	return candidates, nil
}

func (s *Store) basenameCandidates(ctx context.Context, name string, hasRepoRemote bool) ([]basenameCandidate, error) {
	remoteColumn := "''"
	if hasRepoRemote {
		remoteColumn = "COALESCE(repo_remote, '')"
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, path, `+remoteColumn+` FROM projects WHERE name = ?`, name)
	if err != nil {
		return nil, err
	}
	return scanProjectCandidates(rows)
}

const pathCandidatesQuery = `SELECT id, name, path, %s FROM projects
		WHERE (path = ?
		   OR REPLACE(path, '\', '/') = ?
		   OR substr(?, 1, LENGTH(REPLACE(path, '\', '/')) + 1) = REPLACE(path, '\', '/') || '/')
		  AND LENGTH(path) > 10
		ORDER BY LENGTH(path) DESC`

func (s *Store) pathCandidates(ctx context.Context, input string, hasRepoRemote bool) ([]basenameCandidate, error) {
	remoteColumn := "''"
	if hasRepoRemote {
		remoteColumn = "COALESCE(repo_remote, '')"
	}
	norm := absoluteSessionPath(input)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(pathCandidatesQuery, remoteColumn), norm, norm, norm)
	if err != nil {
		return nil, err
	}
	return scanProjectCandidates(rows)
}

func (s *Store) resolveByPath(ctx context.Context, input string, hasRepoRemote bool, remoteOnce func() string) (best basenameCandidate, tied, found bool, err error) {
	inputCanon, rErr := canonicalPath(absoluteSessionPath(input))
	if rErr != nil {
		return basenameCandidate{}, false, false, nil
	}
	rows, qErr := s.db.QueryContext(ctx, `SELECT id, path FROM projects`)
	if qErr != nil {
		return basenameCandidate{}, false, false, qErr
	}
	type hit struct {
		id, path, canon string
	}
	var hits []hit
	for rows.Next() {
		var h hit
		if scanErr := rows.Scan(&h.id, &h.path); scanErr != nil {
			_ = rows.Close()
			return basenameCandidate{}, false, false, scanErr
		}
		if !storedPathIsUsable(h.path) {
			continue
		}
		canon, ok := s.canon.lookup(h.path)
		if !ok || !samePath(inputCanon, canon) {
			continue
		}
		h.canon = canon
		hits = append(hits, h)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		_ = rows.Close()
		return basenameCandidate{}, false, false, rowsErr
	}
	_ = rows.Close()

	remoteColumn := "''"
	if hasRepoRemote {
		remoteColumn = "COALESCE(repo_remote, '')"
	}
	bestLength := -1
	for _, h := range hits {
		fresh, fErr := canonicalPath(h.path)
		if fErr != nil || !samePath(inputCanon, fresh) {
			continue
		}
		h.canon = fresh
		c := basenameCandidate{id: h.id, path: h.path}
		if scanErr := s.db.QueryRowContext(ctx,
			`SELECT name, `+remoteColumn+` FROM projects WHERE id = ?`, h.id).Scan(&c.name, &c.remote); scanErr != nil {
			if errors.Is(scanErr, sql.ErrNoRows) {
				continue // deleted since the first read
			}
			return basenameCandidate{}, false, false, scanErr
		}
		remote := ""
		if c.remote != "" {
			remote = remoteOnce()
		}
		if c.remoteConflicts(remote) {
			continue
		}
		switch length := pathRankLength(h.canon); {
		case length > bestLength:
			best, bestLength, tied, found = c, length, false, true
		case length == bestLength:
			tied = true
		}
	}
	return best, tied, found, nil
}

func (c basenameCandidate) remoteConflicts(remote string) bool {
	return remote != "" && c.remote != "" && remote != c.remote
}

func (c basenameCandidate) agreesWithSession(input, remote string) bool {
	if c.remoteConflicts(remote) {
		return false // both asserted an identity, and they disagree
	}
	if !IsPathShaped(input) {
		return true // a bare name reports no location to disagree with
	}
	if !storedPathIsUsable(c.path) {
		return false
	}
	return pathsAgree(absoluteSessionPath(input), c.path)
}

func absoluteSessionPath(input string) string {
	norm := strings.ReplaceAll(input, `\`, "/")
	if strings.HasPrefix(norm, "/") || isWindowsDrivePath(norm) || isDriveRelative(norm) {
		return norm
	}
	abs, err := filepath.Abs(norm)
	if err != nil {
		return norm
	}
	return strings.ReplaceAll(abs, `\`, "/")
}

func isWindowsDrivePath(p string) bool {
	return len(p) >= 3 && p[1] == ':' && p[2] == '/' && isASCIILetter(p[0])
}

func isDriveRelative(p string) bool {
	return len(p) >= 2 && p[1] == ':' && isASCIILetter(p[0])
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// IsPathShaped reports whether s looks like a filesystem path rather than a
func IsPathShaped(s string) bool {
	if strings.ContainsAny(s, `/\`) {
		return true
	}
	return isDriveRelative(s)
}

func storedPathIsUsable(stored string) bool {
	norm := strings.ReplaceAll(stored, `\`, "/")
	if isWindowsDrivePath(norm) {
		norm = norm[2:]
	}
	norm = pathpkg.Clean(norm)
	return strings.HasPrefix(norm, "/") && strings.Trim(norm, "/") != ""
}

func canonicalPath(p string) (string, error) {
	normalized := strings.ReplaceAll(p, `\`, "/")
	resolved, err := filepath.EvalSymlinks(filepath.FromSlash(normalized))
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(filepath.ToSlash(resolved), `\`, "/"), nil
}

func pathsAgree(input, stored string) bool {
	resolvedInput, err := canonicalPath(input)
	if err != nil {
		return false
	}
	resolvedStored, err := canonicalPath(stored)
	if err != nil {
		return false
	}
	return samePath(resolvedInput, resolvedStored)
}

func pathRankLength(p string) int {
	return utf8.RuneCountInString(strings.ReplaceAll(p, `\`, "/"))
}

func samePath(a, b string) bool {
	b = strings.TrimSuffix(b, "/")
	return a == b || strings.HasPrefix(a, b+"/")
}

// ListProjectNames returns all known project names, ordered the same way as
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

// CreateFromCorpus inserts a memory from a third-party benchmark dataset,
func (s *Store) CreateFromCorpus(ctx context.Context, projectID string, m Memory) (string, error) {
	return s.insertMemory(ctx, projectID, m, insertOptions{})
}

func secretContentAndTags(content string, tags []string) []secretField {
	out := make([]secretField, 0, len(tags)+1)
	out = append(out, secretField{"content", content})
	return append(out, secretTagFields(tags)...)
}

func secretTagFields(tags []string) []secretField {
	out := make([]secretField, 0, len(tags))
	for i, tag := range tags {
		out = append(out, secretField{fmt.Sprintf("tags[%d]", i), tag})
	}
	return out
}

func secretSourceRefField(sourceRef string) []secretField {
	if sourceRef == "" {
		return nil
	}
	return []secretField{{"source_ref", sourceRef}}
}

// Create inserts a new memory and returns its ID. The insert and the history
func (s *Store) Create(ctx context.Context, projectID string, m Memory) (string, error) {
	if err := checkMemoryWrite(m); err != nil {
		return "", err
	}
	return s.insertMemory(ctx, projectID, m, insertOptions{recordVerification: true})
}

func checkMemoryWrite(m Memory) error {
	if err := rejectSecretFields(secretContentAndTags(m.Content, m.Tags)...); err != nil {
		return err
	}
	if err := rejectSecretFields(secretSourceRefField(m.SourceRef)...); err != nil {
		return err
	}
	if _, err := boundedSourceRef(m.SourceRef); err != nil {
		return err
	}
	if _, err := boundedAgent(m.Agent); err != nil {
		return err
	}
	if err := checkScope(m.Scope); err != nil {
		return err
	}
	return nil
}

// CreateWithID is Create under a caller-chosen id, and it exists for one caller:
func (s *Store) CreateWithID(ctx context.Context, projectID, id string, m Memory) (string, error) {
	if id == "" {
		return "", errors.New("create memory: no id given; CreateWithID stores the row under the id it is given, and an empty one would be a row the caller cannot find again")
	}
	if err := checkMemoryWrite(m); err != nil {
		return "", err
	}
	return s.insertMemory(ctx, projectID, m, insertOptions{id: id, recordVerification: true})
}

// CreateWithIDFromCorpus is CreateWithID with the corpus opt-out: the caller's id,
func (s *Store) CreateWithIDFromCorpus(ctx context.Context, projectID, id string, m Memory) (string, error) {
	if id == "" {
		return "", errors.New("create memory: no id given; CreateWithIDFromCorpus stores the row under the id it is given, and an empty one would be a row the caller cannot find again")
	}
	if err := checkMemoryWrite(m); err != nil {
		return "", err
	}
	return s.insertMemory(ctx, projectID, m, insertOptions{id: id})
}

type insertOptions struct {
	recordVerification bool

	id string
}

func (s *Store) insertMemory(ctx context.Context, projectID string, m Memory, opts insertOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tags, _ := json.Marshal(m.Tags)
	retention, err := NormalizeRetention(m.Retention)
	if err != nil {
		return "", err
	}
	expires := sessionExpiry(retention, time.Now())
	if retention == RetentionSession && m.ExpiresAt != nil {
		expires = *m.ExpiresAt
	}

	tx, _, err := s.beginWrite(ctx, "create")
	if err != nil {
		return "", fmt.Errorf("begin create: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var id string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO memories (id, project_id, category, content, source, importance, tags,
		                      agent, session_id, source_ref, confidence, scope,
		                      valid_from, valid_until, verified_at,
		                      retention, expires_at)
		VALUES (COALESCE(NULLIF(?, ''), hex(randomblob(16))), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id
	`, opts.id, projectID, m.Category, m.Content, m.Source, m.Importance, string(tags),
		nullIfEmpty(m.Agent), nullIfEmpty(m.SessionID),
		nullIfEmpty(m.SourceRef), m.Confidence, scopeJSON(m.Scope),
		nullIfEmptyPtr(m.ValidFrom), nullIfEmptyPtr(m.ValidUntil),
		nullIfEmptyPtr(m.VerifiedAt), retention, expires).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create memory: %w", err)
	}
	if err := appendHistoryTx(ctx, tx, id, phaseSave, provenanceFromMemory(m)); err != nil {
		return "", err
	}
	if err := appendEvidenceTx(ctx, tx, id, evidenceObserved, provenanceFromMemory(m), false); err != nil {
		return "", err
	}
	if opts.recordVerification {
		if err := appendVerificationIfStatedTx(ctx, tx, id, provenanceFromMemory(m), m.VerifiedAt); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit create: %w", err)
	}
	if s.onSave != nil {
		s.onSave(projectID)
	}
	return id, nil
}

// Upsert checks for an existing similar memory, probing in two stages:

// MaxSourceRefLen is the byte cap on a write-time source reference, at the store
const MaxSourceRefLen = 512

// MaxAgentLen is the byte cap on a write-time agent, for the same reason and by
const MaxAgentLen = 128

// StoredStampLayout is the layout a writer stores a validity stamp in: SQLite's
const StoredStampLayout = "2006-01-02 15:04:05"

// DateStampLayout is the whole-day form, the other shape SQLite's date() produces.
const DateStampLayout = "2006-01-02"

// StampLayouts is every layout a reader of a validity column accepts, in the order
var StampLayouts = []string{StoredStampLayout, DateStampLayout}

// ParseStamp reads a stored validity stamp over every layout StampLayouts names,
func ParseStamp(s string) (time.Time, bool) {
	for _, layout := range StampLayouts {
		if at, err := time.Parse(layout, s); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}

// AcceptedStampForms are the forms a caller may write a validity stamp in, in
var AcceptedStampForms = []string{time.RFC3339, DateStampLayout}

// NormalizeStampArg normalizes one caller-supplied stamp into the stored layout.
func NormalizeStampArg(field, s string, endOfDay bool) (*string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	for _, layout := range AcceptedStampForms {
		if t, err := time.Parse(layout, s); err == nil {
			stored := t.UTC()
			if endOfDay && layout == DateStampLayout {
				stored = stored.Add(24*time.Hour - time.Second)
			}
			out := stored.Format(StoredStampLayout)
			return &out, nil
		}
	}
	return nil, fmt.Errorf("%s must be RFC 3339 (2026-10-01T09:00:00Z) or a date (2026-10-01), got %s", field, RefusalArg(field, s))
}

// NormalizeImportedStamp is NormalizeStampArg for a stamp read from a portable
func NormalizeImportedStamp(field string, raw *string, endOfDay bool) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	if _, err := time.Parse(StoredStampLayout, *raw); err == nil {
		out := *raw
		return &out, nil
	}
	return NormalizeStampArg(field, *raw, endOfDay)
}

// CheckConfidence refuses a trust rating outside [0,1], the rule the writer tools
func CheckConfidence(f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("confidence must be a finite number between 0 and 1, got %v", f)
	}
	if f < 0 || f > 1 {
		return fmt.Errorf("confidence must be between 0 and 1, got %v", f)
	}
	return nil
}

// CheckWindowOrder refuses a window that does not end after it starts, judging the
func CheckWindowOrder(v, stored Validity) error {
	from, until := v.ValidFrom, v.ValidUntil
	if from == nil {
		from = stored.ValidFrom
	}
	if until == nil {
		until = stored.ValidUntil
	}
	if from == nil || until == nil {
		return nil
	}
	fromAt, fromOK := ParseStamp(*from)
	untilAt, untilOK := ParseStamp(*until)
	if !fromOK || !untilOK {
		return nil
	}
	if !untilAt.After(fromAt) {
		return fmt.Errorf("valid_until %s is not after valid_from %s — a window must end after it starts, or a claim that is true for no time at all", *until, *from)
	}
	return nil
}

// Validity is the optional temporal claim attached to one write: when the
type Validity struct {
	ValidFrom  *string
	ValidUntil *string
	VerifiedAt *string
}

// IsZero reports whether the claim is entirely absent, so a write can leave the
func (v Validity) IsZero() bool {
	return v.ValidFrom == nil && v.ValidUntil == nil && v.VerifiedAt == nil
}

// Provenance is the optional write-time context attached to a single save.
type Provenance struct {
	Agent      string
	SessionID  string
	SourceRef  string
	Confidence *float64
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullStringPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

func readableStampPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	if at, ok := ParseStamp(s.String); !ok || at.IsZero() {
		return nil
	}
	return &s.String
}

func nullIfEmptyPtr(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

func boundedAgent(agent string) (any, error) {
	if agent == "" {
		return nil, nil
	}
	if len(agent) > MaxAgentLen {
		return nil, fmt.Errorf("agent must be at most %d bytes, got %d — it names the harness that wrote a row, not a document", MaxAgentLen, len(agent))
	}
	return agent, nil
}

func boundedSourceRef(s string) (any, error) {
	if s == "" {
		return nil, nil
	}
	if len(s) > MaxSourceRefLen {
		return nil, fmt.Errorf("source_ref must be at most %d bytes, got %d — it is a file path, commit or URL, not a document", MaxSourceRefLen, len(s))
	}
	return s, nil
}

// Upsert stores a memory with no provenance, exactly as before.
type UpsertOptions struct {
	Provenance Provenance
	Scope      map[string]string
	Validity   Validity

	FoldOnly bool

	Pin bool

	Retention string

	SkipExactContent bool

	foldReplaceableOnly bool
}

func mergeRetention(current, incoming string) string {
	rank := func(tier string) int {
		switch tier {
		case RetentionSession:
			return 1
		case RetentionPersistent:
			return 3
		default:
			return 2
		}
	}
	if IsValidRetention(current) && rank(current) >= rank(incoming) {
		return current
	}
	if IsValidRetention(incoming) {
		return incoming
	}
	return RetentionProject
}

func raiseRetentionTx(ctx context.Context, tx *sql.Tx, id, incoming string, now time.Time) (string, error) {
	if id == "" {
		return incoming, nil
	}
	var current string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(retention, 'project') FROM memories WHERE id = ?`, id).Scan(&current)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return incoming, nil
		}
		return "", fmt.Errorf("read the fold target's retention: %w", err)
	}
	merged := mergeRetention(current, incoming)
	if merged == current {
		if merged == RetentionSession {
			if _, err := tx.ExecContext(ctx,
				`UPDATE memories SET expires_at = ? WHERE id = ?`, sessionExpiry(merged, now), id); err != nil {
				return "", fmt.Errorf("refresh the fold target's session expiry: %w", err)
			}
		}
		return merged, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE memories
		SET retention = ?,
		    expires_at = CASE WHEN ? = 'session' THEN expires_at ELSE NULL END
		WHERE id = ?`, merged, merged, id); err != nil {
		return "", fmt.Errorf("raise retention: %w", err)
	}
	return merged, nil
}

func sessionExpiry(tier string, now time.Time) any {
	if tier != RetentionSession {
		return nil
	}
	return now.UTC().Add(SessionTTL).Format("2006-01-02 15:04:05")
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) pinMemoryTx(ctx context.Context, tx *sql.Tx, id string, pin bool) error {
	if !pin || id == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memories SET pinned = 1 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("pin memory: %w", err)
	}
	return nil
}

func foldTargetStillLive(ctx context.Context, tx *sql.Tx, projectID, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	var live int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM memories m
		WHERE m.id = ? AND m.project_id = ? AND m.resolved_at IS NULL
		  AND NOT EXISTS (
		      SELECT 1
		      FROM memory_links l
		      JOIN memories s ON s.id = l.source_id
		      WHERE l.target_id = m.id
		        AND l.relation = 'supersedes'
		        AND l.invalidated_at IS NULL
		        AND NOT `+scopesConflictSQL("s.scope", "m.scope")+`
		  )
	`, id, projectID).Scan(&live)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return live == 1, nil
}

func (s *Store) Upsert(ctx context.Context, projectID, category, content, source string, importance float32, tags []string) (id string, duplicateOf string, score float64, err error) {
	return s.UpsertWithOptions(ctx, projectID, category, content, source, importance, tags, UpsertOptions{})
}

// UpsertWithProvenance is Upsert plus optional write-time provenance.
func (s *Store) UpsertWithProvenance(ctx context.Context, projectID, category, content, source string, importance float32, tags []string, prov Provenance) (id string, duplicateOf string, score float64, err error) {
	return s.UpsertWithOptions(ctx, projectID, category, content, source, importance, tags, UpsertOptions{Provenance: prov})
}

type sqlExecutor interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type storeTxContextKey struct{}

func withStoreTx(ctx context.Context, tx *sql.Tx) context.Context {
	return context.WithValue(ctx, storeTxContextKey{}, tx)
}

func storeTxFromContext(ctx context.Context) (*sql.Tx, bool) {
	tx, ok := ctx.Value(storeTxContextKey{}).(*sql.Tx)
	return tx, ok && tx != nil
}

type applyViewContextKey struct{}

type applyView struct {
	concurrent          map[string]bool
	scopeKept           map[string]bool
	overflowKept        map[string]bool
	crossProjectFrom    int
	crossProjectDropped EmissionDrops
	promotions          []pendingPromotion
	promoted            int
	failedPromotions    []failedPromotion
	promotionDropped    EmissionDrops
}

type pendingPromotion struct {
	m     Memory
	scope map[string]string
}

type failedPromotion struct {
	pendingPromotion
	err error
}

func withApplyView(ctx context.Context, view *applyView) context.Context {
	return context.WithValue(ctx, applyViewContextKey{}, view)
}

func applyViewFromContext(ctx context.Context) (*applyView, bool) {
	view, ok := ctx.Value(applyViewContextKey{}).(*applyView)
	return view, ok && view != nil
}

// CheckUpsertArguments is every refusal UpsertWithOptions makes from its
func CheckUpsertArguments(content string, tags []string, opts UpsertOptions) error {
	if err := rejectSecretFields(secretContentAndTags(content, tags)...); err != nil {
		return err
	}
	if err := rejectSecretFields(secretSourceRefField(opts.Provenance.SourceRef)...); err != nil {
		return err
	}
	if err := checkReservedSourceRef(opts.Provenance.SourceRef); err != nil {
		return err
	}
	if _, err := boundedSourceRef(opts.Provenance.SourceRef); err != nil {
		return err
	}
	if err := checkScope(opts.Scope); err != nil {
		return err
	}
	if _, err := boundedAgent(opts.Provenance.Agent); err != nil {
		return err
	}
	_, err := NormalizeRetention(opts.Retention)
	return err
}

// UpsertWithOptions is Upsert plus provenance and/or scope.
func (s *Store) UpsertWithOptions(ctx context.Context, projectID, category, content, source string, importance float32, tags []string, opts UpsertOptions) (id string, duplicateOf string, score float64, err error) {
	if err := CheckUpsertArguments(content, tags, opts); err != nil {
		return "", "", 0, err
	}
	retention, err := NormalizeRetention(opts.Retention)
	if err != nil {
		return "", "", 0, err
	}
	now := time.Now()
	expires := sessionExpiry(retention, now)

	parentTx, inTx := storeTxFromContext(ctx)
	if !inTx {
		s.mu.Lock()
		defer s.mu.Unlock()
	}

	tx := parentTx
	ownTx := !inTx
	var lock writeLock
	if ownTx {
		if tx, lock, err = s.beginWrite(ctx, "upsert"); err != nil {
			return "", "", 0, fmt.Errorf("begin upsert tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
	}
	db := sqlExecutor(tx)
	commit := func() error {
		if !ownTx {
			return nil
		}
		if cerr := tx.Commit(); cerr != nil {
			return fmt.Errorf("commit upsert tx: %w", cerr)
		}
		lock.reportHold("upsert", time.Now())
		return nil
	}

	var existingID string

	if opts.SkipExactContent {
		var present string
		switch qerr := db.QueryRowContext(ctx, `
			SELECT id FROM memories WHERE project_id = ? AND content = ? LIMIT 1
		`, projectID, content).Scan(&present); {
		case qerr == nil:
			return "", present, 0, nil
		case !errors.Is(qerr, sql.ErrNoRows):
			return "", "", 0, fmt.Errorf("look up exact content: %w", qerr)
		}
	}

	ftsQuery := sanitizeFTSN(content, 30, s.logger)
	incomingScope := scopeJSON(opts.Scope)
	scopeClause := ""
	scopeArgs := []any{}
	if incomingScope != nil {
		scopeClause = "AND NOT " + scopesConflictSQL("m.scope", "?")
		scopeArgs = []any{incomingScope, incomingScope}
	}
	if opts.foldReplaceableOnly {
		scopeClause += " AND " + replaceableRowSQL
	}
	sameQuery, sameArgs := dedupLookupStatement(false, scopeClause, scopeArgs, projectID, category, ftsQuery)
	rows, err := db.QueryContext(ctx, sameQuery, sameArgs...)
	if err != nil {
		s.logger.Warn("upsert dedup probe failed; saving without dedup", "probe", "same-category", "err", err)
	}
	if err == nil {
		newTokens := tokenizeContent(content)
		var bestSim float64
		for len(newTokens) > 0 && rows.Next() {
			var candID, candContent string
			if scanErr := rows.Scan(&candID, &candContent); scanErr != nil {
				continue
			}
			candTokens := tokenizeContent(candContent)
			if opts.FoldOnly && !foldOnlyEquivalent(content, candContent) {
				continue
			}
			if statesDifferentValue(content, candContent) || retires(content, candContent) {
				continue
			}
			sim := mergeScore(newTokens, candTokens)
			if sim > 0 && sim > bestSim {
				bestSim = sim
				existingID = candID
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			existingID = "" // treat a broken candidate scan as no match
		}
		_ = rows.Close()
		score = bestSim
	}

	if existingID == "" {
		crossQuery, crossArgs := dedupLookupStatement(true, scopeClause, scopeArgs, projectID, category, ftsQuery)
		crossRows, crossErr := db.QueryContext(ctx, crossQuery, crossArgs...)
		if crossErr != nil {
			s.logger.Warn("upsert dedup probe failed; saving without dedup", "probe", "cross-category", "err", crossErr)
		}
		if crossErr == nil {
			newTokens := tokenizeContent(content)
			var bestJaccard float64
			for len(newTokens) > 0 && crossRows.Next() {
				var candID, candContent string
				if scanErr := crossRows.Scan(&candID, &candContent); scanErr != nil {
					continue
				}
				candTokens := tokenizeContent(candContent)
				if opts.FoldOnly && !foldOnlyEquivalent(content, candContent) {
					continue
				}
				if statesDifferentValue(content, candContent) || retires(content, candContent) {
					continue
				}
				j := jaccard(newTokens, candTokens)
				if j >= upsertCrossCategoryThreshold && j > bestJaccard {
					bestJaccard = j
					existingID = candID
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
	}

	tagsJSON, _ := json.Marshal(tags)

	if existingID != "" {

		if opts.FoldOnly {
			live, liveErr := foldTargetStillLive(ctx, tx, projectID, existingID)
			if liveErr != nil {
				return "", "", 0, fmt.Errorf("re-verify fold target: %w", liveErr)
			}
			if !live {
				existingID = ""
			}
		}

		if existingID != "" {
			if opts.Validity.ValidFrom != nil || opts.Validity.ValidUntil != nil {
				var targetFrom, targetUntil sql.NullString
				readErr := tx.QueryRowContext(ctx,
					`SELECT valid_from, valid_until FROM memories WHERE id = ? AND project_id = ?`,
					existingID, projectID,
				).Scan(&targetFrom, &targetUntil)
				if readErr != nil && readErr != sql.ErrNoRows {
					return "", "", 0, fmt.Errorf("lookup fold target window: %w", readErr)
				}
				if orderErr := CheckWindowOrder(opts.Validity, Validity{
					ValidFrom:  nullStringPtr(targetFrom),
					ValidUntil: nullStringPtr(targetUntil),
				}); orderErr != nil {
					return "", "", 0, orderErr
				}
			}
			targetSourceRef := opts.Provenance.SourceRef
			if targetSourceRef != "" {
				var targetRef sql.NullString
				readErr := tx.QueryRowContext(ctx,
					`SELECT source_ref FROM memories WHERE id = ? AND project_id = ?`,
					existingID, projectID,
				).Scan(&targetRef)
				if readErr != nil && readErr != sql.ErrNoRows {
					return "", "", 0, fmt.Errorf("lookup fold target source_ref: %w", readErr)
				}
				keptRef, refErr := preserveDecisionLinks(targetSourceRef, targetRef.String)
				if refErr != nil {
					return "", "", 0, refErr
				}
				targetSourceRef = keptRef
			}
			res, updateErr := tx.ExecContext(ctx, `
				UPDATE memories
				SET importance = MIN(1.0, importance + ?), access_count = access_count + 1,
				    valid_from  = COALESCE(?, valid_from),
				    valid_until = COALESCE(?, valid_until),
				    verified_at = COALESCE(?, verified_at),
				    confidence  = COALESCE(?, confidence),
				    agent       = COALESCE(NULLIF(?, ''), agent),
				    session_id  = COALESCE(NULLIF(?, ''), session_id),
				    source_ref  = COALESCE(NULLIF(?, ''), source_ref)
				WHERE id = ? AND project_id = ?
			`, importance*0.2,
				nullIfEmptyPtr(opts.Validity.ValidFrom), nullIfEmptyPtr(opts.Validity.ValidUntil),
				nullIfEmptyPtr(opts.Validity.VerifiedAt), opts.Provenance.Confidence,
				opts.Provenance.Agent, opts.Provenance.SessionID, targetSourceRef,
				existingID, projectID)
			if updateErr != nil {
				return "", "", 0, fmt.Errorf("strengthen memory: %w", updateErr)
			}
			affected, affectedErr := res.RowsAffected()
			if affectedErr != nil {
				return "", "", 0, fmt.Errorf("strengthen memory rows: %w", affectedErr)
			}
			if affected == 0 {
				return "", "", 0, fmt.Errorf("fold target %s disappeared during the update", RefusalArg("memory_id", existingID))
			}
			if opts.FoldOnly {
				err := appendHistoryEventsTx(ctx, tx, []historyEvent{{
					phase:         phaseMerge,
					prov:          opts.Provenance,
					mergedContent: content,
				}}, []string{existingID})
				if err != nil {
					return "", "", 0, err
				}
			} else if err := appendHistoryTx(ctx, tx, existingID, phaseMerge, opts.Provenance); err != nil {
				return "", "", 0, err
			}
			if err := appendEvidenceTx(ctx, tx, existingID, evidenceObserved, opts.Provenance, false); err != nil {
				return "", "", 0, err
			}
			if err := appendVerificationIfStatedTx(ctx, tx, existingID, opts.Provenance, opts.Validity.VerifiedAt); err != nil {
				return "", "", 0, err
			}
		}

		if opts.FoldOnly && existingID != "" {
			if err = s.pinMemoryTx(ctx, tx, existingID, opts.Pin); err != nil {
				return "", "", 0, err
			}
			if _, err = raiseRetentionTx(ctx, tx, existingID, retention, now); err != nil {
				return "", "", 0, err
			}
			if err = commit(); err != nil {
				return "", "", 0, err
			}
			return existingID, existingID, score, nil
		}

		if err = tx.QueryRowContext(ctx, `
			INSERT INTO memories (project_id, category, content, source, importance, tags,
			                      agent, session_id, source_ref, confidence, scope, pinned,
			                      valid_from, valid_until, verified_at,
			                      retention, expires_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			RETURNING id
		`, projectID, category, content, source, importance, string(tagsJSON),
			nullIfEmpty(opts.Provenance.Agent), nullIfEmpty(opts.Provenance.SessionID),
			nullIfEmpty(opts.Provenance.SourceRef), opts.Provenance.Confidence,
			scopeJSON(opts.Scope), boolToInt(opts.Pin),
			nullIfEmptyPtr(opts.Validity.ValidFrom), nullIfEmptyPtr(opts.Validity.ValidUntil),
			nullIfEmptyPtr(opts.Validity.VerifiedAt), retention, expires).Scan(&id); err != nil {
			return "", "", 0, fmt.Errorf("create memory: %w", err)
		}
		if err := appendHistoryTx(ctx, tx, id, phaseSave, opts.Provenance); err != nil {
			return "", "", 0, err
		}
		if err := appendEvidenceTx(ctx, tx, id, evidenceObserved, opts.Provenance, false); err != nil {
			return "", "", 0, err
		}
		if err := appendVerificationIfStatedTx(ctx, tx, id, opts.Provenance, opts.Validity.VerifiedAt); err != nil {
			return "", "", 0, err
		}

		if err = s.pinMemoryTx(ctx, tx, existingID, opts.Pin); err != nil {
			return "", "", 0, err
		}
		if _, err = raiseRetentionTx(ctx, tx, existingID, retention, now); err != nil {
			return "", "", 0, err
		}

		if existingID != "" {
			if _, err = tx.ExecContext(ctx, `
				INSERT INTO memory_links (source_id, target_id, relation, strength, source)
				VALUES (?, ?, 'duplicate', ?, 'auto')
				ON CONFLICT(source_id, target_id, relation) DO UPDATE SET
					strength = MAX(strength, excluded.strength),
					invalidated_at = NULL
			`, id, existingID, score); err != nil {
				return "", "", 0, fmt.Errorf("link duplicate: %w", err)
			}
		}

		if err = commit(); err != nil {
			return "", "", 0, err
		}

		if !inTx && s.onSave != nil {
			s.onSave(projectID)
		}
		return id, existingID, score, nil
	}

	if err = db.QueryRowContext(ctx, `
		INSERT INTO memories (project_id, category, content, source, importance, tags,
		                      agent, session_id, source_ref, confidence, scope, pinned,
		                      valid_from, valid_until, verified_at,
		                      retention, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id
	`, projectID, category, content, source, importance, string(tagsJSON),
		nullIfEmpty(opts.Provenance.Agent), nullIfEmpty(opts.Provenance.SessionID),
		nullIfEmpty(opts.Provenance.SourceRef), opts.Provenance.Confidence,
		scopeJSON(opts.Scope), boolToInt(opts.Pin),
		nullIfEmptyPtr(opts.Validity.ValidFrom), nullIfEmptyPtr(opts.Validity.ValidUntil),
		nullIfEmptyPtr(opts.Validity.VerifiedAt), retention, expires).Scan(&id); err != nil {
		return "", "", 0, fmt.Errorf("create memory: %w", err)
	}
	if err := appendHistoryTx(ctx, tx, id, phaseSave, opts.Provenance); err != nil {
		return "", "", 0, err
	}
	if err := appendEvidenceTx(ctx, tx, id, evidenceObserved, opts.Provenance, false); err != nil {
		return "", "", 0, err
	}
	if err := appendVerificationIfStatedTx(ctx, tx, id, opts.Provenance, opts.Validity.VerifiedAt); err != nil {
		return "", "", 0, err
	}
	if err = commit(); err != nil {
		return "", "", 0, err
	}
	if !inTx && s.onSave != nil {
		s.onSave(projectID)
	}
	return id, "", 0, nil
}

// DecayRankingSQL is the composite-score ranking expression shared by
var DecayRankingSQL = decayRankingSQL(true)

// DecayRankingSQLWithTier is the same expression with or without the tier half,
func DecayRankingSQLWithTier(hasTier bool) string {
	return decayRankingSQL(hasTier)
}

func decayRankingSQL(hasTier bool) string {
	tier := ""
	if hasTier {
		tier = retentionDecayFactorSQL
	}
	return `
	importance
	* CASE
		WHEN pinned = 1 THEN 1.0
		WHEN category IN ('preference', 'convention') THEN 1.0
		WHEN category = 'fact' AND content NOT GLOB '*[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*' THEN 1.0
		WHEN category IN ('pattern', 'architecture')
			OR (category = 'fact' AND content GLOB '*[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]*') THEN
			MAX(0.3, 1.0 / (1.0 + (julianday('now') - julianday(created_at)) / 45.0))
		ELSE
			MAX(0.15, 1.0 / (1.0 + (julianday('now') - julianday(created_at)) / 30.0))
	END` + tier + `
`
}

// GetTopMemories returns the top N memories ranked by composite score
func (s *Store) GetTopMemories(ctx context.Context, projectID string, limit int) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+memoryColumns+`
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

	if len(results) >= 2 {
		ids := make([]string, len(results))
		supersedeProtected := make(map[string]bool, len(results))
		for i, m := range results {
			ids[i] = m.ID
			supersedeProtected[m.ID] = RetentionExempt(m)
		}
		penalty, err := SupersedePenalties(ctx, s.queryDB(), ids, supersedeProtected)
		if err != nil {
			s.logger.Debug("get top memories: supersede demotion lookup failed", "error", err)
		} else if len(penalty) > 0 {
			results = StableDemote(results, func(m Memory) string { return m.ID }, penalty)
		}
	}
	if len(results) > limit {
		ids := make([]string, len(results))
		protected := make(map[string]bool, len(results))
		for i, m := range results {
			ids[i] = m.ID
			protected[m.ID] = m.Pinned || RetentionExempt(m)
		}
		pairs, err := nearDuplicatePenaltyRows(ctx, s.queryDB(), ids, protected, s.demotionThreshold)
		if err != nil {
			s.logger.Debug("get top memories: demotion lookup failed", "error", err)
		} else {
			kept, xerr := exemptReplacementPairs(ctx, s.queryDB(), pairs, results, time.Now().UTC())
			if xerr != nil {
				s.logger.Debug("get top memories: replacement pair lookup failed, demoting without the exemption", "error", xerr)
			}
			penalty, _ := demotionVerdicts(kept)
			results = StableDemote(results, func(m Memory) string { return m.ID }, penalty)
		}
		results = results[:min(limit, len(results))]
	}
	return results, nil
}

// SearchFTS searches memories using full-text search. It returns every match in
func (s *Store) SearchFTS(ctx context.Context, projectID, query string, limit int) ([]Memory, error) {
	memories, err := s.searchFTSValidating(ctx, projectID, query, limit, time.Time{}, false)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(memories) > limit {
		memories = memories[:limit]
	}
	return memories, nil
}

// SearchFTSAll searches memories across ALL projects using full-text search.
func (s *Store) SearchFTSAll(ctx context.Context, query string, limit int) ([]Memory, error) {
	memories, err := s.searchFTSValidating(ctx, "", query, limit, time.Time{}, true)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(memories) > limit {
		memories = memories[:limit]
	}
	return memories, nil
}

func (s *Store) searchFTSValidating(ctx context.Context, projectID, query string, limit int, now time.Time, allProjects bool) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stmt, args := ftsSearchStatement(projectID, sanitizeFTS(query, s.logger), limit, now, allProjects)
	rows, err := s.queryDB().QueryContext(ctx, stmt, args...)
	if err != nil {
		if allProjects {
			return nil, fmt.Errorf("search all memories: %w", err)
		}
		return nil, fmt.Errorf("search memories: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMemories(rows)
}

// GetByCategory returns memories of a specific category.
func (s *Store) GetByCategory(ctx context.Context, projectID, category string, limit int) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+memoryColumns+`
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

	rows, err := s.queryDB().QueryContext(ctx, `
		SELECT `+memoryColumns+`
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

// ListMemories returns a project's memories for browsing, narrowed by the two
func (s *Store) ListMemories(ctx context.Context, projectID, category, retention string, limit int) ([]Memory, error) {
	if category != "" && !IsValidCategory(category) {
		return nil, fmt.Errorf("invalid category %s — must be one of %s", RefusalArg("category", category), categoryList())
	}
	if retention != "" && !IsValidRetention(retention) {
		return nil, InvalidRetentionError(retention)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	scope := "project_id = ?"
	if category != "" || retention != "" {
		scope = "(project_id = ? OR project_id = '_global')"
	}
	query := `
		SELECT ` + memoryColumns + `
		FROM memories
		WHERE ` + scope
	var args []any
	args = append(args, projectID)
	if category != "" {
		query += " AND category = ?"
		args = append(args, category)
	}
	if retention != "" {
		query += " AND retention = ?"
		args = append(args, retention)
	}
	query += " ORDER BY importance DESC, created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.queryDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list memories: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMemories(rows)
}

func categoryList() string {
	return "architecture, decision, pattern, convention, gotcha, dependency, preference, fact"
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

	_, err := s.execGuardedWrite(ctx, "touch", query, args...)
	return err
}

// ResolveCandidates returns the project's memories that are eligible for
func (s *Store) ResolveCandidates(ctx context.Context, projectID string) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+memoryColumns+`
		FROM memories
		WHERE project_id = ?
		  AND resolved_at IS NULL
		  AND pinned = 0
		  AND category NOT IN ('convention', 'preference')
		  AND `+retentionExemptSQL+`
		ORDER BY created_at DESC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("resolve candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMemories(rows)
}

// ResolvedCandidates returns the project's memories resolve has ALREADY
func (s *Store) ResolvedCandidates(ctx context.Context, projectID string) ([]Memory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+memoryColumns+`
		FROM memories
		WHERE project_id = ?
		  AND resolved_at IS NOT NULL
		  AND pinned = 0
		  AND category NOT IN ('convention', 'preference')
		  AND `+retentionExemptSQL+`
		ORDER BY created_at DESC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("resolved candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanMemories(rows)
}

// ClearResolved clears resolved_at on the given memory IDs, returning them to
func (s *Store) ClearResolved(ctx context.Context, projectID string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "clear-resolved")
	if err != nil {
		return 0, fmt.Errorf("begin clear resolved: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	total := 0
	for len(ids) > 0 {
		batch := ids
		if len(batch) > setResolvedBatchSize {
			batch = ids[:setResolvedBatchSize]
		}
		ids = ids[len(batch):]

		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch)+1)
		for i, id := range batch {
			placeholders[i] = "?"
			args = append(args, id)
		}
		args = append(args, projectID)
		changed, err := selectIDs(ctx, tx, `
			SELECT id FROM memories
			WHERE id IN (`+strings.Join(placeholders, ",")+`)
			  AND project_id = ?
			  AND resolved_at IS NOT NULL
			  AND pinned = 0`, args...)
		if err != nil {
			return 0, fmt.Errorf("select rows to unresolve: %w", err)
		}
		if len(changed) == 0 {
			continue
		}
		q := `UPDATE memories SET resolved_at = NULL
		      WHERE id IN (` + strings.Join(placeholders, ",") + `)
		        AND project_id = ?
		        AND resolved_at IS NOT NULL
		        AND pinned = 0`
		result, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return 0, fmt.Errorf("clear resolved: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("clear resolved rows affected: %w", err)
		}
		total += int(n)
		if err := appendHistoryForIDsTx(ctx, tx, changed, phaseUnresolve, Provenance{}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit clear resolved: %w", err)
	}
	return total, nil
}

const setResolvedBatchSize = 500

const (
	setResolvedGuardSQL = `
		  AND project_id != ''
		  AND (? = '' OR project_id = ?)
		  AND resolved_at IS NULL
		  AND pinned = 0
		  AND category NOT IN ('convention', 'preference')
		  AND ` + retentionExemptSQL
	setResolvedSelectSQL = `
		SELECT id FROM memories
		WHERE id IN (%s)
		` + setResolvedGuardSQL
	setResolvedUpdateSQL = `
		UPDATE memories SET resolved_at = datetime('now'), resolve_kept_hash = ''
		WHERE id IN (%s)
		` + setResolvedGuardSQL
	setResolvedJudgedSelectSQL = `
		SELECT id FROM memories
		WHERE (id, content, project_id) IN (VALUES %s)
		` + setResolvedGuardSQL
	setResolvedJudgedUpdateSQL = `
		UPDATE memories SET resolved_at = datetime('now'), resolve_kept_hash = ''
		WHERE (id, content, project_id) IN (VALUES %s)
		` + setResolvedGuardSQL
)

type judgedRow struct {
	content string
	project string
}

// ResolveJustification is the deterministic signal a resolve pass read, before
type ResolveJustification struct {
	EdgeSources []string
	Corrections []Memory
}

func (j ResolveJustification) empty() bool {
	return len(j.EdgeSources) == 0 && len(j.Corrections) == 0
}

func resolveJustificationHoldsTx(ctx context.Context, tx *sql.Tx, target string, j ResolveJustification) (bool, error) {
	for _, src := range j.EdgeSources {
		var one int
		err := tx.QueryRowContext(ctx, `
			SELECT 1 FROM memory_links
			WHERE source_id = ? AND target_id = ? AND relation = 'supersedes'
			  AND source = 'llm' AND invalidated_at IS NULL`, src, target).Scan(&one)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("check supersedes edge: %w", err)
		}
	}
	for _, c := range j.Corrections {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM memories WHERE id = ? AND content = ?`, c.ID, c.Content).Scan(&one)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("check correction: %w", err)
		}
	}
	return false, nil
}

func (s *Store) setResolvedStampTx(ctx context.Context, ids []string, projectID string, prov Provenance, judged map[string]judgedRow, why map[string]ResolveJustification) (stamped, stale []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "set-resolved")
	if err != nil {
		return nil, nil, fmt.Errorf("begin set resolved: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for len(ids) > 0 {
		batch := ids
		if len(batch) > setResolvedBatchSize {
			batch = ids[:setResolvedBatchSize]
		}
		ids = ids[len(batch):]

		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch)+2)
		for i, id := range batch {
			placeholders[i] = "?"
			args = append(args, id)
		}
		args = append(args, projectID, projectID)
		in := strings.Join(placeholders, ",")

		if judged == nil {
			changed, err := selectIDs(ctx, tx, fmt.Sprintf(setResolvedSelectSQL, in), args...)
			if err != nil {
				return nil, nil, fmt.Errorf("select rows to resolve: %w", err)
			}
			if len(changed) == 0 {
				continue
			}
			result, err := tx.ExecContext(ctx, fmt.Sprintf(setResolvedUpdateSQL, in), args...)
			if err != nil {
				return nil, nil, fmt.Errorf("set resolved: %w", err)
			}
			if _, err := result.RowsAffected(); err != nil {
				return nil, nil, fmt.Errorf("set resolved rows affected: %w", err)
			}
			stamped = append(stamped, changed...)
			if err := appendHistoryForIDsTx(ctx, tx, changed, phaseResolve, prov); err != nil {
				return nil, nil, err
			}
			continue
		}

		eligible, err := selectIDs(ctx, tx, fmt.Sprintf(setResolvedSelectSQL, in), args...)
		if err != nil {
			return nil, nil, fmt.Errorf("select rows to resolve: %w", err)
		}
		if len(eligible) == 0 {
			continue
		}
		pairs := make([]string, len(batch))
		pairArgs := make([]interface{}, 0, len(batch)*3+2)
		for i, id := range batch {
			pairs[i] = "(?,?,?)"
			pairArgs = append(pairArgs, id, judged[id].content, judged[id].project)
		}
		pairArgs = append(pairArgs, projectID, projectID)
		matched, err := selectIDs(ctx, tx, fmt.Sprintf(setResolvedJudgedSelectSQL, strings.Join(pairs, ",")), pairArgs...)
		if err != nil {
			return nil, nil, fmt.Errorf("select rows still holding the judged content: %w", err)
		}
		matchedSet := make(map[string]bool, len(matched))
		for _, id := range matched {
			matchedSet[id] = true
		}
		fresh := make([]string, 0, len(eligible))
		for _, id := range eligible {
			if matchedSet[id] {
				fresh = append(fresh, id)
			} else {
				stale = append(stale, id)
			}
		}
		if len(why) > 0 {
			held := fresh[:0:0]
			for _, id := range fresh {
				j := why[id]
				if j.empty() {
					held = append(held, id)
					continue
				}
				ok, err := resolveJustificationHoldsTx(ctx, tx, id, j)
				if err != nil {
					return nil, nil, err
				}
				if ok {
					held = append(held, id)
				} else {
					stale = append(stale, id)
				}
			}
			fresh = held
		}
		if len(fresh) == 0 {
			continue
		}
		pairs = pairs[:0]
		pairArgs = pairArgs[:0]
		for _, id := range fresh {
			pairs = append(pairs, "(?,?,?)")
			pairArgs = append(pairArgs, id, judged[id].content, judged[id].project)
		}
		pairArgs = append(pairArgs, projectID, projectID)
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(setResolvedJudgedUpdateSQL, strings.Join(pairs, ",")), pairArgs...); err != nil {
			return nil, nil, fmt.Errorf("set resolved: %w", err)
		}
		stamped = append(stamped, fresh...)
		if err := appendHistoryForIDsTx(ctx, tx, fresh, phaseResolve, prov); err != nil {
			return nil, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit set resolved: %w", err)
	}
	return stamped, stale, nil
}

// SetResolved stamps resolved_at = now on the given memory IDs, dropping them
func (s *Store) SetResolved(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	stamped, _, err := s.setResolvedStampTx(ctx, ids, "", Provenance{}, nil, nil)
	return len(stamped), err
}

// SetResolvedJudged stamps resolved_at on the memories a classifier confirmed
func (s *Store) SetResolvedJudged(ctx context.Context, judged []Memory) (stamped, stale []string, err error) {
	return s.SetResolvedJustified(ctx, judged, nil)
}

// SetResolvedJustified is SetResolvedJudged for a pass that demoted some rows on
func (s *Store) SetResolvedJustified(ctx context.Context, judged []Memory, why map[string]ResolveJustification) (stamped, stale []string, err error) {
	if len(judged) == 0 {
		return nil, nil, nil
	}
	ids := make([]string, 0, len(judged))
	want := make(map[string]judgedRow, len(judged))
	for _, m := range judged {
		if _, ok := want[m.ID]; ok {
			continue
		}
		want[m.ID] = judgedRow{content: m.Content, project: m.ProjectID}
		ids = append(ids, m.ID)
	}
	return s.setResolvedStampTx(ctx, ids, "", Provenance{}, want, why)
}

// MarkResolved stamps resolved_at on the named ids IN projectID and returns the
func (s *Store) MarkResolved(ctx context.Context, projectID string, ids []string, prov Provenance) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	switch projectID {
	case "":
		return nil, fmt.Errorf("mark resolved: a project is required, so no memory can be reached from it")
	case GlobalProjectID:
		return nil, fmt.Errorf("mark resolved: refusing to mark in the %s project — it holds the promoted memories every project injects, and a lifecycle pass over one project cannot decide for all of them", GlobalProjectID)
	}
	stamped, _, err := s.setResolvedStampTx(ctx, ids, projectID, prov, nil, nil)
	return stamped, err
}

// ResolveKeptHashes returns the key resolve recorded when it last judged each
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

// MarkResolveKept records KEEP verdicts (id -> resolve's keep stamp: the
func (s *Store) MarkResolveKept(ctx context.Context, projectID string, hashes map[string]string) error {
	if len(hashes) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "mark-resolve-kept")
	if err != nil {
		return fmt.Errorf("begin mark resolve kept: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for id, hash := range hashes {
		if _, err := tx.ExecContext(ctx,
			`UPDATE memories SET resolve_kept_hash = ? WHERE id = ? AND project_id = ?`,
			hash, id, projectID,
		); err != nil {
			return fmt.Errorf("mark resolve kept %s: %w", RefusalArg("memory_id", id), err)
		}
	}
	return tx.Commit()
}

// SupersedeCheck is a cached NEITHER verdict for an ordered pair: the content
type SupersedeCheck struct {
	NewerHash string
	OlderHash string
}

// SupersedeChecked returns the cached NEITHER verdicts for a project, keyed by
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
func (s *Store) MarkSupersedeNeither(ctx context.Context, projectID string, checks map[[2]string]SupersedeCheck) error {
	if len(checks) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "mark-supersede-neither")
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
			return fmt.Errorf("mark supersede neither %s->%s: %w", RefusalArg("memory_id", key[0]), RefusalArg("memory_id", key[1]), err)
		}
	}
	return tx.Commit()
}

// DeleteOptions is what one delete is asked to do beyond removing the row.
type DeleteOptions struct {
	PurgeHistory bool
	ProjectID    string
}

// Delete removes a specific memory and KEEPS its history. The row's last state is
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.DeleteWithOptions(ctx, id, DeleteOptions{})
}

// DeleteWithOptions is Delete with the caller's choice about the history: kept (a
func (s *Store) DeleteWithOptions(ctx context.Context, id string, opts DeleteOptions) error {
	_, err := s.deleteWithOptionsLocked(ctx, id, opts)
	return err
}

// DeletePurge deletes a memory and erases every recorded copy of its text,
func (s *Store) DeletePurge(ctx context.Context, id string) (PurgeOutcome, error) {
	return s.deleteWithOptionsLocked(ctx, id, DeleteOptions{PurgeHistory: true})
}

// DeletePurgeInProject is DeletePurge scoped to the memory's owning project, the
func (s *Store) DeletePurgeInProject(ctx context.Context, projectID, id string) (PurgeOutcome, error) {
	if projectID == "" {
		return PurgeOutcome{}, fmt.Errorf("delete purge: project_id is required")
	}
	return s.deleteWithOptionsLocked(ctx, id, DeleteOptions{PurgeHistory: true, ProjectID: projectID})
}

func (s *Store) deleteWithOptionsLocked(ctx context.Context, id string, opts DeleteOptions) (PurgeOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out PurgeOutcome
	tx, _, err := s.beginWrite(ctx, "delete")
	if err != nil {
		return out, fmt.Errorf("begin delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if opts.ProjectID != "" {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT project_id FROM memories WHERE id = ?`, id).Scan(&owner)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, fmt.Errorf("lookup memory: %w", err)
		}
		if err == nil && owner != opts.ProjectID {
			return out, fmt.Errorf("memory %s not found in project %s",
				RefusalArg("memory_id", id), RefusalArg("project_id", opts.ProjectID))
		}
	}

	if !opts.PurgeHistory {
		if err := appendHistoryTx(ctx, tx, id, phaseDelete, Provenance{}); err != nil &&
			!errors.Is(err, errHistoryNoMemory) {
			return out, err
		}
	}

	if opts.PurgeHistory {
		n, err := purgeHistoryTx(ctx, tx, id)
		if err != nil {
			return out, err
		}
		out.Rows = n
	}

	result, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, id)
	if err != nil {
		return out, fmt.Errorf("delete memory: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return out, fmt.Errorf("memory not found: %s", RefusalArg("memory_id", id))
	}
	if opts.PurgeHistory {
		if err := rebuildFTSAfterPurgeTx(ctx, tx); err != nil {
			return out, err
		}
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	if opts.PurgeHistory {
		out.CheckpointComplete = s.checkpointAfterPurgeUncancelled(ctx)
	}
	return out, nil
}

// UpdateOptions is the optional set of facts an edit may change. Every field is
type UpdateOptions struct {
	Content    *string
	Category   *string
	Importance *float32
	Tags       []string
	Validity   Validity
	Provenance Provenance
}

// UpdateMemory applies a partial update to a memory. Nil content/category/
func (s *Store) UpdateMemory(ctx context.Context, projectID, id string, content, category *string, importance *float32, tags []string) error {
	return s.UpdateMemoryWithOptions(ctx, projectID, id, UpdateOptions{
		Content: content, Category: category, Importance: importance, Tags: tags,
	})
}

// UpdateMemoryWithOptions is UpdateMemory over the full partial-edit set, adding
func (s *Store) UpdateMemoryWithOptions(ctx context.Context, projectID, id string, opts UpdateOptions) error {
	if opts.Content != nil {
		if err := rejectSecret("content", *opts.Content); err != nil {
			return err
		}
	}
	if err := rejectSecretList("tags", opts.Tags); err != nil {
		return err
	}
	if err := rejectSecretFields(secretSourceRefField(opts.Provenance.SourceRef)...); err != nil {
		return err
	}
	if err := checkReservedSourceRef(opts.Provenance.SourceRef); err != nil {
		return err
	}
	if _, err := boundedSourceRef(opts.Provenance.SourceRef); err != nil {
		return err
	}
	if _, err := boundedAgent(opts.Provenance.Agent); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, lock, err := s.beginWrite(ctx, "update")
	if err != nil {
		return fmt.Errorf("begin update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var curContent, curCategory, curTags string
	var curImportance float32
	var curFrom, curUntil sql.NullString
	var curSourceRef string
	err = tx.QueryRowContext(ctx,
		`SELECT content, category, importance, tags, valid_from, valid_until,
		        COALESCE(source_ref, '')
		 FROM memories WHERE id = ? AND project_id = ?`, id, projectID,
	).Scan(&curContent, &curCategory, &curImportance, &curTags, &curFrom, &curUntil, &curSourceRef)
	if err == sql.ErrNoRows {
		return fmt.Errorf("memory %s not found in project %s",
			RefusalArg("memory_id", id), RefusalArg("project_id", projectID))
	}
	if err != nil {
		return fmt.Errorf("lookup memory: %w", err)
	}

	if opts.Validity.ValidFrom != nil || opts.Validity.ValidUntil != nil {
		if err := CheckWindowOrder(opts.Validity, Validity{
			ValidFrom:  nullStringPtr(curFrom),
			ValidUntil: nullStringPtr(curUntil),
		}); err != nil {
			return err
		}
	}

	newContent, newCategory, newImportance, newTags := curContent, curCategory, curImportance, curTags
	if opts.Content != nil {
		newContent = *opts.Content
	}
	if opts.Category != nil {
		newCategory = *opts.Category
	}
	if opts.Importance != nil {
		newImportance = *opts.Importance
	}
	if opts.Tags != nil {
		b, mErr := json.Marshal(opts.Tags)
		if mErr != nil {
			return fmt.Errorf("marshal tags: %w", mErr)
		}
		newTags = string(b)
	}

	if err := recordBaselineHistoryTx(ctx, tx, id, phaseBaseline, Provenance{}); err != nil {
		return err
	}

	clearResolved := 0
	if newContent != curContent {
		clearResolved = 1
	}
	newSourceRef := opts.Provenance.SourceRef
	if newSourceRef != "" {
		keptRef, refErr := preserveDecisionLinks(newSourceRef, curSourceRef)
		if refErr != nil {
			return refErr
		}
		newSourceRef = keptRef
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE memories
		SET content = ?, category = ?, importance = ?, tags = ?, updated_at = datetime('now'),
		    resolved_at = CASE WHEN ? THEN NULL ELSE resolved_at END,
		    valid_from   = COALESCE(?, valid_from),
		    valid_until  = COALESCE(?, valid_until),
		    verified_at  = COALESCE(?, verified_at),
		    confidence   = COALESCE(?, confidence),
		    agent        = COALESCE(NULLIF(?, ''), agent),
		    session_id   = COALESCE(NULLIF(?, ''), session_id),
		    source_ref   = COALESCE(NULLIF(?, ''), source_ref)
		WHERE id = ?
	`, newContent, newCategory, newImportance, newTags, clearResolved,
		nullIfEmptyPtr(opts.Validity.ValidFrom), nullIfEmptyPtr(opts.Validity.ValidUntil),
		nullIfEmptyPtr(opts.Validity.VerifiedAt), opts.Provenance.Confidence,
		opts.Provenance.Agent, opts.Provenance.SessionID, newSourceRef,
		id); err != nil {
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
	if err := appendHistoryTx(ctx, tx, id, phaseUpdate, opts.Provenance); err != nil {
		return err
	}
	if err := appendVerificationIfStatedTx(ctx, tx, id, opts.Provenance, opts.Validity.VerifiedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	lock.reportHold("update", time.Now())
	if s.onSave != nil && newContent != curContent {
		s.onSave(projectID)
	}
	return nil
}

// PromoteToGlobal moves a memory into the shared _global project. The memory
func (s *Store) PromoteToGlobal(ctx context.Context, projectID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "promote")
	if err != nil {
		return fmt.Errorf("begin promote: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')
		ON CONFLICT(id) DO NOTHING
	`); err != nil {
		return fmt.Errorf("ensure _global project: %w", err)
	}
	_, _ = tx.ExecContext(ctx, `INSERT OR IGNORE INTO ghost_state (project_id) VALUES ('_global')`)

	res, err := tx.ExecContext(ctx, `
		UPDATE memories SET project_id = '_global'
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
		return fmt.Errorf("memory %s not found in project %s",
			RefusalArg("memory_id", id), RefusalArg("project_id", projectID))
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE memory_history SET project_id = ? WHERE memory_id = ?`, GlobalProjectID, id,
	); err != nil {
		return fmt.Errorf("promote memory history: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE memory_flags SET project_id = ? WHERE memory_id = ?`, GlobalProjectID, id,
	); err != nil {
		return fmt.Errorf("promote memory flags: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE supersede_checked SET project_id = ? WHERE newer_id = ? OR older_id = ?`,
		GlobalProjectID, id, id,
	); err != nil {
		return fmt.Errorf("promote supersede checks: %w", err)
	}
	return tx.Commit()
}

// TogglePin sets or clears the pinned flag.
func (s *Store) TogglePin(ctx context.Context, id string, pinned bool) error {
	return s.togglePin(ctx, "", id, pinned)
}

// TogglePinInProject is TogglePin scoped the way PromoteToGlobal is: the UPDATE
func (s *Store) TogglePinInProject(ctx context.Context, projectID, id string, pinned bool) error {
	if projectID == "" {
		return fmt.Errorf("toggle pin: project_id is required")
	}
	return s.togglePin(ctx, projectID, id, pinned)
}

func (s *Store) togglePin(ctx context.Context, projectID, id string, pinned bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	pinnedInt := 0
	if pinned {
		pinnedInt = 1
	}
	res, err := s.execGuardedWrite(ctx, "toggle-pin", `
		UPDATE memories SET pinned = ? WHERE id = ? AND (? = '' OR project_id = ?)
	`, pinnedInt, id, projectID, projectID)
	if err != nil {
		return fmt.Errorf("toggle pin: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("toggle pin rows: %w", err)
	}
	if n == 0 {
		if projectID == "" {
			return fmt.Errorf("memory %s not found", RefusalArg("memory_id", id))
		}
		return fmt.Errorf("memory %s not found in project %s",
			RefusalArg("memory_id", id), RefusalArg("project_id", projectID))
	}
	return nil
}

// CurrentTimestamp returns the database's own notion of "now", formatted
func (s *Store) CurrentTimestamp(ctx context.Context) (string, error) {
	var ts string
	if err := s.db.QueryRowContext(ctx, `SELECT datetime('now')`).Scan(&ts); err != nil {
		return "", fmt.Errorf("current timestamp: %w", err)
	}
	return ts, nil
}

type replaceCandidate struct {
	id         string
	content    string
	category   string
	importance float64
	tags       string
	scope      sql.NullString
	updatedAt  string
}

// CorpusRow is one row as a reflection pass READ it: the identity and the state
type CorpusRow struct {
	ID         string
	Content    string
	UpdatedAt  string
	Category   string
	Importance float32
	Tags       []string
}

func (f CorpusRow) sameAs(c replaceCandidate) bool {
	return f.Content == c.content &&
		f.UpdatedAt == c.updatedAt &&
		f.Category == c.category &&
		float32(c.importance) == f.Importance &&
		sameTags(c.tags, f.Tags)
}

type fedGuard struct {
	inactive        bool
	candidates      []replaceCandidate
	candidateByID   map[string]replaceCandidate
	fedIDsByContent map[string][]string
	concurrent      map[string]bool
	sameSecond      map[string]bool
}

func newFedGuard(candidates []replaceCandidate, fed []CorpusRow, concurrent map[string]bool, sameSecond map[string]bool) *fedGuard {
	g := &fedGuard{
		candidates:      candidates,
		candidateByID:   make(map[string]replaceCandidate, len(candidates)),
		fedIDsByContent: make(map[string][]string, len(fed)),
		concurrent:      concurrent,
		sameSecond:      make(map[string]bool),
	}
	if len(fed) == 0 {
		g.inactive = true
		return g
	}
	fedByID := make(map[string]CorpusRow, len(fed))
	for _, f := range fed {
		fedByID[f.ID] = f
		g.fedIDsByContent[f.Content] = append(g.fedIDsByContent[f.Content], f.ID)
	}
	for _, c := range candidates {
		g.candidateByID[c.id] = c
	}
	for _, c := range candidates {
		f, read := fedByID[c.id]
		if !read || !f.sameAs(c) {
			concurrent[c.id] = true
			continue
		}
		if sameSecond[c.id] {
			g.sameSecond[c.id] = true
		}
	}
	return g
}

func (g *fedGuard) dropReasonFor(m Memory) dropReason {
	if g.inactive {
		return dropNone
	}
	fails := g.claimFailures(m)
	if len(fails) == 0 {
		return dropNone
	}
	for _, replaced := range m.ReplacesIDs {
		if _, stored := g.candidateByID[replaced]; stored {
			g.concurrent[replaced] = true
		}
	}
	for _, c := range g.candidates {
		if c.content == m.Content {
			g.concurrent[c.id] = true
		}
	}
	for _, id := range fails {
		if !g.sameSecond[id] {
			return dropChanged
		}
	}
	return dropSameSecond
}

func (g *fedGuard) claimFailures(m Memory) []string {
	var fails []string
	for _, fid := range g.fedIDsByContent[m.Content] {
		if _, stored := g.candidateByID[fid]; !stored || g.concurrent[fid] {
			fails = append(fails, fid)
		}
	}
	for _, replaced := range m.ReplacesIDs {
		if _, stored := g.candidateByID[replaced]; !stored || g.concurrent[replaced] {
			fails = append(fails, replaced)
		}
	}
	return fails
}

func emissionClaimIDs(candidates []replaceCandidate, m Memory) []string {
	ids := append([]string(nil), m.ReplacesIDs...)
	for _, c := range candidates {
		if c.content == m.Content {
			ids = append(ids, c.id)
		}
	}
	return ids
}

// EmissionClaimsRow reports whether an emission whose sources are named and
func EmissionClaimsRow(named map[string]bool, content, rowID, rowContent string) bool {
	return named[rowID] || rowContent == content
}

func keepEmissionClaims(candidates []replaceCandidate, concurrent map[string]bool, m Memory) []string {
	named := make(map[string]bool, len(m.ReplacesIDs))
	for _, id := range m.ReplacesIDs {
		named[id] = true
	}
	var kept []string
	for _, c := range candidates {
		if EmissionClaimsRow(named, m.Content, c.id, c.content) {
			concurrent[c.id] = true
			kept = append(kept, c.id)
		}
	}
	return kept
}

func reusePreservesAge(stored replaceCandidate, emitted Memory) bool {
	return stored.content == emitted.Content && stored.category == emitted.Category
}

func reuseChangesNothing(stored replaceCandidate, emitted Memory) bool {
	return stored.content == emitted.Content &&
		stored.category == emitted.Category &&
		float32(stored.importance) == emitted.Importance &&
		sameTags(stored.tags, emitted.Tags) &&
		scopeUnchanged(stored.scope, emitted.Scope)
}

func sameTags(stored string, emitted []string) bool {
	var parsed []string
	if err := json.Unmarshal([]byte(stored), &parsed); err != nil {
		return false
	}
	if len(parsed) != len(emitted) {
		return false
	}
	for i := range parsed {
		if parsed[i] != emitted[i] {
			return false
		}
	}
	return true
}

func scopeUnchanged(stored sql.NullString, emitted map[string]string) bool {
	if len(emitted) == 0 {
		return true
	}
	return maps.Equal(parseScope(stored), emitted)
}

func takeReusableRow(matches []replaceCandidate, emitted Memory) (replaceCandidate, []replaceCandidate, bool) {
	if len(matches) == 0 {
		return replaceCandidate{}, nil, false
	}
	for i, c := range matches {
		if c.category == emitted.Category {
			return c, append(matches[:i:i], matches[i+1:]...), true
		}
	}
	return matches[0], matches[1:], true
}

func (s *Store) pruneSnapshotsTx(ctx context.Context, tx *sql.Tx, projectID string) {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM memory_snapshots
		WHERE project_id = ? AND snapshot_id NOT IN (
			SELECT DISTINCT snapshot_id FROM memory_snapshots
			WHERE project_id = ?
			ORDER BY snapshot_id DESC
			LIMIT 10
		)
	`, projectID, projectID)
	if err != nil && s.logger != nil {
		s.logger.Warn("prune old snapshots", "error", err, "project_id", RefusalArg("project_id", projectID))
	}
	if _, err = tx.ExecContext(ctx, `
		DELETE FROM memory_snapshot_evidence
		WHERE snapshot_id NOT IN (SELECT DISTINCT snapshot_id FROM memory_snapshots)
	`); err != nil && s.logger != nil {
		s.logger.Warn("prune old snapshot evidence", "error", err, "project_id", RefusalArg("project_id", projectID))
	}
}

func (s *Store) snapshotReplaceableTx(ctx context.Context, tx *sql.Tx, projectID string) (snapshotID string, err error) {
	snapshotID = fmt.Sprintf("%s-%d", projectID, time.Now().UnixNano())
	_, err = tx.ExecContext(ctx, `
		INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source, tags,
		                              created_at, updated_at, memory_id, access_count, last_accessed,
		                              agent, session_id, source_ref, confidence,
		                              valid_from, valid_until, verified_at, scope, scope_captured,
		                              retention, retention_captured)
		SELECT ?, project_id, category, content, importance, source, tags,
		       created_at, updated_at, id, access_count, last_accessed,
		       agent, session_id, source_ref, confidence,
		       valid_from, valid_until, verified_at, scope, 1,
		       -- The tier is copied with the same captured marker scope has
		       -- (#1341): the snapshot is the only undo history a replace
		       -- has, and a tier the restore cannot put back is the lifetime
		       -- it cannot put back — prunePredicate reaches a session row
		       -- only through retention AND a derived expires_at, so losing
		       -- them turned the undo into a permanent, unprunable row.
		       retention, 1
		FROM memories WHERE project_id = ? AND source NOT IN ('manual', 'builtin') AND pinned = 0 AND resolved_at IS NULL
		      AND `+retentionExemptSQL+`
	`, snapshotID, projectID)
	if err != nil {
		return "", fmt.Errorf("snapshot memories: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO memory_snapshot_evidence
			(snapshot_id, memory_id, kind, agent, session_id, source_ref, confidence, observed_at, verified_at)
		SELECT ?, s.memory_id, p.kind, p.agent, p.session_id, p.source_ref, p.confidence,
		       p.observed_at, p.verified_at
		FROM memory_snapshots s
		JOIN memory_provenance p ON p.memory_id = s.memory_id
		WHERE s.snapshot_id = ? AND s.memory_id IS NOT NULL
	`, snapshotID, snapshotID); err != nil {
		return "", fmt.Errorf("snapshot memory evidence: %w", err)
	}
	return snapshotID, nil
}

type rowClaims struct {
	validFrom, validUntil, verifiedAt *string
	confidence                        *float64
	agent, sessionID, sourceRef       string
}

func inheritedClaims(ctx context.Context, tx *sql.Tx, projectID string, m Memory, keptLive map[string]bool) (rowClaims, error) {
	claims := rowClaims{
		validFrom:  m.ValidFrom,
		validUntil: m.ValidUntil,
		verifiedAt: m.VerifiedAt,
		confidence: m.Confidence,
		agent:      m.Agent,
		sessionID:  m.SessionID,
		sourceRef:  m.SourceRef,
	}
	var completeWindow, firstHalfWindow Validity
	var haveComplete, haveHalf bool
	var decisionRefs []string
	for _, id := range m.ReplacesIDs {
		if id == "" {
			continue
		}
		var validFrom, validUntil, verifiedAt sql.NullString
		var confidence sql.NullFloat64
		var agent, sessionID, sourceRef sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT valid_from, valid_until, verified_at, confidence, agent, session_id, source_ref
			 FROM memories WHERE id = ? AND project_id = ?`, id, projectID,
		).Scan(&validFrom, &validUntil, &verifiedAt, &confidence, &agent, &sessionID, &sourceRef)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return rowClaims{}, fmt.Errorf("read replaced memory %s claims: %w", RefusalArg("memory_id", id), err)
		}
		if keptLive[id] {
			decisionRefs = append(decisionRefs, decisionRefsInSourceRef(sourceRef.String)...)
			continue
		}
		candidate := Validity{ValidFrom: readableStampPtr(validFrom), ValidUntil: readableStampPtr(validUntil)}
		switch {
		case candidate.ValidFrom != nil && candidate.ValidUntil != nil:
			if err := CheckWindowOrder(candidate, Validity{}); err == nil && !haveComplete {
				completeWindow, haveComplete = candidate, true
			}
		case !haveHalf && (candidate.ValidFrom != nil || candidate.ValidUntil != nil):
			firstHalfWindow, haveHalf = candidate, true
		}
		if claims.verifiedAt == nil {
			claims.verifiedAt = readableStampPtr(verifiedAt)
		}
		if claims.confidence == nil && confidence.Valid {
			f := confidence.Float64
			claims.confidence = &f
		}
		if claims.agent == "" {
			claims.agent = agent.String
		}
		if claims.sessionID == "" {
			claims.sessionID = sessionID.String
		}
		if claims.sourceRef == "" {
			claims.sourceRef = sourceRef.String
		}
		decisionRefs = append(decisionRefs, decisionRefsInSourceRef(sourceRef.String)...)
	}
	ref, err := boundedDecisionRefs(claims.sourceRef, decisionRefs)
	if err != nil {
		return rowClaims{}, err
	}
	claims.sourceRef = ref
	if claims.validFrom == nil && claims.validUntil == nil {
		switch {
		case haveComplete:
			claims.validFrom, claims.validUntil = completeWindow.ValidFrom, completeWindow.ValidUntil
		case haveHalf:
			claims.validFrom, claims.validUntil = firstHalfWindow.ValidFrom, firstHalfWindow.ValidUntil
		}
	}
	return claims, nil
}

func inheritedRetention(ctx context.Context, tx *sql.Tx, projectID string, m Memory) (string, error) {
	merged, ok, err := sourceRetentionTx(ctx, tx, projectID, m.ReplacesIDs)
	if err != nil {
		return "", err
	}
	if ok {
		return merged, nil
	}
	return NormalizeRetention(m.Retention)
}

func foldedRetention(ctx context.Context, tx *sql.Tx, projectID string, m, folded Memory) (string, error) {
	if tier, ok, err := sourceRetentionTx(ctx, tx, projectID, folded.ReplacesIDs); err != nil || ok {
		return tier, err
	}
	return inheritedRetention(ctx, tx, projectID, m)
}

var errScopeConflict = errors.New("scope conflict")

func inheritedScope(ctx context.Context, tx *sql.Tx, projectID string, m Memory) (map[string]string, error) {
	if m.Scope != nil {
		return m.Scope, nil
	}

	var fold scopeFold
	for _, id := range m.ReplacesIDs {
		if id == "" {
			continue
		}
		var scopeJSONStr sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT scope FROM memories WHERE id = ? AND project_id = ?`, id, projectID,
		).Scan(&scopeJSONStr)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read replaced memory %s scope: %w", RefusalArg("memory_id", id), err)
		}

		var sourceScope map[string]string
		if scopeJSONStr.Valid && scopeJSONStr.String != "" {
			if err := json.Unmarshal([]byte(scopeJSONStr.String), &sourceScope); err != nil {
				return nil, fmt.Errorf("unmarshal scope for memory %s: %w", RefusalArg("memory_id", id), err)
			}
		}

		if err := fold.add(id, sourceScope); err != nil {
			return nil, err
		}
	}
	return fold.result()
}

type scopeFold struct {
	agreed       map[string]string
	haveAgreed   bool
	haveUnscoped bool
}

func (f *scopeFold) add(id string, sourceScope map[string]string) error {
	if sourceScope == nil {
		f.haveUnscoped = true
		return nil
	}
	if !f.haveAgreed {
		f.agreed = sourceScope
		f.haveAgreed = true
		return nil
	}
	if !ScopeEquals(f.agreed, sourceScope) {
		return fmt.Errorf("%w: source %s disagrees with sources already read", errScopeConflict, RefusalArg("memory_id", id))
	}
	return nil
}

func (f *scopeFold) result() (map[string]string, error) {
	if f.haveAgreed && f.haveUnscoped {
		return nil, fmt.Errorf("%w: sources mix scoped and unscoped rows", errScopeConflict)
	}
	return f.agreed, nil
}

// SourceScope is one source row of a merge or rewrite, by id and stored scope.
type SourceScope struct {
	ID    string
	Scope map[string]string
}

// FoldSourceScopes is inheritedScope's agreement rule over rows the caller
func FoldSourceScopes(sources []SourceScope) (map[string]string, error) {
	var fold scopeFold
	for _, src := range sources {
		if err := fold.add(src.ID, src.Scope); err != nil {
			return nil, err
		}
	}
	return fold.result()
}

// IsScopeConflict reports whether err is the scope refusal FoldSourceScopes
func IsScopeConflict(err error) bool { return errors.Is(err, errScopeConflict) }

func sourceRetentionTx(ctx context.Context, tx *sql.Tx, projectID string, ids []string) (tier string, found bool, err error) {
	for _, id := range ids {
		if id == "" {
			continue
		}
		var current string
		err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(retention, 'project') FROM memories WHERE id = ? AND project_id = ?`, id, projectID,
		).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", false, fmt.Errorf("read replaced memory %s retention: %w", RefusalArg("memory_id", id), err)
		}
		if !found {
			tier, found = current, true
			continue
		}
		tier = mergeRetention(tier, current)
	}
	return tier, found, nil
}

func raiseReusedRetentionTx(ctx context.Context, tx *sql.Tx, projectID, id string, m Memory) error {
	var current string
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(retention, 'project') FROM memories WHERE id = ?`, id,
	).Scan(&current); err != nil {
		return fmt.Errorf("read the reused memory's retention: %w", err)
	}
	others := make([]string, 0, len(m.ReplacesIDs))
	for _, replaced := range m.ReplacesIDs {
		if replaced != id {
			others = append(others, replaced)
		}
	}
	inherited, found, err := sourceRetentionTx(ctx, tx, projectID, others)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	merged := mergeRetention(current, inherited)
	if merged == current {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE memories SET retention = ?, expires_at = NULL WHERE id = ?`, merged, id); err != nil {
		return fmt.Errorf("raise the reused memory's retention: %w", err)
	}
	return nil
}

func fillReusedClaimsTx(ctx context.Context, tx *sql.Tx, projectID, id string, m Memory, keptLive map[string]bool) error {
	others := make([]string, 0, len(m.ReplacesIDs))
	for _, replaced := range m.ReplacesIDs {
		if replaced != "" && replaced != id {
			others = append(others, replaced)
		}
	}
	if len(others) == 0 {
		return nil
	}
	claims, err := inheritedClaims(ctx, tx, projectID, Memory{ReplacesIDs: others}, keptLive)
	if err != nil {
		return err
	}
	if claims == (rowClaims{}) {
		return nil
	}
	sourceRef := claims.sourceRef
	var own sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT source_ref FROM memories WHERE id = ?`, id).Scan(&own); err != nil {
		return fmt.Errorf("read the reused memory's source_ref: %w", err)
	}
	if own.String != "" {
		if sourceRef, err = boundedDecisionRefs(own.String, decisionRefsInSourceRef(claims.sourceRef)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE memories SET
			valid_from  = CASE WHEN NULLIF(valid_from, '') IS NULL AND NULLIF(valid_until, '') IS NULL
			                   THEN COALESCE(?, valid_from) ELSE valid_from END,
			valid_until = CASE WHEN NULLIF(valid_from, '') IS NULL AND NULLIF(valid_until, '') IS NULL
			                   THEN COALESCE(?, valid_until) ELSE valid_until END,
			verified_at = COALESCE(NULLIF(verified_at, ''), ?, verified_at),
			confidence  = COALESCE(confidence, ?),
			agent       = COALESCE(NULLIF(agent, ''), ?, agent),
			session_id  = COALESCE(NULLIF(session_id, ''), ?, session_id),
			source_ref  = COALESCE(?, source_ref)
		WHERE id = ?`,
		nullIfEmptyPtr(claims.validFrom), nullIfEmptyPtr(claims.validUntil), nullIfEmptyPtr(claims.verifiedAt),
		claims.confidence, nullIfEmpty(claims.agent), nullIfEmpty(claims.sessionID),
		nullIfEmpty(sourceRef), id); err != nil {
		return fmt.Errorf("fill the reused memory's claims: %w", err)
	}
	return nil
}

func markReusedSuccessorSourceTx(ctx context.Context, tx *sql.Tx, id string, m Memory, consumed map[string]bool) (bool, error) {
	if !reuseAbsorbsConsumed(id, m, consumed) {
		return false, nil
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE memories SET source = 'reflection', updated_at = datetime('now') WHERE id = ? AND source <> 'reflection'`, id)
	if err != nil {
		return false, fmt.Errorf("stamp the reused memory's provenance: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count the reused memory's provenance stamp: %w", err)
	}
	return n > 0, nil
}

func absorbSourcesTx(ctx context.Context, tx *sql.Tx, projectID, id string, m, folded Memory, keptLive, consumed map[string]bool) (bool, error) {
	stamped, err := markReusedSuccessorSourceTx(ctx, tx, id, m, consumed)
	if err != nil {
		return false, err
	}
	if err := carryEvidenceTx(ctx, tx, id, folded.ReplacesIDs); err != nil {
		return false, err
	}
	if err := fillReusedClaimsTx(ctx, tx, projectID, id, m, keptLive); err != nil {
		return false, err
	}
	return stamped, nil
}

const replaceableRowSQL = `source NOT IN ('manual', 'builtin') AND pinned = 0 AND resolved_at IS NULL
		  AND ` + retentionExemptSQL

func replaceableRowTx(ctx context.Context, tx *sql.Tx, id string) (bool, error) {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM memories WHERE id = ? AND `+replaceableRowSQL, id).Scan(&n); err != nil {
		return false, fmt.Errorf("read whether %s is replaceable: %w", RefusalArg("memory_id", id), err)
	}
	return n == 1, nil
}

func sourcesNotKeptLive(ids []string, keptLive map[string]bool) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if keptLive[id] {
			continue
		}
		out = append(out, id)
	}
	return out
}

func reuseAbsorbsConsumed(id string, m Memory, consumed map[string]bool) bool {
	for _, replaced := range m.ReplacesIDs {
		if replaced != "" && replaced != id && consumed[replaced] {
			return true
		}
	}
	return false
}

func (s *Store) replaceableCandidatesTx(ctx context.Context, tx *sql.Tx, projectID string) ([]replaceCandidate, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, content, category, importance, tags, scope, updated_at FROM memories
		WHERE project_id = ? AND `+replaceableRowSQL+`
		ORDER BY created_at, id
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list replaceable memories: %w", err)
	}
	var candidates []replaceCandidate
	for rows.Next() {
		var c replaceCandidate
		if err := rows.Scan(&c.id, &c.content, &c.category, &c.importance, &c.tags, &c.scope, &c.updatedAt); err != nil {
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
	return candidates, nil
}

func (s *Store) concurrentSinceTx(ctx context.Context, tx *sql.Tx, projectID, consolidatedSince string, concurrent, sameSecond map[string]bool) error {
	if consolidatedSince == "" {
		return nil
	}
	crows, err := tx.QueryContext(ctx, `
		SELECT id, created_at FROM memories
		WHERE project_id = ? AND source NOT IN ('manual', 'builtin') AND pinned = 0 AND resolved_at IS NULL AND created_at >= ?
		  AND `+retentionExemptSQL+`
	`, projectID, consolidatedSince)
	if err != nil {
		return fmt.Errorf("find concurrent memories: %w", err)
	}
	for crows.Next() {
		var id, createdAt string
		if err := crows.Scan(&id, &createdAt); err != nil {
			crows.Close() //nolint:errcheck
			return fmt.Errorf("scan concurrent memory: %w", err)
		}
		concurrent[id] = true
		if sameSecond != nil && createdAt == consolidatedSince {
			sameSecond[id] = true
		}
	}
	crowsErr := crows.Err()
	crows.Close() //nolint:errcheck
	if crowsErr != nil {
		return fmt.Errorf("iterate concurrent memories: %w", crowsErr)
	}
	return nil
}

func (s *Store) fedGuardFor(ctx context.Context, tx *sql.Tx, projectID, consolidatedSince string, fed []CorpusRow, concurrent map[string]bool) (*fedGuard, error) {
	candidates, err := s.replaceableCandidatesTx(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}
	sameSecond := make(map[string]bool)
	if err := s.concurrentSinceTx(ctx, tx, projectID, consolidatedSince, concurrent, sameSecond); err != nil {
		return nil, err
	}
	return newFedGuard(candidates, fed, concurrent, sameSecond), nil
}

// consolidatedSince should be a timestamp (see CurrentTimestamp) captured
func (s *Store) ReplaceNonManual(ctx context.Context, projectID string, memories []Memory, consolidatedSince string, fed []CorpusRow) (preserved []string, dropped EmissionDrops, deleted int, appliedByCategory map[string]int, err error) {
	if len(memories) == 0 {
		return nil, EmissionDrops{}, 0, nil, fmt.Errorf("refusing to replace memories with empty set — reflection likely malformed")
	}

	parentTx, inTx := storeTxFromContext(ctx)
	if !inTx {
		s.mu.Lock()
		defer s.mu.Unlock()
	}

	tx := parentTx
	ownTx := !inTx
	var lock writeLock
	if ownTx {
		if tx, lock, err = s.beginWrite(ctx, "replace"); err != nil {
			return nil, EmissionDrops{}, 0, nil, fmt.Errorf("begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
	}

	snapshotID, err := s.snapshotReplaceableTx(ctx, tx, projectID)
	if err != nil {
		return nil, EmissionDrops{}, 0, nil, err
	}

	candidates, err := s.replaceableCandidatesTx(ctx, tx, projectID)
	if err != nil {
		return nil, EmissionDrops{}, 0, nil, err
	}
	view, shared := applyViewFromContext(ctx)
	concurrent := make(map[string]bool)
	if shared {
		for id := range view.concurrent {
			concurrent[id] = true
		}
	}
	sameSecond := make(map[string]bool)
	if err := s.concurrentSinceTx(ctx, tx, projectID, consolidatedSince, concurrent, sameSecond); err != nil {
		return nil, EmissionDrops{}, 0, nil, err
	}

	guard := newFedGuard(candidates, fed, concurrent, sameSecond)
	candidatesByID := make(map[string]replaceCandidate, len(candidates))
	for _, c := range candidates {
		candidatesByID[c.id] = c
	}
	fedIDs := make(map[string]bool, len(fed))
	for _, row := range fed {
		fedIDs[row.ID] = true
	}
	dropEmission := make([]bool, len(memories))
	dropReasons := make([]dropReason, len(memories))
	overflowed := make([]bool, len(memories))
	overflowLinks := make([]int, len(memories))
	scopeRefused := make([]bool, len(memories))
	moved := make(map[string]bool, len(concurrent))
	for id := range concurrent {
		if shared && (view.scopeKept[id] || view.overflowKept[id]) {
			continue
		}
		moved[id] = true
	}
	scopeKept := make(map[string]bool)
	if shared {
		for id := range view.scopeKept {
			scopeKept[id] = true
		}
	}
	keepRefusedSources := func(m Memory) {
		for _, replaced := range m.ReplacesIDs {
			if _, stored := candidatesByID[replaced]; stored {
				concurrent[replaced] = true
				scopeKept[replaced] = true
			}
		}
		for _, c := range candidates {
			if c.content == m.Content {
				concurrent[c.id] = true
				scopeKept[c.id] = true
			}
		}
	}
	overflowKept := make(map[string]bool)
	if shared {
		for id := range view.overflowKept {
			overflowKept[id] = true
		}
	}
	refusalKept := func(id string) bool {
		return (overflowKept[id] || scopeKept[id]) && !moved[id]
	}
	verbatimOfKept := func(m Memory) bool {
		if len(m.ReplacesIDs) != 0 || m.Scope != nil {
			return false
		}
		found := false
		for _, fid := range guard.fedIDsByContent[m.Content] {
			if _, stored := guard.candidateByID[fid]; !stored {
				return false
			}
			if concurrent[fid] {
				if !refusalKept(fid) {
					return false
				}
				found = true
			}
		}
		return found
	}
	for i, m := range memories {
		if !verbatimOfKept(m) {
			dropReasons[i] = guard.dropReasonFor(m)
			dropEmission[i] = dropReasons[i] != dropNone
		}
		if !dropEmission[i] {
			links, over, err := decisionLinksOverflowTx(ctx, tx, projectID, emissionClaimIDs(candidates, m))
			if err != nil {
				return nil, EmissionDrops{}, 0, nil, err
			}
			if over {
				dropEmission[i] = true
				overflowed[i] = true
				overflowLinks[i] = links
				for _, id := range keepEmissionClaims(candidates, concurrent, m) {
					overflowKept[id] = true
				}
			}
		}
	}

	emittedScope := make([]map[string]string, len(memories))
	for i, m := range memories {
		if dropEmission[i] {
			continue
		}
		sc, err := inheritedScope(ctx, tx, projectID, m)
		if err != nil {
			if !errors.Is(err, errScopeConflict) {
				return nil, EmissionDrops{}, 0, nil, err
			}
			dropEmission[i] = true
			scopeRefused[i] = true
			keepRefusedSources(m)
			continue
		}
		emittedScope[i] = sc
	}

	reusable := make(map[string][]replaceCandidate)
	for _, c := range candidates {
		if concurrent[c.id] {
			continue
		}
		reusable[c.content] = append(reusable[c.content], c)
	}
	reuseFor := make(map[int]replaceCandidate, len(memories))
	for i, m := range memories {
		if dropEmission[i] {
			continue
		}
		if chosen, rest, ok := takeReusableRow(reusable[m.Content], m); ok {
			reuseFor[i] = chosen
			reusable[m.Content] = rest
		}
	}
	for i, m := range memories {
		if dropEmission[i] || m.Scope != nil || len(m.ReplacesIDs) == 0 {
			continue
		}
		stored, ok := reuseFor[i]
		if !ok {
			continue
		}
		if ScopeEquals(emittedScope[i], parseScope(stored.scope)) {
			continue
		}
		dropEmission[i] = true
		scopeRefused[i] = true
		delete(reuseFor, i)
		concurrent[stored.id] = true
		scopeKept[stored.id] = true
		keepRefusedSources(m)
	}

	keptAsIs := make(map[int]bool)
	keptClaimed := make(map[string]bool)
	for i, m := range memories {
		if dropEmission[i] || len(m.ReplacesIDs) != 0 || m.Scope != nil {
			continue
		}
		if _, ok := reuseFor[i]; ok {
			continue
		}
		for _, c := range candidates {
			if c.content == m.Content && refusalKept(c.id) && !keptClaimed[c.id] {
				keptClaimed[c.id] = true
				keptAsIs[i] = true
				break
			}
		}
	}

	for i, d := range dropEmission {
		if d {
			reason := dropChanged
			switch {
			case overflowed[i]:
				reason = dropOverflow
			case scopeRefused[i]:
				reason = dropScope
			default:
				if r := dropReasons[i]; r != dropNone {
					reason = r
				}
			}
			dropped.record(reason, overflowLinks[i])
			if shared && view.crossProjectFrom >= 0 && i >= view.crossProjectFrom {
				view.crossProjectDropped.record(reason, overflowLinks[i])
			}
		}
	}

	preserved = make([]string, 0, len(concurrent))
	for id := range concurrent {
		if (overflowKept[id] || scopeKept[id]) && !moved[id] {
			continue
		}
		preserved = append(preserved, id)
	}

	var deleteIDs []string
	for _, unmatched := range reusable {
		for _, c := range unmatched {
			if concurrent[c.id] {
				continue
			}
			deleteIDs = append(deleteIDs, c.id)
		}
	}
	deleted = len(deleteIDs)
	consumed := make(map[string]bool, len(deleteIDs))
	for _, id := range deleteIDs {
		consumed[id] = true
	}
	var reflected []string
	reused := 0
	var reusedIDs []string
	fresh := 0
	successorOf := map[string]string{}
	appliedByCategory = make(map[string]int, len(memories))
	keptLive := make(map[string]bool, len(candidates))
	for _, c := range candidates {
		if !consumed[c.id] {
			keptLive[c.id] = true
		}
	}
	for i, m := range memories {
		if dropEmission[i] {
			continue
		}
		folded := m
		folded.ReplacesIDs = sourcesNotKeptLive(m.ReplacesIDs, keptLive)
		appliedByCategory[m.Category]++
		if keptAsIs[i] {
			continue
		}
		tags, _ := json.Marshal(m.Tags)
		if stored, reusedRow := reuseFor[i]; reusedRow {
			id := stored.id
			reusedIDs = append(reusedIDs, id)
			verbatim := reuseChangesNothing(stored, m)
			switch {
			case verbatim:
			case reusePreservesAge(stored, m):
				if _, err := tx.ExecContext(ctx, `
					UPDATE memories
					SET category = ?, content = ?, importance = ?, tags = ?,
					    scope = COALESCE(?, scope),
					    updated_at = datetime('now')
					WHERE id = ?
				`, m.Category, m.Content, m.Importance, string(tags), scopeJSON(m.Scope), id); err != nil {
					return nil, EmissionDrops{}, 0, nil, fmt.Errorf("update retained memory: %w", err)
				}
				reflected = append(reflected, id)
			default:
				if _, err := tx.ExecContext(ctx, `
					UPDATE memories
					SET category = ?, content = ?, importance = ?, source = 'reflection', tags = ?,
					    scope = COALESCE(?, scope),
					    created_at = datetime('now'), updated_at = datetime('now')
					WHERE id = ?
				`, m.Category, m.Content, m.Importance, string(tags), scopeJSON(m.Scope), id); err != nil {
					return nil, EmissionDrops{}, 0, nil, fmt.Errorf("update reused memory: %w", err)
				}
				reflected = append(reflected, id)
			}
			if err := raiseReusedRetentionTx(ctx, tx, projectID, id, folded); err != nil {
				return nil, EmissionDrops{}, 0, nil, err
			}
			stamped, err := absorbSourcesTx(ctx, tx, projectID, id, m, folded, keptLive, consumed)
			if err != nil {
				return nil, EmissionDrops{}, 0, nil, err
			}
			if stamped && verbatim {
				reflected = append(reflected, id)
			}
			reused++
			for _, replaced := range folded.ReplacesIDs {
				successorOf[replaced] = id
			}
			continue
		}
		claims, err := inheritedClaims(ctx, tx, projectID, m, keptLive)
		if err != nil {
			return nil, EmissionDrops{}, 0, nil, err
		}
		tier, err := foldedRetention(ctx, tx, projectID, m, folded)
		if err != nil {
			return nil, EmissionDrops{}, 0, nil, err
		}
		scope := emittedScope[i]
		var newID string
		if err = tx.QueryRowContext(ctx, `
			INSERT INTO memories (project_id, category, content, source, importance, tags, scope,
			                      valid_from, valid_until, verified_at,
			                      confidence, agent, session_id, source_ref,
			                      retention, expires_at)
			VALUES (?, ?, ?, 'reflection', ?, ?, ?,
			        ?, ?, ?, ?, ?, ?, ?,
			        ?, ?)
			RETURNING id
		`, projectID, m.Category, m.Content, m.Importance, string(tags), scopeJSON(scope),
			nullIfEmptyPtr(claims.validFrom), nullIfEmptyPtr(claims.validUntil), nullIfEmptyPtr(claims.verifiedAt),
			claims.confidence, nullIfEmpty(claims.agent), nullIfEmpty(claims.sessionID),
			nullIfEmpty(claims.sourceRef), tier, sessionExpiry(tier, time.Now())).Scan(&newID); err != nil {
			return nil, EmissionDrops{}, 0, nil, fmt.Errorf("insert memory: %w", err)
		}
		if err := carryEvidenceTx(ctx, tx, newID, folded.ReplacesIDs); err != nil {
			return nil, EmissionDrops{}, 0, nil, err
		}
		reflected = append(reflected, newID)
		fresh++
		for _, replaced := range folded.ReplacesIDs {
			successorOf[replaced] = newID
		}
	}

	if shared {
		for _, p := range view.promotions {
			folded := p.m
			folded.ReplacesIDs = sourcesNotKeptLive(p.m.ReplacesIDs, keptLive)
			id, stamped, promoteErr, err := s.promoteReflectionCandidate(ctx, tx, projectID, p.m, p.scope,
				&promotionSources{folded: folded, keptLive: keptLive, consumed: consumed})
			if err != nil {
				return nil, EmissionDrops{}, 0, nil, err
			}
			var overflow *promotionOverflowError
			if errors.As(promoteErr, &overflow) {
				named := make(map[string]bool, len(p.m.ReplacesIDs))
				for _, replaced := range p.m.ReplacesIDs {
					named[replaced] = true
				}
				keep := make(map[string]bool)
				for _, c := range candidates {
					if _, taken := successorOf[c.id]; consumed[c.id] && !taken && (named[c.id] || c.content == p.m.Content) {
						keep[c.id] = true
					}
				}
				remaining := deleteIDs[:0:0]
				for _, id := range deleteIDs {
					if keep[id] {
						delete(consumed, id)
						keptLive[id] = true
						continue
					}
					remaining = append(remaining, id)
				}
				deleteIDs = remaining
				view.promotionDropped.record(dropOverflow, overflow.links)
				continue
			}
			if promoteErr != nil {
				view.failedPromotions = append(view.failedPromotions, failedPromotion{pendingPromotion: p, err: promoteErr})
				continue
			}
			view.promoted++
			if stamped {
				reflected = append(reflected, id)
			}
			for _, replaced := range folded.ReplacesIDs {
				if _, taken := successorOf[replaced]; !taken {
					successorOf[replaced] = id
				}
			}
		}
		deleted = len(deleteIDs)
	}

	if len(deleteIDs) > 0 {
		if err := appendHistoryForIDsTx(ctx, tx, deleteIDs, phaseDelete, Provenance{}); err != nil {
			return nil, EmissionDrops{}, 0, nil, err
		}
		stmt, err := tx.PrepareContext(ctx, `DELETE FROM memories WHERE id = ?`)
		if err != nil {
			return nil, EmissionDrops{}, 0, nil, fmt.Errorf("prepare delete replaced memory: %w", err)
		}
		for _, id := range deleteIDs {
			if _, err := stmt.ExecContext(ctx, id); err != nil {
				stmt.Close() //nolint:errcheck
				return nil, EmissionDrops{}, 0, nil, fmt.Errorf("delete replaced memory: %w", err)
			}
		}
		stmt.Close() //nolint:errcheck
	}

	if err := appendHistoryForIDsTx(ctx, tx, reflected, phaseReflect, Provenance{}); err != nil {
		return nil, EmissionDrops{}, 0, nil, err
	}
	for oldID, newID := range successorOf {
		if err := linkSuccessorTx(ctx, tx, oldID, newID); err != nil {
			return nil, EmissionDrops{}, 0, nil, err
		}
	}

	stillThere, err := corpusRowsStillPresent(ctx, tx, projectID, fed)
	if err != nil {
		return nil, EmissionDrops{}, 0, nil, err
	}
	retained := fresh + stillThere
	for _, id := range reusedIDs {
		if !fedIDs[id] {
			retained++
		}
	}
	dropped.Retained = retained

	if s.logger != nil && (reused > 0 || len(concurrent) > 0) {
		s.logger.Info("preserved memory identity across reflection replace",
			"project_id", RefusalArg("project_id", projectID), "reused", reused, "concurrent_kept", len(concurrent))
	}

	s.pruneSnapshotsTx(ctx, tx, projectID)

	if s.logger != nil {
		s.logger.Info("memories snapshotted before replace", "project_id", RefusalArg("project_id", projectID), "snapshot_id", snapshotID)
	}
	if ownTx {
		if err := tx.Commit(); err != nil {
			return nil, EmissionDrops{}, 0, nil, fmt.Errorf("commit replace: %w", err)
		}
		lock.reportHold("replace", time.Now())
	}
	return preserved, dropped, deleted, appliedByCategory, nil
}

const corpusChunkIDs = 200

func corpusRowsStillPresent(ctx context.Context, tx *sql.Tx, projectID string, fed []CorpusRow) (int, error) {
	if len(fed) == 0 {
		return 0, nil
	}
	ids := make([]string, 0, len(fed))
	for _, row := range fed {
		ids = append(ids, row.ID)
	}
	present := 0
	for start := 0; start < len(ids); start += corpusChunkIDs {
		end := min(start+corpusChunkIDs, len(ids))
		ph := make([]string, end-start)
		args := make([]any, 0, end-start+1)
		args = append(args, projectID)
		for i, id := range ids[start:end] {
			ph[i] = "?"
			args = append(args, id)
		}
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM memories WHERE project_id = ? AND id IN (`+strings.Join(ph, ",")+`)`,
			args...).Scan(&n); err != nil {
			return 0, fmt.Errorf("count the corpus rows still present: %w", err)
		}
		present += n
	}
	return present, nil
}

// RestoreResult is what a restore did, for the caller that reports it.
type RestoreResult struct {
	Restored            int
	UncapturedRetention int
}

// RestoreSnapshot restores memories from the most recent snapshot for a project,
func (s *Store) RestoreSnapshot(ctx context.Context, projectID string) (RestoreResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "restore-snapshot")
	if err != nil {
		return RestoreResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var snapshotID string
	err = tx.QueryRowContext(ctx, `
		SELECT snapshot_id FROM memory_snapshots
		WHERE project_id = ?
		ORDER BY snapshot_id DESC
		LIMIT 1
	`, projectID).Scan(&snapshotID)
	if err == sql.ErrNoRows {
		return RestoreResult{}, fmt.Errorf("no snapshots found for project %s", RefusalArg(ProjectIdentifierLabel, projectID))
	}
	if err != nil {
		return RestoreResult{}, fmt.Errorf("find snapshot: %w", err)
	}

	var uncapturedRetention int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM memory_snapshots s
		WHERE s.snapshot_id = ? AND s.retention_captured = 0
		  AND (
		        EXISTS (SELECT 1 FROM memories m
		                WHERE m.id = s.memory_id AND m.project_id = s.project_id
		                  AND m.pinned = 0 AND m.resolved_at IS NULL
		                  AND m.retention <> 'persistent')
		    OR (s.memory_id IS NOT NULL AND s.memory_id NOT IN (SELECT id FROM memories))
		    OR (s.memory_id IS NULL
		        AND NOT EXISTS (SELECT 1 FROM memories m
		                        WHERE m.project_id = s.project_id
		                          AND m.content = s.content AND m.source = s.source))
		  )
	`, snapshotID).Scan(&uncapturedRetention); err != nil {
		return RestoreResult{}, fmt.Errorf("count covered rows without a recorded tier: %w", err)
	}

	sessionExp := sessionExpiry(RetentionSession, time.Now())

	removedIDs, err := selectIDs(ctx, tx, `
		SELECT id FROM memories
		WHERE project_id = ? AND source = 'reflection' AND pinned = 0 AND resolved_at IS NULL
		  AND `+retentionExemptSQL+`
		  AND NOT EXISTS (
		      SELECT 1 FROM memory_snapshots s
		      WHERE s.snapshot_id = ?
		        AND ((s.memory_id IS NOT NULL AND s.memory_id = memories.id)
		             OR (s.memory_id IS NULL
		                 AND s.content = memories.content AND s.source = memories.source))
		  )`, projectID, snapshotID)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("find replace output to remove: %w", err)
	}
	if err := appendHistoryForIDsTx(ctx, tx, removedIDs, phaseDelete, Provenance{}); err != nil {
		return RestoreResult{}, err
	}
	del, err := tx.ExecContext(ctx, `
		DELETE FROM memories
		WHERE project_id = ? AND source = 'reflection' AND pinned = 0 AND resolved_at IS NULL
		  AND `+retentionExemptSQL+`
		  AND NOT EXISTS (
		      SELECT 1 FROM memory_snapshots s
		      WHERE s.snapshot_id = ?
		        AND ((s.memory_id IS NOT NULL AND s.memory_id = memories.id)
		             OR (s.memory_id IS NULL
		                 AND s.content = memories.content AND s.source = memories.source))
		  )
	`, projectID, snapshotID)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("remove replace output: %w", err)
	}
	removedN, _ := del.RowsAffected()

	changedIDs, err := selectIDs(ctx, tx, `
		SELECT s.memory_id FROM memory_snapshots s
		JOIN memories m ON m.id = s.memory_id
		WHERE s.snapshot_id = ? AND s.memory_id = m.id
		  AND m.project_id = s.project_id
		  AND m.pinned = 0 AND m.resolved_at IS NULL
		  AND m.`+retentionExemptSQL+`
		  AND (s.content != m.content OR s.category != m.category
		       OR s.importance != m.importance OR s.source != m.source)`, snapshotID)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("find rows this restore changes: %w", err)
	}

	revertTextIDsSQL := `
		SELECT m.id FROM memories m
		JOIN memory_snapshots s ON s.memory_id = m.id
		WHERE s.snapshot_id = ? AND m.project_id = s.project_id
		  AND m.pinned = 0 AND m.resolved_at IS NULL
		  AND m.` + retentionExemptSQL + `
		  AND s.content != m.content`
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memory_embeddings WHERE memory_id IN (`+revertTextIDsSQL+`)`, snapshotID); err != nil {
		return RestoreResult{}, fmt.Errorf("invalidate reverted embedding: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM link_scans WHERE memory_id IN (`+revertTextIDsSQL+`)`, snapshotID); err != nil {
		return RestoreResult{}, fmt.Errorf("invalidate reverted link scan: %w", err)
	}
	changedRowIDs, err := selectIDs(ctx, tx, `
		SELECT m.id FROM memories m
		JOIN memory_snapshots s ON s.memory_id = m.id
		WHERE s.snapshot_id = ? AND m.project_id = s.project_id
		  AND m.pinned = 0 AND m.resolved_at IS NULL
		  AND m.`+retentionExemptSQL+`
		  AND (m.category IS NOT s.category OR m.content IS NOT s.content
		       OR m.importance IS NOT s.importance OR m.source IS NOT s.source
		       OR m.tags IS NOT s.tags OR m.created_at IS NOT s.created_at
		       OR m.updated_at IS NOT COALESCE(s.updated_at, m.updated_at)
		       OR m.access_count IS NOT s.access_count
		       OR m.last_accessed IS NOT s.last_accessed
		       OR m.agent IS NOT s.agent OR m.session_id IS NOT s.session_id
		       OR m.source_ref IS NOT s.source_ref OR m.confidence IS NOT s.confidence
		       OR m.valid_from IS NOT s.valid_from OR m.valid_until IS NOT s.valid_until
		       OR m.verified_at IS NOT s.verified_at
		       OR (s.scope_captured = 1 AND m.scope IS NOT s.scope)
		       OR (s.retention_captured = 1 AND s.retention IS NOT NULL
		           AND (m.retention IS NOT s.retention
		                OR m.expires_at IS NOT CASE
		                     WHEN s.retention = 'session' AND m.retention = 'session'
		                          AND m.expires_at IS NOT NULL THEN m.expires_at
		                     WHEN s.retention = 'session' THEN ?
		                     ELSE NULL END)))`, snapshotID, sessionExp)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("find rows this restore rewrites: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE memories SET
		    category = s.category, content = s.content, importance = s.importance,
		    source = s.source, tags = s.tags, created_at = s.created_at,
		    updated_at = COALESCE(s.updated_at, memories.updated_at),
		    access_count = s.access_count, last_accessed = s.last_accessed,
		    agent = s.agent, session_id = s.session_id, source_ref = s.source_ref,
		    confidence = s.confidence, valid_from = s.valid_from,
		    valid_until = s.valid_until, verified_at = s.verified_at,
		    -- scope_captured distinguishes a snapshot that recorded the
		    -- memory's scope from a pre-v14 one whose scope column is NULL
		    -- because that build had no column to write. For the former the
		    -- recorded value wins, NULL included (the memory was unscoped
		    -- then, and the restore is a revert, not an upgrade). For the
		    -- latter the snapshot says nothing about scope, so the live row
		    -- keeps its own — otherwise rolling back to an old snapshot
		    -- silently unscoped a fact a later reflection had scoped
		    -- correctly, making the corpus less specific than it was before
		    -- the replace being undone.
		    scope = CASE WHEN s.scope_captured = 1 THEN s.scope ELSE memories.scope END,
		    -- retention reads the marker the same way, and the tier it puts
		    -- back is the lifetime the undo was missing (#1341): a session
		    -- row restored without it became a project row nothing could
		    -- prune, and a tier raise the replace's reuse branch made
		    -- (raiseReusedRetentionTx) outlived the undo that should have
		    -- undone it. A row the snapshot cannot speak about keeps the live
		    -- tier — reverting one to the column DEFAULT would be worse,
		    -- because the DEFAULT is a claim the snapshot never made — and
		    -- the caller reports that count.
		    retention = CASE WHEN s.retention_captured = 1 AND s.retention IS NOT NULL
		                     THEN s.retention ELSE memories.retention END,
		    -- The expiry is RE-DERIVED, never copied, because sessionExpiry
		    -- is the only source of one — with one exception, which is the
		    -- one that keeps a restore idempotent (#1341). A row whose tier
		    -- moves onto session gets a fresh now+SessionTTL exactly as a save
		    -- writes it; a tier that never moved (session already, with an
		    -- expiry) keeps the expiry it holds, expiry and all, so a second
		    -- restore changes nothing; a durable tier is restored with none.
		    -- A session row coming back to life therefore still ends up
		    -- prunable rather than permanent, which is the property the tier
		    -- exists to carry.
		    expires_at = CASE
		        WHEN s.retention_captured = 0 OR s.retention IS NULL THEN memories.expires_at
		        -- Idempotence (#1341): a snapshot the replace reproduced verbatim
		        -- must not extend a session row's life every restore. The UPDATE
		        -- touches every covered row, so re-deriving expires_at = now +
		        -- SessionTTL would push the expiry further out on each run and
		        -- break the claim that a second run leaves the corpus alone. A row
		        -- whose tier already matches the recorded snapshot's therefore
		        -- keeps the expiry it holds, including one a fold refreshed; only
		        -- a tier that actually moves, or a row carrying none, is
		        -- re-derived. The tier itself still takes the snapshot's value
		        -- (see retention above), matching how a restore reverts content,
		        -- scope and importance.
		        WHEN s.retention = 'session'
		             AND memories.retention = 'session'
		             AND memories.expires_at IS NOT NULL THEN memories.expires_at
		        WHEN s.retention = 'session' THEN ?
		        ELSE NULL END
		FROM memory_snapshots s
		WHERE s.snapshot_id = ? AND s.memory_id = memories.id
		  AND memories.project_id = s.project_id
		  AND memories.pinned = 0 AND memories.resolved_at IS NULL
		  AND memories.`+retentionExemptSQL+`
	`, sessionExp, snapshotID); err != nil {
		return RestoreResult{}, fmt.Errorf("restore existing rows: %w", err)
	}
	updatedN := int64(len(changedRowIDs))

	insertedRows, err := tx.QueryContext(ctx, `
		INSERT INTO memories (id, project_id, category, content, source, importance, tags,
		                      created_at, updated_at, access_count, last_accessed,
		                      agent, session_id, source_ref, confidence,
		                      valid_from, valid_until, verified_at, scope,
		                      retention, expires_at)
		SELECT COALESCE(memory_id, hex(randomblob(16))), project_id, category, content,
		       source, importance, tags, created_at, COALESCE(updated_at, created_at), access_count, last_accessed,
		       agent, session_id, source_ref, confidence,
		       valid_from, valid_until, verified_at,
		       -- The same marker the UPDATE above honours. A snapshot whose
		       -- scope_captured is 0 either predates the column or was
		       -- backfilled by hand, and migrateV14 deliberately calls such a
		       -- value unverified — so a row restored from one comes back
		       -- unscoped rather than carrying a scope nobody recorded. The
		       -- alternative is restoring a scope that was never true of that
		       -- memory, which is worse than restoring none: there is no
		       -- provenance to tell the two apart afterwards.
		       CASE WHEN scope_captured = 1 THEN scope ELSE NULL END,
		       -- And the tier, through its own marker (#1341). A row a
		       -- pre-v26 snapshot held has no recorded tier, so it comes back
		       -- at the column DEFAULT: project, kept longer, and never
		       -- pruned. That is the one honest answer for a row arriving
		       -- from nowhere — inventing a session tier would schedule a
		       -- memory for deletion on a clock nobody set, the direction
		       -- the portable importer refuses too — and the caller reports
		       -- the count so the incomplete undo is visible.
		       CASE WHEN retention_captured = 1 AND retention IS NOT NULL
		            THEN retention ELSE 'project' END,
		       -- The recorded session tier is restored with a derived expiry,
		       -- the same sessionExpiry a save writes, because a session row
		       -- without one is a row no ghost prune could ever reach.
		       CASE WHEN retention_captured = 1 AND retention = 'session' THEN ? ELSE NULL END
		FROM memory_snapshots
		WHERE snapshot_id = ?
		  AND ((memory_id IS NOT NULL AND memory_id NOT IN (SELECT id FROM memories))
		       OR (memory_id IS NULL
		           AND NOT EXISTS (SELECT 1 FROM memories m
		                           WHERE m.project_id = memory_snapshots.project_id
		                             AND m.content = memory_snapshots.content
		                             AND m.source = memory_snapshots.source)))
		RETURNING id
	`, sessionExp, snapshotID)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("restore snapshot: %w", err)
	}
	var reinserted []string
	for insertedRows.Next() {
		var id string
		if err := insertedRows.Scan(&id); err != nil {
			insertedRows.Close() //nolint:errcheck
			return RestoreResult{}, fmt.Errorf("scan restored memory id: %w", err)
		}
		reinserted = append(reinserted, id)
	}
	if err := insertedRows.Err(); err != nil {
		insertedRows.Close() //nolint:errcheck
		return RestoreResult{}, fmt.Errorf("iterate restored memories: %w", err)
	}
	insertedRows.Close() //nolint:errcheck
	insertedN := int64(len(reinserted))

	if err := uncarryRestoredSourcesTx(ctx, tx, snapshotID); err != nil {
		return RestoreResult{}, err
	}
	if len(reinserted) > 0 {
		if err := restoreSnapshotEvidenceTx(ctx, tx, snapshotID, reinserted); err != nil {
			return RestoreResult{}, err
		}
	}

	restored := make([]string, 0, len(changedIDs)+len(reinserted))
	restored = append(restored, changedIDs...)
	for _, id := range reinserted {
		if !slices.Contains(changedIDs, id) {
			restored = append(restored, id)
		}
	}
	if err := appendHistoryForIDsTx(ctx, tx, restored, phaseRestore, Provenance{}); err != nil {
		return RestoreResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return RestoreResult{}, fmt.Errorf("commit: %w", err)
	}

	n := int(removedN + updatedN + insertedN)
	s.logger.Info("memories restored from snapshot", "project_id", RefusalArg("project_id", projectID), "snapshot_id", snapshotID,
		"removed", removedN, "updated", updatedN, "reinserted", insertedN, "count", n,
		"uncaptured_retention", uncapturedRetention)
	return RestoreResult{Restored: n, UncapturedRetention: uncapturedRetention}, nil
}

// CountMemories returns the total number of memories for a project.
func (s *Store) CountMemories(ctx context.Context, projectID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.queryDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM memories WHERE project_id = ?`, projectID).Scan(&count)
	return count, err
}

// ConsolidatableCounts reports how many consolidatable memories each project
func (s *Store) ConsolidatableCounts(ctx context.Context) (map[string]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.queryDB().QueryContext(ctx, `
		SELECT project_id, COUNT(*)
		FROM memories
		WHERE project_id != '_global'
		  AND pinned = 0
		  AND source NOT IN ('manual', 'builtin')
		  AND retention != 'persistent'
		  AND resolved_at IS NULL
		GROUP BY project_id
	`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]int)
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// CountActiveMemories counts the project's memories that pass the one predicate
func (s *Store) CountActiveMemories(ctx context.Context, projectID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE project_id = ? AND resolved_at IS NULL`, projectID).Scan(&count)
	return count, err
}

// CountConsolidatableMemories counts the project\'s memories that `ghost reflect`
func (s *Store) CountConsolidatableMemories(ctx context.Context, projectID string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var count int
	err := s.queryDB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM memories WHERE project_id = ? AND `+replaceableRowSQL,
		projectID).Scan(&count)
	return count, err
}

// IncrementInteraction increments the interaction count and returns the new value.
func (s *Store) IncrementInteraction(ctx context.Context, projectID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, lock, err := s.beginWrite(ctx, "increment-interaction")
	if err != nil {
		return 0, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `
		UPDATE ghost_state
		SET interaction_count = interaction_count + 1, updated_at = datetime('now')
		WHERE project_id = ?
		RETURNING interaction_count
	`, projectID).Scan(&count); err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	lock.reportHold("increment-interaction", time.Now())
	return count, nil
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
	if err := rejectSecret("learned_context", learnedContext); err != nil {
		return err
	}
	if err := rejectSecret("reflection_summary", summary); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	res, err := s.execGuardedWrite(ctx, "update-learned-context", `
		UPDATE ghost_state
		SET learned_context = ?, reflection_summary = ?,
		    last_reflection_at = datetime('now'), updated_at = datetime('now')
		WHERE project_id = ?
	`, learnedContext, summary, projectID)
	if err != nil {
		return err
	}
	updated, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return fmt.Errorf("update learned context: no ghost_state row for %s", ProjectArg(ProjectIdentifierLabel, projectID))
	}
	return nil
}

// GetReflectInputSignature returns the fingerprint of the memory set that
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
func (s *Store) SetReflectInputSignature(ctx context.Context, projectID, sig string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.execGuardedWrite(ctx, "set-reflect-signature", `
		UPDATE ghost_state
		SET reflect_input_sig = ?, updated_at = datetime('now')
		WHERE project_id = ?
	`, sig, projectID)
	if err != nil {
		return fmt.Errorf("set reflect signature: %w", err)
	}
	return nil
}

var memoryColumnNames = []string{
	"id", "project_id", "category", "content", "importance", "access_count",
	"last_accessed", "source", "tags", "pinned", "resolved_at", "created_at", "updated_at",
	"agent", "session_id", "source_ref", "confidence", "scope",
	"valid_from", "valid_until", "verified_at",
	"retention", "expires_at",
}

var memoryColumnsPrefixed = qualifyColumns("m")

func qualifyColumns(prefix string) string { return qualifyColumnsFrom(memoryColumnNames, prefix) }

func qualifyColumnsFrom(names []string, prefix string) string {
	out := make([]string, len(names))
	for i, c := range names {
		if prefix != "" && !strings.Contains(c, " ") && !strings.Contains(c, ".") {
			out[i] = prefix + "." + c
			continue
		}
		out[i] = c
	}
	return strings.Join(out, ", ")
}

var memoryColumns = qualifyColumns("")

func scanMemories(rows *sql.Rows) ([]Memory, error) {
	var memories []Memory
	for rows.Next() {
		m, err := scanMemoryRow(rows)
		if err != nil {
			return nil, err
		}
		memories = append(memories, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory rows: %w", err)
	}
	return memories, nil
}

func scanMemoryRow(rows *sql.Rows) (Memory, error) {
	var m Memory
	var lastAccessed sql.NullString
	var resolvedAt sql.NullString
	var tagsJSON string
	var pinned int
	var agent, sessionID, sourceRef sql.NullString
	var confidence sql.NullFloat64
	var scopeRaw sql.NullString
	var validFrom, validUntil, verifiedAt sql.NullString
	var expiresAt sql.NullString

	if err := rows.Scan(
		&m.ID, &m.ProjectID, &m.Category, &m.Content, &m.Importance,
		&m.AccessCount, &lastAccessed, &m.Source, &tagsJSON,
		&pinned, &resolvedAt, &m.CreatedAt, &m.UpdatedAt,
		&agent, &sessionID, &sourceRef, &confidence, &scopeRaw,
		&validFrom, &validUntil, &verifiedAt,
		&m.Retention, &expiresAt,
	); err != nil {
		return m, fmt.Errorf("scan memory: %w", err)
	}

	if lastAccessed.Valid {
		m.LastAccessed = &lastAccessed.String
	}
	if resolvedAt.Valid {
		m.ResolvedAt = &resolvedAt.String
	}
	m.Pinned = pinned == 1

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
	if validFrom.Valid {
		m.ValidFrom = &validFrom.String
	}
	if validUntil.Valid {
		m.ValidUntil = &validUntil.String
	}
	if verifiedAt.Valid {
		m.VerifiedAt = &verifiedAt.String
	}
	if expiresAt.Valid {
		m.ExpiresAt = &expiresAt.String
	}
	if m.Retention == "" {
		m.Retention = RetentionProject
	}

	if err := json.Unmarshal([]byte(tagsJSON), &m.Tags); err != nil {
		m.Tags = []string{}
	}

	return m, nil
}

const upsertMergeThreshold = 0.5

const upsertCrossCategoryThreshold = 0.7

const (
	upsertOverlapThreshold = 0.5
	upsertOverlapMinTokens = 5
	upsertOverlapMinRatio  = 0.6
)

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

const ftsSearchWordLimit = 10

func sanitizeFTS(text string, logger *slog.Logger) string {
	return sanitizeFTSN(text, ftsSearchWordLimit, logger)
}

func sanitizeFTSN(text string, maxWords int, logger *slog.Logger) string {
	terms := ftsQueryTerms(text, maxWords, logger)
	if len(terms) == 0 {
		return `""`
	}
	words := make([]string, len(terms))
	for i, t := range terms {
		words[i] = t.text
	}
	return strings.Join(words, " OR ")
}

func ftsEdgeRuneTrimmed(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r)
}

func ftsHasBaseRune(word string) bool {
	for _, r := range word {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func ftsQueryTerms(text string, maxWords int, logger *slog.Logger) []ftsTerm {
	if logger == nil {
		logger = slog.Default()
	}

	var terms []ftsTerm
	for _, word := range strings.Fields(text) {
		prefix := strings.HasSuffix(word, "*")
		clean := strings.TrimFunc(word, ftsEdgeRuneTrimmed)
		clean = strings.ReplaceAll(clean, "\x00", " ")
		if !wordYieldsTerm(word) {
			continue
		}
		escaped := strings.ReplaceAll(clean, `"`, `""`)
		term := `"` + escaped + `"`
		if prefix {
			term += "*"
		}
		terms = append(terms, ftsTerm{
			text:   term,
			clean:  clean,
			value:  ftsTermValue(clean),
			pos:    len(terms),
			prefix: prefix,
		})
	}
	if len(terms) == 0 {
		return nil
	}
	if len(terms) > maxWords {
		logger.Warn("fts query truncated",
			"original_terms", len(terms),
			"limit", maxWords)
		terms = selectFTSTERMs(terms, maxWords)
	}
	return terms
}
