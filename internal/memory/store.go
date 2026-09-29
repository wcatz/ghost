package memory

// Package memory provides the SQLite-backed persistence layer for Ghost memories,
// including CRUD operations, FTS5 search, hybrid vector/FTS retrieval, time-decay
// scoring with category-aware decay, pin exemptions, project lifecycle management,
// tasks, decisions, memory links (supersedes/contradicts/elaborates/causes).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// GlobalProjectID is the reserved project id Ghost stores its own shipped
// rules under. It is a persisted value, not an in-memory convention: it is the
// projects.id row the global seeds are written into, the memories.project_id
// every global row carries, and the value cross-project queries filter on.
//
// A row's project comes from the row. A caller that stamps this sentinel onto
// memory it merely recognised as global would make the origin rewrite apply to
// rows it never owned.
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

	// Validity: when this memory was true, and when someone last checked.
	//
	// All three are pointers because nil is the honest reading of a NULL
	// column — no claim was made — while an empty string would look like a
	// claim about the empty moment. The tools write them (ghost_memory_save,
	// ghost_save_global, ghost_memory_update) and Create writes them, so a row
	// with a window is now expressible; ImportMemory and RestoreSnapshot carry
	// the triple too, and every row written before any of those reads nil, which
	// is what stage 2 calls unset. SQLite holds them as unconstrained text, so
	// the values are the stored strings, not parsed times: interpreting them
	// belongs to the caller, which is the only layer that knows the request
	// clock. The tools normalize what they accept into memory.StoredStampLayout,
	// but nothing here may assume it — an imported artifact, a restored snapshot
	// or a hand-edited row can hold the whole-day form the readers also accept.
	ValidFrom  *string `json:"valid_from,omitempty"`
	ValidUntil *string `json:"valid_until,omitempty"`
	VerifiedAt *string `json:"verified_at,omitempty"`

	// Retention is how long this memory is wanted: session, project (the
	// default) or persistent. It is the lifecycle axis, and the only one that
	// can take a row away on its own — and only a session row, only past its
	// expiry, only past a grace period, and only behind `ghost prune --apply`.
	//
	// Empty means "this row came from a query that did not select the column",
	// or "no version ever recorded one" — the historical read is the second, and
	// leaves it empty on purpose (see AsOfRow.Retention). It is never read as
	// session, because the one tier whose absence has consequences is the one no
	// reader may infer.
	Retention string `json:"retention"`
	// ExpiresAt is when a session row stops being wanted, derived on save as
	// now+SessionTTL. NULL for every other tier, and NULL means "no expiry is
	// claimed": a row without one is never a prune candidate. Like the validity
	// triple it is the stored string, not a parsed time — interpreting it belongs
	// to the caller, the only layer that has the request clock.
	ExpiresAt *string `json:"expires_at,omitempty"`

	// ReplacesIDs names the input rows this row stands in for — the ids a
	// consolidation folded together or rewrote into this one. Only
	// ReplaceNonManual reads it, and only to stamp the successor id on those
	// rows' delete history: a rewrite or a merge gives the row a NEW id, so
	// without this a reader following one memory's history hits a wall at exactly
	// the point where the memory changed. Set by the reflection apply path from
	// the merge and rewrite operations, which know the ids; never persisted,
	// hence json:"-", because it is a proposal's bookkeeping and not a field of
	// the memory.
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
// explanations and candidate retrieval use it to keep their production result
// and diagnostic reads on one SQLite snapshot: a method that takes a *sql.DB
// cannot be redirected onto a transaction, and a transaction that issued its
// reads through the pool would wait for a second connection the single-
// connection store does not have.
type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Store manages the SQLite memory database.
type Store struct {
	db *sql.DB
	// snapshot is set only on the short-lived Store a snapshot read builds for
	// itself. It redirects read-only search helpers to the transaction's
	// consistent view.
	snapshot Queryer
	// readDB is the read-only handle a production store is given so
	// Candidates can take its snapshot transaction without taking the write
	// lock the primary handle's BEGIN IMMEDIATE would. nil means "use db",
	// which is correct for in-memory and bench stores (no concurrent writer)
	// and logged for a file-backed one.
	readDB   *sql.DB
	mu       sync.RWMutex
	warnOnce sync.Once
	logger   *slog.Logger
	onSave   func(projectID string) // optional callback after memory create/upsert

	// demotionThreshold gates near-duplicate demotion in GetTopMemories (see
	// DemotionPenalties). Defaults to DefaultDemotionThreshold; callers that
	// have loaded config override it via SetDemotionThreshold.
	demotionThreshold float64

	// vectorMinSimilarity is the cosine floor applied to the vector leg of
	// hybrid search (SearchParams.MinSimilarity). Defaults to 0 (historical:
	// only non-positive cosines dropped); config search.min_similarity
	// overrides via SetVectorMinSimilarity.
	vectorMinSimilarity float32

	// embeddingIdentity is the vector space this process embeds into — the
	// model, its dimensions and its task-prefix setting, as produced by
	// embedding.VectorIdentity. Vector search only scores a stored vector whose
	// recorded identity matches it, and the embedding worker re-embeds the ones
	// that do not. Empty (the default, and the state of the bench harness and
	// of tests) means no identity is configured: every recorded vector is
	// searched and only the presence of a row matters.
	embeddingIdentity string

	// foreignWarned gates the warning loadVectorRows logs when a search
	// skips vectors recorded under a retired identity: it records which
	// retired identities have already been reported, so each is logged once
	// per process rather than once per search — a re-embed lasts many queries,
	// and one line per query buries the state it reports. The gate is keyed on
	// the identity rather than being a single flag because a long-lived MCP
	// server can reconfigure twice in one session; the second retirement is a
	// new diagnosis with its own stored_identity and must warn again. It is a
	// pointer so ExplainSearch can hand its trace store the same gate (see
	// explain.go): an explain run must not spend a second warning on rows the
	// real search already reported.
	foreignWarned *foreignWarnGate

	// scratch recycles the per-search corpus snapshot a vector search copies
	// candidate rows into. Concurrent searches each take their own, and the pool
	// exists so that a steady-state search refills the column buffers it grew last
	// time instead of allocating the whole corpus again per query — see #556.
	//
	// A pointer, not a value, because a Store literal standing in for another
	// store — the snapshot Candidates builds, the trace store ExplainSearch
	// builds — must share it rather than bring its own. A sync.Pool must not be
	// copied after use, and a pool of its own on a literal is a pool of one that is
	// garbage the moment the literal goes out of scope: the corpus snapshot would
	// be allocated per query and dropped with the store, which is the cost this
	// exists to avoid.
	scratch *vectorScratch
}

// foreignWarnGate is the per-identity warning gate: the set of retired
// identities whose foreign-vector warning this process has already logged.
type foreignWarnGate struct {
	mu     sync.Mutex
	warned map[string]bool
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

// SetEmbeddingIdentity declares which vector space this process embeds into —
// the model, its dimensions and its task-prefix setting, as produced by
// embedding.VectorIdentity. Call after NewStore once config is loaded, with
// the same string the embedding client stamps into memory_embeddings.model.
//
// It is what makes a model change safe: vectors recorded under a different
// identity are excluded from the vector leg (they are not comparable with a
// query embedded in the configured space) and reported back to the embedding
// worker as unembedded, so they are rewritten in place. Until it is called, the
// store searches whatever vectors exist regardless of which model wrote them —
// the pre-identity behaviour.
func (s *Store) SetEmbeddingIdentity(identity string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.embeddingIdentity = identity
}

// configuredEmbeddingIdentity returns the identity set by
// SetEmbeddingIdentity under the read lock. It is not called with the lock
// already held.
func (s *Store) configuredEmbeddingIdentity() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.embeddingIdentity
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
	s := &Store{db: db, logger: logger, demotionThreshold: DefaultDemotionThreshold, foreignWarned: &foreignWarnGate{warned: make(map[string]bool)}}
	s.scratch = newVectorScratch()
	return s
}

// NewStoreWithRead is NewStore with a read-only handle for snapshot reads.
//
// The production DSN issues BEGIN IMMEDIATE, so a transaction on the primary
// handle takes the write lock for its whole lifetime. Candidates needs a
// transaction (legs, hydration, edges and penalty lookups on one snapshot), and
// holding the write lock across a full retrieval would block every concurrent
// writer in the machine — the reflection, embedding and linking workers, and
// any save arriving on the live MCP server. The read handle's DSN has no
// _txlock, so the same transaction is a plain deferred read: WAL readers do not
// wait on a writer, which is the whole point of running the store in WAL.
func NewStoreWithRead(db, readDB *sql.DB, logger *slog.Logger) *Store {
	s := NewStore(db, logger)
	s.readDB = readDB
	return s
}

// readHandle is the handle snapshot reads run on: the injected read-only
// connection when there is one, otherwise the primary handle.
func (s *Store) readHandle() *sql.DB {
	if s.readDB != nil {
		return s.readDB
	}
	return s.db
}

// warnNoReadHandle records the cost of running a snapshot read on the primary
// handle, once per store. The primary DSN issues BEGIN IMMEDIATE, so a
// transaction there takes the write lock for its whole lifetime; a store
// without a read handle is the in-memory, bench and test case, where there is
// no second process and no writer to block, and a file-backed production store
// that was wired up wrongly.
func (s *Store) warnNoReadHandle() {
	if s.db == nil {
		return
	}
	s.warnOnce.Do(func() {
		s.logger.Warn("store has no read-only handle: candidate retrieval takes its snapshot on the primary connection, whose DSN issues BEGIN IMMEDIATE, so the transaction holds the write lock; build the store with NewStoreWithRead and memory.OpenReadDB for a file-backed database")
	})
}

// queryDB is the read surface the search seams use: the snapshot transaction
// when one is in force, otherwise the store's own handle.
func (s *Store) queryDB() Queryer {
	if s.snapshot != nil {
		return s.snapshot
	}
	return s.db
}

// seedGlobalMemory defines a memory that Ghost ships with out of the box.
// These are inserted as source='builtin', pinned=1, so consolidation never
// touches them and provenance surfaces can distinguish them from user writes.
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
		Content:    builtinSeedContent,
		Importance: 1.0,
		Tags:       []string{"git", "commits", "non-negotiable"},
	},
}

// SeedGlobalMemories ensures the _global project exists and inserts any
// missing seed memories. Seeds are pinned and source='builtin' so they
// survive consolidation without being presented as user-authored. Idempotent —
// skips memories whose content already exists.
func (s *Store) SeedGlobalMemories(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Ensure _global project.
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
		// Skip if content already exists in _global (exact match).
		var exists int
		err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memories WHERE project_id = '_global' AND content = ?`,
			seed.Content).Scan(&exists)
		if err == nil && exists > 0 {
			continue
		}

		tags, _ := json.Marshal(seed.Tags)
		// One transaction per seed, and a failed seed stays a warning rather
		// than aborting the rest: this runs on every open, and a store missing
		// a builtin rule must not be a store with no rules at all. The history
		// row goes in the same transaction, so "which rows did Ghost ship
		// itself" is answerable from the same table as everything else.
		if err := s.seedMemoryTx(ctx, seed, string(tags)); err != nil {
			s.logger.Warn("seed memory insert failed", "content", seed.Content, "error", err)
		}
	}

	return nil
}

// seedMemoryTx inserts one builtin seed and its first history row in a single
// transaction. A seed is a memory like any other, so it gets a recorded origin:
// the phase is save rather than a seed-specific value because the row's own
// source column already says `builtin`, and a phase nothing reads is a phase no
// filter can use.
func (s *Store) seedMemoryTx(ctx context.Context, seed seedGlobalMemory, tagsJSON string) error {
	tx, _, err := s.beginWrite(ctx, "seed")
	if err != nil {
		return fmt.Errorf("begin seed tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
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
// Close releases the store's handles. The injected read handle is a second
// connection this Store owns, so it is closed too: leaving it open holds a WAL
// reader for the life of the process, which is exactly what the read handle
// exists to avoid holding during a retrieval.
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

// BindingRefusalKind names the rule that stopped a repository from claiming
// the project its name matched.
type BindingRefusalKind string

const (
	// RefusedAmbiguousName: the name matched more than one project, so none
	// of them is evidence for any of the others.
	RefusedAmbiguousName BindingRefusalKind = "ambiguous-name"
	// RefusedDifferentRemote: the one project of that name already records
	// another repository.
	RefusedDifferentRemote BindingRefusalKind = "different-remote"
	// RefusedPathMismatch: the one project of that name records a checkout
	// the saving directory is not inside.
	RefusedPathMismatch BindingRefusalKind = "path-mismatch"
)

// BindingRefusal reports that a repository was not allowed to claim the
// project its name matched, and names the project the save went to instead.
//
// Every refusal is a fail-safe, and none of them costs the save: the memory is
// still written, to SavedTo. What a refusal costs is the caller's ability to
// tell "this save opened a new project" from "a project of this name exists
// and was left behind" — the second is context the agent has just stopped
// seeing, and it is indistinguishable from the first unless it is reported.
//
// It is returned rather than only logged because the log is not answerable by
// the agent that made the save. Only the caller knows which project it
// addressed and what it can tell the user about the result.
type BindingRefusal struct {
	// Kind is the rule that refused the binding.
	Kind BindingRefusalKind
	// Name is the name derived from the remote that no project could be bound
	// by — never a directory basename.
	Name string
	// ProjectIDs are the projects that record Name, id-ordered, at most
	// maxNamedCandidates of them: the read that produces them is bounded (see
	// projectsNamedTx), and the rest are counted rather than listed.
	ProjectIDs []string
	// CandidateCount is how many projects record Name in total, which is more
	// than len(ProjectIDs) whenever the list is capped. It is what makes the
	// notice say "7 projects" beside five names.
	CandidateCount int
	// RecordedPath is the checkout the refusing project records, "" for an
	// ambiguous name and for a project that records no location.
	RecordedPath string
	// RecordedRemote is the repository the refusing project already claims,
	// "" when it claims none — which is the case RefusedPathMismatch turns on.
	RecordedRemote string
	// SavedTo is the project this save was routed to instead.
	SavedTo string
}

// Notice renders the refusal for whoever asked for the save: what kept the
// name, where the memory went, and what — if anything — the reader can run to
// make the split go away. Empty for no refusal, so a caller can append it to
// every save result without a nil check.
//
// What each kind can be repaired by is not the same, and a command that does
// not repair it is worse than no command: a reader who follows advice that
// does not work is further from a fix than one who was told there is nothing
// to run. Only RefusedPathMismatch has a repair, and it takes two commands in
// a fixed order — a merge on its own deletes the project the save landed in
// and with it the repository it recorded, so the next save from that checkout
// is refused again and opens a second project; the bind is what puts the
// checkout and its repository on the project that kept the name. The merge
// comes first because the bind is refused while the other project still
// records that directory.
//
// RefusedDifferentRemote has none: two projects that claim two different
// repositories are not a split to be closed, and the project this save used
// already records the checkout, so the next save finds it by id. It names both
// projects by id and by path rather than by a command to run: `ghost project`
// dispatches delete, merge and bind and nothing else (cmd/ghost/main.go), so
// there is no listing command for a notice here to point at.
//
// The commands are shell-quoted (shellQuote) rather than Go-quoted, because a
// project id here is often a checkout path and the reader is meant to paste the
// sentence: %q doubles the backslashes of a Windows path, leaves a $ or a
// backtick for the shell to expand inside its double quotes, and escapes a
// control character the shell cannot read back. Prose keeps %q, which is the
// clearer way to set a name or a path inside a sentence that is read rather than
// run.
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
	// A refusal built without the count still says how many it found, so a
	// caller cannot render "0 projects" for a name two projects share.
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
		// A kind added later renders here rather than borrowing another
		// kind's sentence, which would suggest commands that repair a split
		// this refusal never described.
		return fmt.Sprintf("project %q was not bound to this repository (%s); saved to %s instead — save under the project you meant, by name, id or path",
			r.Name, r.Kind, saved)
	}
}

// maxNamedCandidates bounds how many candidate projects a notice names, and
// equally how many this decision carries into its log line and that notice.
// The text is returned to the agent that made the save and is often still in
// its context for the rest of the session, and the count of same-named
// projects is not bounded by anything — it is exactly what a project-merge
// accident leaves behind.
const maxNamedCandidates = 5

// namedCandidates lists the candidate ids a refusal carries, at most
// maxNamedCandidates of them, and counts the rest. The caller has to know that
// more exist than it is being shown — it is about to choose a project to save
// under — while which ones to start with is not a decision this text should
// make. matched is the true total, which is more than the ids whenever the
// read that found them was capped.
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
// and creates that project when no identity match exists. The complete
// resolve-or-create sequence runs in one immediate SQLite transaction, so
// separate Ghost processes cannot both create the first project for a remote.
//
// It returns the project the save must be written to, and a refusal when the
// unique-name fallback was not allowed to bind the repository to the project
// its name matched. The refusal never replaces the id: the save is still
// routed, and a caller that reports it is telling the truth about a memory
// that landed, not declining one. Only the log used to record it (#613).
//
// projectRef is the id or path ordinary resolution produced; repoName is the
// final component of the normalized remote, never a directory basename. An
// explicit project already bound to another repository is an error rather than
// a reason to route the save to that remote's existing owner.
//
// path is the session's directory on this filesystem, not a synthetic id. The
// unique-name fallback claims a project of the same name only when its
// recorded path is unusable or contains path, so a caller that cannot report a
// real directory loses that fallback: the save opens its own project instead —
// one extra project per distinct id rather than one per save, since the
// fallback row is then found by id.
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

	// Before the store lock, and this is the only reason the answer is computed
	// here rather than inside the transaction: deciding it can cost a
	// `git config`, and everything below holds both the store-wide mutex and the
	// single write connection. A hung git there would stall every other reader
	// and writer on this Store to answer one save.
	saving := s.savingRepository(ctx, projectRef, repoRemote)

	s.mu.Lock()
	defer s.mu.Unlock()

	// OpenDB configures database/sql transactions as BEGIN IMMEDIATE. Holding
	// SQLite's write lock before the first lookup serializes repository-project
	// creation across Store handles and independent processes.
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
		return "", false, fmt.Errorf("project reference %q matches multiple projects", projectRef)
	}
	var existingRemote, matchedPath string
	norm := strings.ReplaceAll(projectRef, `\`, "/")
	// prefixMatch records that the project was found by a project's recorded
	// path containing the saving directory rather than by the caller's own
	// reference, which is the only difference between the two lookups below and
	// the reason one of them may not write repository identity. The exact
	// lookup matched id = ? OR path = ?, so its candidate is the project the
	// caller named; the prefix lookup matched a project whose path is an
	// ANCESTOR of the saving directory, and a directory inside a checkout is not
	// that checkout.
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
		return "", true, fmt.Errorf("project %q belongs to a different repository", id)
	}

	// The repository found in a directory is that directory's own, so the
	// question before writing one onto a project is whether it is the project's
	// repository at all. A checkout inside the project's directory is a
	// different repository — a submodule, a vendored clone, a worktree of
	// something else — and binding its remote, or merging the enclosing project
	// into the nested checkout's own, is how a save from ~/git/infra/vendor/lib
	// took other/lib away from ~/git/infra and then locked the real checkout out
	// of its own project at every path except exactly its root.
	//
	// The save still lands in the enclosing project, which is what the prefix
	// step decided and what a session standing in that directory expects.
	// Routing a save is not the same claim as identifying the repository, so
	// the refusal covers both writes below and only those; the exact-id lookup
	// above is untouched, because there the caller named the project and a save
	// made from inside it says nothing to contradict that.
	//
	// "Is the same repository" is deliberately not "is the root". A session is
	// usually started in a subdirectory, and `git config --get remote.origin.url`
	// walks up, so the remote found there is the enclosing checkout's own — and
	// for a v11-era row, which is a recorded path and no remote, that save is
	// the only thing that will ever identify the project. Keying the guard on
	// containment instead of on the repository leaves every pre-v11 project
	// unclaimed forever.
	//
	// A nested checkout does not open a project of its own as a result. The
	// prefix match has already answered, so resolution stops here, which is
	// deliberate: the save was told a directory, not a repository, and #612's
	// `ghost project bind` is the command that states which checkout is which
	// project — once the enclosing project records a remote, which is what
	// checkBindPathConflicts requires before it will bind a path inside one. A
	// save from the enclosing checkout's own root is what gives it one.
	//
	// Logged for the same reason the unique-name refusal is: the save landed
	// somewhere and the project that answered for it is deliberately left
	// unclaimed, which is a fact a user with a nested checkout will want.
	//
	// The evidence checks are the guard's own: the answer describes one project
	// at one path, read before the lock, and it is consulted only when the
	// transaction matched that same project at that same path. Both are needed.
	// The id alone is not enough, because a project re-pointed by
	// BindProjectPath between the two reads is the same row at a different
	// checkout, and the answer says nothing about the checkout it now records —
	// a remote identified at /a/infra does not describe /b/other. The window
	// between the two reads is not small: it contains s.mu.Lock() and a BEGIN
	// IMMEDIATE that can block on another process's write lock. Every other
	// case — no answer, an answer about another row, an answer about a
	// different path — is no evidence, and no evidence refuses, because this
	// guard exists to stop a wrong remote being written and a missing reading is
	// not permission to write one.
	//
	// The evidence keys are the pre-lock read's and describe a row that is not
	// necessarily the one matched here, so they are named as what they are
	// rather than as properties of recorded_path. The causes that reach this
	// line have to be tellable apart, because each needs a different fix: a
	// different repository found at the recorded path (evidence_project == id
	// and evidence_recorded_remote names another remote), nothing readable there
	// (same id, empty remote), and evidence that was never collected, or was
	// about another row or another path (evidence_project or evidence_path
	// empty or different, which is a project created or re-pointed between the
	// two reads). Reporting the pre-lock values next to the transaction's
	// recorded_path as if they described the same row would read as the second
	// cause in the first and third, sending an operator after a nested checkout
	// they do not have.
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

// savingRepository is the answer to "does the saving directory belong to the
// repository the enclosing project is a checkout of", which is the only thing
// that lets a save identify the project it was routed to.
//
// It is decided before the store's write lock is taken, because deciding it can
// cost a `git config` and everything inside that lock holds the single write
// connection too.
//
// It carries the row it was read from rather than a bare yes, because the
// transaction has to be able to tell an answer about the project it matched, at
// the path it matched, from no answer at all. The transaction is the authority:
// it consults this only when projectID is the project it just matched and
// recordedPath is the path that project records, and anything else — a project
// created or re-pointed between the two reads, or a read that failed — leaves
// the guard with no evidence, which refuses. A guard that treated a missing
// answer as permission would be no guard at all on exactly the paths where its
// input could not be collected.
type savingRepository struct {
	// projectID is the project this answer describes, "" when there is none.
	projectID string
	// recordedPath is the path the answer was decided from, which is not the
	// same question as projectID: BindProjectPath re-points a project in place,
	// so the same id can begin recording a different checkout between this read
	// and the transaction. Empty when there is no answer.
	recordedPath string
	// recordedRemote is what that project records for itself. A project that
	// records one is never decided by this answer — the transaction returns
	// before the guard — so it is read to skip the detection rather than to
	// decide anything, which is what keeps every save after the first free.
	recordedRemote string
	// speaksForProject reports whether the saving directory belongs to the
	// repository that project is a checkout of.
	speaksForProject bool
}

// savingRepository answers savingRepository for a save from projectRef, whose
// repository the caller has already detected as repoRemote.
//
// Three answers, and the first two are permissions that cost nothing. The
// project already records a remote, so when the transaction matches that same
// row — the ordinary case — it returns before the guard is ever reached, and
// nothing here can change what happens. (When the pre-lock read named a
// different row, that answer is not evidence for the row the transaction
// matched, whatever remote it recorded: the guard is reached and refuses on the
// projectID comparison alone — unless the matched row records a remote of its
// own, in which case the transaction has already returned or failed above.) The
// saving directory IS the project's recorded root: one directory, so there is no
// second repository to have meant, and still no detection spent.
//
// Only the third is a decision, and it is the one that costs: the saving
// directory is somewhere else under the recorded path, so the question is
// whether it belongs to the same repository. That is what an ordinary
// subdirectory of the checkout looks like — sessions start in src/api more often
// than at the root, and `git config` walks up — and what a nested checkout does
// not. The remote at the saving directory is the one the caller already detected
// and passed in, so the only detection here is the one at the recorded path: at
// most one additional `git config`, bounded by repo.detectTimeout.
//
// Anything else refuses, and so does a recorded path whose remote cannot be
// detected at all: that proves nothing, and it is the same direction as a
// subdirectory that turns out to be another repository. Both refusals leave the
// enclosing project unidentified while the save still lands in it.
// internal/memory never spawns git itself, so a store built without a detector —
// the default, and what every test but the ones that pin one gets — cannot tell a
// subdirectory from a submodule here, and guessing towards handing one
// repository's identity to another is the failure this exists to stop.
//
// The candidate is read on the pool rather than inside the transaction for the
// reason above, and the read mirrors the transaction's own prefix query — the same
// three path clauses, the same length filter, the same longest-path winner, and
// the same exclusion of _global — so the two name the same project. Two reads
// that both fail leave the guard without evidence, which is the safe direction to
// be wrong in: a project this save could have identified stays unclaimed and is
// identified by the next save, rather than being handed a repository that
// belongs to a checkout inside it.
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
	// The project already says which repository it is, and the transaction
	// returns on that before the guard: this answer cannot change what happens,
	// so it costs nothing to leave it at "speaks" and spend no detection.
	if winner.remote != "" || savingPathIsProjectRoot(winner.path, norm) {
		return saving
	}
	recordedRemote := s.inputRemote(winner.path)
	saving.recordedRemote = recordedRemote
	saving.speaksForProject = recordedRemote != "" && recordedRemote == repoRemote
	return saving
}

// savingPathIsProjectRoot reports whether the directory a save came from is the
// project's recorded checkout root rather than a directory inside it.
//
// The comparison is canonicalPath on both sides followed by exact equality,
// because canonicalPath is already this file's one definition of how a path is
// resolved: two spellings of one directory must agree, and a symlinked checkout
// must not read as a different place. pathsAgree is deliberately not used here:
// it accepts a directory inside the recorded path, which is exactly the
// containment this is not asking about — it answers "is the session standing in
// that project", where this answers "is the session standing at its root".
//
// A path that cannot be resolved is not the root. canonicalPath fails on a
// directory that does not exist, and a recorded path that no longer resolves
// cannot be shown to be the directory the save came from, so the conservative
// answer is the one that writes nothing.
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

// bindUniqueProjectNameRepoTx is the last resort of write-side resolution: a
// repository whose name matches exactly one project that has claimed no
// remote of its own may take that project as its own — unless that project
// says it lives somewhere the save did not come from, which is the guard
// below and the reason savingPath is a parameter.
//
// A refusal is returned as well as logged, and every kind logs: the caller is
// the one who can say what the split costs to the agent that made the save, and
// the operator is the one who needs a durable trace of a project that was not
// claimed — the write path is the only place the split is ever visible.
func (s *Store) bindUniqueProjectNameRepoTx(ctx context.Context, tx *sql.Tx, name, savingPath, repoRemote string) (string, *BindingRefusal, error) {
	candidates, matching, err := s.projectsNamedTx(ctx, tx, name, maxNamedCandidates)
	if err != nil {
		return "", nil, err
	}
	if matching == 0 {
		// No project carries the name, so there is nothing to refuse: the
		// fallback project the caller is about to create is a first save, and
		// reporting it as a refusal would make the notice unreadable.
		return "", nil, nil
	}
	if matching > 1 {
		// A name two projects share is evidence for neither. They are named in
		// the refusal because the caller's next question is which one to save
		// under, and a count alone cannot answer it.
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
		// A project that already speaks for another repository keeps it: the
		// remote on a project is the evidence that lets two checkouts of one
		// repository be one project, and overwriting it here would move that
		// project's identity onto a directory the evidence does not describe.
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
	// A name does not outweigh where a project says it lives. An unrelated
	// clone whose directory is called "infra" would otherwise claim the
	// project at ~/git/infra: it would take over that project's remote, route
	// this save into memories that are not its own, and lock the real
	// checkout out behind a remote conflict. So a usable recorded path must
	// agree with the directory the save came from; only a path that cannot be
	// compared — the id sentinel a name-as-id project records, or a relative
	// one — leaves the name to decide alone.
	//
	// A recorded path that no longer resolves counts as a disagreement, which
	// is what agreesWithSession already does on the read side: the same two
	// rules decide both directions, so a moved or deleted checkout keeps its
	// memories and stays addressable by a save from its own path instead of
	// being inherited by whatever directory took its name. The price is that a
	// second checkout of a project with no recorded remote keeps its own
	// project rather than inheriting the first one's memories — the trade
	// #610 asked for, and the reason the refusal is reported rather than
	// silent.
	if storedPathIsUsable(storedPath) && !pathsAgree(absoluteSessionPath(savingPath), storedPath) {
		// pathsAgree fails for a path that no longer resolves as well as for
		// one that resolves somewhere else, and the two need different fixes,
		// so the log says which it was.
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

// recordedCheckout returns the checkout a project records, or "" when the path
// column holds the id sentinel that a project created by name records instead
// of a location. The sentinel is a real value in that column and a useless one
// in a sentence about where a project lives, so a refusal reports the location
// or nothing.
func recordedCheckout(stored string) string {
	if !storedPathIsUsable(stored) {
		return ""
	}
	return stored
}

// nameCandidate is one project that records a given name, with the two fields
// the unique-name decision reads: the repository it already claims, if any, and
// the checkout it records.
type nameCandidate struct {
	id     string
	remote string
	path   string
}

// projectsNamedTx reads the projects recording name, at most limit of them and
// id-ordered, together with how many record it in total.
//
// The count decides the refusal and the rows decide the rest, so both come from
// one query: COUNT(*) OVER () is evaluated before LIMIT, which is what keeps
// the two consistent.
//
// What the limit bounds is what the caller carries — at most limit rows reach
// the Go slice, the log line and the notice. It does not bound the work SQLite
// does under the store mutex and the write lock: the window is computed over
// every matching row, and projects.name carries no index. The count has to be
// there either way, and reading the names to go with it costs nothing extra.
//
// limit is clamped to at least one because SQLite reads LIMIT 0 as "no rows":
// the query would then report that nothing records the name, and the decision
// that calls it would take its no-candidate path and open a project of its own
// and bind the repository to it — a silent duplicate of the same-named project
// rather than a decision to bind or refuse, which is the split this whole path
// exists to report. A negative limit means no limit at all, which costs only
// the bound the limit was passed to set.
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
		// No rows means no project records the name, and with them the window
		// count that would have said so.
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
		return fmt.Errorf("project %q repository changed concurrently", id)
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
			return "", fmt.Errorf("project %q belongs to a different repository", id)
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
				return "", fmt.Errorf("refusing to merge the _global project (old=%q new=%q)", id, ownerID)
			}
			if ownerRemote != "" && ownerRemote != repoRemote {
				return "", fmt.Errorf("project %q belongs to a different repository", ownerID)
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

// reconcileMergeRepoTx binds repoRemote to a merge target that does not record
// one yet. excluded names the other participants of the merge, which may
// legitimately hold the remote already inside this transaction — the incoming
// row is written before the merge runs and is deleted by it, so treating its own
// temporary claim as a conflicting holder would make every merge fail.
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
			return fmt.Errorf("project %q belongs to a different repository", existingID)
		}
		return nil
	}
	// Binding a remote the target does not have can collide with a project that
	// already records it. v15 makes repo_remote unique, so that write is refused
	// by the index — and a raw constraint failure names neither the project
	// already holding the identity nor the fact that two rows claim to be the
	// same repository. Name both, and point at the migration that merges them.
	args := []any{repoRemote, existingID}
	clause := ""
	for _, id := range excluded {
		clause += " AND id != ?"
		args = append(args, id)
	}
	var holder string
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM projects WHERE repo_remote = ? AND id != ?`+clause+` ORDER BY id`, args...).Scan(&holder); err == nil {
		return fmt.Errorf("%w: repository %q is already recorded on project %q, so it cannot also be bound to %q",
			ErrAmbiguousProject, repoRemote, holder, existingID)
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
		return fmt.Errorf("project %q repository changed concurrently", existingID)
	}
	return nil
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
			s.logger.Info("merged duplicate project", "old_id", mergedOld, "new_id", mergedNew)
		}
		return nil
	}

	// Repository identity: one remote means one project, whatever the local
	// checkout path happens to be. _global is excluded on both sides for the
	// same reason MergeProject refuses it — merging it would move or dump the
	// bucket that global injection reads from.
	if repoRemote != "" && id != "_global" {
		var incomingRemote string
		incomingErr := tx.QueryRowContext(ctx,
			`SELECT COALESCE(repo_remote, '') FROM projects WHERE id = ?`, id).Scan(&incomingRemote)
		if incomingErr != nil && incomingErr != sql.ErrNoRows {
			return fmt.Errorf("check project repository: %w", incomingErr)
		}
		if incomingRemote != "" && incomingRemote != repoRemote {
			return fmt.Errorf("project %q belongs to a different repository", id)
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

	// Check if another project already owns this path. If so, merge
	// any orphaned child records into the canonical project and skip
	// creating a duplicate. This self-heals duplicates caused by MCP
	// passing raw filesystem paths as project IDs.
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

	// Auto-merge: if this project has a real filesystem path, merge a
	// same-name project created by MCP only when that name has one candidate.
	// Two candidates are ambiguous, not an arbitrary LIMIT 1 choice.
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

// projectMergeStatements reassigns every child table from the outgoing project
// to the survivor. Both project_id columns that CASCADE — memory_snapshots and
// memory_history — must be in this list: a merge KEEPS the corpus and deletes
// only the projects row, so a table missing here loses every row it holds to
// that DELETE, and the memories it describes survive with their past erased.
//
// This list and the identical one inside the s.mergeProjectTx method are two
// copies of the same reassignment, left duplicated on main since #565 because
// the bind-recovery paths call the method and the merge/migration paths call the
// function. Both are updated here deliberately rather than deduplicated in a
// provenance change; collapsing them is a separate, mechanical cleanup.
var projectMergeStatements = []string{
	`UPDATE memories SET project_id = ? WHERE project_id = ?`,
	`UPDATE tasks SET project_id = ? WHERE project_id = ?`,
	`UPDATE decisions SET project_id = ? WHERE project_id = ?`,
	`UPDATE token_usage SET project_id = ? WHERE project_id = ?`,
	`UPDATE audit_log SET project_id = ? WHERE project_id = ?`,
	`UPDATE memory_snapshots SET project_id = ? WHERE project_id = ?`,
	`UPDATE memory_history SET project_id = ? WHERE project_id = ?`,
	`UPDATE supersede_checked SET project_id = ? WHERE project_id = ?`,
}

// mergeProjectTx folds oldID's rows into newID and deletes oldID.
//
// The _global refusal lives here, not in MergeProject, because this primitive
// has three callers and only one of them is the public wrapper. migrateV14
// discovers duplicate repository identities and picks an order for them, and
// ensureProjectLocked's unique-constraint recovery merges whichever row already
// holds the remote — and a v13 database can legitimately hold that remote on
// _global, because EnsureProjectWithRepo has always written the supplied remote
// for that ID. A guard only the wrapper applies would leave both of those paths
// able to move a project's entire corpus into the global bucket, or delete
// _global outright. The bucket global injection reads from is not a project, so
// every merge involving it is refused; callers that discover such a group return
// this error rather than choosing an order that happens to spare it.
func mergeProjectTx(ctx context.Context, tx *sql.Tx, oldID, newID string) error {
	if oldID == newID {
		return nil
	}
	// _global is the bucket global injection reads from, not a project: merging
	// into it would move a corpus into every session, and merging out of it
	// would delete it. The guard lives in the shared primitive so every caller
	// inherits it — the public wrapper, this merge, and migrateV15.
	if oldID == "_global" || newID == "_global" {
		return fmt.Errorf("%w: refusing to merge the _global project (old=%q new=%q)", ErrAmbiguousProject, oldID, newID)
	}
	for _, stmt := range projectMergeStatements {
		if _, err := tx.ExecContext(ctx, stmt, newID, oldID); err != nil {
			return fmt.Errorf("merge reassign: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ghost_state WHERE project_id = ?`, oldID); err != nil {
		return fmt.Errorf("merge delete ghost_state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, oldID); err != nil {
		return fmt.Errorf("merge delete project: %w", err)
	}
	return nil
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

	s.logger.Info("merged duplicate project", "old_id", oldID, "new_id", newID)
	return nil
}

func (s *Store) mergeProjectWithRepoTx(ctx context.Context, tx *sql.Tx, oldID, newID, repoRemote string) error {
	if oldID == newID {
		return nil
	}
	if oldID == "_global" || newID == "_global" {
		return fmt.Errorf("refusing to merge the _global project (old=%q new=%q)", oldID, newID)
	}
	// Validate the outgoing row's recorded identity first: it must not claim a
	// DIFFERENT repository, or the merge would silently move that project's
	// memories under an identity they were never verified against. Read-only —
	// the row is about to be deleted, so binding anything to it is wasted work.
	if err := validateMergeRepoTx(ctx, tx, oldID, repoRemote); err != nil {
		return err
	}
	// Then delete oldID, and only then bind the remote to the survivor. The
	// order matters: v15 makes repo_remote unique, and the incoming row is
	// written before the merge runs, so it already holds the identity this
	// function may have to give the survivor. Binding first would collide with
	// a row that is about to be deleted and fail the whole merge.
	if err := mergeProjectTx(ctx, tx, oldID, newID); err != nil {
		return err
	}
	return reconcileMergeRepoTx(ctx, tx, newID, repoRemote, oldID)
}

// validateMergeRepoTx refuses a merge whose outgoing project records a
// different repository from the one the merge is being performed under.
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
		return fmt.Errorf("project %q belongs to a different repository", oldID)
	}
	return nil
}

func (s *Store) mergeProjectTx(ctx context.Context, tx *sql.Tx, oldID, newID string) error {
	if oldID == newID {
		return nil
	}
	if oldID == "_global" || newID == "_global" {
		return fmt.Errorf("refusing to merge the _global project (old=%q new=%q)", oldID, newID)
	}

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
		// And the append-only history, for the same reason and with a sharper
		// edge: a merge KEEPS the memories, so a cascade here would leave live
		// rows whose recorded past is gone — `ghost history <id>` reporting a
		// memory that was never written. The history travels with the corpus it
		// describes, and the rows' own project_id is only how a reader filters.
		`UPDATE memory_history SET project_id = ? WHERE project_id = ?`,
		`UPDATE supersede_checked SET project_id = ? WHERE project_id = ?`,
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, oldID); err != nil {
		return fmt.Errorf("merge delete project: %w", err)
	}
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

	tx, _, err := s.beginWrite(ctx, "delete-project")
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

// ErrAmbiguousProject means an identifier matched more than one project and
// choosing one would silently send a caller's data to an arbitrary project.
// Callers that create projects must propagate this error instead of treating
// it as a miss.
var ErrAmbiguousProject = errors.New("project identifier is ambiguous")

// ResolveProject resolves an exact ID/name, a canonical path prefix, an exact
// repository remote, or one evidence-backed basename candidate. A genuine miss
// returns ("", "", nil); multiple surviving candidates return ErrAmbiguousProject.
// ResolveExactProjectID reports whether input is literally a project's id. It
// is deliberately narrower than ResolveProject: that function also accepts a
// path prefix, a repository remote and a basename fallback, and the write side
// must not use those — reclassifying a save that named an existing project by
// the repository the session happens to be standing in would move the memory to
// a different project than the caller addressed. Only the id is a claim the
// caller can make without evidence.
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
		return "", "", fmt.Errorf("%w: %q matches multiple projects", ErrAmbiguousProject, input)
	}

	// inputRemote can spawn git, so the session's remote is derived at most
	// once per resolve and only by the steps that read it. remoteOnce
	// memoizes the cost; it does not cache it across calls, because the
	// detector observes a directory that a save may have re-pointed since.
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
		pathMatches, qErr := s.pathCandidates(ctx, input, hasRepoRemote)
		if qErr != nil {
			return "", "", fmt.Errorf("resolve project by path: %w", qErr)
		}
		survivors := make([]basenameCandidate, 0, len(pathMatches))
		for _, candidate := range pathMatches {
			// The session's remote is read for one reason only: to reject a
			// candidate that asserts a different one. A candidate that
			// asserts none can neither agree nor disagree, so a prefix hit
			// against unclaimed projects settles without detection.
			if candidate.remote != "" {
				remote = remoteOnce()
			}
			if candidate.agreesWithSession(input, remote) {
				survivors = append(survivors, candidate)
			}
		}
		if len(survivors) > 0 {
			bestLength := pathRankLength(survivors[0].path)
			for _, candidate := range survivors[1:] {
				if pathRankLength(candidate.path) == bestLength {
					return "", "", fmt.Errorf("%w: %q has tied path-prefix matches", ErrAmbiguousProject, input)
				}
			}
			return survivors[0].id, survivors[0].name, nil
		}
	}
	// A name-shaped input spawns no process here, because inputRemote gates
	// detection on IsPathShaped. It is not a no-op, though: an input that
	// parses as a remote without carrying a separator ("github.com:owner")
	// normalizes to one, and the repository step below is reached for it.

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
	// it would disagree about which project it is.
	//
	// Detection is gated on the input being path-shaped rather than on
	// filepath.IsAbs, because IsAbs is false for a drive-relative Windows
	// path such as \work\ghost — skipping detection for exactly the shape a
	// second checkout arrives in is how one repository became two projects.
	// The raw input is normalised as a remote first, because a relative path
	// such as ../checkout would otherwise be mistaken for a host/path remote;
	// non-directory remote strings then fail the detector's os.Stat before it
	// starts Git, and normalise normally. The cost of the gate is bounded and
	// stated here rather than implied: an id never spawns a process, and a name
	// never does either unless it carries a separator, which this function
	// cannot tell apart from a path. A separator-shaped miss costs one
	// `git config` call, which returns nothing for anything that is not a
	// directory — once per resolve, whether or not the path step above already
	// spent it. inputRemote is that policy, shared with the write side.
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

	// A basename is accepted only after the same canonical-path and remote
	// checks as the path-prefix step; ambiguity is an error, not a miss.
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
		return "", "", fmt.Errorf("%w: %q matches multiple projects", ErrAmbiguousProject, input)
	}
	return survivors[0].id, survivors[0].name, nil
}

// basenameCandidate is one projects row that answers to a name.
type basenameCandidate struct{ id, name, path, remote string }

// hasRepoRemoteColumn reports whether the projects table has the optional
// repository-identity column. Read-only lifecycle consumers can legitimately
// open a pre-v11 database, so resolution must not require that column.
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

// basenameCandidates returns every project carrying this name. There is
// deliberately no LIMIT: evidence can reject a candidate, and truncating the
// set could hide a second candidate that agrees.
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

// pathCandidatesQuery selects the rows whose recorded path text could contain
// input, longest first. It is only a narrowing step, never proof of a project
// match: the caller still applies storedPathIsUsable, pathsAgree, and the
// remote checks.
//
// LENGTH(path) > 10 is part of the narrowing, not a rule about what a location
// is: it drops the id sentinel and paths too short to be worth a directory
// match, so a project whose recorded path is that short is invisible to path
// resolution. Project binding therefore has to apply the same filter, which is
// why the query is a named constant rather than a literal inside one function —
// changing the filter here changes what bind is allowed to record.
const pathCandidatesQuery = `SELECT id, name, path, %s FROM projects
		WHERE (path = ?
		   OR REPLACE(path, '\', '/') = ?
		   OR substr(?, 1, LENGTH(REPLACE(path, '\', '/')) + 1) = REPLACE(path, '\', '/') || '/')
		  AND LENGTH(path) > 10
		ORDER BY LENGTH(path) DESC`

// pathCandidates returns rows whose recorded path text could contain input.
// The caller still applies storedPathIsUsable, pathsAgree, and remote checks;
// this query is only a narrowing step, never proof of a project match.
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

// agreesWithSession applies the three rules in ResolveProject to one
// candidate. They are independent, so the order they run in carries no
// meaning; each is a reason to refuse. That is also why this is a predicate
// over candidates rather than a sequence of guards over one: ambiguity is
// decided on whoever survives, so a duplicate that the same evidence rejects
// cannot strand a session standing in the project it names.
func (c basenameCandidate) agreesWithSession(input, remote string) bool {
	// Rule 3. remote != c.remote is redundant — an exact repo_remote match
	// returned earlier — but it states the rule being enforced.
	if remote != "" && c.remote != "" && remote != c.remote {
		return false // both asserted an identity, and they disagree
	}
	if !IsPathShaped(input) {
		return true // a bare name reports no location to disagree with
	}
	// Rule 2. A stored path that is not an absolute location with something
	// below the root is not something a directory can be compared against:
	// the id sentinel recorded no location at all, a relative one would be
	// resolved against the process's working directory, and "/" contains
	// every absolute path. Refusing here also spares the two failed
	// EvalSymlinks calls pathsAgree would otherwise make.
	if !storedPathIsUsable(c.path) {
		return false
	}
	// Compare the absolute form, for the same reason the SQL prefilter above
	// uses it: a relative input otherwise never matches, because EvalSymlinks
	// leaves a relative path relative and every usable stored path is absolute.
	return pathsAgree(absoluteSessionPath(input), c.path)
}

// absoluteSessionPath normalizes a caller-reported session directory to the
// absolute, forward-slash form the rest of resolution compares in.
//
// A relative path is resolved against the process's working directory, which is
// where a caller reporting a relative directory is standing. Paths that are
// already absolute in either spelling are only separator-normalized: a Windows
// path arriving on a POSIX host is absolute, and prefixing the process's
// working directory onto it would both corrupt the string and hide a project
// that the prefilter had already matched. So the "is it absolute" test is
// deliberately spelling-agnostic rather than filepath.IsAbs, which is false for
// a backslash path on Linux and for a drive-relative path on Windows.
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

// isWindowsDrivePath reports whether p begins with a drive specifier and a
// separator, e.g. "C:/repo" — the absolute Windows form.
func isWindowsDrivePath(p string) bool {
	return len(p) >= 3 && p[1] == ':' && p[2] == '/' && isASCIILetter(p[0])
}

// isDriveRelative reports whether p begins with a bare drive specifier, e.g.
// "C:repo". It is relative to that drive's current directory, which the process
// cannot know, so it is left in its own spelling rather than resolved against
// this host's working directory — resolving it would claim a location the
// caller never reported.
func isDriveRelative(p string) bool {
	return len(p) >= 2 && p[1] == ':' && isASCIILetter(p[0])
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// IsPathShaped reports whether s looks like a filesystem path rather than a
// project name or a repository remote: it contains a path separator, or it
// begins with a drive-relative Windows prefix such as C:repo.
//
// This is the one definition of "the caller is reporting a location", shared
// by store resolution (the path steps and the basename evidence rules) and
// by mcpserver's repository detection, so a drive-relative Windows path
// cannot be treated as a name on one side of a boundary and as a path on the
// other.
//
// filepath.IsAbs is deliberately not this test: it is false for a
// drive-relative Windows path such as \work\ghost, which is exactly the
// shape a second checkout of a repository arrives in. The separator check alone
// was not enough either, because C:repo carries no separator at all — a
// single ASCII letter, a colon, and then text — so it read as a project name
// and both the resolver's path steps and the server's repository detection
// skipped it. A one-letter prefix is what makes this unambiguous: it is the
// whole of a Windows drive specifier, and a project name that begins with a
// single letter and a colon is not a shape anything else produces.
func IsPathShaped(s string) bool {
	if strings.ContainsAny(s, `/\`) {
		return true
	}
	return isDriveRelative(s)
}

// storedPathIsUsable reports whether a recorded path is a location a session
// directory can be compared against: absolute, in either separator spelling,
// with at least one segment below the root.
//
// Three shapes are refused, each for its own reason:
//   - the id sentinel (path == id) and any relative path: nothing was
//     recorded that a directory could contradict, and EvalSymlinks would
//     resolve a relative path against the PROCESS working directory, so
//     agreement would depend on where the caller happens to run rather than
//     where the session is standing;
//   - a bare root ("/" or "C:/"): it contains every absolute path on the
//     machine, so a project recorded there would claim any session whose
//     directory shares its name.
func storedPathIsUsable(stored string) bool {
	norm := strings.ReplaceAll(stored, `\`, "/")
	// Drop a Windows drive prefix so the root test is the same one on both
	// platforms: "C:/x" and "/x" are the same shape.
	if isWindowsDrivePath(norm) {
		norm = norm[2:]
	}
	norm = pathpkg.Clean(norm)
	return strings.HasPrefix(norm, "/") && strings.Trim(norm, "/") != ""
}

// canonicalPath resolves one path to its canonical, forward-slash form, or
// reports why it cannot: a path that does not exist, or cannot be resolved,
// stays unresolved. Callers treat that as a failure to agree rather than as a
// match.
func canonicalPath(p string) (string, error) {
	normalized := strings.ReplaceAll(p, `\`, "/")
	resolved, err := filepath.EvalSymlinks(filepath.FromSlash(normalized))
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(filepath.ToSlash(resolved), `\`, "/"), nil
}

// pathsAgree reports whether input and stored are the same place: input
// resolves to stored, or to a directory inside it. Both paths are resolved
// before comparison, so dot segments and symlink escapes cannot pass a textual
// prefix check and two spellings of one directory (Windows 8.3 short names
// against the long form, a symlink in the middle, a trailing separator) do
// agree. Comparison is separator-agnostic — Windows stores backslashes and the
// path step above already normalizes them, so a literal prefix test using "/"
// can never match a native Windows path. A path that does not exist or cannot
// be resolved stays unresolved and fails, so an unreadable directory never
// becomes evidence that it is the project it claims to be.
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

// samePath is exact equality or a directory-prefix match on a segment
// boundary, so /x/infra does not accept /x/infra-other.
func samePath(a, b string) bool {
	b = strings.TrimSuffix(b, "/")
	return a == b || strings.HasPrefix(a, b+"/")
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

// CreateFromCorpus inserts a memory from a third-party benchmark dataset,
// without the credential guard Create applies.
//
// It exists because a public eval corpus contains real credentials and a
// benchmark that refuses to load one cannot run. Measured on longmemeval-s
// (500 records, ~25k conversation turns) at the time this was written: three
// distinct lines fire, and all three are genuine leaked credentials — a
// `dckr_pat_` Docker Hub token, a `gho_` GitHub OAuth token, and a 40-character
// hex SparkPost key. The detector is right about all three; the harness still
// has to ingest them, because the measurement is about retrieval over the
// corpus as published.
//
// That is why this is a named function rather than a flag. The claim justifying
// it is deliberately the narrow one, stated exactly: the row lands in a scratch
// database that dies with the run, and it is never injected into a session's
// context, never mirrored to the Obsidian vault, and never quoted into a
// reflect, resolve or supersede prompt. Those are the exposures the guard
// exists for.
//
// "Never embedded" is NOT part of the claim. The two seeders that run a
// non-fts condition embed every turn they ingest and send it to Ollama. The
// narrow claim is the one that holds, and it is the one quoted, because the
// wide one is what a future author would cite to widen this carve-out to a
// bench run against a real store — where the vault and reflect exposures are
// real.
//
// Nothing in a normal session may reach this: it is not on
// provider.MemoryStore, it is not in the MCP surface, and its only callers are
// the three dataset seeders under bench/.
//
// It is also the second false positive this round found, in the opposite
// direction from the first: the earlier claim that guarding Create cost nothing
// was true only of the corpora in this repository, and a downloaded corpus
// proved it wrong on the first CI run.
func (s *Store) CreateFromCorpus(ctx context.Context, projectID string, m Memory) (string, error) {
	return s.insertMemory(ctx, projectID, m, insertOptions{})
}

// secretTagFields is the guard's view of a memory's tags.
//
// Tags are guarded, and the reason is worth keeping in one place because an
// earlier version of secret_guard.go claimed they were not worth guarding: they
// are marshalled into the row ghost_memory_search returns, and
// BuildReflectionPrompt writes them into the prompt sent to a CLI harness — so a
// token pasted as a tag is embedded, returned and quoted into a model exactly as
// a token in the body would be. validateTags permits ten tags of 64 characters,
// which is more than room for every provider token format.
// secretContentAndTags is the guard's field list for a memory write: the body and
// every tag, in that order, so the refusal names whichever was contaminated.
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

// secretSourceRefField checks a write-time source reference, or nothing when the
// caller stated none.
//
// A reference is a path, a commit, a URL or a ticket, and it is stored on the same
// row and rendered on the same line as the content — the shared item line prints
// it as a labelled field on every listing, and an exported artifact carries it to
// whoever imports one. A caller pasting a token into it would be storing and
// replaying a credential by exactly the route the guard exists to close, and the
// field is the one place on the line where a credential looks least like one:
// a long unbroken string after "source_ref=" reads as a URL.
func secretSourceRefField(sourceRef string) []secretField {
	if sourceRef == "" {
		return nil
	}
	return []secretField{{"source_ref", sourceRef}}
}

// Create inserts a new memory and returns its ID. The insert and the history
// row that records it share one transaction, so a memory cannot exist without
// its own first entry in its history.
func (s *Store) Create(ctx context.Context, projectID string, m Memory) (string, error) {
	if err := checkMemoryWrite(m); err != nil {
		return "", err
	}
	return s.insertMemory(ctx, projectID, m, insertOptions{recordVerification: true})
}

// checkMemoryWrite is the whole of what a memory write is checked for before it
// reaches SQL: the credential guard over the body, the tags and the source
// reference, and the two length bounds. It is a named function rather than the
// body of Create because CreateWithID is the same write with a different id, and
// two copies of a guard eventually disagree about which fields it covers — the
// failure being a check that one writer applies and the other does not.
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
	return nil
}

// CreateWithID is Create under a caller-chosen id, and it exists for one caller:
// the benchmark, which seeds a store from a committed corpus and needs the
// store's contents to be a function of that corpus rather than of the id column's
// `hex(randomblob(16))` default.
//
// Why the benchmark needs it: the store breaks tied fused scores by memory id
// (fuseCandidatePool) precisely so production results are stable, and a bench
// corpus that drew its ids at random re-drew that tie-break on every run. A grid
// point weighting its two legs EQUALLY collides often enough that its published
// interval moved between runs of one binary (#708). Nothing about the tie-break
// is wrong; the benchmark was discarding the input it reads. So the ids become a
// function of the corpus item and the production order is untouched.
//
// The write is otherwise Create's: same transaction, same history and evidence
// rows, same recordVerification, and the same checks — an id a caller chose is
// not a licence to write a body Create would have refused. An empty id is
// refused rather than falling back to the column default, because a caller that
// asked for a named row and silently got a random one would never find out, and
// reproducibility is the only reason to be here.
//
// Not on provider.MemoryStore and not in the MCP surface: nothing a session can
// reach names its own id. A row whose verified_at is a THIRD-PARTY DATASET's claim
// rather than a check made through this store is not this function's business —
// see CreateWithIDFromCorpus, which is the same write with that one bit off.
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
// the credential guard and the whole write are CreateWithID's, and the one bit that
// differs is recordVerification — so a verified_at the DATASET states is stored
// verbatim as the column's value and no `verified` record is appended for it.
//
// It exists because those are two different claims and only one of them is an
// event. `insertOptions.recordVerification`'s comment carries the argument in full
// — a record's stamp is the store's clock by design, so appending one for a
// dataset's own claim says "this was checked now" about a check the dataset may
// date to years ago, which is the inversion the store-clock rule exists to
// prevent. CreateFromCorpus has always been the route that declines it; this is
// that same decline for a caller which also has to name the row.
//
// The credential guard is the deliberate difference from CreateFromCorpus, and it
// is the safer direction to be surprised in: THIS function still refuses a body
// carrying a credential, and only the id and the record bit are the corpus route's.
// The corpora that need the wider carve-out are the downloaded public eval sets
// under bench/, and they keep calling CreateFromCorpus. The benchmark's own corpora
// are committed fixtures a review has read, seeded into a scratch store that dies
// with the run — so it can have the rule without giving up the guard.
//
// Not on provider.MemoryStore and not in the MCP surface, for the same reason
// CreateWithID is not: a corpus row is not something a session saves.
func (s *Store) CreateWithIDFromCorpus(ctx context.Context, projectID, id string, m Memory) (string, error) {
	if id == "" {
		return "", errors.New("create memory: no id given; CreateWithIDFromCorpus stores the row under the id it is given, and an empty one would be a row the caller cannot find again")
	}
	if err := checkMemoryWrite(m); err != nil {
		return "", err
	}
	return s.insertMemory(ctx, projectID, m, insertOptions{id: id})
}

// insertOptions is the one bit insertMemory cannot infer from the Memory it is
// given, so the caller has to say it.
//
// It is a parameter rather than a field on Memory because Memory is what a reader
// gets back: a stored verified_at is true of the row whichever route wrote it, and
// a flag on the read shape would suggest the two rows differ when the only
// difference is who observed them.
type insertOptions struct {
	// recordVerification appends a `verified` evidence record when m carries a
	// VerifiedAt. Every writer that stores a verified_at from a live call sets it.
	//
	// CreateFromCorpus does not, and the reason is the stamp rather than the
	// claim. A corpus row's verified_at is DATA ABOUT THE DATASET — a third-party
	// benchmark asserting when its own rows were checked, possibly years ago — and
	// the record's verified_at is the STORE's clock by design, because a verifier
	// that could date its own check could date it before the thing it checked.
	// Appending one therefore says "this fact was checked now", which is the exact
	// inversion that rule exists to prevent, reached through a path the rule was
	// never stated on. The column keeps the dataset's value verbatim: losing the
	// dataset's own data would be the opposite defect, and appendVerificationIf
	// StatedTx's comment is what says so.
	//
	// Nothing can read the fabricated record today — this path's own contract says
	// the row is never injected, mirrored or quoted, and it lands in a scratch
	// database that dies with the run — which is exactly why the invariant is
	// stated and pinned rather than left to the accident that no corpus sets the
	// field yet. TestCreateFromCorpusRecordsNoVerification is that pin, and
	// TestCreateStillRecordsAVerificationBesideTheCorpusOptOut is what keeps the
	// opt-out from being implemented by dropping the append wholesale.
	recordVerification bool

	// id is the row's primary key, or "" to take the column's own default
	// (`hex(randomblob(16))`). Create leaves it empty, which is why the INSERT
	// spells both cases in one statement — a second INSERT that has to stay in
	// step with the schema is a worse failure mode than a COALESCE, and this is
	// the same expression importEvidenceTx already uses for the same reason.
	//
	// Only CreateWithID sets it, and only the benchmark calls that: every other
	// writer draws its id, so a row's identity is still nobody's choice but the
	// column's.
	id string
}

// insertMemory is Create's statement, with the credential guard and nothing
// else above it. Split out so CreateFromCorpus is the same write rather than a
// second copy of an INSERT that has to stay in step with the schema — a worse
// failure mode than a less obvious call graph.
func (s *Store) insertMemory(ctx context.Context, projectID string, m Memory, opts insertOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tags, _ := json.Marshal(m.Tags)
	// A Memory carries its own tier, and this is the writer that stores the
	// struct rather than a list of arguments — so a caller that set Retention
	// and got `project` back would have been silently overruled. Refused before
	// the transaction for the same reason Upsert refuses: a bad value costs
	// nothing.
	retention, err := NormalizeRetention(m.Retention)
	if err != nil {
		return "", err
	}
	expires := sessionExpiry(retention, time.Now())
	if retention == RetentionSession && m.ExpiresAt != nil {
		// A session row's expiry may be stated rather than derived, and ONLY a
		// session row's: the gate is the point. Without it this would put an expiry
		// on a durable row, which is a claim about when the user stops wanting a
		// memory that no caller ever made -- and one that prune would then ignore,
		// so the column would lie and the prune would not even be able to say so.
		// A durable row's NULL is the unprunable direction, and it stays.
		//
		// The callers that exist are the corpus seeders, which replay a row they
		// already hold, and a test restoring a shape. `Store.RestoreSnapshot` is
		// NOT one of them: it writes through its own INSERT ... SELECT, which does
		// not name the column, so a restored row takes the DEFAULT — the same
		// "the change log holds no tier" answer as everything else in that path.
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
	// The evidence record shares this transaction for the same reason, and its
	// kind is observed because that is what a save is: something reported this
	// fact and the store kept what it reported about itself.
	if err := appendEvidenceTx(ctx, tx, id, evidenceObserved, provenanceFromMemory(m), false); err != nil {
		return "", err
	}
	// And a second record when the same call said the fact was CHECKED. A column
	// that says verified_at and a table that says nothing would be two answers to
	// one question, and EvidenceCounts.Verified is the one a reader is meant to
	// trust. appendVerificationIfStatedTx carries the "stated in THIS call" rule;
	// here a fresh INSERT has no stored value to keep, so m.VerifiedAt is exactly
	// the caller's statement.
	//
	// Gated on opts.recordVerification as well, and the gate is the only reason
	// this is not simply "append whenever m.VerifiedAt is set": the record's stamp
	// is the store's clock, so on the corpus route it would assert that a dataset's
	// own check happened now. See insertOptions.recordVerification, which is where
	// that argument is written down in full.
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

// MaxSourceRefLen is the byte cap on a write-time source reference, at the store
// rather than at the tool boundary.
//
// The column is caller-supplied text that the shared item line prints as a labelled
// field on every listing and every browsing surface, so an unbounded one is a
// megabyte echoed into every answer that touches the row — and that is a property
// of the column, not of the MCP tools — and the tools are the only writers an MCP
// caller reaches, so the cap lives here and the refusal surfaces from the store.
// It is not repeated in the tool argument resolver: one cap, one message, and a
// second copy could disagree about the size without anything noticing.
//
// A reference is a path, a commit, a URL or a ticket — a few hundred bytes at the
// outside — and it is NOT clamped the way content is: ClampContent cuts prose and
// leaves a marker in its place, while a truncated path or URL is a different
// reference, and a wrong one that looks right is worse than an error.
//
// THREE writers of the column do not reach the bound, and all three are the
// deliberate exclusions this codebase already draws for MaxContentLen and the
// credential guard. `RestoreSnapshot` copies the column in SQL from the snapshot
// table and would need the check per restored row; `CreateFromCorpus` reaches
// insertMemory's INSERT directly rather than through Create; and
// `ReplaceNonManual`, since #677, inherits the replaced row's provenance onto the
// row a rewrite becomes — a COPY of a value this database already bounded, not new
// caller text, and filtering there deletes the stored row rather than refusing one.
// `assemble.SourceRefLabel` bounds what a listing PRINTS for exactly those three,
// since the renderer cannot assume its input came from a writer that enforces the
// cap. A new statement that writes the column has to call boundedSourceRef — the
// bound is one call each writer remembers, not something the schema enforces.
const MaxSourceRefLen = 512

// MaxAgentLen is the byte cap on a write-time agent, for the same reason and by
// the same route as MaxSourceRefLen: the shared item line prints `agent=` beside
// `source_ref=` on every listing, so a value with no bound is a value echoed into
// every answer that touches the row.
//
// On the harness path the column holds one of four tokens, so the cap can only
// bite on an artifact — which is the point. A portable artifact's `agent` is
// whatever wrote the export, and internal/portable accepts a 1 MiB record, so
// without a bound an export could plant close to a megabyte of it in a column the
// renderer had started echoing in this change. The same three writers are exempt —
// RestoreSnapshot, CreateFromCorpus, and ReplaceNonManual, which inherits the
// replaced row's agent onto the row a rewrite becomes — and assemble.AgentLabel
// bounds the display.
const MaxAgentLen = 128

// StoredStampLayout is the layout a writer stores a validity stamp in: SQLite's
// own datetime() shape, in UTC.
//
// The columns are unconstrained text, so nothing in the schema enforces this and
// a row may legitimately hold a date alone — a portable artifact, a hand edit,
// or the short form SQLite's date() produces. Writers normalize what they accept
// to this one layout so their own rows need a single case to read, and every
// reader still has to try both. It lives here because the column is the storage
// layer's and the layout is part of its contract, not a detail of any one
// caller: a writer that stored one shape and a renderer that printed another
// would show a caller a moment Ghost never recorded.
const StoredStampLayout = "2006-01-02 15:04:05"

// DateStampLayout is the whole-day form, the other shape SQLite's date() produces.
const DateStampLayout = "2006-01-02"

// StampLayouts is every layout a reader of a validity column accepts, in the order
// to try them: what Ghost's own writers store first, then the whole-day form a
// portable artifact, a hand edit or a date() call can leave behind.
//
// One list, owned here, because the column is the storage layer's. A writer
// validating a value it did not store and a reader interpreting one have to agree
// on what is readable, and two private copies of the layout set are two answers to
// that question free to drift: a writer that accepted only the shape it writes
// would wave through a contradiction on a date-only row, and a reader that
// accepted only its own would read a real claim as no claim at all.
var StampLayouts = []string{StoredStampLayout, DateStampLayout}

// ParseStamp reads a stored validity stamp over every layout StampLayouts names,
// reporting whether it was readable. The store owns it because a writer judging a
// value it did not store and a reader interpreting one have to reach the same
// answer: a check that understood only the shape Ghost writes would wave through
// a contradiction on every imported, restored or hand-edited row, and a reader
// that understood only its own would read a real claim as no claim at all.
func ParseStamp(s string) (time.Time, bool) {
	for _, layout := range StampLayouts {
		if at, err := time.Parse(layout, s); err == nil {
			return at, true
		}
	}
	return time.Time{}, false
}

// CheckWindowOrder refuses a window that does not end after it starts, judging the
// effective pair: the boundaries the caller supplied, with a missing half taken
// from what the row already holds.
//
// Both halves matter because a save carries a window or nothing, while an edit
// carries one boundary and inherits the other. Only a contradiction is refused: a
// window that has already closed, or one that has not opened, is a legitimate
// thing to record — it is what a caller writes to say a claim is over, or not yet
// in force.
//
// A half no layout reads is not a claim, and the reader treats it as unset, so
// there is nothing here to contradict. Refusing on one would make an unrelated
// edit fail on a row whose own stored value this build cannot interpret.
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
// memory became true, when it stops being true, and when someone last checked.
//
// Every field is a pointer and every nil means "no claim was made", which is
// distinct from a claim about the zero moment — the same reason the columns
// themselves are nullable. A caller that states nothing is recorded as NULL and
// read back as unset, so a store can tell the difference between "nobody has
// said" and "said to be valid forever".
//
// It is separate from Provenance rather than a fifth field on it because the two
// answer different questions: Provenance is who wrote this and how much they
// were trusted, Validity is when it stopped being true. A memory can be
// certainly true right now and expire next month, and collapsing the two would
// make the next check of a fact look like a reason to doubt the agent that
// stated it.
type Validity struct {
	// ValidFrom is when the memory became true. A time after now makes the row
	// unreadable until then, which is a real claim about a scheduled change and
	// not a data error.
	ValidFrom *string
	// ValidUntil is when the memory stops being true. A time at or before now
	// withholds the row from ranked retrieval; see internal/assemble stage 2.
	ValidUntil *string
	// VerifiedAt is when a human or agent last checked the claim. In v1 it is a
	// flag: it marks a row as re-checked and does not by itself decide anything.
	VerifiedAt *string
}

// IsZero reports whether the claim is entirely absent, so a write can leave the
// three columns alone without the caller having to know which of them exist. It
// is the one place that question is answered, because the callers that need it —
// a tool deciding whether a request stated anything at all — must not each hold
// their own copy of it and disagree about what "stated nothing" means.
func (v Validity) IsZero() bool {
	return v.ValidFrom == nil && v.ValidUntil == nil && v.VerifiedAt == nil
}

// Provenance is the optional write-time context attached to a single save.
// Every field is optional and its zero value means "not known": passing no
// Provenance at all records NULL across the board rather than a guess.
//
// SessionID is never auto-populated from process state. It names a session the
// host controls, so only a caller that has one may supply it: the MCP tools
// pass the transport's session id, which a stdio host does not have, and every
// other path leaves it empty. A store-invented value would be fabricated
// provenance, which is the exact thing these columns exist to avoid.
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

// nullStringPtr turns a nullable column into the pointer shape a Validity field
// carries, and NULL into the nil that means "no claim was made".
func nullStringPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	return &s.String
}

// readableStampPtr is nullStringPtr for the places that must not treat a value
// Ghost cannot read as a claim.
//
// nullStringPtr maps only SQL NULL to nil, so a stored empty string — or any text
// no layout parses — arrives as a non-nil boundary. That is the right answer when
// the caller is reporting what is in the column, and the wrong one when a value is
// being judged for whether it says anything: CheckWindowOrder deliberately returns
// nil for a pair it cannot read, so an unreadable pair passes the check meant to
// qualify it and looks like a real assertion. So a boundary counts as stated only
// when ParseStamp reads it, which is the same rule nullIfEmptyPtr applies to the
// empty moment at the writers that bind the triple.
//
// Such a value is reachable rather than hypothetical: Store.Create stores a stamp
// verbatim, and ImportMemory and RestoreSnapshot write the column with no check at
// all, so a database can hold a window recorded in prose.
func readableStampPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	// A zero time is not a stated window here, even though ParseStamp reports it
	// as readable: time.Parse succeeds on "0001-01-01 00:00:00" and yields the
	// zero time. The reason is NOT that the readers disagree — it used to be, and
	// that was the original justification, but #583 removed it: assemble's
	// parseStampPtr now reads through ParseStamp, so readValidity, ValidityStateOf
	// and ValidityLabel all treat a zero-instant stamp as a real boundary, and a
	// row carrying one is read as a complete window. Citing that here would leave
	// one value with two documented answers, which is what this branch exists to
	// end.
	//
	// The reason that survives is what this function is FOR. Its callers are
	// ReplaceNonManual's, which compose the SUCCESSOR's window out of the sources'
	// windows, and a successor is a different memory written later. A source whose
	// lower bound is the zero instant almost always carries it as a "no lower
	// bound" artefact — a hand-edited artifact, a restored snapshot, a row written
	// by something that filled the column with an epoch — and inheriting it would
	// stamp the successor with a valid_from of year 1: a claim that it was true
	// from a moment before the memory that says so existed. So this reader treats
	// the zero instant as no lower bound stated, and says which value it is
	// reading.
	//
	// That is a deliberate DIVERGENCE from the state rule, not agreement with it.
	// The difference is between two questions — "is this row retired?" and "which
	// bounds does the memory replacing it inherit?" — and the zero instant answers
	// them differently. Anyone narrowing either rule has to reckon with this one;
	// TestReplaceNonManualDoesNotTreatTheZeroInstantAsAStatedWindow pins it.
	if at, ok := ParseStamp(s.String); !ok || at.IsZero() {
		return nil
	}
	return &s.String
}

// nullIfEmptyPtr maps a validity stamp to SQL NULL, and an empty string to NULL
// as well, so no writer can record a claim about the empty moment. A nil pointer
// is the caller's statement that no claim was made; an empty string would be a
// claim with no readable value, which stage 2 reports as validity_unparseable —
// a permanently confusing row to leave in a store on the caller's behalf.
func nullIfEmptyPtr(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

// boundedAgent applies MaxAgentLen, refusing rather than truncating, and maps an
// empty agent to NULL. It refuses because the column's own values are short: a
// value past the cap is not a harness name, so there is no honest prefix of it to
// keep.
func boundedAgent(agent string) (any, error) {
	if agent == "" {
		return nil, nil
	}
	if len(agent) > MaxAgentLen {
		return nil, fmt.Errorf("agent must be at most %d bytes, got %d — it names the harness that wrote a row, not a document", MaxAgentLen, len(agent))
	}
	return agent, nil
}

// boundedSourceRef applies MaxSourceRefLen, refusing rather than truncating, and
// maps an empty reference to NULL on the way through.
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
// UpsertOptions carries the optional facts about one save. Provenance, scope
// and validity are orthogonal — where a memory came from, where it applies, and
// when it was true — so they are separate fields rather than one growing
// parameter list, and a zero UpsertOptions is exactly what plain Upsert passes.
type UpsertOptions struct {
	Provenance Provenance
	Scope      map[string]string
	// Validity is the temporal claim this save makes, if any. A zero Validity
	// writes NULL to all three columns, which is the record for a memory whose
	// author stated no currency — the state of every memory written before the
	// columns existed, and the state the tools write when the caller says
	// nothing.
	Validity Validity

	// FoldOnly changes what happens when an equivalent row is found (the same
	// text after foldOnlyEquivalent's normalization): the existing row is
	// strengthened and returned, and the incoming text is NOT inserted as a
	// new row. Any other text is inserted as its own row.
	//
	// The default fold keeps the new wording, and that is right for a save —
	// the caller explicitly asked for that text to be stored. It is wrong for
	// promotion into _global, where the same fact arrives as a fresh
	// paraphrase from every project's reflection: _global accumulated 68
	// redundant rows in 19 clusters this way, one of them nine paraphrases of
	// a single gouroboros fact, while every project scope had none (issue
	// #544). Those rows cost window slots and resolve and supersede work,
	// and 93 active duplicate links were needed just to sink them again.
	FoldOnly bool

	// Pin exempts the memory this save is about from consolidation, in the same
	// call that stores it. It is the opt-out the save path otherwise lacks:
	// nothing an agent writes carries a source reflection excludes (seeds are
	// 'builtin', agent saves 'mcp', reflection writes 'reflection'), so until
	// this option the only way to protect a memory from `ghost reflect` was a
	// SECOND call — ghost_memory_pin — and every window between the two left
	// the memory consolidatable (issue #549).
	//
	// A fold pins BOTH rows: the copy inserted here, and the existing row the
	// caller's text was merged into. The existing row is the one the corpus
	// already holds and the one a later consolidation is most likely to absorb,
	// so pinning only the new row would leave the fact the caller asked to
	// protect fully consolidatable; pinning only the existing row would leave
	// the wording that was just stored exempt from nothing. FoldOnly stores no
	// row of its own, so there the target is the only thing the request can
	// mean — a promotion that reported success without pinning would have
	// stored nothing and protected nothing.
	Pin bool

	// Retention is the tier this save asks for: session, project or persistent.
	// Empty is project — the tier a store has always had, and the only reading of
	// "the caller said nothing" that costs nothing when it is wrong.
	//
	// A fold RAISES the surviving row's tier and never lowers it (see
	// raiseRetentionTx), so this option reaches the row a later consolidation
	// would absorb rather than only the copy this call inserts. Lowering would
	// be the dangerous direction: a session-scoped save that demoted another
	// memory's durable tier to its own would be scheduling that memory for
	// deletion on the strength of a near-duplicate.
	Retention string
}

// mergeRetention resolves a fold's two tiers into the one the surviving row
// keeps, by taking the LONGER life. It is a pure function of the two values so
// the rule is testable without a store, and it is the whole of the asymmetry:
// raising is allowed because a protection the caller asked for and did not get
// is a false report, while lowering is not, because the tier a row already has
// is a claim about it that a near-duplicate has no standing to withdraw.
//
// A value that is not one of the three is ranked as project — the value a reader
// resolves an unset column to — so an unexpected string can neither be escalated
// to persistent by a fold nor demote anything.
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

// raiseRetentionTx raises one row's tier to protect, never lowers it, and
// reports the tier the row ended up with so the save result can name the row
// that carries the caller's protection.
//
// expires_at is cleared when the row leaves the session tier: a durable row with
// an expiry is a claim about when the user stops wanting it that nobody made,
// and the value a session row carried described the life of a tier it no longer
// has. It is left alone when the row stays session, because the expiry belongs
// to that row's own save rather than to the save that happened to fold into it.
//
// A row already at the merged tier is not written at all, so a fold that changes
// nothing about protection leaves the row's other columns alone.
func raiseRetentionTx(ctx context.Context, tx *sql.Tx, id, incoming string) (string, error) {
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

// sessionExpiry is the expires_at a row of that tier is stored with: derived
// for a session save, absent for every other tier.
//
// One function because both tiers' shapes are the same decision — "is this row
// scheduled to stop being wanted" — and a writer that inlined the conditional
// would leave a second place to get the durable case wrong. The derivation is
// the ONLY source of an expiry: a caller cannot state one on a save, so there is
// no way for a save to schedule the memory it just wrote for deletion.
func sessionExpiry(tier string, now time.Time) any {
	if tier != RetentionSession {
		return nil
	}
	return now.UTC().Add(SessionTTL).Format("2006-01-02 15:04:05")
}

// boolToInt is SQLite's boolean: the pinned column is an INTEGER and every
// writer supplies it explicitly rather than leaving the DEFAULT to apply, so
// a false must be a 0 and not a missing value.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// pinMemoryTx marks one row pinned inside the caller's write transaction. It is
// a no-op for a false pin or an empty id, so the fold paths can call it
// unconditionally. updated_at is deliberately NOT touched: this is part of a
// save that has already decided what it is writing, and the row's age and
// timestamp are load-bearing for decay and for the --skip-unchanged fingerprint
// (see reusePreservesAge) — the same reason the strengthen UPDATE leaves them
// alone.
//
// The parameter is *sql.Tx rather than the sqlExecutor interface that covers
// both it and *sql.DB, because a write that is only ever legal inside a
// transaction should say so in its type. A wider parameter would let a future
// caller hand it a handle, and the write would then run in autocommit outside
// the check that refuses a store a newer Ghost owns (#746).
func (s *Store) pinMemoryTx(ctx context.Context, tx *sql.Tx, id string, pin bool) error {
	if !pin || id == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memories SET pinned = 1 WHERE id = ?`, id); err != nil {
		return fmt.Errorf("pin memory: %w", err)
	}
	return nil
}

// foldTargetStillLive re-verifies, inside the write transaction, that the row a
// probe selected is still something worth folding into: still in this project,
// still unresolved, and not the target of an active 'supersedes' edge.
//
// A probe result is a name, not a claim. Between the probe and the write the row
// can be resolved by a concurrent pass, superseded, or deleted outright, and
// folding into a dead row is the failure FoldOnly makes unrecoverable: it
// strengthens a record injection already dropped and then returns without
// storing the incoming wording, so the memory exists nowhere.
//
// A 'supersedes' edge only makes the row dead if it is a verdict the store may
// act on, and an edge whose endpoints name the same key with different values is
// not: a development row never replaced a production one. Reading such an edge
// as a verdict blocks every fold of that row — so each re-save of the exact text
// inserts a second copy instead of folding, and a fact that should occupy one
// row fills the store with restatements. The exemption is asked in the same
// statement that reads the row, so the answer arrives with the row instead of in
// a second read that could disagree with it: it is a property of the edge this
// statement already reads, not of the row. (Unlike Upsert's two dedup probes,
// there is no candidate window here to protect — this statement names one row by
// id — and a Go-side check would have been possible; see scopesConflictSQL.) The
// edge is not deleted; it is merely not a reason to refuse a fold.
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

// sqlExecutor is the common read/write surface of *sql.DB and *sql.Tx. It
// lets the reflection apply path reuse Upsert's duplicate/link semantics inside
// its existing transaction instead of opening a second transaction.
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

// UpsertWithOptions is Upsert plus provenance and/or scope.
func (s *Store) UpsertWithOptions(ctx context.Context, projectID, category, content, source string, importance float32, tags []string, opts UpsertOptions) (id string, duplicateOf string, score float64, err error) {
	// Before the lock, the transaction, and the FTS probe: a refused value
	// must cost nothing and write nothing. When the upsert arrives inside a
	// caller's transaction this error rolls that caller's work back too, which
	// is the correct outcome — the secret is not stored, and the caller's
	// caller is told why.
	if err := rejectSecretFields(secretContentAndTags(content, tags)...); err != nil {
		return "", "", 0, err
	}
	if err := rejectSecretFields(secretSourceRefField(opts.Provenance.SourceRef)...); err != nil {
		return "", "", 0, err
	}
	if _, err := boundedSourceRef(opts.Provenance.SourceRef); err != nil {
		return "", "", 0, err
	}
	if _, err := boundedAgent(opts.Provenance.Agent); err != nil {
		return "", "", 0, err
	}
	// Same place, same reason, and for the same caller: a tier the vocabulary
	// does not hold is a typo, and the refusal costs nothing and writes nothing.
	// It has to happen before the FTS probe rather than after it, because a fold
	// returns an existing id and writes a linked copy — a tier the fold dropped
	// on the way would be a protection the caller was promised and did not get.
	retention, err := NormalizeRetention(opts.Retention)
	if err != nil {
		return "", "", 0, err
	}
	expires := sessionExpiry(retention, time.Now())

	parentTx, inTx := storeTxFromContext(ctx)
	if !inTx {
		s.mu.Lock()
		defer s.mu.Unlock()
	}

	// One write transaction covers the duplicate probe AND the writes the
	// probe decides on. It is opened before the probe, and the DSN asks for
	// BEGIN IMMEDIATE, so the write lock is held while the probe reads.
	//
	// Probing outside this transaction (issue #560) left a window between the
	// probe and the write that any other ghost process could use: a row it
	// strengthened in that window was overwritten by this save's arithmetic
	// on a value read before the fact, and a row it committed in that window
	// was invisible to the probe, so two saves of one fact became two
	// unlinked rows. Both were cross-process races, and s.mu — a per-Store
	// lock — cannot close either one.
	tx := parentTx
	ownTx := !inTx
	var lock writeLock
	if ownTx {
		if tx, lock, err = s.beginWrite(ctx, "upsert"); err != nil {
			return "", "", 0, fmt.Errorf("begin upsert tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
	}
	// Every read and write below goes through the transaction. Note the pool
	// must not be used here: OpenDB pins one connection per handle, so a pool
	// query inside an open transaction on that same handle deadlocks.
	db := sqlExecutor(tx)
	commit := func() error {
		if !ownTx {
			return nil
		}
		// Commit only a transaction this call opened. When the upsert arrives
		// inside a caller's transaction, committing here would end it: every
		// later statement of the caller fails with "transaction has already
		// been committed or rolled back", and the caller's own deferred
		// Rollback can no longer undo the partial work.
		if cerr := tx.Commit(); cerr != nil {
			return fmt.Errorf("commit upsert tx: %w", cerr)
		}
		// The lock is free from here, so this is where the hold ends and not
		// where this function returns: everything after the commit is
		// bookkeeping (the onSave callback) and would be counted as time the
		// lock was held when it was not.
		lock.reportHold("upsert", time.Now())
		return nil
	}

	var existingID string

	// Two-stage duplicate detection. Stage 1 (recall): the FTS OR-probe over
	// the first 30 words retrieves merge candidates cheaply. Stage 2
	// (precision): mergeScore over the full token sets confirms a true
	// duplicate — the OR-probe alone treats a single shared word as a match,
	// which silently swallowed unrelated saves.
	ftsQuery := sanitizeFTSN(content, 30)
	// A candidate that names a shared key differently is a different fact about
	// a different place, not a second wording of this one, so it is not a
	// candidate at all — and "not a candidate" has to be decided before the
	// window is cut, not after. This statement's LIMIT chooses the rows the fold
	// is made from, so a conflicting row that spent one of the fifteen slots
	// cost a compatible duplicate ranked just below them the fold, and a save
	// that restated a fact already stored was written a second time, unlinked
	// (issue #665). Stated in SQL and held to ScopesConflict by
	// TestScopesConflictSQLAgreesWithScopesConflict, so the rule is asked once.
	//
	// The incoming scope is a bound parameter, not text spliced into the
	// statement, and scopeJSONExpr names its expression twice — inside
	// json_valid and as the value — so the predicate's two placeholders take
	// the same argument.
	//
	// A save with no scope conflicts with nothing (ScopesConflict needs a key
	// both sides name), so the predicate — a correlated subquery over two
	// json_each scans, run once per FTS match — is only added when the save
	// carries one. Most saves carry none.
	incomingScope := scopeJSON(opts.Scope)
	scopeClause := ""
	scopeArgs := []any{}
	if incomingScope != nil {
		scopeClause = "AND NOT " + scopesConflictSQL("m.scope", "?")
		scopeArgs = []any{incomingScope, incomingScope}
	}
	probeArgs := func(lead ...any) []any {
		args := append([]any{}, lead...)
		args = append(args, scopeArgs...)
		return append(args, ftsQuery)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.content
		FROM memories m
		JOIN memories_fts f ON f.rowid = m.rowid
		WHERE m.project_id = ?
		  AND m.category = ?
		  `+scopeClause+`
		  AND memories_fts MATCH ?
		ORDER BY rank, m.importance DESC
		LIMIT 15
	`, probeArgs(projectID, category)...)
	if err != nil {
		// Upsert treats a failed probe as "no candidate" and inserts, so a
		// broken statement would switch dedup off silently. Say so.
		s.logger.Warn("upsert dedup probe failed; saving without dedup", "probe", "same-category", "err", err)
	}
	if err == nil {
		// Token-free content (punctuation/single-char words only) can still
		// FTS-match — sanitizeFTS keeps single-char words that tokenizeContent
		// drops — and jaccard(∅,∅) scores 1.0. Never merge on empty tokens.
		newTokens := tokenizeContent(content)
		var bestSim float64
		for len(newTokens) > 0 && rows.Next() {
			var candID, candContent string
			if scanErr := rows.Scan(&candID, &candContent); scanErr != nil {
				continue
			}
			candTokens := tokenizeContent(candContent)
			// The Jaccard bar above is a similarity test, and a similar text
			// can state the opposite ("always deploy staging" / "never deploy
			// staging"). The default fold keeps the caller's wording, so the
			// incoming text survives either way; FoldOnly drops it, so it may
			// only fold into a row whose text is equivalent — see
			// foldOnlyEquivalent. Scoped to FoldOnly so the default fold keeps
			// recording ordinary near-duplicate links.
			if opts.FoldOnly && !foldOnlyEquivalent(content, candContent) {
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
	// same-category probe predates those exclusions and keeps its behaviour
	// deliberately, because a default upsert that re-saves the text of a
	// resolved memory is supposed to strengthen it and leave it resolved
	// (TestUnresolveOnWrite). FoldOnly cannot afford that: it strengthens the
	// row and then returns WITHOUT inserting the incoming wording, so folding
	// into a dead row would leave a promotion that reports success while the
	// memory is in neither _global nor anywhere it is read from. Its target is
	// therefore re-verified inside the write transaction below and the
	// insertion is taken instead when the probe named a dead row.
	//
	// The exclusion is scoped, for the reason foldTargetStillLive gives in full:
	// an edge whose endpoints name different environments asserts no
	// replacement, and excluding the row anyway would make every re-save of it
	// insert a duplicate. The check is inside the statement because this
	// statement's LIMIT chooses the candidates — filtering after it would spend
	// the budget on rows no fold may use.
	//
	// The candidate's own scope is excluded the same way, and for the same
	// reason: near-identical wording across environments scores high whatever
	// the category, so scope is the only thing that separates these
	// candidates, and a conflicting row that reached the window ahead of a
	// compatible duplicate cost that duplicate the fold (issue #665). Only rows
	// a fold may use are counted against the fifteen.
	if existingID == "" {
		crossRows, crossErr := db.QueryContext(ctx, `
			SELECT m.id, m.content
			FROM memories m
			JOIN memories_fts f ON f.rowid = m.rowid
			WHERE m.project_id = ?
			  AND m.category != ?
			  AND m.resolved_at IS NULL
			  AND NOT EXISTS (
			      SELECT 1
			      FROM memory_links l
			      JOIN memories s ON s.id = l.source_id
			      WHERE l.target_id = m.id
			        AND l.relation = 'supersedes'
			        AND l.invalidated_at IS NULL
			        AND NOT `+scopesConflictSQL("s.scope", "m.scope")+`
			  )
			  `+scopeClause+`
			  AND memories_fts MATCH ?
			ORDER BY rank, m.importance DESC
			LIMIT 15
		`, probeArgs(projectID, category)...)
		if crossErr != nil {
			s.logger.Warn("upsert dedup probe failed; saving without dedup", "probe", "cross-category", "err", crossErr)
		}
		if crossErr == nil {
			// Same empty-token guard as the same-category probe: token-free
			// content can still FTS-match, and jaccard(∅,∅) scores 1.0.
			newTokens := tokenizeContent(content)
			var bestJaccard float64
			for len(newTokens) > 0 && crossRows.Next() {
				var candID, candContent string
				if scanErr := crossRows.Scan(&candID, &candContent); scanErr != nil {
					continue
				}
				candTokens := tokenizeContent(candContent)
				// The same contradiction guard the same-category probe uses, for
				// the same reason. This probe runs whenever the same-category one
				// misses, so a FoldOnly promotion whose candidate happens to
				// differ in category would otherwise fold into a contradicting
				// row: "port 80" as a fact and "port 81" as a gotcha clear the
				// 0.7 bar easily, and the fold would strengthen one and drop the
				// other. Scoped to FoldOnly for the same reason as before — the
				// default fold keeps the caller's wording either way.
				if opts.FoldOnly && !foldOnlyEquivalent(content, candContent) {
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
		//
		// The UPDATE (strengthen), INSERT (new row), and INSERT (link) all run
		// in the transaction opened above, so a failure partway through cannot
		// leave the new memory row orphaned: unlinked, un-embedded (onSave
		// never fires), and invisible.

		if opts.FoldOnly {
			// Re-verify the chosen target before folding into it, and take the
			// insert path instead when the probe named a row that is not a fold
			// target. The cross-category probe already excludes resolved and
			// superseded rows, but the same-category probe deliberately does not
			// (a re-save of resolved text is meant to strengthen it and leave it
			// resolved — TestUnresolveOnWrite), so a FoldOnly save can still be
			// handed a dead target.
			//
			// This is a statement about the row the probe named, not a race
			// guard: the probe ran in this same transaction, which has held the
			// write lock since before it, so nothing can have changed in
			// between. Before the probe moved inside the transaction it was
			// explicitly not a race guard either — a concurrent process could
			// insert the same fact between probe and write, leaving one
			// redundant row, never a lost one (issue #560 closed that window by
			// moving the probe in).
			live, liveErr := foldTargetStillLive(ctx, tx, projectID, existingID)
			if liveErr != nil {
				return "", "", 0, fmt.Errorf("re-verify fold target: %w", liveErr)
			}
			if !live {
				existingID = ""
			}
		}

		if existingID != "" {
			// The increment is added in SQL, against the value the row holds
			// when this statement runs, and not computed from an importance
			// read earlier: with the read outside the write transaction, any
			// save that strengthened this row in between was silently
			// overwritten (issue #560). MIN(1.0, …) is the same cap the Go
			// arithmetic applied.
			//
			// The caller's claim rides along to the target, for the reason pin
			// does: the target is the row that keeps being retrieved and the one a
			// later consolidation absorbs, so a save of near-identical text
			// carrying valid_until that reached only the copy just inserted would
			// report success and leave the row search returns with no window at
			// all. COALESCE, as in the update path, so a save that says nothing
			// about the window leaves whatever the target already holds — a fold
			// re-states a fact, and re-stating it is not a retraction of a
			// boundary somebody else recorded.
			//
			// agent and source_ref take the same COALESCE(NULLIF(?, ''), …) form
			// for the same reason: an empty value means "the caller said nothing",
			// and a detectable harness on this save is no evidence against the
			// author of the row being folded into.
			//
			// Merging a stated boundary onto the target's other one makes this the
			// second writer that can produce a window out of two halves stated
			// independently, so it is judged the same way UpdateMemoryWithOptions
			// judges it and for the same reason: the read is inside the transaction
			// that already holds the write lock, so the pair is the target's window
			// at the moment the merge lands. Before this COALESCE the target kept
			// its own consistent pair and a contradictory claim stayed on the copy
			// this save inserts; readValidity tests expiry first, so the merged row
			// would read as `future` and then `expired` and drop out of ranked
			// retrieval with no error anywhere. Gated on the caller stating a
			// boundary, for the update path's reason: a fold that says nothing about
			// the window cannot make it inconsistent.
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
				opts.Provenance.Agent, opts.Provenance.SessionID, opts.Provenance.SourceRef,
				existingID, projectID)
			if updateErr != nil {
				return "", "", 0, fmt.Errorf("strengthen memory: %w", updateErr)
			}
			// A target that this transaction's own re-check did not clear can
			// still report zero rows affected if it was deleted or moved by an
			// earlier statement in the same transaction. Reporting that as a
			// successful fold would count a promotion that stored nothing, so
			// it is a check on the statement's own report rather than a race
			// guard: no other writer can run between the two.
			affected, affectedErr := res.RowsAffected()
			if affectedErr != nil {
				return "", "", 0, fmt.Errorf("strengthen memory rows: %w", affectedErr)
			}
			if affected == 0 {
				return "", "", 0, fmt.Errorf("fold target %s disappeared during the update", existingID)
			}
			// The fold is a write to the target — it raises its importance and
			// access count — so it gets its own history entry, attributed to the
			// agent whose save did the folding rather than to whoever wrote the
			// row first.
			//
			// FoldOnly additionally records the incoming wording, because that is
			// the one path that throws it away: the ordinary fold stores the new
			// text as a linked copy of its own (so it is already in the history,
			// under its own id), while a FoldOnly fold returns the target and
			// stores nothing. Without this the only record of a promotion's
			// discarded paraphrase was the database itself, before this table
			// existed.
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
			// A second REPORT of the same fact, which is not the same thing as a
			// second memory: the survivor is the row the corpus keeps, so the
			// evidence the fold was about has to land on it. Before this the folding
			// agent and session went with the incoming wording, and the store could
			// say who wrote a memory first but never that a second agent later
			// agreed with it.
			//
			// Appended for FoldOnly too, and that path needs it more: it stores no
			// row of its own, so without this the report would leave no trace.
			if err := appendEvidenceTx(ctx, tx, existingID, evidenceObserved, opts.Provenance, false); err != nil {
				return "", "", 0, err
			}
			// And the same for a VERIFICATION this call stated, on the same
			// survivor and for the same reason: the copy that fold inserted may be
			// the row a reader never sees, while this one is what the corpus keeps
			// and what a later consolidation absorbs. Gated on what the CALLER
			// stated, never on what the row now holds — the target may already be
			// verified from an earlier save, and re-recording that would inflate
			// EvidenceCounts.Verified for a check nobody repeated.
			if err := appendVerificationIfStatedTx(ctx, tx, existingID, opts.Provenance, opts.Validity.VerifiedAt); err != nil {
				return "", "", 0, err
			}
		}

		if opts.FoldOnly && existingID != "" {
			// The caller does not want the incoming wording stored — a
			// promotion whose fact _global already knows. The strengthen above
			// is still the right outcome: the duplicate is evidence the fact
			// keeps recurring, and throwing that away would make promotion
			// lose the signal. Only the UPDATE is wanted.
			//
			// A Pin still has to land here: this path stores no row of its own,
			// so the target is the only thing the request can mean, and a fold
			// that reported success without it would leave the memory the
			// caller asked to protect fully consolidatable.
			if err = s.pinMemoryTx(ctx, tx, existingID, opts.Pin); err != nil {
				return "", "", 0, err
			}
			// The tier reaches the target here for the same reason the pin does:
			// this path stores no row of its own, so the target is the only thing
			// the request can mean.
			if _, err = raiseRetentionTx(ctx, tx, existingID, retention); err != nil {
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
		// Evidence for the row just inserted, in the same transaction. The stored
		// copy carries its own observation even on a fold, because it is a row of
		// its own with its own text; the fold's SECOND report is the one that goes on
		// the survivor above.
		if err := appendEvidenceTx(ctx, tx, id, evidenceObserved, opts.Provenance, false); err != nil {
			return "", "", 0, err
		}
		// And its own verification, for the same reason the observation is its own:
		// this row's verified_at column says somebody checked the text on this row,
		// and a record that counted only the survivor's would leave the two
		// disagreeing about whether THIS memory was checked.
		if err := appendVerificationIfStatedTx(ctx, tx, id, opts.Provenance, opts.Validity.VerifiedAt); err != nil {
			return "", "", 0, err
		}

		// The row this save folded into is the one a later consolidation can
		// absorb — it is the text the corpus already holds, and the copy inserted
		// above is only linked to it — so Pin has to reach it too. Same
		// transaction as the strengthen and the link, or a failure here would
		// report a successful save whose request was silently dropped.
		if err = s.pinMemoryTx(ctx, tx, existingID, opts.Pin); err != nil {
			return "", "", 0, err
		}
		// And the tier, for the reason raiseRetentionTx gives: the row a fold
		// strengthens is the one the corpus keeps, so a tier the caller asked
		// for has to land there rather than only on the copy inserted above.
		// Same transaction as the strengthen and the link, or a failure here
		// would report a save whose request was silently dropped.
		if _, err = raiseRetentionTx(ctx, tx, existingID, retention); err != nil {
			return "", "", 0, err
		}

		// CreateLink takes s.mu itself — calling it here would deadlock, since
		// Upsert already holds the lock for its entire body. Inline the same
		// upsert-link SQL directly instead. Direction is fixed (source_id=new,
		// target_id=existing), not normalized like symmetric relations.
		//
		// No link when the fold target was cleared above: the row that was
		// re-verified as dead is not a duplicate of anything worth linking to,
		// and the insert would fail the foreign key on an empty id.
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

	// No match — create new, in the same transaction as the probe that found
	// nothing: a save that decides to create must not be interleaved with
	// another writer between that decision and the row.
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
	// A save that verified the fact it is storing leaves a record of the check as
	// well as the column: a fresh row's verified_at is written by the INSERT above,
	// and the table is where a reader counts checks.
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
// preference/convention/fact, which already never decay, and for every tier but
// session: the tier factor is the one that has to multiply rather than stand in,
// because a session row decays by its category AND by how long it is wanted.
//
// A var, not a const, because the tier half is built from the Go constants that
// define its floor — see retentionDecayFactorSQL. Every reader interpolates it
// into its own ORDER BY, so the two spellings cannot disagree.
var DecayRankingSQL = decayRankingSQL(true)

// DecayRankingSQLWithTier is the same expression with or without the tier half,
// for a reader that may not be able to name memories.retention. The
// session-start loaders hold a read-only handle that runs no migration
// (memory.OpenReadDB), so on a store from before schema v19 the column is not
// there and naming it fails the whole query with SQLite's "no such column" —
// which both loaders read as no rows, so a user whose first session after the
// upgrade starts the hook would get a digest with no memories and nothing saying
// why. That is the same window `scopeColumnExpr` has always covered for
// memories.scope, and the remedy is the same shape: the expression a reader
// cannot afford to fail on drops the half it cannot spell.
//
// One template, so the category half cannot drift from DecayRankingSQL — a second
// hand-copied expression is the failure mode DecayFactor's parity test exists to
// prevent, and this is the same formula by another route.
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
		WHEN category IN ('preference', 'convention', 'fact') THEN 1.0
		WHEN category IN ('pattern', 'architecture') THEN
			MAX(0.3, 1.0 / (1.0 + (julianday('now') - julianday(created_at)) / 45.0))
		ELSE
			MAX(0.15, 1.0 / (1.0 + (julianday('now') - julianday(created_at)) / 30.0))
	END` + tier + `
`
}

// GetTopMemories returns the top N memories ranked by composite score
// with category-aware time decay and pinned exemption.
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

	// Supersede demote runs whenever the window has a pair to reorder —
	// membership-preserving, same helper the search path uses — so a
	// superseded memory cannot outrank its replacement in injection even
	// when the result fits under limit (order alone still matters).
	if len(results) >= 2 {
		ids := make([]string, len(results))
		supersedeProtected := make(map[string]bool, len(results))
		for i, m := range results {
			ids[i] = m.ID
			// The KEEP-FOREVER tier only — a `supersedes` edge says the target's
			// claim was replaced, and a pin keeps a row visible rather than
			// declaring it current, so a pinned superseded target sinks exactly as
			// it did before tiers existed. Same rule as the search path's
			// demoteSuperseded, and unlike the near-duplicate map below, whose
			// pin half predates tiers and is described there.
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
		// A protection map, not a pin list (see DemotionPenalties): this is the
		// session-start injection read, so a keep-forever memory that is the
		// lower-ranked member of a near-duplicate pair would be cut out of the
		// very block the tier exists to keep it in. Unlike the supersede
		// protection above, THIS map still carries a plain pin: sparing a pinned
		// row from the near-duplicate demotion is what DemotionPenalties did
		// before tiers existed, and the tier only extends that standing rule to
		// keep-forever rows.
		protected := make(map[string]bool, len(results))
		for i, m := range results {
			ids[i] = m.ID
			protected[m.ID] = m.Pinned || RetentionExempt(m)
		}
		penalty, err := DemotionPenalties(ctx, s.queryDB(), ids, protected, s.demotionThreshold)
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

	rows, err := s.queryDB().QueryContext(ctx, `
		SELECT `+memoryColumnsPrefixed+`
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

	rows, err := s.queryDB().QueryContext(ctx, `
		SELECT `+memoryColumnsPrefixed+`
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

	rows, err := s.db.QueryContext(ctx, `
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
// row-level filters a caller can state and left open for everything else. An
// empty category or retention is no filter on that axis.
//
// It is one query rather than GetAll followed by a filter, for the same reason
// the assembler's predicates run before the window closes: a filter applied to
// the result spends the LIMIT on rows the caller cannot use and then reports the
// ones it wanted as absent. The tier predicate is bound, never interpolated, and
// the vocabulary is checked here rather than trusted — a browse surface that
// silently ignored a value it did not recognise would answer a mistyped filter
// with the whole corpus.
//
// The SCOPE is the project's own rows alone, which is what the unfiltered browse
// has always answered: a caller asking "what does this project know" is asking
// about the project. A filtered browse also brings the `_global` rows, which is
// what the category filter has always done and what makes a tier filter useful on
// a keep-forever rule the user wrote once for every repository. That asymmetry is
// inherited from the two readers this replaces (GetAll and GetByCategory
// disagreed about it) and is kept rather than tidied, because the wider reading
// applied to the DEFAULT answer is a behaviour change nobody asked for: a project
// with no memories of its own would come back holding the per-install builtin
// seeds, and the "no memories yet" answer a caller reads as a measurement would
// become unreachable.
func (s *Store) ListMemories(ctx context.Context, projectID, category, retention string, limit int) ([]Memory, error) {
	if category != "" && !IsValidCategory(category) {
		return nil, fmt.Errorf("invalid category %q — must be one of %s", category, categoryList())
	}
	if retention != "" && !IsValidRetention(retention) {
		return nil, InvalidRetentionError(retention)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	scope := "project_id = ?"
	if category != "" || retention != "" {
		// A filtered browse keeps the wider scope the category filter has always
		// had. See the method comment: this is inherited, not chosen, and it is
		// deliberately NOT applied to the unfiltered default.
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

// categoryList is the category vocabulary in schema order, for the refusal
// ListMemories names. The map cannot be ranged in that order, which is why this
// is a list rather than a join over the map's keys.
func categoryList() string {
	return "architecture, decision, pattern, convention, gotcha, dependency, preference, fact"
}

// Touch increments access_count and updates last_accessed.
//
// It is guarded like every other write (#746), so an access counter is never the
// one field an old server keeps bumping in a store a newer Ghost owns. It has no
// production caller today — the search paths that would have used it do not touch
// access_count — and that is the reason it is worth saying out loud: an
// unreferenced method is exactly the kind of thing that gets wired up on a read
// path later, where a refusal would turn a search into an error.
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
// resolution classification: not yet resolved, not pinned, and not in a
// standing-preference category (convention/preference are never evictable —
// see the guardrail in the resolution-classifier spec §4). Globals are
// excluded by the project_id filter. Newest first, so a batch reviews the most
// recent work first.
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
// stamped resolved_at, in ResolveCandidates' eligibility shape with the
// resolved_at predicate inverted: not pinned, and not in a standing-preference
// category. Those two exclusions are load-bearing for the repair path
// (internal/resolve.Reassess, issue #640) — a pin is an explicit user override
// and an exempt category is never resolve's to change — and Newest first, so
// the report reads like the ordinary pass. Globals are excluded by the
// project_id filter.
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
// ranked injection and browse while leaving them searchable — the inverse of
// SetResolved, used by `ghost resolve --reassess` to repair a resolution that
// should not have been made (issue #640). The WHERE clause re-checks the same
// guard as SetResolved (already resolved, unpinned) and binds project_id, so a
// stale or wrong-project caller cannot resurrect another project's rows.
// Returns the count actually cleared, which callers should report instead of
// len(ids). A no-op on an empty slice.
//
// All batches run in ONE transaction, unlike SetResolved's independent ones: a
// repair is reported as a single outcome, and a caller that treats an error as
// "nothing happened" must be able to trust that. Half a repair reported as a
// failure is the same class of harm as a false repair, so the rollback is the
// behaviour, not a nicety.
//
// updated_at is deliberately untouched: nothing about the memory's content or
// authorship changed, only resolve's verdict about it, and bumping freshness
// would perturb the reflect signature and decay ranking for every repaired note.
//
// The transaction also records one history row per row it changed, so both
// verdicts stay readable: a memory stamped resolved by one pass and cleared by a
// later --reassess says so. The ids are read inside the transaction, which has
// held the write lock since its first statement, so the set a row is recorded
// for is exactly the set the UPDATE changed.
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

// setResolvedBatchSize bounds how many IDs go into a single IN (...) clause
// per SetResolved call, well under SQLite's SQLITE_MAX_VARIABLE_NUMBER
// (32766 on modern builds) so an unusually large batch can't hit that limit.
const setResolvedBatchSize = 500

// The two statements SetResolved and MarkResolved both issue, written once so
// the two writers cannot drift apart on the guard that decides what resolve is
// allowed to stamp. They differ in three placeholders each — the project
// predicate, the KEEP-cache column and the id list — and in nothing else, which
// is the point: the ordinary pass and an operator's named mark are the same
// decision about the same row, so the row must reach the same answer whichever
// door it came through.
const (
	// setResolvedSelectSQL reads the ids an UPDATE is about to change, inside
	// the transaction that will change them. The project predicate is the `?`
	// bound to projectID when there is one (SetResolved passes ""); SQLite
	// treats `project_id = ''` as false against every real id, which is how one
	// statement serves a caller that has no project to bind and one that must not
	// reach outside its own.
	setResolvedSelectSQL = `
		SELECT id FROM memories
		WHERE id IN (%s)
		  AND project_id != ''
		  AND (? = '' OR project_id = ?)
		  AND resolved_at IS NULL
		  AND pinned = 0
		  AND category NOT IN ('convention', 'preference')
		  AND ` + retentionExemptSQL
	// setResolvedUpdateSQL stamps the row and drops its KEEP cache in ONE
	// statement, so no reader can observe a resolved row still holding a
	// verdict the next pass would honour.
	//
	// Both carry retentionExemptSQL, so a `persistent` row is not stamped by
	// either half of the one transaction that would stamp it — the SELECT is what
	// decides the row is eligible and the UPDATE re-checks it at write time, and a
	// protection asked at only one of them is lost the moment the two disagree.
	setResolvedUpdateSQL = `
		UPDATE memories SET resolved_at = datetime('now'), resolve_kept_hash = ''
		WHERE id IN (%s)
		  AND project_id != ''
		  AND (? = '' OR project_id = ?)
		  AND resolved_at IS NULL
		  AND pinned = 0
		  AND category NOT IN ('convention', 'preference')
		  AND ` + retentionExemptSQL
)

// setResolvedStampTx is the shared body of the two resolve writers: one
// transaction, batches of setResolvedBatchSize, and one history row per row the
// UPDATE actually changed. It returns the ids it stamped, which is what a caller
// that has to report per-row outcomes needs, and len() of it is the count the
// older caller returns. An error returns NO ids, because an error here is always
// a rollback of everything this call wrote.
//
// projectID empty means the caller's ids are already project-scoped by the
// selection it made — which is the ordinary pass's position, since it read
// ResolveCandidates for one project — and is deliberately not "any project":
// binding the project here costs one compared parameter and makes the guard the
// same statement as the projection, so a caller cannot forget it.
func (s *Store) setResolvedStampTx(ctx context.Context, ids []string, projectID string, prov Provenance) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "set-resolved")
	if err != nil {
		return nil, fmt.Errorf("begin set resolved: %w", err)
	}
	// A rollback is the whole point of one transaction, so every error path
	// returns an EMPTY id list rather than the ids it had collected. Returning
	// them would claim a stamp that the rollback undid, which is the one thing a
	// caller of this method has no way to check for itself.
	defer func() { _ = tx.Rollback() }()

	var stamped []string
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

		changed, err := selectIDs(ctx, tx, fmt.Sprintf(setResolvedSelectSQL, in), args...)
		if err != nil {
			return nil, fmt.Errorf("select rows to resolve: %w", err)
		}
		if len(changed) == 0 {
			continue
		}
		result, err := tx.ExecContext(ctx, fmt.Sprintf(setResolvedUpdateSQL, in), args...)
		if err != nil {
			return nil, fmt.Errorf("set resolved: %w", err)
		}
		if _, err := result.RowsAffected(); err != nil {
			return nil, fmt.Errorf("set resolved rows affected: %w", err)
		}
		stamped = append(stamped, changed...)
		if err := appendHistoryForIDsTx(ctx, tx, changed, phaseResolve, prov); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit set resolved: %w", err)
	}
	return stamped, nil
}

// SetResolved stamps resolved_at = now on the given memory IDs, dropping them
// from the ranked injection/browse surface while leaving them searchable. The
// WHERE clause re-checks the same eligibility guard as ResolveCandidates
// (unresolved, unpinned, non-exempt category) at write time, not just at read
// time — a candidate pinned or recategorized during the classify loop is
// excluded rather than stamped anyway. Returns the count actually stamped,
// which callers should report instead of len(ids). A no-op on an empty slice.
//
// Every batch runs in one transaction (it used to be an independent statement
// each) and records one history row per row it stamped, so "which pass resolved
// this, and when" is answerable from the database. The transaction is what makes
// the history exact: it has held the write lock since its first statement, so
// the ids read inside it are the ids the UPDATE goes on to change.
//
// It passes no project and no provenance, and both are deliberate. Its ids come
// from ResolveCandidates for ONE project, so a project predicate would be a
// second statement of what the caller already ensured — and a lifecycle pass
// knows no session, so the `resolve` history row it writes carries no performer
// and an empty field is an admission rather than a claim. An operator's named
// mark is neither of those things: see MarkResolved.
func (s *Store) SetResolved(ctx context.Context, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	stamped, err := s.setResolvedStampTx(ctx, ids, "", Provenance{})
	return len(stamped), err
}

// MarkResolved stamps resolved_at on the named ids IN projectID and returns the
// ids it actually stamped, in the order it stamped them. It is SetResolved for a
// caller that names its rows — `ghost resolve --mark` (#714) — and the two
// differences are the point of the separate method rather than a wrapper:
//
//   - It BINDS projectID. A ref resolves through the project (plus _global), so
//     a promoted row is nameable from here; a stamp that reached one would let a
//     project's repair pass bury a memory every project shares, which is not
//     what a caller that asked about ONE project meant.
//   - It RECORDS the performer. The history row an operator's mark writes is the
//     only resolve row that can say a person did it, and it is the one worth
//     being able to say: a name on a `resolve` row is a statement that nobody
//     passed a classifier over that note, which is precisely the judgement a
//     reader of the audit cannot otherwise check.
//
// It also drops resolve_kept_hash on every row it stamps, in the same
// statement. The cache is a KEEP verdict keyed by content, and a row the
// operator has just stamped is not a KEEP the next pass may honour: leaving the
// hash would make the ordinary pass skip the row as `N KEEP cached` for as long
// as its text stood, so a note buried on purpose came straight back the moment
// anything rewrote it. SetResolved reaches the same statement, so a row a pass
// resolves cannot be resurrected by a verdict from before it.
//
// Returns the stamped ids and no count, because a caller that names rows has to
// say which of them moved: an already-resolved row, a pinned one and one in a
// standing category are all rows this method declines, and they are declined for
// different reasons the caller can only read off the row it already loaded.
//
// An error is fatal for the whole request and returns no ids. One transaction
// covers every batch, so a failure is a rollback and a caller may treat the error
// as "nothing happened" — which is the property a repair needs to be reportable
// as a single outcome rather than as a count of whatever it reached before the
// failure.
//
// That leaves one outcome the caller cannot see, and it is why the guard is a
// read-time AND a write-time check rather than either: a row that was eligible
// when the caller read it and is not by the time this runs is simply absent from
// the returned ids. It is not an error — a concurrent pin or a recategorisation
// is somebody else's decision, not a failure here — so the caller has to treat
// "not returned" as its own state rather than as a stamp.
func (s *Store) MarkResolved(ctx context.Context, projectID string, ids []string, prov Provenance) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	switch projectID {
	case "":
		// The one guard this method exists to hold. A caller with no project has
		// not asked about a project, and the shared statement treats an empty
		// binding as "no binding" — so the refusal is here rather than left to
		// the argument it would otherwise weaken.
		return nil, fmt.Errorf("mark resolved: a project is required, so no memory can be reached from it")
	case GlobalProjectID:
		// And the sentinel, for the same reason one layer up: `_global` is not a
		// project a caller runs a lifecycle pass against. It holds promoted rows
		// that EVERY project injects, so naming it would let one command bury a
		// memory in all of them — and a caller that got there by typing the
		// reserved name would be doing it deliberately, which is exactly the case
		// the guard is for. Every other store method that refuses `_global` refuses
		// it here (see DeleteProject), so the refusal is at this layer rather than
		// left to each caller to re-derive: a guard one caller re-implements is a
		// guard the next caller forgets.
		return nil, fmt.Errorf("mark resolved: refusing to mark in the %s project — it holds the promoted memories every project injects, and a lifecycle pass over one project cannot decide for all of them", GlobalProjectID)
	}
	return s.setResolvedStampTx(ctx, ids, projectID, prov)
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
			return fmt.Errorf("mark supersede neither %s->%s: %w", key[0], key[1], err)
		}
	}
	return tx.Commit()
}

// DeleteOptions is what one delete is asked to do beyond removing the row.
type DeleteOptions struct {
	// PurgeHistory removes every memory_history row for this memory in the
	// same transaction, leaving nothing behind.
	//
	// It is the redaction path, and the default is deliberately not it. The
	// history keeps the text a memory USED to hold, so a plain delete preserves
	// it — which is what makes the history worth having, and is also how a
	// credential "removed" by deleting its memory survives in a table with a
	// longer life than the row. A delete meant to ERASE rather than to retire has
	// to say so here. It erases every copy of the text this database holds,
	// including the reflection snapshot that could restore the row; it cannot
	// reach a backup taken earlier. See the ghost_memory_delete purge_history
	// argument and `ghost history purge`.
	PurgeHistory bool
}

// Delete removes a specific memory and KEEPS its history. The row's last state is
// recorded first, in the same transaction: memory_history.memory_id
// deliberately has no foreign key, so a delete leaves behind a tombstone carrying
// the text the memory held — the only remaining record of it, and the reason a
// cascading history table would be empty exactly when the audit is asked.
//
// For a redaction, use DeleteWithOptions with PurgeHistory.
func (s *Store) Delete(ctx context.Context, id string) error {
	return s.DeleteWithOptions(ctx, id, DeleteOptions{})
}

// DeleteWithOptions is Delete with the caller's choice about the history: kept (a
// tombstone) or purged (nothing). Both happen inside the one transaction that
// removes the row, so a memory and its history can never come apart.
func (s *Store) DeleteWithOptions(ctx context.Context, id string, opts DeleteOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, _, err := s.beginWrite(ctx, "delete")
	if err != nil {
		return fmt.Errorf("begin delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if !opts.PurgeHistory {
		// Before the DELETE, because afterwards there is nothing left to read. A
		// missing row is not this call's error to report: the DELETE below says
		// "memory not found" in its own words, and a bad id must produce that.
		if err := appendHistoryTx(ctx, tx, id, phaseDelete, Provenance{}); err != nil &&
			!errors.Is(err, errHistoryNoMemory) {
			return err
		}
	}

	// A purge runs BEFORE the DELETE, not after. The text it has to erase is the
	// text the row holds, and once the row is gone the collection that finds it
	// comes back empty — a redaction that silently ran with nothing to redact is
	// the one failure a redaction must not have. The DELETE below still reports
	// "memory not found" for a bad id, and the rollback that follows undoes the
	// purge with it, so a failed delete leaves the tombstone-free history intact.
	//
	// And it writes no tombstone of its own, which is the point: a purge must
	// leave nothing behind that could carry the text being redacted.
	if opts.PurgeHistory {
		if _, err := purgeHistoryTx(ctx, tx, id); err != nil {
			return err
		}
	}

	result, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("memory not found: %s", id)
	}
	return tx.Commit()
}

// UpdateOptions is the optional set of facts an edit may change. Every field is
// optional and its zero value means "not part of this edit", which is what makes
// the edit partial: a caller correcting a port number must not silently drop the
// validity window and the source reference the row already carries.
//
// The one asymmetry is Tags, where nil means keep and a non-nil empty slice
// means clear. A caller that wants to remove every tag has no other way to say
// it, and an empty tag list is a real state rather than an absent one.
type UpdateOptions struct {
	Content    *string
	Category   *string
	Importance *float32
	Tags       []string
	// Validity replaces only the stamps it carries. A nil field leaves the
	// stored value alone, so a caller restating one boundary does not erase the
	// other, and an edit about something else erases nothing.
	Validity Validity
	// Provenance re-attributes the row to the session performing this edit. An
	// empty field keeps the stored value rather than clearing it, because a
	// write whose author Ghost could not identify is no evidence that the
	// author of the previous write was wrong.
	Provenance Provenance
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
//
// The validity triple and the provenance fields are not reachable through this
// signature: they need UpdateMemoryWithOptions, which is the only path that
// writes them. This wrapper keeps the pre-existing callers (the reflection and
// maintenance paths, which know no validity claim) from having to spell out a
// zero UpdateOptions each. The credential guards are in the method it delegates
// to, not here, so that the options path is guarded on the same terms as this
// one.
func (s *Store) UpdateMemory(ctx context.Context, projectID, id string, content, category *string, importance *float32, tags []string) error {
	return s.UpdateMemoryWithOptions(ctx, projectID, id, UpdateOptions{
		Content: content, Category: category, Importance: importance, Tags: tags,
	})
}

// UpdateMemoryWithOptions is UpdateMemory over the full partial-edit set, adding
// the validity triple and the write-time provenance an MCP edit may carry.
//
// A validity stamp that changes does not invalidate the embedding: the vector
// describes the text, and an edit that only re-dates a claim leaves the text —
// and therefore the vector — exactly as it was. Invalidating it would re-embed
// every row whose window was merely corrected, for no change in what the memory
// says.
func (s *Store) UpdateMemoryWithOptions(ctx context.Context, projectID, id string, opts UpdateOptions) error {
	// The credential guards live here rather than in UpdateMemory, because this
	// is where the fields arrive on both paths and a guard one call above would
	// leave the options path — the one the MCP edit tool uses — unguarded.
	//
	// The incoming content only, never the composed result. A row written before
	// the guard existed can hold a credential, and a user clearing it out must
	// still be able to retag, re-categorise, or re-prioritise that row on the way
	// to deleting it — refusing a metadata-only edit would strand it with
	// ghost_memory_delete as the only remaining move.
	if opts.Content != nil {
		if err := rejectSecret("content", *opts.Content); err != nil {
			return err
		}
	}
	// The tags, unconditionally, and that unconditional part is the point: this
	// method is reachable with tags and no content, so a content-only guard skips
	// exactly the call that carries them. validateTags upstream caps the shape at
	// ten tags of 64 characters and nothing else, and a tag is returned by search
	// and quoted into the next reflect prompt — so the exposure is the same one
	// the body has.
	if err := rejectSecretList("tags", opts.Tags); err != nil {
		return err
	}
	// The reference, on the same terms as the content and the tags: it is
	// caller-supplied text stored on the row and rendered beside the content on
	// every listing. Empty is skipped, so a caller that mentions no reference is
	// not charged for a check.
	if err := rejectSecretFields(secretSourceRefField(opts.Provenance.SourceRef)...); err != nil {
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

	// An edit is a knowledge write like a save, so it gets the same bounded
	// BEGIN retry and is measured under the same budget: the multi-process
	// fleet's `cli` role edits as well as saves, and under saturation it is
	// the edit that failed when the save did not (issue #671).
	tx, lock, err := s.beginWrite(ctx, "update")
	if err != nil {
		return fmt.Errorf("begin update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var curContent, curCategory, curTags string
	var curImportance float32
	// The stored window comes out of the same statement as the fields this edit
	// replaces, because it is judged against and has to be the row's at the moment
	// the write lands. Reading it before the transaction — as a tool-level
	// pre-check does — would leave two concurrent edits to two different
	// boundaries able to each pass against a snapshot the other has already
	// invalidated, and store a window that ends before it starts.
	//
	// The two boundaries only, because the rule is the window's: verified_at has
	// no order to be out of, and the UPDATE below writes all three by COALESCE
	// rather than read-modify-write, so nothing here would use it.
	var curFrom, curUntil sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT content, category, importance, tags, valid_from, valid_until
		 FROM memories WHERE id = ? AND project_id = ?`, id, projectID,
	).Scan(&curContent, &curCategory, &curImportance, &curTags, &curFrom, &curUntil)
	if err == sql.ErrNoRows {
		return fmt.Errorf("memory %s not found in project %s", id, projectID)
	}
	if err != nil {
		return fmt.Errorf("lookup memory: %w", err)
	}

	//
	// Only when the caller supplied a boundary, which is the only way an edit can
	// change the window. With nothing supplied the stored pair is judged against
	// itself, and a row whose own window was written out of order — Store.Create,
	// ImportMemory and RestoreSnapshot all pass the stamps through with no order
	// check, and that is deliberate — would refuse an unrelated content or tag
	// edit over a defect the caller cannot see or fix without restating a window
	// they never touched. An edit that only moves verified_at is not a window
	// edit either, and is not refused.
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

	// A memory with NO history yet is pre-v17 — migrateV17 records no starting
	// row — and this write is the one that overwrites text nothing else keeps. So
	// the state it held a moment ago is filed first, while the row still holds it;
	// see recordBaselineHistoryTx, whose contract is that it runs before its
	// caller's write. It is a no-op for every memory this build wrote, so the
	// common case pays one indexed existence check.
	if err := recordBaselineHistoryTx(ctx, tx, id, phaseBaseline, Provenance{}); err != nil {
		return err
	}

	// resolved_at is a resolve pass's verdict, not part of the editable fields.
	// Only a content change may clear it — a metadata-only edit (tags,
	// importance, category) must not resurrect resolved evidence into ranked
	// injection. Content edits are treated as the memory being reasserted.
	clearResolved := 0
	if newContent != curContent {
		clearResolved = 1
	}
	// The validity and provenance columns are written with COALESCE rather than
	// read, merged and written back: this statement is the only thing that
	// touches them, so an omitted field has to be the stored value by
	// construction. A read-modify-write would make two concurrent edits to two
	// different fields of the same row able to lose one of the two, and it would
	// put the current values in Go variables the caller cannot see.
	//
	// NULLIF(?, '') is what makes an empty provenance string mean "keep" rather
	// than "clear": an undetectable harness is no evidence against the author of
	// the previous write, and NULLIF turns "" into the NULL COALESCE skips.
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
		opts.Provenance.Agent, opts.Provenance.SessionID, opts.Provenance.SourceRef,
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
	// The state this write produced, which is the only other copy of the new text
	// once the next edit overwrites it.
	//
	// The caller's Provenance, not a zero one: a history row records who
	// performed the write, and an edit through a session knows that. The MCP
	// path passes provenanceFor's result, whose empty fields mean "nobody
	// reported an agent" rather than "nobody acted" — which is the same
	// admission every other history phase makes, and the reason this row can
	// name an editor that the pre-history UPDATE could not.
	if err := appendHistoryTx(ctx, tx, id, phaseUpdate, opts.Provenance); err != nil {
		return err
	}
	// An edit that also asserted the fact was CHECKED leaves a record of the
	// check, in the transaction that wrote verified_at, so a row cannot read
	// verified with no account of who checked it. Note what this path does NOT
	// append: an `observed` record. #682's writers append one to Create and to
	// every Upsert branch but not here, and an edit is not a new report of a fact
	// — it is a change to one that is already recorded. Adding the observed leg
	// here would be widening #673's design from inside a PR about #575, and it
	// would make every content edit inflate Observations.
	//
	// Gated on the caller's stated verified_at, and the gate is what makes this
	// path's COALESCE safe: an edit that fixes a typo on a memory verified last
	// month leaves the stored column alone, and appending for that would
	// manufacture a check. See appendVerificationIfStatedTx.
	if err := appendVerificationIfStatedTx(ctx, tx, id, opts.Provenance, opts.Validity.VerifiedAt); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	lock.reportHold("update", time.Now())
	return nil
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
//
// The memory's history moves with it, in the same transaction. It has to:
// memory_history.project_id is ON DELETE CASCADE, so history rows left
// naming the project the memory just left are taken by the next
// `ghost project delete` of that project — while the promoted memory, now in
// _global, survives with no recorded past at all.
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
	if _, err := tx.ExecContext(ctx,
		`UPDATE memory_history SET project_id = ? WHERE memory_id = ?`, GlobalProjectID, id,
	); err != nil {
		return fmt.Errorf("promote memory history: %w", err)
	}
	return tx.Commit()
}

// TogglePin sets or clears the pinned flag.
func (s *Store) TogglePin(ctx context.Context, id string, pinned bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	pinnedInt := 0
	if pinned {
		pinnedInt = 1
	}
	res, err := s.execGuardedWrite(ctx, "toggle-pin", `
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

// replaceCandidate is a stored row ReplaceNonManual may delete, or an emitted
// memory may reuse in place: the identity it would reuse, plus the stored state
// the two reuse decisions are made from — reusePreservesAge reads the text and
// category, and reuseChangesNothing (below) reads all five recorded fields.
type replaceCandidate struct {
	id         string
	content    string
	category   string
	importance float64
	tags       string
	scope      sql.NullString
}

// reusePreservesAge reports whether a reused row is unchanged by this replace
// — the consolidator re-emitted the stored text byte-identically, in the same
// category — and must therefore keep its created_at, source and provenance
// instead of being stamped as newly consolidated (#623).
//
// Reuse is matched on content alone, so an identical sentence re-emitted under
// a different category also lands here. That is a rewrite, and must keep
// #279's behaviour: category is one of the two things DecayRankingSQL decays
// on, so a recategorized row is not the same knowledge with a new age. Only
// the fields reflection actually restated move in the unchanged case — a
// reweight, retag or scope narrowing is still applied.
//
// The bug this guards: RetainGuardedDrops (#549, every category since #619)
// hands back a memory the consolidator omitted as stale, byte-identically, and
// the reuse UPDATE then re-stamped it created_at = datetime('now') and
// source = 'reflection'. Omitting a memory therefore made it rank fresher than
// folding it would have — its 30/45-day decay restarted on every applied
// reflect, so an architecture/decision/pattern memory the model kept dropping
// never aged out, and its original mcp provenance was overwritten.
//
// Note that emitted.Source plays no part: the one caller does set it
// (cmd/ghost's reflectMemoriesToMemory hardcodes 'reflection'), but this
// function never reads it, and neither does the insert or the rewrite path
// below — both hardcode 'reflection' in SQL. The stored source therefore
// survives an unchanged re-emission only because that branch stopped assigning
// it, which is also why a future reader of Memory.Source must not assume it
// decides this.
func reusePreservesAge(stored replaceCandidate, emitted Memory) bool {
	return stored.content == emitted.Content && stored.category == emitted.Category
}

// reuseChangesNothing reports whether this replace leaves the reused row exactly
// as it found it — the emission restates every field the row records, so there
// is no write to make (#727).
//
// It is a STRICTER question than reusePreservesAge, and the two are separate on
// purpose. reusePreservesAge asks whether the row is the same knowledge with the
// same age, which decides created_at and source; this asks whether anything at
// all moves, which decides whether the pass writes the row and records it. A
// reweight, a retag or a stated scope passes the first and fails the second,
// because each of those is a change to what the memory says about itself.
//
// The five fields are the ones the reuse UPDATE writes, which is what makes this
// predicate checkable against the statement rather than against a reading of
// "unchanged": anything the UPDATE would assign, this compares. source and
// created_at are not here because the unchanged branch does not assign them.
//
// A value that cannot be read counts as CHANGED, never as unchanged. The safe
// direction is the loud one — a tags column that is not a JSON list makes the
// pass write the row and record it, where treating it as equal would leave a
// value the caller asked for unrecorded and its timestamp unmoved.
//
// importance is compared at the EMISSION's precision, and that is the whole
// reason this predicate is not a plain ==. Memory.Importance is a float32, the
// column is a float64, and the READ already narrows the column into a Memory
// (scanMemories), so that is the precision at which anything downstream can tell
// one value from another. Upsert's strengthen is `SET importance = MIN(1.0,
// importance + ?)` with `importance*0.2` BOUND as a float32, so the multiply is
// Go arithmetic on the caller's own value and the ADDITION is the one SQLite
// does in its own float64 — the add is what #655 moved into SQL, where it had
// been Go arithmetic on a float32 that left the column holding a float32-exact
// value. The increment is therefore the caller's float32, NOT the column's value
// times 0.2, and the two are not the same number. Either way the sum is a float64
// a float32 cannot name: float32(0.55) is 0.550000011920929, the bound increment
// widens to 0.11000000685453415, and the column lands at 0.6600000187754631,
// which narrows to 0.6600000262260437. (The column's own value times 0.2 gives
// 0.66000001430511479 — a different float64, though it narrows to the same
// float32, so the defect and the fix are the same either way.) Comparing the two
// at full width therefore reported a change on a row that had not moved, so
// every applied reflect appended a byte-identical version of it and stamped it
// touched — the whole of #727 reinstated for every strengthened memory (15 of
// 1,170 on the store it was measured on) #750.
//
// So the stored value is NARROWED to the emission's precision and the two are
// then equal. That is the only lossy direction, and what it loses is one float32
// ULP: an emission of 0.66 reads as equal to a row holding 0.6600000187754631.
// No reader can observe that, because every reader narrows this column the same
// way the emission does, and a reweight is a different float32, which no
// rounding of the stored value imitates. The float64 the column holds is left
// alone rather than rounded, because a no-op is a no-op — a write here would
// replace a fold's exact value with the emission's widening of it and change
// nothing else.
func reuseChangesNothing(stored replaceCandidate, emitted Memory) bool {
	return stored.content == emitted.Content &&
		stored.category == emitted.Category &&
		float32(stored.importance) == emitted.Importance &&
		sameTags(stored.tags, emitted.Tags) &&
		scopeUnchanged(stored.scope, emitted.Scope)
}

// sameTags compares the stored tags column with an emission's list by VALUE
// rather than by the bytes json.Marshal produced for each.
//
// The bytes are not a stable spelling of the same list. A row saved with no tags
// holds "null" and a keep that normalises nil to an empty list holds "[]", and
// the two read back identically — GetByIDs hands both to the caller as the same
// empty list — so comparing the column text would report a difference on every
// run of a corpus whose memories were saved untagged, and a pass that reported a
// difference writes the row and stamps it touched. That is the whole defect
// #727 removes, reached by a different road.
//
// An unreadable column is not equal (see reuseChangesNothing).
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

// scopeUnchanged reports whether the emission's scope leaves the stored scope
// alone, which is the reuse UPDATE's own COALESCE rule read as a predicate.
//
// An emission that states no scope never changes the column — reflection has no
// machine-readable scope, so every memory it emits arrives with none, and
// "unchanged" is the answer the UPDATE already gives it. A stated scope is
// compared through parseScope, because that is how every reader in the codebase
// reads the column: a value it cannot parse is no scope, so a stored column that
// decodes to nothing equals an emission that states nothing, and an emission
// that states something is a change the UPDATE applies.
func scopeUnchanged(stored sql.NullString, emitted map[string]string) bool {
	if len(emitted) == 0 {
		return true
	}
	return maps.Equal(parseScope(stored), emitted)
}

// takeReusableRow removes and returns the row an emitted memory should reuse
// from the same-content candidates left for it, along with the rest. A project
// legitimately holds one text under several categories — Upsert keeps the
// incoming category on a linked copy so nothing the caller saved is lost — so
// the content-only bucket is not a single row, and claiming the wrong one
// makes reusePreservesAge report a category change: the memory that IS the same
// knowledge would be deleted, its other-category twin recategorized onto it, and
// both re-stamped. Preferring the category the consolidator actually emitted
// keeps the retention case a retention.
//
// Falls back to the first candidate when no category matches, so a genuine
// recategorization still updates its row in place — the behaviour the
// content-only match had, and the one the rewrite branch's "must still be
// applied in place" case depends on. Without the fallback the recategorized
// row would be left in the bucket, deleted with the rest of the unmatched
// candidates, and the emission inserted under a fresh id: the row identity
// gone and, because memory_embeddings and memory_links are ON DELETE CASCADE,
// its embedding and link graph with it (#452), plus a reset created_at. So the
// failure mode this guards is a lost row, not a redundant one. In the fallback
// case the caller takes the rewrite branch, exactly as it did previously.
//
// Returns false for an empty bucket, so a future caller cannot index an empty
// slice here. Callers pass candidates already ordered oldest first (see the
// candidate query), so the fallback keeps the oldest age and stays
// deterministic.
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

// ReplaceNonManual atomically replaces all non-manual memories for a project.
// Manual-sourced memories are preserved. Refuses to replace with an empty set.
//
// It returns the IDs of memories preserved because they were saved during the
// consolidation round trip (the consolidatedSince race) — callers must not
// treat the post-apply corpus as fully consolidated when this is non-empty.
//
// rowClaims is the set of columns a rewrite inherits from the rows it replaces: the
// validity triple, the trust rating, and the three provenance strings.
type rowClaims struct {
	validFrom, validUntil, verifiedAt *string
	confidence                        *float64
	agent, sessionID, sourceRef       string
}

// inheritedClaims resolves the columns a rewritten or merged row takes from the
// rows it stands in for.
//
// One rule, the same one the reuse branch inside ReplaceNonManual already follows:
// a value the EMISSION states wins, and silence inherits. Nothing is defaulted and
// nothing invented, because a consolidation has no authority to re-attribute a
// claim it was never told about. The sources are read in the order ReplacesIDs
// names them — for a merge that is the consolidator's own order — and the FIRST
// source stating a value supplies it, so the result is a function of the proposal
// rather than of rowid order.
//
// THE WINDOW IS THE EXCEPTION, and it is not a smaller version of that rule. The
// two boundaries are resolved TOGETHER after every source has been read, because
// filling them independently composes a pair no row ever asserted, and deciding
// per source lets a single-boundary row lock the unit and lose a later source's
// complementary end — a loss that depends on the order the consolidator named. See
// the resolution below for the order of preference. verified_at, confidence, agent,
// session_id and source_ref remain first-source-wins: each is one value with no
// partner to disagree with, and a half-window problem cannot arise for any of
// them. verified_at is a single value by that same reasoning AND is read by the
// same reader as the two boundaries, so it is judged by readability too rather
// than for pairing alone. A maintainer reading this paragraph must not
// reimplement the window the per-value way — that is the bug, and the resolution
// below exists because of it.
//
// It is the SOURCES rather than the emission alone because an emission carries none
// of this: reflectMemoriesToMemory builds it from the consolidator's output and has
// nothing to copy a window from, so binding only the emission's columns would store
// NULL every time and fix nothing. Read here because the delete below is what
// disposes of the sources, and this loop runs before it — the same reordering
// carryEvidenceTx already depends on.
func inheritedClaims(ctx context.Context, tx *sql.Tx, projectID string, m Memory) (rowClaims, error) {
	claims := rowClaims{
		validFrom:  m.ValidFrom,
		validUntil: m.ValidUntil,
		verifiedAt: m.VerifiedAt,
		confidence: m.Confidence,
		agent:      m.Agent,
		sessionID:  m.SessionID,
		sourceRef:  m.SourceRef,
	}
	// The window is decided AFTER every source has been read, because the choice
	// between a complete pair and a single boundary is only knowable once the whole
	// set has been seen — deciding per source is the bug this shape exists to fix.
	var completeWindow, firstHalfWindow Validity
	var haveComplete, haveHalf bool
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
			// A source the proposal named but this store does not hold, or one
			// another writer took. Its evidence is not carried either, so there is
			// nothing to inherit and the row is written on what the emission said.
			continue
		}
		if err != nil {
			return rowClaims{}, fmt.Errorf("read replaced memory %s claims: %w", id, err)
		}
		// This source is a WINDOW CANDIDATE, remembered rather than decided on.
		// See the resolution after the loop for why, and for what a candidate is
		// allowed to become. Only a boundary readableStampPtr recognises counts:
		// a source whose window is prose in the column states nothing Ghost can
		// act on, and letting it qualify as a complete pair would let it win rule
		// 1 from any position and discard a real window.
		candidate := Validity{ValidFrom: readableStampPtr(validFrom), ValidUntil: readableStampPtr(validUntil)}
		switch {
		case candidate.ValidFrom != nil && candidate.ValidUntil != nil:
			// A source states a COMPLETE window. It is preferred, but only if the
			// pair it states is one that passes CheckWindowOrder; a source whose
			// own window is out of order contributes NOTHING, not a half. Keeping
			// one of its boundaries would pick a side the row itself never took,
			// and the pair is the only thing it ever asserted.
			if err := CheckWindowOrder(candidate, Validity{}); err == nil && !haveComplete {
				completeWindow, haveComplete = candidate, true
			}
		case !haveHalf && (candidate.ValidFrom != nil || candidate.ValidUntil != nil):
			// Exactly one boundary: a genuine half, and the fallback's source.
			firstHalfWindow, haveHalf = candidate, true
		}
		// verified_at is the third value of the same triple and is read by the same
		// reader, so it takes the same rule: a value no layout reads is not a stated
		// verification. nullIfEmptyPtr at the INSERT maps only the empty string, so
		// without this a source's prose in the column was copied onto the successor
		// verbatim and stage 2 reported validity_unparseable forever.
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
	}
	// The window is inherited AS A UNIT, from one source or from none — never half
	// from one row and half from another.
	//
	// Filling the two boundaries independently is what a merge must not do. A source
	// stating only valid_from and another stating only valid_until compose a pair
	// neither row ever asserted, and the halves can land backwards: a row born
	// expired, with both sources' evidence carried onto it as though the pair were
	// one claim, so stage 2 withholds it from ranked retrieval with no error
	// anywhere. That is the contradiction UpdateMemoryWithOptions and
	// UpsertWithOptions' fold both refuse by running CheckWindowOrder over the pair
	// they compose.
	//
	// Order of preference, and the second rule is the one a single-pass version
	// gets wrong:
	//
	//  1. A source holding a COMPLETE consistent pair of READABLE stamps wins,
	//     whichever id the consolidator named first. Readable is load-bearing
	//     here, not decoration: rule 1 is position-independent, so an unreadable
	//     pair that qualified would win from anywhere and discard a real window,
	//     which is the mirror of the loss these rules exist to remove. Deciding per source instead would let a
	//     single-boundary row claim the unit and lock the window, discarding a
	//     later source's complete pair — and a row that inherits a start and
	//     loses its end never retires, which is the outcome valid_until exists to
	//     prevent, on a path that runs unattended over the whole corpus.
	//  2. Failing that, ONE boundary from the first source that states any. Half a
	//     real claim beats none, and the other half is not invented. Which half
	//     survives is the first id's, and that order dependence is a stated cost
	//     rather than a silent one: it is bounded by rule 1 removing it whenever
	//     any source in the set states a complete pair.
	//
	// The CheckWindowOrder on rule 1 is not redundant with the unit. A source row
	// can hold a window that is ITSELF out of order, because Store.Create,
	// ImportMemory and RestoreSnapshot write the triple with no order check, so
	// inheriting a pair whole still needs judging. A source failing it falls to
	// rule 2 and contributes no half.
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
			return nil, fmt.Errorf("begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
	}

	// Snapshot existing non-manual/non-builtin memories before deleting.
	// Pinned memories and resolved memories are excluded throughout this
	// function — manual and builtin sources are explicit preservation classes,
	// while a pin is an explicit user override and a resolved_at stamp is a
	// resolve pass's verdict. Reflection must never delete, rewrite, or
	// silently drop any of them (it has no way to know a consolidated memory
	// it emits corresponds to a pinned/resolved one it never saw as such, so
	// preservation has to mean "don't touch it" rather than "carry the flag
	// through"). See issue #318.
	snapshotID := fmt.Sprintf("%s-%d", projectID, time.Now().UnixNano())
	_, err = tx.ExecContext(ctx, `
		INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source, tags,
		                              created_at, memory_id, access_count, last_accessed,
		                              agent, session_id, source_ref, confidence,
		                              valid_from, valid_until, verified_at, scope, scope_captured)
		SELECT ?, project_id, category, content, importance, source, tags,
		       created_at, id, access_count, last_accessed,
		       agent, session_id, source_ref, confidence,
		       valid_from, valid_until, verified_at, scope, 1
		FROM memories WHERE project_id = ? AND source NOT IN ('manual', 'builtin') AND pinned = 0 AND resolved_at IS NULL
		      AND `+retentionExemptSQL+`
	`, snapshotID, projectID)
	if err != nil {
		return nil, fmt.Errorf("snapshot memories: %w", err)
	}
	// The evidence those memories carry, in the second read rather than a column on
	// the snapshot row: a memory can have several records, and this is the only
	// copy that will exist by the time the rows are gone.
	//
	// It is here for the restore. A consolidation that rewrites a memory deletes
	// the row, and the foreign key takes its evidence with it, so a restore that
	// brought back only the text would return a memory that reads as never observed
	// — in a database that still had the evidence to return. Joining through the
	// snapshot's own rows bounds the read to the memories this snapshot covers, so
	// nothing outside the replaced corpus is copied.
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO memory_snapshot_evidence
			(snapshot_id, memory_id, kind, agent, session_id, source_ref, confidence, observed_at, verified_at)
		SELECT ?, s.memory_id, p.kind, p.agent, p.session_id, p.source_ref, p.confidence,
		       p.observed_at, p.verified_at
		FROM memory_snapshots s
		JOIN memory_provenance p ON p.memory_id = s.memory_id
		WHERE s.snapshot_id = ? AND s.memory_id IS NOT NULL
	`, snapshotID, snapshotID); err != nil {
		return nil, fmt.Errorf("snapshot memory evidence: %w", err)
	}

	// Identify which existing rows this replace may delete, and which emitted
	// memory can reuse one. A row whose content the consolidator re-emits
	// unchanged is updated in place instead of being deleted and re-inserted: a fresh ID
	// would cascade its memory_embeddings and memory_links away (both are ON
	// DELETE CASCADE), so identical content used to mean a re-embedded memory
	// and a lost link graph on every reflection. Rows saved concurrently with
	// the consolidation round trip are kept in place for the same reason.
	// ORDER BY pins the order takeReusableRow consumes, so which same-content
	// row a reuse claims is a decision and not whatever order the planner
	// happens to return (this predicate is served by idx_memories_project_cat
	// or idx_memories_project_source). created_at ascending means a duplicate
	// keeps the age of the oldest copy — the one the age belongs to — and id
	// breaks same-second ties deterministically. Before #623 the order was
	// irrelevant, because every reused row was re-stamped created_at = now
	// anyway; now it decides the age that survives.
	rows, err := tx.QueryContext(ctx, `
		SELECT id, content, category, importance, tags, scope FROM memories
		WHERE project_id = ? AND source NOT IN ('manual', 'builtin') AND pinned = 0 AND resolved_at IS NULL
		  AND `+retentionExemptSQL+`
		ORDER BY created_at, id
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list replaceable memories: %w", err)
	}
	var candidates []replaceCandidate
	for rows.Next() {
		var c replaceCandidate
		if err := rows.Scan(&c.id, &c.content, &c.category, &c.importance, &c.tags, &c.scope); err != nil {
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
			WHERE project_id = ? AND source NOT IN ('manual', 'builtin') AND pinned = 0 AND resolved_at IS NULL AND created_at >= ?
			  AND `+retentionExemptSQL+`
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

	// Content -> reusable rows. Concurrent rows are excluded: they are kept
	// as they are, never claimed by an emitted memory. The whole stored row
	// travels, not just its ID, because the reuse decisions read the stored
	// state rather than the emission alone — reusePreservesAge needs the
	// category, and reuseChangesNothing needs every field the reuse UPDATE
	// writes.
	reusable := make(map[string][]replaceCandidate)
	for _, c := range candidates {
		if concurrent[c.id] {
			continue
		}
		reusable[c.content] = append(reusable[c.content], c)
	}
	reuseFor := make(map[int]replaceCandidate, len(memories))
	for i, m := range memories {
		// Matching is exact, not trimmed: reuse preserves the existing
		// embedding, so it is only valid when the stored text is byte-identical
		// to what the consolidator emitted. A whitespace-only difference takes
		// the insert path instead, which leaves the memory to be re-embedded
		// rather than keeping a vector that no longer describes its content.
		if chosen, rest, ok := takeReusableRow(reusable[m.Content], m); ok {
			reuseFor[i] = chosen
			reusable[m.Content] = rest
		}
	}

	var deleteIDs []string
	for _, unmatched := range reusable {
		for _, c := range unmatched {
			deleteIDs = append(deleteIDs, c.id)
		}
	}
	// The emissions run BEFORE the delete below, which is a reordering with a
	// reason: a rewrite or a merge carries its sources' evidence onto the row it
	// becomes, and that copy has to READ those rows. The foreign key takes them
	// the moment the source memory is deleted, and there is no second copy — the
	// snapshot holds the pre-replace corpus, not the post-rewrite one. So the carry
	// is the constraint, and the delete waits for it.
	//
	// Nothing else in the two blocks depends on the order. The delete set was
	// decided above, from the candidates takeReusableRow did NOT claim, so a row
	// this loop updates in place is never one the delete removes; the fresh inserts
	// mint their own ids and collide with nothing; and every statement here runs
	// inside one transaction that already holds the write lock, so no other writer
	// can see the interval.
	// reflected is every row this pass WROTE, collected so the history appends
	// happen in one place at the end: the ids of the fresh inserts are only known
	// as they are made, and interleaving the two statements per row would triple
	// the statement count for no gain. A verbatim re-emission leaves the list
	// (#727) — it is not a write, so there is no version to record.
	var reflected []string
	reused := 0
	// successorOf maps the ids this pass consumed to the row that now holds
	// their content, so the consumed rows' delete history can say where their
	// knowledge went. Only ids the caller declared (Memory.ReplacesIDs, which the
	// reflection operations fill in) are eligible: a row this pass deleted for
	// any other reason — a drop, a concurrent save it outran — has no successor
	// and must not be given one.
	successorOf := map[string]string{}
	for i, m := range memories {
		tags, _ := json.Marshal(m.Tags)
		if stored, reusedRow := reuseFor[i]; reusedRow {
			id := stored.id
			// created_at is reset deliberately: consolidated knowledge counts
			// as refreshed (issue #279). The row identity — and with it its
			// embeddings, links, and access stats — survives.
			// A replacement that states no scope must not erase the scope the
			// live row already carries. `ghost reflect --apply` is the only
			// caller and reflection.ReflectMemory has no machine-readable
			// scope, so every emitted memory arrives here with Scope nil: with
			// a plain `scope = ?` the reuse path NULLed out a real scope on
			// every reflection that re-emitted a scoped fact verbatim, and
			// nothing else about the row moved to make it visible. COALESCE
			// keeps "not stated" reading as "leave it alone" and still lets a
			// replacement that does state a scope rewrite it. There is no
			// clear-the-scope operation anywhere in the API, so nothing can
			// express "deliberately unscoped" and the ambiguity is not
			// hiding a real case.
			// Two exceptions: reusePreservesAge (#623) below, and
			// reuseChangesNothing (#727), which skips the write entirely.
			switch {
			case reuseChangesNothing(stored, m):
				// A verbatim re-emission is not a write (#727). The row keeps
				// its updated_at, and the pass records no version of it, because
				// there is no new state to record. Measured on a real store, the
				// row-per-run this replaced was 80% of memory_history and put
				// both caps days from evicting real events; updated_at became
				// "the last reflect that saw this row", which is what supersede
				// orients a candidate pair by and what --skip-unchanged's
				// fingerprint carries. Which run touched it is not state, and
				// the run is already in lifecycle.log.
			case reusePreservesAge(stored, m):
				// Unchanged re-emission, not a rewrite: the row keeps its
				// created_at and its source, and only the fields reflection
				// actually restated move. See reusePreservesAge.
				if _, err := tx.ExecContext(ctx, `
					UPDATE memories
					SET category = ?, content = ?, importance = ?, tags = ?,
					    scope = COALESCE(?, scope),
					    updated_at = datetime('now')
					WHERE id = ?
				`, m.Category, m.Content, m.Importance, string(tags), scopeJSON(m.Scope), id); err != nil {
					return nil, fmt.Errorf("update retained memory: %w", err)
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
					return nil, fmt.Errorf("update reused memory: %w", err)
				}
				reflected = append(reflected, id)
			}
			reused++
			for _, replaced := range m.ReplacesIDs {
				successorOf[replaced] = id
			}
			continue
		}
		// The claims the row stands in for, resolved from the rows it replaces. The
		// insert below used to list none of these columns, so a rewrite or a merge
		// dropped a source row's window, its verification, its confidence and its
		// reference while carryEvidenceTx carried that row's EVIDENCE forward — which
		// is how a row comes to read verified_at NULL beside evidence that says
		// somebody checked it, and a reader comparing the two finds the table
		// contradicting the memory it describes.
		//
		// From the SOURCES, not from the emission, because an emission carries none:
		// reflectMemoriesToMemory builds it from the consolidator's output and has
		// nothing to copy a window from, so binding m's columns here would store
		// NULL every time and fix nothing. It is also the rule the reuse branch
		// above already follows — an emission inherits rather than invents — and the
		// one carryEvidenceTx follows for the records.
		//
		// Read here because the delete below is what takes the sources, and this loop
		// runs before it: the same reordering the carry already depends on.
		claims, err := inheritedClaims(ctx, tx, projectID, m)
		if err != nil {
			return nil, err
		}
		var newID string
		if err = tx.QueryRowContext(ctx, `
			INSERT INTO memories (project_id, category, content, source, importance, tags, scope,
			                      valid_from, valid_until, verified_at,
			                      confidence, agent, session_id, source_ref)
			VALUES (?, ?, ?, 'reflection', ?, ?, ?,
			        ?, ?, ?, ?, ?, ?, ?)
			RETURNING id
		`, projectID, m.Category, m.Content, m.Importance, string(tags), scopeJSON(m.Scope),
			nullIfEmptyPtr(claims.validFrom), nullIfEmptyPtr(claims.validUntil), nullIfEmptyPtr(claims.verifiedAt),
			claims.confidence, nullIfEmpty(claims.agent), nullIfEmpty(claims.sessionID),
			nullIfEmpty(claims.sourceRef)).Scan(&newID); err != nil {
			return nil, fmt.Errorf("insert memory: %w", err)
		}
		// The support this emission was consolidated FROM, carried onto the row that
		// now holds the text. Without it a rewrite or a merge mints an id whose
		// evidence the foreign key has just deleted, and every consolidated memory
		// reads "no recorded evidence" from then on — the one write path that runs
		// unattended over the whole corpus, silently forgetting the whole corpus's
		// support.
		//
		// The reuse branch above needs none of this: it updates a row in place, so
		// that row's evidence never left it. And an emission with no ReplacesIDs
		// carries nothing, because a brand-new memory has no inherited support and
		// "none" is the answer.
		if err := carryEvidenceTx(ctx, tx, newID, m.ReplacesIDs); err != nil {
			return nil, err
		}
		reflected = append(reflected, newID)
		for _, replaced := range m.ReplacesIDs {
			successorOf[replaced] = newID
		}
	}

	if len(deleteIDs) > 0 {
		// Recorded before the rows go, and in the same transaction, so the
		// consolidation's dropped memories keep the text that explains why
		// they are missing. This is the writer that makes an unbounded history
		// a real concern rather than a theoretical one: one applied reflect can
		// drop and rewrite the whole non-manual corpus at once.
		if err := appendHistoryForIDsTx(ctx, tx, deleteIDs, phaseDelete, Provenance{}); err != nil {
			return nil, err
		}
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

	// One append for every row this pass actually WROTE, reused and fresh
	// alike: from outside, a reflection rewrite and the row it rewrote are the
	// same event, and phaseReflect is what answers "which consolidation run
	// changed this". A verbatim re-emission is not in reflected (#727): it wrote
	// nothing, and a version row records the state a write left behind.
	if err := appendHistoryForIDsTx(ctx, tx, reflected, phaseReflect, Provenance{}); err != nil {
		return nil, err
	}
	// Now that every emission's final id is known, close the chain: each consumed
	// row's delete row names the row that replaced it. Without this a rewrite or a
	// merge ends one memory's history at a wall, because the row it became has a
	// different id and nothing says so.
	for oldID, newID := range successorOf {
		if err := linkSuccessorTx(ctx, tx, oldID, newID); err != nil {
			return nil, err
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
	// The evidence goes with the snapshot it belongs to, and the same statement
	// shape: memory_snapshot_evidence has no foreign key to cascade from (the
	// snapshots it describes are addressed by (snapshot_id, memory_id), which is
	// not a key), so a pruned snapshot's evidence would otherwise accumulate
	// forever — the only unbounded growth this table would have had.
	if _, err = tx.ExecContext(ctx, `
		DELETE FROM memory_snapshot_evidence
		WHERE snapshot_id NOT IN (SELECT DISTINCT snapshot_id FROM memory_snapshots)
	`); err != nil {
		s.logger.Warn("prune old snapshot evidence", "error", err, "project_id", projectID)
	}

	if s.logger != nil {
		s.logger.Info("memories snapshotted before replace", "project_id", projectID, "snapshot_id", snapshotID)
	}
	if ownTx {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit replace: %w", err)
		}
		lock.reportHold("replace", time.Now())
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

	tx, _, err := s.beginWrite(ctx, "restore-snapshot")
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
	//
	// The rows going are selected before they are deleted, by the identical
	// predicate, so the delete tombstones in memory_history name exactly the
	// rows this statement removes. One transaction has held the write lock
	// since its first statement, so the two selects cannot disagree.
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
		return 0, fmt.Errorf("find replace output to remove: %w", err)
	}
	if err := appendHistoryForIDsTx(ctx, tx, removedIDs, phaseDelete, Provenance{}); err != nil {
		return 0, err
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
	//
	// Which rows the UPDATE will actually change is read FIRST, because the
	// history append below runs after it and needs the set. Restore is explicitly
	// repeatable — the snapshot is deliberately kept and a second run must leave
	// the corpus alone — so the id set cannot be re-derived afterwards: by then
	// every row matches the snapshot, and a row a previous run already restored
	// looks exactly like one this run restores. The same join with a difference
	// predicate on the recorded columns is what separates them, and it is decided
	// before the write so the append still records the restored state.
	//
	// Only the recorded columns gate it (content, category, importance, source).
	// A restore that revives nothing but a row's tags, age or provenance changes
	// no state the history holds, so it records nothing — the rule every other
	// writer follows.
	changedIDs, err := selectIDs(ctx, tx, `
		SELECT s.memory_id FROM memory_snapshots s
		JOIN memories m ON m.id = s.memory_id
		WHERE s.snapshot_id = ? AND s.memory_id = m.id
		  AND m.pinned = 0 AND m.resolved_at IS NULL
		  AND m.`+retentionExemptSQL+`
		  AND (s.content != m.content OR s.category != m.category
		       OR s.importance != m.importance OR s.source != m.source)`, snapshotID)
	if err != nil {
		return 0, fmt.Errorf("find rows this restore changes: %w", err)
	}
	updated, err := tx.ExecContext(ctx, `
		UPDATE memories SET
		    category = s.category, content = s.content, importance = s.importance,
		    source = s.source, tags = s.tags, created_at = s.created_at,
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
		    scope = CASE WHEN s.scope_captured = 1 THEN s.scope ELSE memories.scope END
		FROM memory_snapshots s
		WHERE s.snapshot_id = ? AND s.memory_id = memories.id
		  AND memories.pinned = 0 AND memories.resolved_at IS NULL
		  AND memories.`+retentionExemptSQL+`
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
	//
	// RETURNING rather than RowsAffected because the history rows below need
	// the ids, and a pre-v13 snapshot mints a fresh one per restored row — so
	// they cannot be known before the statement runs.
	insertedRows, err := tx.QueryContext(ctx, `
		INSERT INTO memories (id, project_id, category, content, source, importance, tags,
		                      created_at, updated_at, access_count, last_accessed,
		                      agent, session_id, source_ref, confidence,
		                      valid_from, valid_until, verified_at, scope)
		SELECT COALESCE(memory_id, hex(randomblob(16))), project_id, category, content,
		       source, importance, tags, created_at, created_at, access_count, last_accessed,
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
		       CASE WHEN scope_captured = 1 THEN scope ELSE NULL END
		FROM memory_snapshots
		WHERE snapshot_id = ?
		  AND ((memory_id IS NOT NULL AND memory_id NOT IN (SELECT id FROM memories))
		       OR (memory_id IS NULL
		           AND NOT EXISTS (SELECT 1 FROM memories m
		                           WHERE m.project_id = memory_snapshots.project_id
		                             AND m.content = memory_snapshots.content
		                             AND m.source = memory_snapshots.source)))
		RETURNING id
	`, snapshotID)
	if err != nil {
		return 0, fmt.Errorf("restore snapshot: %w", err)
	}
	var reinserted []string
	for insertedRows.Next() {
		var id string
		if err := insertedRows.Scan(&id); err != nil {
			insertedRows.Close() //nolint:errcheck
			return 0, fmt.Errorf("scan restored memory id: %w", err)
		}
		reinserted = append(reinserted, id)
	}
	if err := insertedRows.Err(); err != nil {
		insertedRows.Close() //nolint:errcheck
		return 0, fmt.Errorf("iterate restored memories: %w", err)
	}
	insertedRows.Close() //nolint:errcheck
	insertedN := int64(len(reinserted))

	// The evidence of the rows that were genuinely GONE, from the snapshot that
	// carried it. Only the reinserted half needs it: a row restored IN PLACE was
	// never deleted, so its evidence was never in danger, and copying it would
	// double its records on every repeated restore.
	//
	// The copy replaces rather than appends, and it is scoped to the ids the
	// INSERT above just created — so it cannot touch a row that came back earlier
	// or one this restore left alone. The records come back verbatim, with
	// carried_from empty: a restore puts a row back where it was, under its own id,
	// so its evidence is its own again rather than a successor's.
	//
	// A pre-v13 snapshot recorded no memory_id, so it carries no evidence to
	// restore and the row comes back with none. That is the truth — this database
	// never recorded the support of a row it could not name — and the alternative
	// would be inventing it.
	if len(reinserted) > 0 {
		if err := restoreSnapshotEvidenceTx(ctx, tx, snapshotID, reinserted); err != nil {
			return 0, err
		}
	}

	// One history row per row this restore actually changed, in the same
	// transaction: without them the corpus reads as though the replace never
	// happened, and the state a restore reverted FROM is unrecoverable from the
	// history. The in-place restorers are the set read before the UPDATE, and the
	// reinserted rows report theirs above — a pre-v13 snapshot mints a fresh id
	// per restored row, so that half cannot be known before its statement runs.
	//
	// A row an earlier restore already put back is not in the set, so re-running
	// a restore appends nothing. The event says the row was restored; appending it
	// again per run would spend one of the per-memory version slots the growth
	// policy keeps on a row that changed nothing.
	restored := make([]string, 0, len(changedIDs)+len(reinserted))
	restored = append(restored, changedIDs...)
	for _, id := range reinserted {
		// An id can be in both sets only if the INSERT re-created a row the
		// UPDATE had just restored, which the mutually exclusive predicates
		// above make impossible — and a duplicate would append the same event
		// twice, so it is dropped rather than assumed away.
		if !slices.Contains(changedIDs, id) {
			restored = append(restored, id)
		}
	}
	if err := appendHistoryForIDsTx(ctx, tx, restored, phaseRestore, Provenance{}); err != nil {
		return 0, err
	}

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

	// A guarded transaction, for the same reason CreateTask is one (#746):
	// `RETURNING interaction_count` is a read, and the value comes back from
	// the UPDATE that produced it. The `+ 1` stays atomic — it is one statement,
	// and the transaction here exists to run the newer-store check, not to make
	// the arithmetic atomic, which SQLite already guarantees for a single
	// statement.
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
//
// Both fields are model-written text, and both are injected verbatim into every
// later session for the project, so they are guarded like any other save.
// A refusal here is the cheap outcome: the caller is the reflect command,
// which already treats a failure on this write as a warning and carries on
// (cmd/ghost/lifecycle.go), so a hallucinated credential costs one unrecorded
// summary rather than the whole consolidation — the memories it summarised have
// already been applied by this point.
func (s *Store) UpdateLearnedContext(ctx context.Context, projectID, learnedContext, summary string) error {
	if err := rejectSecret("learned_context", learnedContext); err != nil {
		return err
	}
	if err := rejectSecret("reflection_summary", summary); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.execGuardedWrite(ctx, "update-learned-context", `
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

// memoryColumns is the column list every full-row reader selects, in the order
// scanMemories scans them. One list for six queries: a column added to Memory and
// forgotten in one reader hydrates as a zero value in that reader and a real one
// everywhere else, which reads as a bug in the column rather than as the omission
// it is — and the omission is silent precisely where it matters, because a
// session row that reads as "no tier stated" is one a prune cannot see.
//
// A reader that selects a SUBSET on purpose (the portable artifact, an as_of
// version, a snapshot) keeps its own list and its own scanner; this one is for
// the readers that hydrate a whole Memory.
var memoryColumnNames = []string{
	"id", "project_id", "category", "content", "importance", "access_count",
	"last_accessed", "source", "tags", "pinned", "resolved_at", "created_at", "updated_at",
	"agent", "session_id", "source_ref", "confidence", "scope",
	"valid_from", "valid_until", "verified_at",
	"retention", "expires_at",
}

// memoryColumnsPrefixed is the same list with every column qualified, for the
// two FTS queries that alias memories as m (and therefore cannot say `scope` or
// `retention` unqualified beside the FTS table). Derived from the one list for
// the reason that list exists.
var memoryColumnsPrefixed = qualifyColumns("m")

func qualifyColumns(prefix string) string {
	out := make([]string, len(memoryColumnNames))
	for i, c := range memoryColumnNames {
		if prefix != "" {
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
		// Validity columns are read the way they are stored: SQLite holds them
		// as unconstrained text, and deciding what a value means needs the
		// request clock, which lives above this layer. NULL stays nil so "no
		// claim" cannot be confused with a claim about the empty string.
		if validFrom.Valid {
			m.ValidFrom = &validFrom.String
		}
		if validUntil.Valid {
			m.ValidUntil = &validUntil.String
		}
		if verifiedAt.Valid {
			m.VerifiedAt = &verifiedAt.String
		}
		// Same rule as the validity triple: the expiry is the stored string, and
		// NULL stays nil because "no expiry is claimed" is the claim that keeps a
		// row out of a prune's candidate set.
		if expiresAt.Valid {
			m.ExpiresAt = &expiresAt.String
		}
		// A row whose tier reads empty is a row a query did not select, not a
		// fourth tier. Resolving it here — once — means no reader downstream has
		// to know that shape exists, and the one value it resolves to is the one
		// that costs nothing: a row nobody classified behaves as the corpus
		// always has.
		if m.Retention == "" {
			m.Retention = RetentionProject
		}

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
	terms := ftsQueryTerms(text, maxWords)
	if len(terms) == 0 {
		return `""`
	}
	words := make([]string, len(terms))
	for i, t := range terms {
		words[i] = t.text
	}
	return strings.Join(words, " OR ")
}

// ftsQueryTerms is the term extraction sanitizeFTSN emits, one step earlier: the
// cleaned terms, their selection values and the prefix flag, capped at maxWords
// SELECTED by value. It is a function because two callers need the terms
// themselves rather than an FTS5 query string, and a second extraction would be
// free to disagree with the first about which terms a query has.
//
// The only other caller is the historical keyword matcher (asof.go), which
// cannot hand a query to FTS5: the index holds CURRENT content, and a read at T
// has to match the text as it stood at T. It reuses the term selection and the
// phrase/prefix semantics here, so an as_of query selects the same terms a
// current one would.
func ftsQueryTerms(text string, maxWords int) []ftsTerm {
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
				text:   term,
				clean:  clean,
				value:  ftsTermValue(clean),
				pos:    len(terms),
				prefix: prefix,
			})
		}
	}
	if len(terms) == 0 {
		return nil
	}
	// Select the maxWords highest-value terms (stopwords only fill the cap
	// when content words can't) instead of truncating the tail positionally.
	if len(terms) > maxWords {
		slog.Warn("fts query truncated",
			"original_terms", len(terms),
			"limit", maxWords)
		terms = selectFTSTERMs(terms, maxWords)
	}
	return terms
}
