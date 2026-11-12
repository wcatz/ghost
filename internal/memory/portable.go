package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PortableProject is a project as the export/import artifact carries it.
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
type PortableMemory struct {
	ID           string            `json:"id"`
	ProjectID    string            `json:"project_id"`
	Category     string            `json:"category"`
	Content      string            `json:"content"`
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

	Retention string `json:"retention"`

	Evidence []PortableEvidence `json:"evidence,omitempty"`
}

// PortableEvidence is one evidence record as the artifact carries it: the same
// columns the table holds, minus memory_id, which the enclosing memory record
// already names, and minus carried_from, for the reason below.
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

// PortableLink is one edge of the memory graph as the artifact carries it.
type PortableLink struct {
	SourceID  string  `json:"source_id"`
	TargetID  string  `json:"target_id"`
	Relation  string  `json:"relation"`
	Strength  float64 `json:"strength"`
	Source    string  `json:"source"`
	CreatedAt string  `json:"created_at"`
}

// CarriedLinkRelations are the relations of the memory graph the portable
// artifact carries, and the rule for which ones they are is the rule the rest
// of the artifact follows: a value travels when nothing recomputes it.
func CarriedLinkRelations() []string {
	return []string{"supersedes", "causes"}
}

func carriedLinkRelation(relation string) bool {
	for _, r := range CarriedLinkRelations() {
		if r == relation {
			return true
		}
	}
	return false
}

func carriedLinkSource(source string) bool {
	switch source {
	case "auto", "llm", "manual":
		return true
	}
	return false
}

func anyCarriedVerification(records []PortableEvidence) bool {
	for _, e := range records {
		if recordedVerification(e.VerifiedAt) {
			return true
		}
	}
	return false
}

func recordedVerification(at *string) bool {
	return at != nil && *at != ""
}

// ImportOptions is what one import run is doing with the records it is given.
type ImportOptions struct {
	Apply bool

	TrustProvenance bool
}

// DowngradedSource is the source every imported memory is stamped with unless
// the run was told to trust the artifact's own. `onboarding` is the value
// internal/claudeimport already uses for memories brought in from outside Ghost
// at first contact, and it is the only one of the eight that means "this did not
// originate in a session" without also claiming authorship. It is in the
// memories table's CHECK already, so downgrading needs no migration.
const DowngradedSource = "onboarding"

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

var (
	validTaskStatuses = map[string]bool{
		"pending": true, "active": true, "done": true, "blocked": true,
	}
	validDecisionStatuses = map[string]bool{
		"active": true, "superseded": true, "revisit": true,
	}
)

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

func (s *Store) PortableMemories(ctx context.Context, projectIDs []string) ([]PortableMemory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT id, project_id, category, content, importance, access_count,
		       last_accessed, source, tags, pinned, created_at, updated_at,
		       resolved_at, valid_from, valid_until, verified_at,
		       agent, session_id, source_ref, confidence, scope, retention
		FROM memories`
	var args []any
	var where string
	if len(projectIDs) > 0 {
		placeholders := make([]string, len(projectIDs))
		for i, id := range projectIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		where = " WHERE project_id IN (" + strings.Join(placeholders, ",") + ")"
	} else {
		where = " WHERE 1 = 1"
	}
	if seedContents := shippedSeedContents(); len(seedContents) > 0 {
		placeholders := make([]string, len(seedContents))
		for i, content := range seedContents {
			placeholders[i] = "?"
			args = append(args, content)
		}
		where += ` AND NOT (project_id = '_global' AND content IN (` + strings.Join(placeholders, ",") + `))`
	}
	query += where
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
	evidence, err := portableEvidence(ctx, s.db, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Evidence = evidence[out[i].ID]
	}
	return out, nil
}

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

func (s *Store) PortableLinks(ctx context.Context, projectIDs []string) ([]PortableLink, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(projectIDs) == 0 {
		return nil, nil
	}
	relations := CarriedLinkRelations()
	relPh := make([]string, len(relations))
	args := make([]any, 0, len(relations)+2*len(projectIDs))
	for i, r := range relations {
		relPh[i] = "?"
		args = append(args, r)
	}
	projPh := make([]string, len(projectIDs))
	for i := range projectIDs {
		projPh[i] = "?"
	}
	for _, id := range projectIDs {
		args = append(args, id)
	}
	for _, id := range projectIDs {
		args = append(args, id)
	}
	projects := strings.Join(projPh, ",")

	rows, err := s.db.QueryContext(ctx, `
		SELECT l.source_id, l.target_id, l.relation, l.strength, l.source, l.created_at
		FROM memory_links l
		WHERE l.invalidated_at IS NULL
		  AND l.relation IN (`+strings.Join(relPh, ",")+`)
		  AND (l.source_id IN (SELECT id FROM memories WHERE project_id IN (`+projects+`))
		    OR l.target_id IN (SELECT id FROM memories WHERE project_id IN (`+projects+`)))
		ORDER BY l.source_id, l.target_id, l.relation
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("portable links: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []PortableLink
	for rows.Next() {
		var l PortableLink
		if err := rows.Scan(&l.SourceID, &l.TargetID, &l.Relation, &l.Strength, &l.Source, &l.CreatedAt); err != nil {
			return nil, fmt.Errorf("portable links: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) ImportLink(ctx context.Context, l PortableLink, apply bool) (created bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var present int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM memory_links WHERE source_id = ? AND target_id = ? AND relation = ?`,
		l.SourceID, l.TargetID, l.Relation,
	).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("import link: check whether the edge is present: %w", err)
		}
	} else {
		return false, nil
	}
	if err := CheckImportedLink(l); err != nil {
		return false, err
	}
	if !apply {
		return true, nil
	}

	tx, _, err := s.beginWrite(ctx, "import-link")
	if err != nil {
		return false, fmt.Errorf("import link: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx, `
		INSERT INTO memory_links (source_id, target_id, relation, strength, source, created_at)
		VALUES (?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), datetime('now')))
		ON CONFLICT(source_id, target_id, relation) DO NOTHING
	`, l.SourceID, l.TargetID, l.Relation, l.Strength, l.Source, l.CreatedAt)
	if err != nil {
		return false, fmt.Errorf("import link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("import link rows: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	if l.Relation == "supersedes" {
		if err := appendHistoryEventsTx(ctx, tx, []historyEvent{{
			phase:     phaseSupersede,
			relatedID: l.SourceID,
		}}, []string{l.TargetID}); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit import link: %w", err)
	}
	return true, nil
}

func (s *Store) projectCollision(ctx context.Context, q Queryer, p PortableProject) error {
	remote := NormalizeRepoRemote(p.RepoRemote)
	var byPath, foundRemote string
	err := q.QueryRowContext(ctx, `
		SELECT
			coalesce((SELECT id FROM projects WHERE path = ? AND id != ? LIMIT 1), ''),
			coalesce((SELECT id FROM projects
			          WHERE repo_remote = ? AND repo_remote <> '' AND id != ? LIMIT 1), '')
	`, p.Path, p.ID, remote, p.ID).Scan(&byPath, &foundRemote)
	if err != nil {
		return fmt.Errorf("import project %s: %w", RefusalArg(ProjectIdentifierLabel, p.ID), err)
	}
	switch {
	case byPath != "":
		return fmt.Errorf("project %s cannot be imported: this Ghost already records %s as project %s — import into that project, or merge it with `ghost project merge`",
			RefusalArg(ProjectIdentifierLabel, p.ID), p.Path, RefusalArg(ProjectIdentifierLabel, byPath))
	case foundRemote != "":
		return fmt.Errorf("project %s cannot be imported: this Ghost already records repository %s as project %s — import into that project, or merge it with `ghost project merge`",
			RefusalArg(ProjectIdentifierLabel, p.ID), remote, RefusalArg(ProjectIdentifierLabel, foundRemote))
	}
	return nil
}

func (s *Store) ProjectForCheckout(ctx context.Context, path, repoRemote string) (id, name string, err error) {
	remote := NormalizeRepoRemote(repoRemote)
	s.mu.RLock()
	defer s.mu.RUnlock()

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

func requireProject(ctx context.Context, db *sql.DB, kind, id, projectID string) error {
	var present int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, projectID).Scan(&present)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%s %s: project %s not found", kind, RefusalArg(kind+"_id", id), RefusalArg("project_id", projectID))
	}
	if err != nil {
		return fmt.Errorf("%s %s: lookup project %s: %w", kind, RefusalArg(kind+"_id", id), RefusalArg("project_id", projectID), err)
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanPortableMemory(sc rowScanner) (PortableMemory, error) {
	var m PortableMemory
	var tagsJSON, scopeJSON sql.NullString
	var agent, sessionID, sourceRef sql.NullString
	var confidence sql.NullFloat64
	var importance float64
	if err := sc.Scan(
		&m.ID, &m.ProjectID, &m.Category, &m.Content, &importance, &m.AccessCount,
		&m.LastAccessed, &m.Source, &tagsJSON, &m.Pinned, &m.CreatedAt, &m.UpdatedAt,
		&m.ResolvedAt, &m.ValidFrom, &m.ValidUntil, &m.VerifiedAt,
		&agent, &sessionID, &sourceRef, &confidence, &scopeJSON, &m.Retention,
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

func (s *Store) ImportProject(ctx context.Context, p PortableProject, apply bool) (created bool, err error) {
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
	if err := CheckImportedProject(p); err != nil {
		return false, err
	}
	if err := s.projectCollision(ctx, s.db, p); err != nil {
		return false, err
	}
	if !apply {
		return true, nil
	}

	tx, _, err := s.beginWrite(ctx, "import-project")
	if err != nil {
		return false, fmt.Errorf("import project %s: begin tx: %w", RefusalArg(ProjectIdentifierLabel, p.ID), err)
	}
	defer tx.Rollback() //nolint:errcheck
	var presentInTx int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id = ?`, p.ID).Scan(&presentInTx); err != nil {
		if err != sql.ErrNoRows {
			return false, fmt.Errorf("import project %s: re-check presence: %w", RefusalArg(ProjectIdentifierLabel, p.ID), err)
		}
	} else {
		return false, nil
	}
	if err := s.projectCollision(ctx, tx, p); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO projects (id, path, name, repo_remote, created_at, updated_at)
		VALUES (?, ?, ?, ?, COALESCE(NULLIF(?, ''), datetime('now')),
		                   COALESCE(NULLIF(?, ''), datetime('now')))
	`, p.ID, p.Path, p.Name, nullIfEmpty(NormalizeRepoRemote(p.RepoRemote)), p.CreatedAt, p.UpdatedAt); err != nil {
		return false, fmt.Errorf("import project %s: %w", RefusalArg(ProjectIdentifierLabel, p.ID), err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO ghost_state (project_id) VALUES (?)`, p.ID); err != nil {
		return false, fmt.Errorf("import project %s: state row: %w", RefusalArg(ProjectIdentifierLabel, p.ID), err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("import project %s: commit: %w", RefusalArg(ProjectIdentifierLabel, p.ID), err)
	}
	return true, nil
}

func (s *Store) ImportMemory(ctx context.Context, m PortableMemory, opts ImportOptions) (created, clamped, downgraded bool, err error) {
	apply := opts.Apply
	s.mu.Lock()
	defer s.mu.Unlock()

	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM memories WHERE id = ?`, m.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, false, false, fmt.Errorf("import memory: check whether the id is present: %w", err)
		}
	} else {
		return false, false, false, nil
	}
	if err := CheckImportedMemory(m); err != nil {
		return false, false, false, err
	}
	content, cut := ClampContent(m.Content)
	validity, err := ImportedValidity(m)
	if err != nil {
		return false, false, false, err
	}
	evidence := make([]PortableEvidence, len(m.Evidence))
	for i, e := range m.Evidence {
		read, err := ImportedEvidence(i, e)
		if err != nil {
			return false, false, false, &EvidenceRefusalError{Err: err}
		}
		evidence[i] = read
	}
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
	retention, err := NormalizeRetention(m.Retention)
	if err != nil {
		return false, false, false, err
	}
	source, pinned := m.Source, m.Pinned
	if !opts.TrustProvenance {
		source, pinned = DowngradedSource, false
		retention = RetentionProject
		downgraded = true
	}

	var historic int
	if err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM memory_history WHERE memory_id = ? LIMIT 1`, m.ID,
	).Scan(&historic); err == nil {
		return false, false, false, fmt.Errorf(
			"import memory %s: that id has recorded history from a memory that was deleted here, "+
				"and importing would splice two memories' histories under one id — "+
				"purge it first with `ghost history purge %s`, or give the record a new id in the artifact",
			RefusalArg("memory_id", m.ID), RefusalArg("memory_id", m.ID))
	} else if err != sql.ErrNoRows {
		return false, false, false, fmt.Errorf("import memory %s: %w", RefusalArg("memory_id", m.ID), err)
	}

	if !apply {
		return true, cut, downgraded, nil
	}

	if err := requireProject(ctx, s.db, "memory", m.ID, m.ProjectID); err != nil {
		return false, false, downgraded, err
	}

	tags, _ := json.Marshal(m.Tags)
	expires := sessionExpiry(retention, time.Now())
	tx, _, err := s.beginWrite(ctx, "import")
	if err != nil {
		return false, false, downgraded, fmt.Errorf("import memory %s: begin tx: %w", RefusalArg("memory_id", m.ID), err)
	}
	defer tx.Rollback() //nolint:errcheck
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
			confidence, scope, retention, expires_at)
		VALUES (?, ?, ?, ?, COALESCE(?, 0.5), ?, ?, ?, ?, ?,
		        COALESCE(NULLIF(?, ''), datetime('now')),
		        COALESCE(NULLIF(?, ''), datetime('now')), ?,
		        ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, m.ID, m.ProjectID, m.Category, content, importance, m.AccessCount,
		m.LastAccessed, source, string(tags), pinned,
		m.CreatedAt, m.UpdatedAt, m.ResolvedAt,
		validity.ValidFrom, validity.ValidUntil, validity.VerifiedAt,
		nullIfEmpty(m.Agent), nullIfEmpty(m.SessionID), nullIfEmpty(m.SourceRef),
		m.Confidence, scopeJSON(m.Scope), retention, expires)
	if err != nil {
		return false, false, downgraded, fmt.Errorf("import memory %s: %w", RefusalArg("memory_id", m.ID), err)
	}
	if err := appendHistoryTx(ctx, tx, m.ID, phaseImport, Provenance{
		Agent:     m.Agent,
		SessionID: m.SessionID,
	}); err != nil {
		return false, false, downgraded, err
	}
	for _, e := range evidence {
		if err := importEvidenceTx(ctx, tx, m.ID, e); err != nil {
			return false, false, downgraded, err
		}
	}
	arrivalStamped := recordedVerification(validity.VerifiedAt) && !anyCarriedVerification(evidence)
	if err := appendEvidenceTx(ctx, tx, m.ID, evidenceImported, Provenance{
		Agent:      m.Agent,
		SessionID:  m.SessionID,
		SourceRef:  m.SourceRef,
		Confidence: m.Confidence,
	}, arrivalStamped); err != nil {
		return false, false, downgraded, err
	}
	if err := tx.Commit(); err != nil {
		return false, false, downgraded, fmt.Errorf("import memory %s: commit: %w", RefusalArg("memory_id", m.ID), err)
	}
	if s.onSave != nil {
		s.onSave(m.ProjectID)
	}
	return true, cut, downgraded, nil
}

func (s *Store) ImportTask(ctx context.Context, t Task, apply bool) (created bool, clamped []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, t.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, nil, fmt.Errorf("import task: check whether the id is present: %w", err)
		}
	} else {
		return false, nil, nil
	}
	if err := CheckImportedTask(t); err != nil {
		return false, nil, err
	}
	title := TruncateUTF8(t.Title, MaxTitleLen)
	description, descriptionCut := ClampContent(t.Description)
	notes, notesCut := ClampContent(t.Notes)
	if title != t.Title {
		clamped = append(clamped, "title")
	}
	if descriptionCut {
		clamped = append(clamped, "description")
	}
	if notesCut {
		clamped = append(clamped, "notes")
	}
	if !apply {
		return true, clamped, nil
	}

	if err := requireProject(ctx, s.db, "task", t.ID, t.ProjectID); err != nil {
		return false, nil, err
	}
	if t.BlockedBy != "" {
		var blocker int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, t.BlockedBy).Scan(&blocker); err != nil {
			if err == sql.ErrNoRows {
				return false, nil, fmt.Errorf("task %s: blocked_by names task %s, which is not in this store", RefusalArg("task_id", t.ID), RefusalArg("blocked_by", t.BlockedBy))
			}
			return false, nil, fmt.Errorf("import task %s: %w", RefusalArg("task_id", t.ID), err)
		}
	}

	tx, _, err := s.beginWrite(ctx, "import-task")
	if err != nil {
		return false, nil, fmt.Errorf("import task %s: begin tx: %w", RefusalArg("task_id", t.ID), err)
	}
	defer tx.Rollback() //nolint:errcheck
	var presentInTx int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = ?`, t.ID).Scan(&presentInTx); err != nil {
		if err != sql.ErrNoRows {
			return false, nil, fmt.Errorf("import task %s: re-check presence: %w", RefusalArg("task_id", t.ID), err)
		}
	} else {
		return false, nil, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tasks (id, project_id, title, description, status, priority,
			blocked_by, branch, pr_number, notes, created_at, updated_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, 0), NULLIF(?, ''),
			COALESCE(NULLIF(?, ''), datetime('now')),
			COALESCE(NULLIF(?, ''), datetime('now')),
			NULLIF(?, ''))
	`, t.ID, t.ProjectID, title, description, t.Status, t.Priority,
		nullIfEmpty(t.BlockedBy), nullIfEmpty(t.Branch), t.PRNumber, notes,
		t.CreatedAt, t.UpdatedAt, t.CompletedAt); err != nil {
		return false, nil, fmt.Errorf("import task %s: %w", RefusalArg("task_id", t.ID), err)
	}
	if err := tx.Commit(); err != nil {
		return false, nil, fmt.Errorf("import task %s: commit: %w", RefusalArg("task_id", t.ID), err)
	}
	return true, clamped, nil
}

func (s *Store) HasTask(ctx context.Context, projectID, id string) (bool, error) {
	return s.hasRow(ctx, `SELECT 1 FROM tasks WHERE id = ? AND project_id = ?`, id, projectID)
}

func (s *Store) HasDecision(ctx context.Context, projectID, id string) (bool, error) {
	return s.hasRow(ctx, `SELECT 1 FROM decisions WHERE id = ? AND project_id = ?`, id, projectID)
}

func (s *Store) hasRow(ctx context.Context, query string, args ...any) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var present int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&present)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ImportDecision(ctx context.Context, d Decision, apply bool) (created bool, clamped []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var present int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM decisions WHERE id = ?`, d.ID).Scan(&present); err != nil {
		if err != sql.ErrNoRows {
			return false, nil, fmt.Errorf("import decision: check whether the id is present: %w", err)
		}
	} else {
		return false, nil, nil
	}
	if err := CheckImportedDecision(d); err != nil {
		return false, nil, err
	}
	title := TruncateUTF8(d.Title, MaxTitleLen)
	decision, decisionCut := ClampContent(d.Decision)
	rationale, rationaleCut := ClampContent(d.Rationale)
	alternatives := BoundAlternatives(d.Alternatives)
	if title != d.Title {
		clamped = append(clamped, "title")
	}
	if decisionCut {
		clamped = append(clamped, "decision")
	}
	if rationaleCut {
		clamped = append(clamped, "rationale")
	}
	if AlternativesClamped(d.Alternatives) {
		clamped = append(clamped, "alternatives")
	}
	if !apply {
		return true, clamped, nil
	}

	if err := requireProject(ctx, s.db, "decision", d.ID, d.ProjectID); err != nil {
		return false, nil, err
	}
	if d.SupersededBy != "" {
		var superseder int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM decisions WHERE id = ?`, d.SupersededBy).Scan(&superseder); err != nil {
			if err == sql.ErrNoRows {
				return false, nil, fmt.Errorf("decision %s: superseded_by names decision %s, which is not in this store", RefusalArg("decision_id", d.ID), RefusalArg("superseded_by", d.SupersededBy))
			}
			return false, nil, fmt.Errorf("import decision %s: %w", RefusalArg("decision_id", d.ID), err)
		}
	}

	alts, _ := json.Marshal(alternatives)
	tags, _ := json.Marshal(d.Tags)
	tx, _, err := s.beginWrite(ctx, "import-decision")
	if err != nil {
		return false, nil, fmt.Errorf("import decision %s: begin tx: %w", RefusalArg("decision_id", d.ID), err)
	}
	defer tx.Rollback() //nolint:errcheck
	var presentInTx int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM decisions WHERE id = ?`, d.ID).Scan(&presentInTx); err != nil {
		if err != sql.ErrNoRows {
			return false, nil, fmt.Errorf("import decision %s: re-check presence: %w", RefusalArg("decision_id", d.ID), err)
		}
	} else {
		return false, nil, nil
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO decisions (id, project_id, title, decision, alternatives, rationale,
			status, superseded_by, tags, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?,
			COALESCE(NULLIF(?, ''), datetime('now')),
			COALESCE(NULLIF(?, ''), datetime('now')))
	`, d.ID, d.ProjectID, title, decision, string(alts), rationale,
		d.Status, nullIfEmpty(d.SupersededBy), string(tags), d.CreatedAt, d.UpdatedAt); err != nil {
		return false, nil, fmt.Errorf("import decision %s: %w", RefusalArg("decision_id", d.ID), err)
	}
	if err := tx.Commit(); err != nil {
		return false, nil, fmt.Errorf("import decision %s: commit: %w", RefusalArg("decision_id", d.ID), err)
	}
	return true, clamped, nil
}
