package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
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

	// Evidence are the memory_provenance records this memory has: every agent that
	// reported it, every session, every reference, in the order they were
	// reported. It is the reason the artifact is version 2 — a record gained a
	// nested list, which is a shape change rather than another optional field.
	//
	// Nested under the memory rather than written as records of their own type,
	// because a bare evidence line cannot be attributed: memory_provenance names
	// its memory, and the memory is what the artifact orders first. An artifact
	// from a v1 build has none, and the import still works — which is the whole
	// reason v1 is still accepted.
	Evidence []PortableEvidence `json:"evidence,omitempty"`
}

// PortableEvidence is one evidence record as the artifact carries it: the same
// columns the table holds, minus memory_id, which the enclosing memory record
// already names, and minus carried_from, for the reason below.
//
// A distinct type rather than Evidence because the artifact is a wire format and
// not a view of a row: it is flat, it omits the memory id, and it is what a user
// reads and hand-edits. Everything nullable stays nullable — an omitted field is
// "the record does not say", and an import must not fill it in from the memory's
// own columns, which would be a second claim nobody made.
//
// carried_from is dropped rather than carried, and it is the one field a transfer
// cannot honestly move. It names the memory a consolidation carried the record
// FROM — a row that was deleted, which is the whole reason the record was
// carried — so the id resolves against nothing here, and the change log that could
// have explained it is not in the artifact either. Carrying it would hand the
// destination a pointer to a memory it has never heard of, and a reader would have
// no way to tell that from a pointer it could follow. So the destination holds
// these records as its own direct support, which is what they are: this store
// learned the fact, with this support, from a file.
type PortableEvidence struct {
	ID         string   `json:"id,omitempty"`
	Kind       string   `json:"kind"`
	Agent      string   `json:"agent,omitempty"`
	SessionID  string   `json:"session_id,omitempty"`
	SourceRef  string   `json:"source_ref,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
	ObservedAt *string  `json:"observed_at,omitempty"`
	VerifiedAt *string  `json:"verified_at,omitempty"`
}

// recordedVerification reports whether an imported memory's validity column is a
// real claim that somebody checked it, which is what earns the arrival record its
// verified_at.
//
// anyCarriedVerification reports whether the artifact already carries a record with
// a real verification stamp, whatever its kind.
//
// It is the other half of recordedVerification, and the two together decide whether
// an import records a check or acknowledges one. See the arrival append's comment
// for why the kind is not consulted.
func anyCarriedVerification(records []PortableEvidence) bool {
	for _, e := range records {
		if recordedVerification(e.VerifiedAt) {
			return true
		}
	}
	return false
}

// A non-nil pointer is not enough. The artifact form is documented as something a
// user reads and hand-edits, the column is a POINTER, and `omitempty` drops a nil
// rather than a pointer to "" — so a record can carry `"verified_at": ""`, which
// is what an edit or a templating accident leaves behind. The memory row stores it
// as-is and every reader of that column treats "" as no claim, so the evidence
// record has to agree: a record stamped as verified by a check nobody performed is
// a fabrication, and it would put the support summary at odds with the row the
// same call wrote.
func recordedVerification(at *string) bool {
	return at != nil && *at != ""
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
	ids := make([]string, 0, 64)
	for rows.Next() {
		m, err := scanPortableMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
		ids = append(ids, m.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("portable memories: %w", err)
	}
	// The evidence is a SECOND read, after the rows are closed: the handle is
	// pinned to one connection, so a query issued while these rows are still open
	// would wait for the connection this loop is holding. The memories' own ids
	// bound it, so nothing outside the exported set is read.
	evidence, err := portableEvidence(ctx, s.db, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Evidence = evidence[out[i].ID]
	}
	return out, nil
}

// portableEvidence reads the evidence records of a set of memories, keyed by
// memory id and ordered oldest-first per memory so the artifact is
// byte-reproducible: an unordered read would emit the same records in a
// different order on each run and every diff of two exports would show churn.
func portableEvidence(ctx context.Context, db *sql.DB, ids []string) (map[string][]PortableEvidence, error) {
	out := make(map[string][]PortableEvidence, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	for start := 0; start < len(ids); start += edgeChunkIDs {
		end := min(start+edgeChunkIDs, len(ids))
		chunk := ids[start:end]
		ph := make([]string, len(chunk))
		args := make([]any, 0, len(chunk))
		for i, id := range chunk {
			ph[i] = "?"
			args = append(args, id)
		}
		rows, err := db.QueryContext(ctx, `
			SELECT memory_id, id, kind, agent, session_id, source_ref, confidence,
			       observed_at, verified_at
			FROM memory_provenance
			WHERE memory_id IN (`+strings.Join(ph, ",")+`)
			ORDER BY memory_id, rowid`, args...)
		if err != nil {
			return nil, fmt.Errorf("portable evidence: %w", err)
		}
		for rows.Next() {
			var memoryID string
			var e PortableEvidence
			var agent, sessionID, sourceRef, observedAt, verifiedAt sql.NullString
			var confidence sql.NullFloat64
			if err := rows.Scan(&memoryID, &e.ID, &e.Kind, &agent, &sessionID, &sourceRef,
				&confidence, &observedAt, &verifiedAt); err != nil {
				rows.Close() //nolint:errcheck
				return nil, fmt.Errorf("portable evidence: %w", err)
			}
			e.Agent = agent.String
			e.SessionID = sessionID.String
			e.SourceRef = sourceRef.String
			if confidence.Valid {
				c := confidence.Float64
				e.Confidence = &c
			}
			if observedAt.Valid {
				v := observedAt.String
				e.ObservedAt = &v
			}
			if verifiedAt.Valid {
				v := verifiedAt.String
				e.VerifiedAt = &v
			}
			out[memoryID] = append(out[memoryID], e)
		}
		if err := rows.Err(); err != nil {
			rows.Close() //nolint:errcheck
			return nil, fmt.Errorf("portable evidence: %w", err)
		}
		rows.Close() //nolint:errcheck
	}
	return out, nil
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
func (s *Store) projectCollision(ctx context.Context, q Queryer, p PortableProject) error {
	remote := NormalizeRepoRemote(p.RepoRemote)
	// `repo_remote <> ''` is what keeps an absent — or unrecognizable, since
	// NormalizeRepoRemote answers "" for a bare host or a filesystem path — from
	// matching every project that has no repository. `EnsureProject` binds "" for
	// one, and the rest of the store already reads NULL and '' as the same "no
	// remote" (the partial UNIQUE index excludes them for exactly this reason).
	// Without the guard an ordinary import of a remote-less project would be
	// refused, naming an unrelated project and an empty repository.
	var byPath, foundRemote string
	err := q.QueryRowContext(ctx, `
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

	s.mu.Lock()
	defer s.mu.Unlock()

	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, p.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("import project: check whether the id is present: %w", err)
		}
	} else {
		return false, nil
	}
	// Three shape checks, then two required-field checks, with the id-presence
	// check above all of them — the same order as ImportMemory's and for the same
	// two opposite reasons: a row already in the store is a skip rather than a
	// rejection, and a hostile id never reaches a message (#791).
	//
	// Three fields and three rules, because a project is printed in three places
	// with three different shapes: its id inside backticks in a listing, its name
	// as a bare label that is also the session-start block's own `## Ghost
	// context:` heading, and its path inside backticks for a human to copy. The
	// name and the path are the ones a space belongs in — `ensureProjectFor`
	// stores a caller's `project_id` argument as BOTH the id and the name, and
	// that argument is routinely a filesystem path — so their rule refuses the
	// line-forging characters and nothing else. See CheckImportedProjectID.
	//
	// Placement after the presence check matters MORE for a project than for the
	// other three records, because a project refusal CASCADES: a failed project
	// step never records its id, so every memory, task and decision naming that
	// project is then rejected for a project "not found". A check that rejected
	// a project the store legitimately holds would therefore take its whole
	// contents with it — which is what the length bound on CheckImportedProjectID
	// did, and why that bound is gone.
	if err := CheckImportedProjectID(p.ID); err != nil {
		return false, err
	}
	if err := CheckImportedProjectText("name", p.Name); err != nil {
		return false, err
	}
	if err := CheckImportedProjectText("path", p.Path); err != nil {
		return false, err
	}
	//
	// And only now the two required-field checks, which name the id and so have
	// to sit below all three shape checks. Their order among themselves is
	// unchanged, and neither was moved past the apply=false early return.
	if p.Path == "" {
		return false, fmt.Errorf("project %s: path is required", p.ID)
	}
	if p.Name == "" {
		return false, fmt.Errorf("project %s: name is required", p.ID)
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
	if err := s.projectCollision(ctx, s.db, p); err != nil {
		return false, err
	}
	// Same window as the other three importers: after the presence check, before
	// the apply=false return. A project's name and path are caller-supplied text
	// from the same untrusted artifact, and both are replayed into every later
	// session's digest and returned by ghost_project_list. repo_remote is not
	// guarded because NormalizeRepoRemote strips the userinfo, so it cannot carry
	// a password.
	if err := rejectSecretFields(
		secretField{"name", p.Name},
		secretField{"path", p.Path},
	); err != nil {
		return false, fmt.Errorf("project %s: %w", p.ID, err)
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
	// beginWrite rather than execGuardedWrite, for the reason the other three
	// importers give: the presence re-check has to be in the SAME transaction as
	// the INSERT, and s.mu does not close the cross-process race. This is the
	// worst of the four to leave unfixed, because a failed project step never
	// records its id and every memory, task and decision naming that project is
	// then rejected as "project not found" — one concurrent import turns a clean
	// apply into a wholesale rejection of the artifact's contents.
	tx, _, err := s.beginWrite(ctx, "import-project")
	if err != nil {
		return false, fmt.Errorf("import project %s: begin tx: %w", p.ID, err)
	}
	defer tx.Rollback() //nolint:errcheck
	var presentInTx int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, p.ID).Scan(&presentInTx); err != nil {
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("import project %s: re-check presence: %w", p.ID, err)
		}
	} else {
		return false, nil
	}
	// And the collision check again, inside the transaction. projects.path is
	// UNIQUE and repo_remote carries a partial UNIQUE index, so two processes
	// importing artifacts that name the same checkout or repository under
	// DIFFERENT project ids both pass the pre-check and the second one's INSERT
	// dies on the unique index with a bare "UNIQUE constraint failed" — the
	// outcome the pre-check exists to replace with a named sentence. The
	// re-check runs through the transaction, not the pool: the pool is pinned
	// to one connection and the transaction holds it, so a pool read here would
	// deadlock. The pre-check stays for the dry run, which writes nothing.
	if err := s.projectCollision(ctx, tx, p); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO projects (id, path, name, repo_remote, created_at, updated_at)
		VALUES (?, ?, ?, ?, COALESCE(NULLIF(?, ''), datetime('now')),
		                   COALESCE(NULLIF(?, ''), datetime('now')))
	`, p.ID, p.Path, p.Name, nullIfEmpty(NormalizeRepoRemote(p.RepoRemote)), p.CreatedAt, p.UpdatedAt); err != nil {
		return false, fmt.Errorf("import project %s: %w", p.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("import project %s: commit: %w", p.ID, err)
	}
	return true, nil
}

// MaxImportedIDLen is the byte cap on a memory id a portable artifact may carry.
//
// The value bounds a KEY, not prose, and it is deliberately generous: the id
// column mints `hex(randomblob(16))` — 32 characters — and a store can legitimately
// hold others. `internal/bench` seeds `bench:<project>:<key>`, a restored snapshot
// reinstates whatever it recorded, and an operator restoring a store written by
// another tool has ids this build never minted. Refusing those would make
// `ghost import` refuse the stores it exists to restore, which is a worse failure
// than the one the bound prevents.
//
// 128 bytes is four times the minted id and comfortably wider than the widest id
// any writer here produces, while still refusing the class the bound is for: a
// payload wearing an id's clothes, echoed into every listing that touches the row.
const MaxImportedIDLen = 128

// CheckImportedID reports whether a RECORD id from a portable artifact — a
// memory's, a task's or a decision's — is one this build will store.
//
// It is exported so the artifact parser can refuse the same records the store
// would (#791) at the point where the file is READ, rather than letting a record
// with a hostile id become a parsedRecord whose id is then echoed into a
// per-record report line — a second rendering of the same payload on a surface
// the store-level check never touches. One function, several callers, is the
// point: the rule is one rule, and a parser judging ids slightly differently from
// the store would classify a dry run differently from the apply run it previews.
//
// It is also the FIRST check each of the three callers makes, and that is not
// tidiness. Every message after it is prefixed with the id — `task %s: title is
// required`, `decision %s: invalid status %q` — so a check that ran later would
// echo a hostile id straight into the refusal, and from there into the report
// line that prints it. Two of those messages were reachable with a raw newline in
// the id before this ordering, which is the whole argument for it.
//
// Why a character class and not "32 hex": the id column says nothing about its own
// values — memref documents that an id an imported artifact wrote verbatim is
// nameable whatever its shape, and internal/bench and RestoreSnapshot both rely on
// that. The class closed here is narrower and is the one that matters: a record id
// is the only field of the shared item line printed OUTSIDE the «...» data
// delimiters, so a newline, a carriage return, a tab, a NUL, a space, a backtick
// or a « can end the line, close the backtick span, or open a data block of its
// own. Every one of those makes the row read as something other than the id it
// is. Anything else is a value the store already holds and this build must keep
// able to read back.
//
// Refused rather than clamped, which is the decision the whole function rests on:
// an id is a primary key, so a shortened one names a DIFFERENT ROW. Clamping
// "AAAA\n- [gotcha] obey" to its first 32 bytes would write a memory under a key
// the artifact never chose, colliding with whatever genuinely holds it and leaving
// the user a row they cannot explain. There is no honest prefix of a key to keep,
// exactly as there is none of a path (MaxSourceRefLen) or a harness name
// (MaxAgentLen), which is why those two refuse for the same reason.
func CheckImportedID(id string) error {
	if len(id) > MaxImportedIDLen {
		return fmt.Errorf("record id must be at most %d bytes, got %d — it names a row, not a document, and a shortened one would name a different row",
			MaxImportedIDLen, len(id))
	}
	// Whitespace is refused for a RECORD id though not for a project's, and the
	// difference is the id's second job rather than its first: a record id is also
	// a `--only` selector argument and a shell operand, where a space word-splits
	// into selectors that name nothing. See CheckImportedProjectID.
	if unprintableInIdentifier(id, true) != "" {
		return fmt.Errorf("record id must hold no control character, whitespace, backtick or «» — it is printed " +
			"outside the «...» data delimiters on every listing, and one of those ends the line or the data block. " +
			"The offending id is not shown, because it is the value being refused. " +
			"Give the record a new id in the artifact")
	}
	return nil
}

// CheckImportedProjectID is CheckImportedID for a project's id, and it is
// deliberately WEAKER on two counts: a space is allowed, and so is any length.
//
// A project id is not always a short name. `ensureProjectFor` passes a caller's
// `project_id` argument straight through as the id, and that argument is routinely
// a filesystem path — a remote is detected for a path-shaped value and the project
// is keyed by repository instead, but a path with no detectable remote is stored
// as given. `/Users/w/My Projects/ghost` is a real project id in a real store, and
// `ghost_list_projects` and the health report print it for a human to copy.
//
// What is refused is the part that forges a LINE: a control character, a backtick
// that closes the span it is printed in, or a «» that opens a data block of its
// own. A space does none of those.
//
// There is deliberately NO LENGTH BOUND, and the first version of this function
// had MaxImportedIDLen here, which was wrong in a way that reached much further
// than one refused record. A deep checkout or a long macOS/Windows username makes
// a path-shaped id exceed 128 bytes easily, and `ghost export` writes that id into
// the artifact — so `ghost import` would refuse a file `ghost export` had just
// written. Worse, the refusal cascades: a project step that fails never records
// its id, so `checkFor` then rejects EVERY memory, task and decision naming that
// project, and a fresh-store restore of that project imports nothing while
// reporting a reason that names neither the length nor the path. Length is not the
// threat class for an id rendered by a line-safe renderer; the characters that end
// a line are. See CheckImportedProjectText, which has never had a bound.
func CheckImportedProjectID(id string) error {
	if unprintableInIdentifier(id, false) != "" {
		return fmt.Errorf("project id must hold no control character, backtick or «» — it is printed inside backticks " +
			"and outside the «...» data delimiters, and one of those ends the line or the span. A space is fine, and so " +
			"is any length: a project id is often a filesystem path, and a deep checkout is a longer one. The offending " +
			"id is not shown, because it is the value being refused")
	}
	return nil
}

// CheckImportedProjectText is the same rule for a project's name and path: no
// control character, no backtick, no «». A space is fine, for the reason
// CheckImportedProjectID gives — a project name is normally full of them.
//
// field names the column so the refusal says which value to fix, and no length
// bound is applied because length is not the threat class here: a long name or
// path is still one line, and the renderer keeps it that way whatever it holds.
func CheckImportedProjectText(field, value string) error {
	if unprintableInIdentifier(value, false) != "" {
		return fmt.Errorf("project %s must hold no control character, backtick or «» — it is printed as a label on "+
			"every listing and in the session-start block's own heading, and one of those ends the line. A space is "+
			"fine. The offending value is not shown, because it is the value being refused", field)
	}
	return nil
}

// CheckImportedTags is CheckImportedID for a memory's TAGS, and it is the id
// guard's rule applied to the one field the id guard does not reach: a tag is
// printed outside the «...» data delimiters too — `assemble.TagsLabel` writes
// ` tags:[…]` on the same line as the content, on both surfaces that render one —
// and `json.Marshal` does not escape « or ».
//
// So a tag holding a « opened a data block of its own, mid-metadata, on the line a
// reader takes the memory's content from. It cannot CLOSE one, so nothing escapes
// the block as instruction; that is the whole difference in severity from the id
// (#791), and the reason this is a guard rather than a rewrite. What it does break
// is the delimiter contract in docs/mcp.md, which is one contract: a reader who
// meets « twice before the content cannot tell which span is the data.
//
// It is exported for the same reason CheckImportedID is: `internal/portable`'s
// exporter has to reach the importer's own rule rather than a second spelling of
// it, or `ghost export` writes tags that `ghost import` then rejects — the
// round-trip break #796 found in the id case.
//
// The rule is `unprintableInIdentifier` with `spaces` FALSE, and that is a
// decision rather than a copy of the id's. A record id is a `--only` selector and
// a shell operand, where a space word-splits into selectors that name nothing. A
// tag is neither: it is a keyword a reader scans inside a JSON array, and a space
// is most of what a tag is made of — "ci timeouts", "session capacity". Refusing
// those would refuse the tags a real store is full of.
//
// The BACKTICK is in the class for a reason specific to this field, and it is
// worth stating because it is the one member that is not obviously a threat here:
// the id is printed inside a backtick span, but the tag label is not, so a backtick
// in a tag cannot close anything of Ghost's. What two of them in one tag do is
// pair into a markdown code span, which swallows the rest of the row — so the
// label stops being a label. That is the same class of harm as the rest of the set
// (the rendered line stops meaning what the renderer said it meant), and no tag a
// real store holds contains one.
//
// There is deliberately NO LENGTH BOUND, for the reason CheckImportedProjectID has
// none and the record id does: a shortened TAG is not a shortened ROW. Refusing an
// over-long id is refusing to name a different row, which is an honesty problem; a
// tag is a label, and the renderer is where a display bound belongs. The characters
// that end a line are the threat class here, and those are refused.
func CheckImportedTags(tags []string) error {
	for i, tag := range tags {
		if reason := unprintableInIdentifier(tag, false); reason != "" {
			return fmt.Errorf("tag %d must hold no %s — a tag is printed outside the «...» data delimiters on "+
				"every listing, so a control character ends the line, a backtick pairs into a code span that "+
				"swallows the rest of the row, and a « opens a data block of its own. A space is fine, and so is "+
				"any length: a tag is a label. The offending tag is not shown, because it is the value being "+
				"refused. Give the record different tags in the artifact", i, reason)
		}
	}
	return nil
}

// unprintableInIdentifier returns "" when s holds nothing that can end a rendered
// line, a backtick span or a «...» data block, and the reason otherwise.
//
// It is the one place that class is written down, because three exported checks
// now depend on it and a second copy would be a second rule. spaces is a
// parameter rather than a constant because the answer genuinely differs by what
// the value is FOR: a record id is a `--only` selector and a shell operand, where
// a space word-splits, and a project id is often a path, where it does not.
//
// It takes the value and returns a reason rather than returning a bool, because
// every caller writes its own message anyway: an id, a project name and a path are
// different fields with different consequences, and only the class is shared.
func unprintableInIdentifier(s string, spaces bool) string {
	for _, r := range s {
		switch {
		case unicode.IsControl(r):
			return "control character"
		case spaces && unicode.IsSpace(r):
			return "whitespace"
		case r == '`':
			return "backtick"
		case r == '«' || r == '»':
			return "guillemet"
		}
	}
	return ""
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
//   - importance is clamped to [0,1], the same bound a normal save applies;
//   - the id is checked for shape and REFUSED if it fails (see checkImportedID),
//     never clamped, because a different id is a different row.
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
	// The lock is taken HERE, above the presence check, and that is the whole
	// point of this round. The presence check is a read whose answer decides
	// whether a write happens, so the two have to be atomic together: outside
	// the lock, two imports of the same new id both see it absent, both pass
	// validation, and the second then fails the INSERT with a UNIQUE constraint
	// error where it used to return a skip. A check-then-write is only a
	// check-then-write if nothing can change between the check and the write,
	// and the mutex is what makes that true. ImportProject has always had it
	// this way; the other three importers were moved and this is the cost.
	//
	// The shape check and the field checks sit inside the lock too, which is
	// where they were before this branch. They are pure validation with no I/O,
	// so the critical section stays short, and holding the lock across them
	// costs nothing that matters: an import is one process writing one file.
	s.mu.Lock()
	defer s.mu.Unlock()

	// The order of the next three blocks is load-bearing in BOTH directions, and
	// this branch got it wrong twice before getting it right: presence, then
	// shape, then the field checks. Neither rule alone picks this order; both
	// have to hold at once, and only this sequence satisfies them.
	//
	// FIRST the id-presence check, and above everything that can refuse. A record
	// already in the store is a SKIP, never a rejection, and that is the portable
	// format's own promise — "never overwrites an id that already exists, so
	// re-running is always safe" — which docs/invariants.md states for importer
	// guards in the same words. Ahead of this check, a store holding a pre-#791 id
	// (a space, a guillemet, a backtick, an over-long one: a pre-guard write, a
	// restored snapshot, a hand edit) would make a re-run FAIL over a row that is
	// not being written and could not be. The shape check was in front of this for
	// a round, and the fix is this position, not a weakened rule.
	//
	// It is a read, and it is under the lock anyway, because a read that decides
	// whether a write happens is part of the write. See the comment on the lock
	// above for the race this closes.
	//
	// It names the id in neither arm, deliberately. This is the only message
	// between the two checks, and a message carrying the id here would be a way
	// for a hostile id to reach a report line without the shape check ever
	// running. A database failure is not a fact about the record, so the operator
	// does not need the id to act on it — the artifact line number in the report
	// is what they act on.
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM memories WHERE id = ?`, m.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, false, false, fmt.Errorf("import memory: check whether the id is present: %w", err)
		}
	} else {
		return false, false, false, nil
	}
	//
	// THEN the id's SHAPE, and above every message that could name the id. The id
	// is the one field of this record that reaches a rendered line OUTSIDE the
	// «...» data delimiters — `Item.Line` and `formatMemories` print it inside
	// backticks ahead of the content, so a newline in it forges a second line
	// reading as Ghost's own memory row, and the «...» contract the content is
	// quoted under is defeated by a field it does not wrap (#791). Every message
	// below is prefixed with the id (`memory %s: ...`), so a hostile id would
	// reach the rejection the caller prints and the report line it lands on.
	// Refusing before the id reaches a format verb is what keeps the refusal
	// itself from carrying the payload.
	//
	// A check added ABOVE this one must therefore not name the id in any message.
	// That is a real constraint on the next person to add one here, and it is the
	// price of the two rules coexisting.
	if err := CheckImportedID(m.ID); err != nil {
		return false, false, false, err
	}
	//
	// THEN the field checks, gathered here from the top of the function for the
	// same reason. They are validation rather than precondition — none of them is
	// about whether this record may be written at all — and every one of them
	// names the id, so every one of them has to sit below the shape check. Their
	// order among themselves is unchanged, and none of them was moved past the
	// apply=false early return, so dry-run/apply parity still holds.
	if m.ProjectID == "" {
		return false, false, false, fmt.Errorf("memory %s: project_id is required", m.ID)
	}
	if m.Content == "" {
		return false, false, false, fmt.Errorf("memory %s: content is required", m.ID)
	}
	if !IsValidCategory(m.Category) {
		return false, false, false, fmt.Errorf("memory %s: invalid category %q — must be one of: architecture, decision, pattern, convention, gotcha, dependency, preference, fact", m.ID, m.Category)
	}
	// The source is checked here rather than in the guarded block below, which is
	// where the SECRET guard sits, not this one: an invalid source is a bad value
	// in a column, and `ghost export` refuses to emit one, so an artifact holding
	// one is already the only way to reach this.
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

	// The secret guard's own window, and it is unchanged: after the presence
	// check, so a record already in the store stays a skip, and before the
	// apply=false early return, so a dry run classifies a record exactly as the
	// apply run it previews. Both of those are now inside the lock with it,
	// which is what makes the check-then-write atomic.
	// After the presence check and before the apply=false early return, which is
	// the only window where both properties hold. Before the presence check a
	// record already in the store would be refused, turning the portable
	// format's "never overwrites an id that already exists, so re-running is
	// always safe" into a hard failure over a row that is not being written.
	// After the apply=false return a dry run would classify a record
	// differently from the apply run it previews — and dry-run/apply parity is
	// what makes the dry run worth running. An artifact is untrusted input
	// arriving from a file, which is why it is guarded at all despite the
	// same idempotence argument applying to Create.
	// agent and session_id join source_ref here, and the reason is the arrival
	// record below: an import writes the artifact's own agent and session onto a
	// memory_provenance row, so an unguarded value here would be stored twice —
	// once where #656 already reaches it and once where it does not. On this route
	// all three are the FILE's content rather than the harness's identity, which is
	// the condition secret_guard.go already names for guarding them.
	if err := rejectSecretFields(
		secretField{"content", m.Content},
		secretField{"source_ref", m.SourceRef},
		secretField{"agent", m.Agent},
		secretField{"session_id", m.SessionID},
	); err != nil {
		return false, false, false, fmt.Errorf("memory %s: %w", m.ID, err)
	}
	// And the length, for the same reason the writers apply it: the reference is
	// printed as a labelled field on every listing, so an artifact is a way to
	// plant a value that reaches every answer touching the row. Refused rather
	// than clamped — a truncated path is a different path — and before the
	// apply check, so a dry run classifies exactly as the apply run it previews.
	if _, err := boundedSourceRef(m.SourceRef); err != nil {
		return false, false, false, fmt.Errorf("memory %s: %w", m.ID, err)
	}
	if _, err := boundedAgent(m.Agent); err != nil {
		return false, false, false, fmt.Errorf("memory %s: %w", m.ID, err)
	}
	// The tags too, and before the apply check so a dry run classifies exactly as
	// the apply run it previews. An artifact's tags column is untrusted input
	// from a file and was being written raw; the record it lands in is an
	// ordinary memory, so it is assembled into every search row and quoted into
	// the next reflect prompt like any other.
	if err := rejectSecretList("tags", m.Tags); err != nil {
		return false, false, false, fmt.Errorf("memory %s: %w", m.ID, err)
	}
	// And their SHAPE, which is a different question from the secret guard above and
	// the id's (#811).
	//
	// A tag is printed OUTSIDE the «...» data delimiters — `assemble.TagsLabel`
	// writes ` tags:[...]` on the same line as the content, in both renderers — and
	// `json.Marshal` escapes a newline, a quote and a backslash but not « or ». So
	// a tag holding a « opened a data block of its own mid-metadata. The renderer
	// now neutralises it, which is the load-bearing layer; this is the one that
	// stops the next artifact carrying one, and it is here for the same reason
	// CheckImportedID is: a store can already hold what a guard refuses (a
	// pre-guard import, a restored snapshot, a hand edit), and refusing on the way
	// in says nothing about the rows already there.
	//
	// It sits in the same window as the other field checks — after the presence
	// check, so a re-run of an artifact against a store that already holds a
	// pre-guard tag is still a skip and not a failure, and before the apply=false
	// early return, so a dry run classifies the record exactly as the apply run it
	// previews. Both of those are properties of the POSITION, which is why the
	// check is a separate statement and not folded into rejectSecretList above.
	if err := CheckImportedTags(m.Tags); err != nil {
		return false, false, false, fmt.Errorf("memory %s: %w", m.ID, err)
	}
	// The evidence records' own text, in the same window and for the same reason.
	// An artifact's evidence rows are the FILE's content, exactly as
	// `source_ref` above is, and this is the one route where that is true: a
	// harness's own identity is not a secret, but a hand-edited or hostile
	// artifact can put anything in a nested field. The memory-level guard cannot
	// see these, because the memory row does not hold them — so without this the
	// table becomes the one place a credential survives, in an append-only store
	// the next `ghost export` re-emits and only the purge's explicit DELETE
	// reaches. The field is named per record, never the value.
	for i, e := range m.Evidence {
		if !IsValidEvidenceKind(e.Kind) {
			return false, false, false, fmt.Errorf("memory %s: evidence record %d has invalid kind %q — must be one of: observed, imported, verified, legacy", m.ID, i, e.Kind)
		}
		if err := rejectSecretFields(
			secretField{fmt.Sprintf("evidence[%d].agent", i), e.Agent},
			secretField{fmt.Sprintf("evidence[%d].session_id", i), e.SessionID},
			secretField{fmt.Sprintf("evidence[%d].source_ref", i), e.SourceRef},
		); err != nil {
			return false, false, false, fmt.Errorf("memory %s: %w", m.ID, err)
		}
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
		`SELECT 1 FROM memory_history WHERE memory_id = ? LIMIT 1`, m.ID,
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
	tx, _, err := s.beginWrite(ctx, "import")
	if err != nil {
		return false, false, downgraded, fmt.Errorf("import memory %s: begin tx: %w", m.ID, err)
	}
	defer tx.Rollback() //nolint:errcheck
	// Re-check presence INSIDE the write transaction. The pre-check above is
	// under s.mu, which is a per-Store lock and closes nothing across processes:
	// a second `ghost import` on the same database can pass the pre-check, see
	// the id as absent, and fail the INSERT with a UNIQUE constraint error where
	// it used to return a skip. This re-check runs after BEGIN IMMEDIATE has
	// taken SQLite's write lock, so a second process's BEGIN IMMEDIATE blocks
	// until this one commits and then sees the row. The check-then-write is
	// atomic because both halves are in the same transaction, which is the only
	// thing that makes it so — the process mutex was never enough.
	var presentInTx int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM memories WHERE id = ?`, m.ID).Scan(&presentInTx); err != nil {
		if err != sql.ErrNoRows {
			return false, false, downgraded, fmt.Errorf("import memory: re-check presence: %w", err)
		}
	} else {
		return false, false, downgraded, nil
	}
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
	// The evidence the artifact itself carried, then the record of the arrival.
	// The order is the order they happened in, and MemoryProvenance reads forwards:
	// the observations came first, on the machine the artifact was exported from.
	for _, e := range m.Evidence {
		if err := importEvidenceTx(ctx, tx, m.ID, e); err != nil {
			return false, false, downgraded, err
		}
	}
	// The arrival is a claim no carried record makes: this store learned the fact
	// from a file, and a restored corpus that kept only the origin machine's
	// observations would say nothing about its own. Its verified_at is the memory's
	// own when the artifact recorded one — the import preserves that column, and
	// the record is stamped with the store's own clock rather than the artifact's
	// text, so the support summary and the row cannot disagree about whether
	// anybody checked this fact, and no date is copied in from a file.
	//
	// TrustProvenance does not gate it, and neither does it gate the history row
	// above: the flag governs what a row IS (its source, its pin), while who the
	// artifact says observed the fact is the artifact's claim about its own
	// contents, kept as such.
	//
	// And the stamp is conditional, which is the whole of a round trip. A record
	// that already carries a verification says the check HAPPENED — on the origin
	// machine, by whoever did it — so stamping the arrival adds a second stamp for
	// one event and a corpus that crosses a machine boundary N times reports N+1
	// verifications of one check. The number a reader trusts has to survive being
	// moved, so the arrival is stamped only when nothing carried already is.
	//
	// Kind-agnostic, and deliberately: the second hop's artifact carries the FIRST
	// hop's arrival, whose kind is `imported`, so a guard that looked only for
	// `verified` records would re-inflate on every hop after the first. A check
	// already recorded is a check already recorded whatever kind recorded it.
	//
	// A non-empty value, because the artifact form is hand-edited and a record can
	// carry `"verified_at": ""`, which every reader of the column treats as no
	// claim. Empty is not a verification, and treating it as one would suppress a
	// real arrival stamp.
	arrivalStamped := recordedVerification(m.VerifiedAt) && !anyCarriedVerification(m.Evidence)
	if err := appendEvidenceTx(ctx, tx, m.ID, evidenceImported, Provenance{
		Agent:      m.Agent,
		SessionID:  m.SessionID,
		SourceRef:  m.SourceRef,
		Confidence: m.Confidence,
	}, arrivalStamped); err != nil {
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
	// The lock is taken HERE, above the presence check, for the reason
	// ImportMemory gives in full: a read that decides whether a write happens is
	// part of the write, and outside the lock two imports of the same new id both
	// see it absent and the second fails the INSERT with a UNIQUE constraint
	// error where it used to return a skip.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Guarded below, after the id-presence check and before the apply=false
	// early return — see ImportMemory for why that window is the only one that
	// keeps both idempotence and dry-run/apply parity. A task's notes are
	// otherwise unvalidated text that a normal save refuses, and an artifact
	// carries them.

	// The order of the next three blocks is the same as ImportMemory's and for
	// the same two reasons, which are opposite: the id-presence check FIRST, so
	// a record already in the store is a skip rather than a rejection and
	// re-running the import stays always-safe; the id's SHAPE second, above every
	// message that could name the id, so a hostile id never reaches a refusal and
	// from there the report line that prints it; the field checks third, because
	// every one of them names the id and none of them is a precondition.
	//
	// ImportMemory carries the full reasoning and the constraint this leaves on
	// the next check added here: a check above the shape check must not name the
	// id in any message. The presence check is a read, so it needs no lock; it sat
	// under the lock, because a read that decides whether a write happens is
	// part of the write.
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, t.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("import task: check whether the id is present: %w", err)
		}
	} else {
		return false, nil
	}
	if err := CheckImportedID(t.ID); err != nil {
		return false, err
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

	// Same window as ImportMemory: after the presence check, before apply=false.
	// Both are inside the lock with it now.
	if err := rejectSecretFields(
		secretField{"title", t.Title},
		secretField{"description", t.Description},
		secretField{"notes", t.Notes},
	); err != nil {
		return false, fmt.Errorf("task %s: %w", t.ID, err)
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

	// The write goes through beginWrite rather than execGuardedWrite so the
	// presence re-check can sit in the SAME transaction as the INSERT. See
	// ImportMemory for why: s.mu is a per-Store lock and closes nothing across
	// processes, so a second `ghost import` can pass the pre-check and fail the
	// INSERT with a UNIQUE constraint error. BEGIN IMMEDIATE takes SQLite's write
	// lock, so the re-check inside it is the atomic half of the check-then-write.
	tx, _, err := s.beginWrite(ctx, "import-task")
	if err != nil {
		return false, fmt.Errorf("import task %s: begin tx: %w", t.ID, err)
	}
	defer tx.Rollback() //nolint:errcheck
	var presentInTx int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, t.ID).Scan(&presentInTx); err != nil {
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("import task %s: re-check presence: %w", t.ID, err)
		}
	} else {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tasks (id, project_id, title, description, status, priority,
			blocked_by, branch, pr_number, notes, created_at, updated_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, 0), NULLIF(?, ''),
			COALESCE(NULLIF(?, ''), datetime('now')),
			COALESCE(NULLIF(?, ''), datetime('now')),
			NULLIF(?, ''))
	`, t.ID, t.ProjectID, t.Title, t.Description, t.Status, t.Priority,
		nullIfEmpty(t.BlockedBy), nullIfEmpty(t.Branch), t.PRNumber, t.Notes,
		t.CreatedAt, t.UpdatedAt, t.CompletedAt); err != nil {
		return false, fmt.Errorf("import task %s: %w", t.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("import task %s: commit: %w", t.ID, err)
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
	// The lock is taken HERE, above the presence check, for the reason
	// ImportMemory gives in full: a read that decides whether a write happens is
	// part of the write, and outside the lock two imports of the same new id both
	// see it absent and the second fails the INSERT with a UNIQUE constraint
	// error where it used to return a skip.
	s.mu.Lock()
	defer s.mu.Unlock()

	// Guarded below, after the id-presence check and before the apply=false
	// early return — see ImportMemory. alternatives is a list because it is
	// one: ghost_decisions_list renders it back to the agent, so an entry is
	// as replayable as the rationale.

	// The order of the next three blocks is the same as ImportMemory's and for
	// the same two reasons, which are opposite: the id-presence check FIRST, so
	// a record already in the store is a skip rather than a rejection and
	// re-running the import stays always-safe; the id's SHAPE second, above every
	// message that could name the id, so a hostile id never reaches a refusal and
	// from there the report line that prints it; the field checks third, because
	// every one of them names the id and none of them is a precondition.
	//
	// ImportMemory carries the full reasoning and the constraint this leaves on
	// the next check added here: a check above the shape check must not name the
	// id in any message. The presence check is a read, so it needs no lock; it sat
	// under the lock, because a read that decides whether a write happens is
	// part of the write.
	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM decisions WHERE id = ?`, d.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("import decision: check whether the id is present: %w", err)
		}
	} else {
		return false, nil
	}
	if err := CheckImportedID(d.ID); err != nil {
		return false, err
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

	// Same window as ImportMemory: after the presence check, before apply=false.
	// Both are inside the lock with it now.
	if err := rejectSecretFields(
		secretField{"title", d.Title},
		secretField{"decision", d.Decision},
		secretField{"rationale", d.Rationale},
	); err != nil {
		return false, fmt.Errorf("decision %s: %w", d.ID, err)
	}
	if err := rejectSecretList("alternatives", d.Alternatives); err != nil {
		return false, fmt.Errorf("decision %s: %w", d.ID, err)
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
	// beginWrite rather than execGuardedWrite, for the reason ImportTask gives:
	// the presence re-check has to be in the same transaction as the INSERT,
	// and s.mu does not close the cross-process race.
	tx, _, err := s.beginWrite(ctx, "import-decision")
	if err != nil {
		return false, fmt.Errorf("import decision %s: begin tx: %w", d.ID, err)
	}
	defer tx.Rollback() //nolint:errcheck
	var presentInTx int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM decisions WHERE id = ?`, d.ID).Scan(&presentInTx); err != nil {
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("import decision %s: re-check presence: %w", d.ID, err)
		}
	} else {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO decisions (id, project_id, title, decision, alternatives, rationale,
			status, superseded_by, tags, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?,
			COALESCE(NULLIF(?, ''), datetime('now')),
			COALESCE(NULLIF(?, ''), datetime('now')))
	`, d.ID, d.ProjectID, d.Title, d.Decision, string(alts), d.Rationale,
		d.Status, nullIfEmpty(d.SupersededBy), string(tags), d.CreatedAt, d.UpdatedAt); err != nil {
		return false, fmt.Errorf("import decision %s: %w", d.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("import decision %s: commit: %w", d.ID, err)
	}
	return true, nil
}
