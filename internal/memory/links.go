package memory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Link represents an edge between two memories.
type Link struct {
	SourceID      string
	TargetID      string
	Relation      string
	Strength      float32
	Source        string
	CreatedAt     string
	InvalidatedAt *string
}

// MaxRelatedDegree limits live related links per memory.
const MaxRelatedDegree = 20

var symmetricRelations = map[string]bool{"related": true}

// CreateLink creates or updates a memory link.
func (s *Store) CreateLink(ctx context.Context, sourceID, targetID, relation string, strength float32, source string) error {
	return s.CreateLinkJudged(ctx, sourceID, targetID, relation, strength, source, "")
}

// CreateLinkJudged creates or updates a memory link with a judgment timestamp.
func (s *Store) CreateLinkJudged(ctx context.Context, sourceID, targetID, relation string, strength float32, source, judgedAt string) error {
	_, err := s.createLink(ctx, sourceID, targetID, relation, strength, source, judgedAt, false)
	return err
}

// CreateLinkUnopposed creates a directed link when no opposite live link exists.
func (s *Store) CreateLinkUnopposed(ctx context.Context, sourceID, targetID, relation string, strength float32, source, judgedAt string) (bool, error) {
	if symmetricRelations[relation] {
		return false, fmt.Errorf("create link: relation %q is symmetric and cannot be unopposed", relation)
	}
	return s.createLink(ctx, sourceID, targetID, relation, strength, source, judgedAt, true)
}

func (s *Store) createLink(ctx context.Context, sourceID, targetID, relation string, strength float32, source, judgedAt string, requireUnopposed bool) (bool, error) {
	if sourceID == targetID {
		return false, fmt.Errorf("create link: self-links not allowed (id %s)", RefusalArg("memory_id", sourceID))
	}
	if symmetricRelations[relation] && sourceID > targetID {
		sourceID, targetID = targetID, sourceID
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if requireUnopposed || relation == "supersedes" || relation == "related" {
		return s.createLinkTx(ctx, sourceID, targetID, relation, strength, source, judgedAt, requireUnopposed)
	}

	if _, err := s.execGuardedWrite(ctx, "create-link-autocommit", linkInsertSQL,
		sourceID, targetID, relation, strength, source, judgedAt); err != nil {
		return false, fmt.Errorf("create link: %w", err)
	}
	return true, nil
}

func (s *Store) createLinkTx(ctx context.Context, sourceID, targetID, relation string, strength float32, source, judgedAt string, requireUnopposed bool) (bool, error) {
	tx, lock, err := s.beginWrite(ctx, "create-link")
	if err != nil {
		return false, fmt.Errorf("begin create link: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if requireUnopposed {
		opposed, err := linkIsActive(ctx, tx, targetID, sourceID, relation)
		if err != nil {
			return false, err
		}
		if opposed {
			return false, nil
		}
	}

	if relation == "supersedes" {
		active, err := linkIsActive(ctx, tx, sourceID, targetID, relation)
		if err != nil {
			return false, err
		}
		if err := insertLinkTx(ctx, tx, sourceID, targetID, relation, strength, source, judgedAt); err != nil {
			return false, err
		}
		if !active {
			if err := appendHistoryEventsTx(ctx, tx, []historyEvent{{
				phase:     phaseSupersede,
				relatedID: sourceID,
			}}, []string{targetID}); err != nil {
				return false, err
			}
		}
	} else {
		if err := insertLinkTx(ctx, tx, sourceID, targetID, relation, strength, source, judgedAt); err != nil {
			return false, err
		}
		if relation == "related" {
			if err := capRelatedDegreeTx(ctx, tx, sourceID); err != nil {
				return false, err
			}
			if err := capRelatedDegreeTx(ctx, tx, targetID); err != nil {
				return false, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit create link: %w", err)
	}
	lock.reportHold("create-link", time.Now())
	return true, nil
}

const capRelatedDegreeSQL = `
WITH touching AS (
  SELECT rowid, strength, created_at FROM memory_links
  WHERE relation = 'related' AND invalidated_at IS NULL AND source_id = ?1
  UNION ALL
  SELECT rowid, strength, created_at FROM memory_links
  WHERE relation = 'related' AND invalidated_at IS NULL AND target_id = ?1
),
keepers AS (
  SELECT rowid FROM touching
  ORDER BY strength DESC, created_at, rowid
  LIMIT ?2
)
UPDATE memory_links
SET invalidated_at = datetime('now')
WHERE relation = 'related'
  AND invalidated_at IS NULL
  AND rowid IN (SELECT rowid FROM touching)
  AND rowid NOT IN (SELECT rowid FROM keepers)
`

func capRelatedDegreeTx(ctx context.Context, tx *sql.Tx, memoryID string) error {
	if _, err := tx.ExecContext(ctx, capRelatedDegreeSQL, memoryID, MaxRelatedDegree); err != nil {
		return fmt.Errorf("cap related degree: %w", err)
	}
	return nil
}

func (s *Store) TrimOverCapRelatedDegrees(ctx context.Context) error {
	s.mu.RLock()
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM (
		  SELECT source_id AS id FROM memory_links
		  WHERE relation = 'related' AND invalidated_at IS NULL
		  UNION ALL
		  SELECT target_id AS id FROM memory_links
		  WHERE relation = 'related' AND invalidated_at IS NULL
		)
		GROUP BY id
		HAVING COUNT(*) > ?
	`, MaxRelatedDegree)
	if err != nil {
		s.mu.RUnlock()
		return fmt.Errorf("find over-cap related degrees: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			s.mu.RUnlock()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		s.mu.RUnlock()
		return err
	}
	_ = rows.Close()
	s.mu.RUnlock()

	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.mu.Lock()
		_, err := s.execGuardedWrite(ctx, "trim-related-degree", capRelatedDegreeSQL, id, MaxRelatedDegree)
		s.mu.Unlock()
		if err != nil {
			return fmt.Errorf("trim related degree %s: %w", id, err)
		}
	}
	return nil
}

const linkInsertSQL = `
	INSERT INTO memory_links (source_id, target_id, relation, strength, source, created_at)
	VALUES (?, ?, ?, ?, ?, COALESCE(NULLIF(?, ''), datetime('now')))
	ON CONFLICT(source_id, target_id, relation) DO UPDATE SET
		strength = MAX(strength, excluded.strength),
		invalidated_at = NULL,
		created_at = excluded.created_at
`

func insertLinkTx(ctx context.Context, tx *sql.Tx, sourceID, targetID, relation string, strength float32, source, judgedAt string) error {
	if _, err := tx.ExecContext(ctx, linkInsertSQL,
		sourceID, targetID, relation, strength, source, judgedAt); err != nil {
		return fmt.Errorf("create link: %w", err)
	}
	return nil
}

func linkIsActive(ctx context.Context, tx *sql.Tx, sourceID, targetID, relation string) (bool, error) {
	var live int
	err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM memory_links
		WHERE source_id = ? AND target_id = ? AND relation = ? AND invalidated_at IS NULL
	`, sourceID, targetID, relation).Scan(&live)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read existing link: %w", err)
	}
	return live == 1, nil
}

// GetLinks returns live links touching a memory.
func (s *Store) GetLinks(ctx context.Context, memoryID string) ([]Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT source_id, target_id, relation, strength, source, created_at, invalidated_at
		FROM memory_links
		WHERE (source_id = ? OR target_id = ?) AND invalidated_at IS NULL
		ORDER BY strength DESC, created_at, rowid
	`, memoryID, memoryID)
	if err != nil {
		return nil, fmt.Errorf("get links: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var links []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.SourceID, &l.TargetID, &l.Relation, &l.Strength, &l.Source, &l.CreatedAt, &l.InvalidatedAt); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// MarkLinkScanned records that a memory was scanned for links.
func (s *Store) MarkLinkScanned(ctx context.Context, memoryID string, scanned []float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.execGuardedWrite(ctx, "mark-link-scanned", `
		INSERT INTO link_scans (memory_id)
		SELECT ? WHERE EXISTS (
			SELECT 1 FROM memory_embeddings WHERE memory_id = ? AND embedding = ?
		)
		ON CONFLICT(memory_id) DO UPDATE SET scanned_at = datetime('now')
	`, memoryID, memoryID, float32sToBytes(scanned))
	if err != nil {
		return fmt.Errorf("mark link scanned: %w", err)
	}
	return nil
}

// UnscannedEmbeddedMemoryIDs returns embedded memories not yet link-scanned.
func (s *Store) UnscannedEmbeddedMemoryIDs(ctx context.Context, projectID string, limit int) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
		SELECT m.id
		FROM memories m
		JOIN memory_embeddings e ON e.memory_id = m.id
		LEFT JOIN link_scans ls ON ls.memory_id = m.id
		WHERE m.project_id = ? AND ls.memory_id IS NULL
		ORDER BY m.created_at DESC
		LIMIT ?
	`
	args := []any{projectID, limit}
	if identity := s.embeddingIdentityValue(); identity != "" {
		query = `
		SELECT m.id
		FROM memories m
		JOIN memory_embeddings e ON e.memory_id = m.id
		LEFT JOIN link_scans ls ON ls.memory_id = m.id
		WHERE m.project_id = ? AND ls.memory_id IS NULL AND e.model = ?
		ORDER BY m.created_at DESC
		LIMIT ?
		`
		args = []any{projectID, identity, limit}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("unscanned embedded memories: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetEmbedding returns a memory embedding in the configured vector space.
func (s *Store) GetEmbedding(ctx context.Context, memoryID string) ([]float32, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var blob []byte
	var model string
	err := s.db.QueryRowContext(ctx, `
		SELECT embedding, model FROM memory_embeddings WHERE memory_id = ?
	`, memoryID).Scan(&blob, &model)
	if err != nil {
		return nil, fmt.Errorf("get embedding: %w", err)
	}
	if identity := s.embeddingIdentityValue(); identity != "" && model != identity {
		return nil, nil
	}
	return bytesToFloat32s(blob), nil
}

// EmbeddingStats reports embedding coverage across memories.
func (s *Store) EmbeddingStats(ctx context.Context) (embedded, stale, total int, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	identity := s.embeddingIdentityValue()
	const countQuery = `
		SELECT
			(SELECT COUNT(*) FROM memory_embeddings e JOIN memories m ON m.id = e.memory_id
				WHERE ? = '' OR e.model = ?),
			(SELECT COUNT(*) FROM memory_embeddings e JOIN memories m ON m.id = e.memory_id
				WHERE ? <> '' AND e.model <> ?),
			(SELECT COUNT(*) FROM memories)
	`
	if err = s.queryDB().QueryRowContext(ctx, countQuery, identity, identity, identity, identity).
		Scan(&embedded, &stale, &total); err != nil {
		return 0, 0, 0, fmt.Errorf("count embeddings: %w", err)
	}
	return embedded, stale, total, nil
}

// LinkStats reports link and link-scan counts.
func (s *Store) LinkStats(ctx context.Context) (links, scans int, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if err = s.queryDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_links WHERE invalidated_at IS NULL`).Scan(&links); err != nil {
		return 0, 0, fmt.Errorf("count links: %w", err)
	}
	if err = s.queryDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM link_scans`).Scan(&scans); err != nil {
		return 0, 0, fmt.Errorf("count link scans: %w", err)
	}
	return links, scans, nil
}

// SupersedesWithin returns supersedes pairs whose endpoints are in ids.
func (s *Store) SupersedesWithin(ctx context.Context, ids []string) ([][2]string, error) {
	if len(ids) < 2 {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	ph := make([]string, len(ids))
	args := make([]interface{}, 0, len(ids)*2)
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	list := strings.Join(ph, ",")
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT l.source_id, l.target_id, source_mem.scope, target_mem.scope
		FROM memory_links l
		JOIN memories source_mem ON source_mem.id = l.source_id
		JOIN memories target_mem ON target_mem.id = l.target_id
		WHERE l.relation = 'supersedes' AND l.invalidated_at IS NULL
		  AND l.source_id IN (%s) AND l.target_id IN (%s)
	`, list, list), args...)
	if err != nil {
		return nil, fmt.Errorf("supersedes within: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var pairs [][2]string
	for rows.Next() {
		var src, tgt string
		var sourceScope, targetScope sql.NullString
		if err := rows.Scan(&src, &tgt, &sourceScope, &targetScope); err != nil {
			return nil, err
		}
		if ScopesConflict(parseScope(sourceScope), parseScope(targetScope)) {
			continue
		}
		pairs = append(pairs, [2]string{src, tgt})
	}
	return pairs, rows.Err()
}

// LinksByRelationSource returns live links for a project by relation and source.
func (s *Store) LinksByRelationSource(ctx context.Context, projectID, relation, source string) ([]Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows, err := s.db.QueryContext(ctx, `
		SELECT l.source_id, l.target_id, l.relation, l.strength, l.source, l.created_at, l.invalidated_at
		FROM memory_links l
		JOIN memories source_mem ON source_mem.id = l.source_id
		JOIN memories target_mem ON target_mem.id = l.target_id
		WHERE (source_mem.project_id = ? OR source_mem.project_id = ?)
		  AND (target_mem.project_id = ? OR target_mem.project_id = ?)
		  AND l.relation = ? AND l.source = ? AND l.invalidated_at IS NULL
		ORDER BY l.created_at, l.rowid
	`, projectID, GlobalProjectID, projectID, GlobalProjectID, relation, source)
	if err != nil {
		return nil, fmt.Errorf("links by relation source: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var links []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.SourceID, &l.TargetID, &l.Relation, &l.Strength, &l.Source, &l.CreatedAt, &l.InvalidatedAt); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// LinksInto returns live links into a memory.
func (s *Store) LinksInto(ctx context.Context, projectID, memoryID, relation string) ([]Link, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if relation == "" {
		rows, err := s.db.QueryContext(ctx, `
			SELECT l.source_id, l.target_id, l.relation, l.strength, l.source, l.created_at, l.invalidated_at
			FROM memory_links l
			JOIN memories source_mem ON source_mem.id = l.source_id
			JOIN memories target_mem ON target_mem.id = l.target_id
			WHERE l.relation IN ('supersedes', 'causes') AND l.invalidated_at IS NULL
			  AND l.target_id = ?
			  AND (source_mem.project_id IN (?, ?) OR target_mem.project_id IN (?, ?))
			ORDER BY CASE l.relation WHEN 'supersedes' THEN 0 ELSE 1 END, l.created_at, l.rowid
		`, memoryID, projectID, GlobalProjectID, projectID, GlobalProjectID)
		if err != nil {
			return nil, fmt.Errorf("links into: %w", err)
		}
		defer rows.Close() //nolint:errcheck
		return scanLinks(rows)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT l.source_id, l.target_id, l.relation, l.strength, l.source, l.created_at, l.invalidated_at
		FROM memory_links l
		JOIN memories source_mem ON source_mem.id = l.source_id
		JOIN memories target_mem ON target_mem.id = l.target_id
		WHERE l.relation = ? AND l.invalidated_at IS NULL
		  AND l.target_id = ?
		  AND (source_mem.project_id IN (?, ?) OR target_mem.project_id IN (?, ?))
		ORDER BY l.created_at, l.rowid
	`, relation, memoryID, projectID, GlobalProjectID, projectID, GlobalProjectID)
	if err != nil {
		return nil, fmt.Errorf("links into: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	return scanLinks(rows)
}

func scanLinks(rows *sql.Rows) ([]Link, error) {
	var links []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.SourceID, &l.TargetID, &l.Relation, &l.Strength, &l.Source, &l.CreatedAt, &l.InvalidatedAt); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// InvalidateLink invalidates a live memory link and reports how many changed.
func (s *Store) InvalidateLink(ctx context.Context, sourceID, targetID, relation string) (int64, error) {
	if symmetricRelations[relation] && sourceID > targetID {
		sourceID, targetID = targetID, sourceID
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if relation != "supersedes" {
		res, err := s.execGuardedWrite(ctx, "invalidate-link", linkInvalidateSQL, sourceID, targetID, relation)
		if err != nil {
			return 0, fmt.Errorf("invalidate link: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, nil
		}
		return n, nil
	}

	tx, _, err := s.beginWrite(ctx, "invalidate-link")
	if err != nil {
		return 0, fmt.Errorf("begin invalidate link: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	active, err := linkIsActive(ctx, tx, sourceID, targetID, relation)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, linkInvalidateSQL, sourceID, targetID, relation)
	if err != nil {
		return 0, fmt.Errorf("invalidate link: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("invalidate link rows: %w", err)
	}
	if n > 0 && active {
		if err := appendHistoryEventsTx(ctx, tx, []historyEvent{{
			phase:     phaseUnsupersede,
			relatedID: sourceID,
		}}, []string{targetID}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit invalidate link: %w", err)
	}
	return n, nil
}

const linkInvalidateSQL = `
		UPDATE memory_links SET invalidated_at = datetime('now')
		WHERE source_id = ? AND target_id = ? AND relation = ? AND invalidated_at IS NULL
	`

// PinnedContradictedRow describes a pinned memory contradicted by a newer one.
type PinnedContradictedRow struct {
	ID                    string
	ProjectID             string
	Content               string
	ContradictedBy        string
	ContradictedByContent string
	AlsoContradictedBy    int
}

const pinnedContradictionArmTarget = `
	SELECT m.id, m.project_id, substr(m.content, 1, ` + pinnedContradictionPreview + `),
	       COALESCE(NULLIF(m.updated_at, ''), m.created_at),
	       o.id, substr(o.content, 1, ` + pinnedContradictionPreview + `),
	       COALESCE(NULLIF(o.updated_at, ''), o.created_at)
	FROM memories m
	CROSS JOIN memory_links ml ON ml.target_id = m.id AND ml.relation = 'contradicts' AND ml.invalidated_at IS NULL
	JOIN memories o ON o.id = ml.source_id
	WHERE m.pinned = 1 AND m.resolved_at IS NULL AND o.resolved_at IS NULL AND o.id <> m.id
	  AND NOT `

const pinnedContradictionArmSource = `
	SELECT m.id, m.project_id, substr(m.content, 1, ` + pinnedContradictionPreview + `),
	       COALESCE(NULLIF(m.updated_at, ''), m.created_at),
	       o.id, substr(o.content, 1, ` + pinnedContradictionPreview + `),
	       COALESCE(NULLIF(o.updated_at, ''), o.created_at)
	FROM memories m
	CROSS JOIN memory_links ml ON ml.source_id = m.id AND ml.relation = 'contradicts' AND ml.invalidated_at IS NULL
	JOIN memories o ON o.id = ml.target_id
	WHERE m.pinned = 1 AND m.resolved_at IS NULL AND o.resolved_at IS NULL AND o.id <> m.id
	  AND NOT `

const pinnedContradictionTail = `
	ORDER BY 2, 1, 5`

const pinnedContradictionPreview = "200"

// PinnedRowsWithContradictions reports pinned memories with live contradictions.
func (s *Store) PinnedRowsWithContradictions(ctx context.Context, limit int) ([]PinnedContradictedRow, int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	scope := scopesConflictSQL("m.scope", "o.scope")
	query := pinnedContradictionArmTarget + scope + " UNION " + pinnedContradictionArmSource + scope + pinnedContradictionTail
	rows, err := s.queryDB().QueryContext(ctx, query)
	if err != nil {
		return nil, 0, fmt.Errorf("pinned rows with contradictions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var result []PinnedContradictedRow
	total := 0
	lastID, shown := "", false
	for rows.Next() {
		var r PinnedContradictedRow
		var pinnedStamp, otherStamp string
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.Content, &pinnedStamp, &r.ContradictedBy, &r.ContradictedByContent, &otherStamp); err != nil {
			return nil, 0, err
		}
		pt, _ := ParseStamp(pinnedStamp)
		ot, _ := ParseStamp(otherStamp)
		if !ot.After(pt) {
			continue
		}
		if r.ID == lastID {
			if shown {
				result[len(result)-1].AlsoContradictedBy++
			}
			continue
		}
		lastID = r.ID
		total++
		shown = len(result) < limit
		if shown {
			result = append(result, r)
		}
	}
	return result, total, rows.Err()
}
