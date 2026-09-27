package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// PortableProject is a project as the export/import artifact carries it.
//
// It is a distinct type from Project rather than a reuse of it because the
// artifact is a wire format, not a view of a row: it adds the repository remote
// (which is what makes two checkouts of one repository one project, and what
// another machine needs to resolve the project from its own directory) and it
// keeps the timestamps, which no read of the projects table returns today.
type PortableProject struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Name       string `json:"name"`
	RepoRemote string `json:"repo_remote,omitempty"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// PortableMemory is a memory as the artifact carries it: every column of the
// memories table that describes the memory itself.
//
// Deliberately absent, and each for a reason a reader can check:
//
//   - memory_embeddings. The vector is derived from the content by a local
//     model, not stored knowledge; the async embedding worker rebuilds it for
//     every imported memory, and shipping a blob would be shipping a model's
//     output as if it were the memory.
//   - memory_links. A link is only meaningful between two memories that are
//     both present, so importing edges ahead of their endpoints would either
//     fail the foreign key or fabricate relationships. The linking worker
//     recomputes related edges after import.
//   - resolve_kept_hash. A cache of the resolve classifier's KEEP verdicts,
//     keyed by content hash. Ghost recomputes it, and a stale one would mark a
//     memory it has never classified as reviewed.
//
// Every other column is here, including the ones Memory does not expose:
// access_count, last_accessed, the validity triple (valid_from, valid_until,
// verified_at) and the pinned flag. Those are what a restore has to put back —
// restoring a memory with a fresh created_at would age it out of injection
// immediately, and restoring it unpinned would silently drop a user's pin.
type PortableMemory struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Category  string `json:"category"`
	Content   string `json:"content"`
	// Importance is a pointer because 0 is a real rating and an absent field is
	// not: `Store.Create` binds a Memory's importance with no default, so a
	// memory saved without one is stored as 0 and exported as `"importance":0`.
	// A plain float64 cannot tell that from a record that states nothing, and
	// folding the two together would promote a 0 to the column's default on every
	// re-import — a silent rewrite of exactly the kind the artifact exists to
	// avoid. nil therefore takes the column default, and only nil.
	Importance   *float64          `json:"importance,omitempty"`
	AccessCount  int               `json:"access_count"`
	LastAccessed *string           `json:"last_accessed,omitempty"`
	Source       string            `json:"source"`
	Tags         []string          `json:"tags,omitempty"`
	Pinned       bool              `json:"pinned"`
	CreatedAt    string            `json:"created_at"`
	UpdatedAt    string            `json:"updated_at"`
	ResolvedAt   *string           `json:"resolved_at,omitempty"`
	ValidFrom    *string           `json:"valid_from,omitempty"`
	ValidUntil   *string           `json:"valid_until,omitempty"`
	VerifiedAt   *string           `json:"verified_at,omitempty"`
	Agent        string            `json:"agent,omitempty"`
	SessionID    string            `json:"session_id,omitempty"`
	SourceRef    string            `json:"source_ref,omitempty"`
	Confidence   *float64          `json:"confidence,omitempty"`
	Scope        map[string]string `json:"scope,omitempty"`
}

// ImportOptions is what one import run is doing with the records it is given.
type ImportOptions struct {
	// Apply writes. False is a dry run: every check that does not depend on what
	// the same run has already written runs, the action that would be taken is
	// returned, and nothing is written.
	Apply bool

	// TrustProvenance keeps each memory's own source and pin state instead of
	// downgrading them. It is off by default, and the default is the safe one to
	// ship: an artifact is a file that arrived from somewhere, and on its own
	// authority it gets to say that a row is `manual` (the user's own words, and
	// exempt from consolidation) or `builtin` (a rule Ghost ships, which
	// CanonicalOriginSource presents as such) or `pinned` (exempt from
	// consolidation whatever its source). A file a user downloaded would then be
	// able to plant rows that read as the user's own material and as Ghost's own
	// rules, permanently exempt from consolidation and indistinguishable from
	// rows Ghost wrote itself. Off, every imported memory is stamped
	// `onboarding` and unpinned — the same source internal/claudeimport uses for
	// memories imported from outside Ghost at first contact — which keeps it
	// consolidatable and honest about where it came from.
	//
	// A user importing their OWN export wants the fidelity, and gets it with
	// this flag. That asymmetry is deliberate: the cost of the default being
	// wrong is planted provenance, and the cost of the flag being wrong is a
	// user who has to pass it once.
	TrustProvenance bool
}

// DowngradedSource is the source every imported memory is stamped with unless
// the run was told to trust the artifact's own. `onboarding` is the value
// internal/claudeimport already uses for memories brought in from outside Ghost
// at first contact, and it is the only one of the eight that means "this did not
// originate in a session" without also claiming authorship. It is in the
// memories table's CHECK already, so downgrading needs no migration.
const DowngradedSource = "onboarding"

// validMemorySources is the set the memories table's source CHECK accepts, and
// is what ImportMemory validates against. The CHECK is the schema's own truth;
// this map is the same list spelled out so an import can name the offending
// value instead of failing a statement with SQLite's own wording.
var validMemorySources = map[string]bool{
	"reflection":   true,
	"chat":         true,
	"manual":       true,
	"tool":         true,
	"mcp":          true,
	"onboarding":   true,
	"decision_log": true,
	"builtin":      true,
}

// IsValidSource reports whether src is one of the source values the memories
// table accepts.
func IsValidSource(src string) bool { return validMemorySources[src] }

// validTaskStatuses and validDecisionStatuses mirror the corresponding CHECK
// constraints, for the same reason as validMemorySources.
var (
	validTaskStatuses = map[string]bool{
		"pending": true, "active": true, "done": true, "blocked": true,
	}
	validDecisionStatuses = map[string]bool{
		"active": true, "superseded": true, "revisit": true,
	}
)

// PortableProjects returns every project in id order.
//
// The order is not cosmetic: it makes an export of an unchanged database
// byte-identical, which is what lets a user diff two artifacts or keep a
// checksum over one. ListProjects orders by name instead, which is the right
// order for a directory listing and the wrong one for a diff.
func (s *Store) PortableProjects(ctx context.Context) ([]PortableProject, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, path, name, COALESCE(repo_remote, ''), created_at, updated_at
		FROM projects ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("portable projects: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []PortableProject
	for rows.Next() {
		var p PortableProject
		if err := rows.Scan(&p.ID, &p.Path, &p.Name, &p.RepoRemote, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PortableMemories returns the portable form of the memories in projectIDs, in
// id order. A nil or empty projectIDs means every project, which is what an
// unfiltered export asks for.
func (s *Store) PortableMemories(ctx context.Context, projectIDs []string) ([]PortableMemory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT id, project_id, category, content, importance, access_count,
		       last_accessed, source, tags, pinned, created_at, updated_at,
		       resolved_at, valid_from, valid_until, verified_at,
		       agent, session_id, source_ref, confidence, scope
		FROM memories`
	// Ghost's own seeds are excluded, and the reason is that no import could ever
	// reconcile them: SeedGlobalMemories writes each seed under the schema's
	// default id, hex(randomblob(16)), which is per-install, so the artifact's
	// copy never equals the destination's own. Every importer dedups by id alone,
	// so a cross-machine import inserts a second row with byte-identical content
	// — with --trust-provenance a second pinned builtin copy of Ghost's shipped
	// rule, surfaced to every session as a rule it already has; by default an
	// onboarding copy that has silently lost the "this is Ghost's own" label,
	// which is the exact downgrade the provenance policy exists to prevent.
	// Nothing repairs it: SeedGlobalMemories skips by content, and no path
	// deletes memories, so the duplicate is permanent.
	//
	// Excluding at the read is the cheap side of the fix. It keeps the import rule
	// simple (dedup by id) and matches what the destination already does on every
	// open — it writes exactly these rows by content — so a restored store ends up
	// with one copy, written by Ghost, not two.
	//
	// The exclusion is on the seed (project AND source), not on the project: a
	// user's own memory filed under _global is the only copy of itself and has no
	// other way in.
	const seedExclusion = ` AND NOT (project_id = '_global' AND source = 'builtin')`
	var args []any
	if len(projectIDs) > 0 {
		// One "?" per id and the ids themselves only ever in args: the query
		// text is assembled from placeholders alone, never from a value.
		placeholders := make([]string, len(projectIDs))
		for i, id := range projectIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		query += " WHERE project_id IN (" + strings.Join(placeholders, ",") + ")" + seedExclusion
	} else {
		query += " WHERE 1 = 1" + seedExclusion
	}
	// Ordered by id for the same byte-reproducibility reason as the projects.
	query += " ORDER BY id"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("portable memories: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []PortableMemory
	for rows.Next() {
		m, err := scanPortableMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// projectCollision reports whether a *different* project already records p's
// path or its repository remote, and says which.
//
// `projects.path` is UNIQUE and `projects.repo_remote` carries a partial UNIQUE
// index created on every open, so a project cannot be inserted beside another
// with the same checkout or the same repository. The remote is compared in
// normalized form because that is the form stored, and an artifact carrying a
// raw `git@host:owner/repo.git` has to collide with the canonical spelling of
// the same repository rather than slip past it.
func (s *Store) projectCollision(ctx context.Context, p PortableProject) error {
	remote := NormalizeRepoRemote(p.RepoRemote)
	// `repo_remote <> ''` is what keeps an absent — or unrecognizable, since
	// NormalizeRepoRemote answers "" for a bare host or a filesystem path — from
	// matching every project that has no repository. `EnsureProject` binds "" for
	// one, and the rest of the store already reads NULL and '' as the same "no
	// remote" (the partial UNIQUE index excludes them for exactly this reason).
	// Without the guard an ordinary import of a remote-less project would be
	// refused, naming an unrelated project and an empty repository.
	var byPath, foundRemote string
	err := s.db.QueryRowContext(ctx, `
		SELECT
			coalesce((SELECT id FROM projects WHERE path = ? AND id != ? LIMIT 1), ''),
			coalesce((SELECT id FROM projects
			          WHERE repo_remote = ? AND repo_remote <> '' AND id != ? LIMIT 1), '')
	`, p.Path, p.ID, remote, p.ID).Scan(&byPath, &foundRemote)
	if err != nil {
		return fmt.Errorf("import project %s: %w", p.ID, err)
	}
	switch {
	case byPath != "":
		return fmt.Errorf("project %s cannot be imported: this Ghost already records %s as project %s — import into that project, or merge it with `ghost project merge`",
			p.ID, p.Path, byPath)
	case foundRemote != "":
		return fmt.Errorf("project %s cannot be imported: this Ghost already records repository %s as project %s — import into that project, or merge it with `ghost project merge`",
			p.ID, remote, foundRemote)
	}
	return nil
}

// ProjectForCheckout reports the id of a project that already records path or
// repoRemote, or "" when the store holds neither.
//
// It is what lets an artifact from another machine attach its records to the
// project this store already has for the same repository, instead of failing on
// the uniqueness constraint that the two would otherwise collide on. The remote
// is matched in normalized form, so two spellings of one repository are one
// answer, and a project with no remote at all is never matched on one.
func (s *Store) ProjectForCheckout(ctx context.Context, path, repoRemote string) (id, name string, err error) {
	remote := NormalizeRepoRemote(repoRemote)
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Path first, then remote: a recorded path is the stronger claim of the two —
	// it is the directory a session in it resolves to, while a remote only says
	// the two checkouts share a repository.
	if path != "" {
		err = s.db.QueryRowContext(ctx,
			`SELECT id, name FROM projects WHERE path = ? AND id != '_global' LIMIT 1`, path).Scan(&id, &name)
		if err == nil {
			return id, name, nil
		}
		if err != sql.ErrNoRows {
			return "", "", fmt.Errorf("lookup project by path %s: %w", path, err)
		}
		id, name = "", ""
	}
	if remote != "" {
		err = s.db.QueryRowContext(ctx,
			`SELECT id, name FROM projects WHERE repo_remote = ? AND id != '_global' LIMIT 1`, remote).Scan(&id, &name)
		if err == nil {
			return id, name, nil
		}
		if err != sql.ErrNoRows {
			return "", "", fmt.Errorf("lookup project by remote %s: %w", remote, err)
		}
	}
	return "", "", nil
}

// requireProject reports whether projectID exists, naming the record and the
// project when it does not. It is the check the three record importers make
// immediately before their write, so a record naming a project this store does
// not hold is refused with a message that says which field of which record is
// wrong rather than with SQLite's foreign-key wording.
func requireProject(ctx context.Context, db *sql.DB, kind, id, projectID string) error {
	var present int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, projectID).Scan(&present)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%s %s: project %q not found", kind, id, projectID)
	}
	if err != nil {
		return fmt.Errorf("%s %s: lookup project %q: %w", kind, id, projectID, err)
	}
	return nil
}

// rowScanner is the one method scanPortableMemory needs, satisfied by both
// *sql.Rows and *sql.Row.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanPortableMemory reads one memories row into the portable form. tags and
// scope arrive as JSON text and are decoded the same defensive way the search
// path decodes them: a malformed value costs that one field, never the record,
// because losing a whole memory over a side-channel field would be the wrong
// trade for a restore.
func scanPortableMemory(sc rowScanner) (PortableMemory, error) {
	var m PortableMemory
	var tagsJSON, scopeJSON sql.NullString
	var agent, sessionID, sourceRef sql.NullString
	var confidence sql.NullFloat64
	// The column is NOT NULL, so the scan target is a plain float64 and the
	// pointer is filled in afterwards: what nil means here is "this build could
	// not record one", which the column never allows.
	var importance float64
	if err := sc.Scan(
		&m.ID, &m.ProjectID, &m.Category, &m.Content, &importance, &m.AccessCount,
		&m.LastAccessed, &m.Source, &tagsJSON, &m.Pinned, &m.CreatedAt, &m.UpdatedAt,
		&m.ResolvedAt, &m.ValidFrom, &m.ValidUntil, &m.VerifiedAt,
		&agent, &sessionID, &sourceRef, &confidence, &scopeJSON,
	); err != nil {
		return PortableMemory{}, err
	}
	m.Importance = &importance
	if tagsJSON.Valid {
		_ = json.Unmarshal([]byte(tagsJSON.String), &m.Tags)
	}
	m.Agent = agent.String
	m.SessionID = sessionID.String
	m.SourceRef = sourceRef.String
	if confidence.Valid {
		c := confidence.Float64
		m.Confidence = &c
	}
	m.Scope = parseScope(scopeJSON)
	return m, nil
}

// ImportProject inserts a project that the store does not have, and reports
// created=false when the id is already present.
//
// It never updates an existing row. A recorded path is a project's identity for
// every other command, and an artifact exported from another machine is the
// older of the two copies: rewriting a live project's path, name or remote from
// it would re-point a project at a checkout that is no longer its own, and would
// do so from the file most likely to be a stale copy.
//
// The project row is inserted directly rather than through EnsureProject, for
// two reasons the resolver-oriented writers cannot satisfy: the timestamps are
// the artifact's, and the id is the artifact's. Both are identity, not
// preferences, and re-deriving either would make a re-import of the same
// artifact create a second project.
func (s *Store) ImportProject(ctx context.Context, p PortableProject, apply bool) (created bool, err error) {
	if p.ID == "" {
		return false, fmt.Errorf("project id is required")
	}
	if p.Path == "" {
		return false, fmt.Errorf("project %s: path is required", p.ID)
	}
	if p.Name == "" {
		return false, fmt.Errorf("project %s: name is required", p.ID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, p.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("import project %s: %w", p.ID, err)
		}
	} else {
		return false, nil
	}
	// The two UNIQUE constraints projects carries are checked here, in both
	// modes, and reported by name. A bare "UNIQUE constraint failed" from the
	// INSERT is the worst possible diagnosis for the common case: project ids are
	// per-install, so an artifact exported on one machine lands on another that
	// already knows the same checkout or the same repository, and the collision
	// is the expected outcome rather than a mistake in the file.
	//
	// The remedy is not in this method's gift: adopting the colliding project is
	// a policy decision about where the records should go, so the caller makes it
	// (the portable importer resolves it before ever asking here). What this
	// guarantees is that a collision which survives that decision arrives as a
	// sentence naming the project it collides with, in the dry run as well as the
	// apply — so the preview cannot promise a create the write would refuse.
	if err := s.projectCollision(ctx, p); err != nil {
		return false, err
	}
	if !apply {
		return true, nil
	}

	// The remote is normalized on the way in, as every other project writer
	// does. The column is UNIQUE (schema v15), so the spelling matters: an
	// artifact that carried a raw `git@host:owner/repo.git` and stored it
	// verbatim would collide with the canonical spelling of the same repository
	// and fail the INSERT with a uniqueness error naming neither.
	//
	// created_at/updated_at default to now, so a record with no timestamp in the
	// artifact (a hand-written one) gets the current time rather than the
	// epoch — and an artifact written by this format always has one.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO projects (id, path, name, repo_remote, created_at, updated_at)
		VALUES (?, ?, ?, ?, COALESCE(NULLIF(?, ''), datetime('now')),
		                   COALESCE(NULLIF(?, ''), datetime('now')))
	`, p.ID, p.Path, p.Name, nullIfEmpty(NormalizeRepoRemote(p.RepoRemote)), p.CreatedAt, p.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("import project %s: %w", p.ID, err)
	}
	return true, nil
}

// ImportMemory inserts a memory under the id the artifact carries, and reports
// created=false when that id is already present. It never updates an existing
// row, for the reason ImportProject gives: the artifact is a copy to be restored
// from, and the row already in the store is whatever the user has since done
// with it.
//
// The record is validated exactly as an MCP save validates one, because an
// import is another way to reach the same table and must not be the way around
// its rules:
//
//   - category and source are checked against the schema's own value sets, so a
//     hand-edited artifact is rejected with a message naming the field rather
//     than failing a statement;
//   - content goes through ClampContent, so a long line from another machine is
//     cut at the same cap with the same marker a normal save would add — and cut
//     is reported so the caller can say so;
//   - importance is clamped to [0,1], the same bound a normal save applies.
//
// It does not run Upsert's near-duplicate probe. Upsert exists to stop a live
// save from adding a redundant row; a restore is not adding knowledge, it is
// putting back what was there, and folding two rows of the artifact into one
// would silently drop a memory the user chose to keep.
//
// apply=false performs every check and reports the action that would be taken
// without writing, which is what makes a dry run a faithful preview rather than
// a separate code path that can disagree with the real one.
//
// The one check a dry run cannot make here is whether the memory's project
// exists: only the caller knows what the same run has created or will create,
// and a caller importing an artifact in dependency order is exactly that. The
// portable package is such a caller, and it makes the check itself, so a dry run
// and the apply run it previews classify every record the same way.
func (s *Store) ImportMemory(ctx context.Context, m PortableMemory, opts ImportOptions) (created, clamped, downgraded bool, err error) {
	apply := opts.Apply
	if m.ID == "" {
		return false, false, false, fmt.Errorf("memory id is required")
	}
	if m.ProjectID == "" {
		return false, false, false, fmt.Errorf("memory %s: project_id is required", m.ID)
	}
	if m.Content == "" {
		return false, false, false, fmt.Errorf("memory %s: content is required", m.ID)
	}
	if !IsValidCategory(m.Category) {
		return false, false, false, fmt.Errorf("memory %s: invalid category %q — must be one of: architecture, decision, pattern, convention, gotcha, dependency, preference, fact", m.ID, m.Category)
	}
	if !IsValidSource(m.Source) {
		return false, false, false, fmt.Errorf("memory %s: invalid source %q — must be one of: reflection, chat, manual, tool, mcp, onboarding, decision_log, builtin", m.ID, m.Source)
	}

	content, cut := ClampContent(m.Content)
	// A stated importance is clamped to [0,1], the same bound a normal save
	// applies. An unstated one is passed as NULL for the COALESCE to default, and
	// a stated 0 stays 0 — see the field's comment for why the two must not be
	// folded together.
	var importance any
	if m.Importance != nil {
		clamped := *m.Importance
		if clamped < 0 {
			clamped = 0
		}
		if clamped > 1 {
			clamped = 1
		}
		importance = clamped
	}
	// Provenance is rewritten before anything else, so a dry run reports the
	// rewrite it would make and not the one the artifact asked for. The flag is
	// what stands between "I am importing someone's file" and "these are my own
	// rows" — see ImportOptions.TrustProvenance for what each of the two values
	// is protecting.
	source, pinned := m.Source, m.Pinned
	if !opts.TrustProvenance {
		source, pinned = DowngradedSource, false
		downgraded = true
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM memories WHERE id = ?`, m.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, false, false, fmt.Errorf("import memory %s: %w", m.ID, err)
		}
	} else {
		return false, false, false, nil
	}
	// A free id is not necessarily a NEW id. A memory deleted locally leaves its
	// recorded history behind (that is what the history is for), and the artifact
	// carries ids verbatim, so importing into an id that still has history would
	// splice two unrelated memories' records under one id: the old text under its
	// save/delete pair, then an import row for text that memory never held, with
	// the two sharing one per-memory version budget and one as_of answer (#647).
	// Refused, with the way out named — purging is the redaction path and is
	// exactly the operation that makes the id safe to reuse.
	var historic int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM memory_provenance WHERE memory_id = ? LIMIT 1`, m.ID,
	).Scan(&historic); err == nil {
		return false, false, false, fmt.Errorf(
			"import memory %s: that id has recorded history from a memory that was deleted here, "+
				"and importing would splice two memories' histories under one id — "+
				"purge it first with `ghost history purge %s`, or give the record a new id in the artifact",
			m.ID, m.ID)
	} else if err != sql.ErrNoRows {
		return false, false, false, fmt.Errorf("import memory %s: %w", m.ID, err)
	}
	if !apply {
		return true, cut, downgraded, nil
	}

	// The project is checked immediately before the write, so a memory naming a
	// project the store does not hold is reported as a missing project rather
	// than as a foreign-key failure at the INSERT.
	if err := requireProject(ctx, s.db, "memory", m.ID, m.ProjectID); err != nil {
		return false, false, downgraded, err
	}

	tags, _ := json.Marshal(m.Tags)
	// The timestamp and importance fallbacks live in the SQL, not in Go, for the
	// reason a bound parameter defeats them: created_at is
	// `NOT NULL DEFAULT (datetime('now'))` and importance defaults to 0.5, but
	// a value bound for a column never lets its default apply. Passing "" would
	// store the empty string, and julianday('') is NULL, so the whole decay
	// expression is NULL and the memory sorts last out of every ranked read —
	// invisible to recall while looking perfectly present in the store. So the
	// COALESCE is here, as it already is in the three sibling importers, and a
	// record that omits either field takes the column's own default.
	//
	// The insert and the history row that records it share a transaction, so an
	// imported memory cannot land without its origin. phaseImport rather than
	// phaseSave because the artifact is a file that arrived from somewhere, and
	// the audit has to be able to tell those rows apart from Ghost's own saves.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, downgraded, fmt.Errorf("import memory %s: begin tx: %w", m.ID, err)
	}
	defer tx.Rollback() //nolint:errcheck
	_, err = tx.ExecContext(ctx, `
		INSERT INTO memories (id, project_id, category, content, importance, access_count,
			last_accessed, source, tags, pinned, created_at, updated_at, resolved_at,
			valid_from, valid_until, verified_at, agent, session_id, source_ref,
			confidence, scope)
		VALUES (?, ?, ?, ?, COALESCE(?, 0.5), ?, ?, ?, ?, ?,
		        COALESCE(NULLIF(?, ''), datetime('now')),
		        COALESCE(NULLIF(?, ''), datetime('now')), ?,
		        ?, ?, ?, ?, ?, ?, ?, ?)
	`, m.ID, m.ProjectID, m.Category, content, importance, m.AccessCount,
		m.LastAccessed, source, string(tags), pinned,
		m.CreatedAt, m.UpdatedAt, m.ResolvedAt,
		m.ValidFrom, m.ValidUntil, m.VerifiedAt,
		nullIfEmpty(m.Agent), nullIfEmpty(m.SessionID), nullIfEmpty(m.SourceRef),
		m.Confidence, scopeJSON(m.Scope))
	if err != nil {
		return false, false, downgraded, fmt.Errorf("import memory %s: %w", m.ID, err)
	}
	if err := appendHistoryTx(ctx, tx, m.ID, phaseImport, Provenance{
		Agent:     m.Agent,
		SessionID: m.SessionID,
	}); err != nil {
		return false, false, downgraded, err
	}
	if err := tx.Commit(); err != nil {
		return false, false, downgraded, fmt.Errorf("import memory %s: commit: %w", m.ID, err)
	}
	if s.onSave != nil {
		// The embedding worker is told there is something new to vectorize. A
		// restored memory has no embedding row and the worker's periodic sweep
		// would find it anyway; this only makes it prompt, and it is the same
		// notification a normal save gives.
		s.onSave(m.ProjectID)
	}
	return true, cut, downgraded, nil
}

// ImportTask inserts a task under the artifact's id, reporting created=false
// when the id is present. Like ImportProject it never updates an existing row.
//
// status and priority are validated against the schema's own value sets before
// the write, so a hand-edited record is refused by name. blocked_by is inserted
// as given: the caller is responsible for having ordered the records so a
// blocker exists before the task that points at it (see the portable package's
// ordering), and a pointer to a task that is not in the artifact at all is
// dropped there rather than written as a foreign key that can never resolve.
//
// The project and the blocker are checked immediately before the write, for the
// reason ImportMemory gives.
func (s *Store) ImportTask(ctx context.Context, t Task, apply bool) (created bool, err error) {
	if t.ID == "" {
		return false, fmt.Errorf("task id is required")
	}
	if t.ProjectID == "" {
		return false, fmt.Errorf("task %s: project_id is required", t.ID)
	}
	if t.Title == "" {
		return false, fmt.Errorf("task %s: title is required", t.ID)
	}
	if !validTaskStatuses[t.Status] {
		return false, fmt.Errorf("task %s: invalid status %q — must be one of: pending, active, done, blocked", t.ID, t.Status)
	}
	if t.Priority < 0 || t.Priority > 4 {
		return false, fmt.Errorf("task %s: invalid priority %d — must be between 0 and 4", t.ID, t.Priority)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, t.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("import task %s: %w", t.ID, err)
		}
	} else {
		return false, nil
	}
	if !apply {
		return true, nil
	}

	if err := requireProject(ctx, s.db, "task", t.ID, t.ProjectID); err != nil {
		return false, err
	}
	// blocked_by is a self-reference, so a pointer to a task that is not present
	// is a reference that can never resolve. Naming it here is what turns a raw
	// SQLite foreign-key error into something that says which field of which
	// record is wrong.
	if t.BlockedBy != "" {
		var blocker int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, t.BlockedBy).Scan(&blocker); err != nil {
			if err == sql.ErrNoRows {
				return false, fmt.Errorf("task %s: blocked_by names task %q, which is not in this store", t.ID, t.BlockedBy)
			}
			return false, fmt.Errorf("import task %s: %w", t.ID, err)
		}
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tasks (id, project_id, title, description, status, priority,
			blocked_by, branch, pr_number, notes, created_at, updated_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, 0), NULLIF(?, ''),
			COALESCE(NULLIF(?, ''), datetime('now')),
			COALESCE(NULLIF(?, ''), datetime('now')),
			NULLIF(?, ''))
	`, t.ID, t.ProjectID, t.Title, t.Description, t.Status, t.Priority,
		nullIfEmpty(t.BlockedBy), nullIfEmpty(t.Branch), t.PRNumber, t.Notes,
		t.CreatedAt, t.UpdatedAt, t.CompletedAt)
	if err != nil {
		return false, fmt.Errorf("import task %s: %w", t.ID, err)
	}
	return true, nil
}

// ImportDecision inserts a decision under the artifact's id, reporting
// created=false when the id is present. It never updates an existing row.
//
// Unlike RecordDecision it writes no companion memory. That companion exists so
// a decision is retrievable by the same search path as everything else, and a
// restored decision already has that companion among the artifact's memories —
// writing a second one would double every decision on every import.
//
// status is validated against the schema's value set, and superseded_by is
// inserted as given: the caller orders the records so a superseding decision
// exists first, and drops a pointer to a decision that is not in the artifact.
// As with a task, the project and the reference are checked immediately before
// the write.
func (s *Store) ImportDecision(ctx context.Context, d Decision, apply bool) (created bool, err error) {
	if d.ID == "" {
		return false, fmt.Errorf("decision id is required")
	}
	if d.ProjectID == "" {
		return false, fmt.Errorf("decision %s: project_id is required", d.ID)
	}
	if d.Title == "" {
		return false, fmt.Errorf("decision %s: title is required", d.ID)
	}
	if d.Decision == "" {
		return false, fmt.Errorf("decision %s: decision is required", d.ID)
	}
	if d.Rationale == "" {
		return false, fmt.Errorf("decision %s: rationale is required", d.ID)
	}
	if !validDecisionStatuses[d.Status] {
		return false, fmt.Errorf("decision %s: invalid status %q — must be one of: active, superseded, revisit", d.ID, d.Status)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM decisions WHERE id = ?`, d.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("import decision %s: %w", d.ID, err)
		}
	} else {
		return false, nil
	}
	if !apply {
		return true, nil
	}

	if err := requireProject(ctx, s.db, "decision", d.ID, d.ProjectID); err != nil {
		return false, err
	}
	// Superseded_by is a self-reference checked for the same reason blocked_by is
	// on a task: a pointer to a decision this store does not hold can never
	// resolve, and the error has to name the field.
	if d.SupersededBy != "" {
		var superseder int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM decisions WHERE id = ?`, d.SupersededBy).Scan(&superseder); err != nil {
			if err == sql.ErrNoRows {
				return false, fmt.Errorf("decision %s: superseded_by names decision %q, which is not in this store", d.ID, d.SupersededBy)
			}
			return false, fmt.Errorf("import decision %s: %w", d.ID, err)
		}
	}

	alts, _ := json.Marshal(d.Alternatives)
	tags, _ := json.Marshal(d.Tags)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO decisions (id, project_id, title, decision, alternatives, rationale,
			status, superseded_by, tags, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?,
			COALESCE(NULLIF(?, ''), datetime('now')),
			COALESCE(NULLIF(?, ''), datetime('now')))
	`, d.ID, d.ProjectID, d.Title, d.Decision, string(alts), d.Rationale,
		d.Status, nullIfEmpty(d.SupersededBy), string(tags), d.CreatedAt, d.UpdatedAt)
	if err != nil {
		return false, fmt.Errorf("import decision %s: %w", d.ID, err)
	}
	return true, nil
}
