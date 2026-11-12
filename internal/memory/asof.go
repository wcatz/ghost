package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// AsOfRow is one memory's state at the requested instant.
// Content, Category, Importance, ResolvedAt and Source come from the version row
type AsOfRow struct {
	Memory

	VersionRecordedAt string
	VersionPhase      string

	// answering from a value the change log never recorded. The statement does not

	SupersededBy string
}

// AsOfSet is one project's memory set as it stood at an instant.
// Rows is every memory that was live at T, tombstoned and not-yet-created
type AsOfSet struct {
	Rows    []AsOfRow
	Unknown []AsOfRow
	AsOf    time.Time
}

// UnknownNote is the sentence a surface must show when Unknown is non-empty, and
// "" when it is not. It is a method rather than a field so a caller cannot
func (s *AsOfSet) UnknownNote() string {
	if s == nil {
		return ""
	}
	return AsOfUnknownNote(len(s.Unknown))
}

// UnknownIDs is the ids Unknown names, for a caller that reports the gap as a
// list rather than a count.
func (s *AsOfSet) UnknownIDs() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Unknown))
	for _, r := range s.Unknown {
		out = append(out, r.ID)
	}
	return out
}

// Live returns the rows a knowledge surface shows: the known ones that were not
// resolved at T. It is a named filter so a caller cannot invent its own idea of
func (s *AsOfSet) Live() []AsOfRow {
	if s == nil {
		return nil
	}
	out := make([]AsOfRow, 0, len(s.Rows))
	for _, r := range s.Rows {
		if r.ResolvedAt == nil {
			out = append(out, r)
		}
	}
	return out
}

const asOfStampLayout = "2006-01-02 15:04:05"

func asOfStamp(t time.Time) string { return t.UTC().Format(asOfStampLayout) }

// rows, memories.project_id for the unversioned ones) and the two must not be
func asOfScopeClause(mode ProjectMode, projectID string) (string, []any) {
	switch mode {
	case AllProjects:
		return "1 = 1", nil
	case GlobalOnly:
		return "project_id = ?", []any{GlobalProjectID}
	default:
		return "(project_id = ? OR project_id = ?)", []any{projectID, GlobalProjectID}
	}
}

// ReadMemoriesAsOf reads the memory set a project held at t, from any handle —
// a Store's snapshot transaction, its own handle, or the read-only connection
func ReadMemoriesAsOf(ctx context.Context, q Queryer, mode ProjectMode, projectID string, t time.Time) (*AsOfSet, error) {
	stamp := asOfStamp(t)
	clause, clauseArgs := asOfScopeClause(mode, projectID)
	historyScope := qualifyScopeClause(clause, "h")
	memoryScope := qualifyScopeClause(clause, "m")

	rows, err := q.QueryContext(ctx, `
		WITH newest AS (
		    SELECT h.memory_id, h.project_id, h.recorded_at, h.phase, h.content,
		           h.category, h.importance, h.resolved_at, h.source, h.related_id,
		           ROW_NUMBER() OVER (PARTITION BY h.memory_id
		                               ORDER BY h.recorded_at DESC, h.rowid DESC) AS rn
		    FROM memory_history h
		    WHERE h.recorded_at <= ? AND `+historyScope+`
		),
		claim AS (
		    SELECT h.memory_id, h.phase, h.related_id,
		           ROW_NUMBER() OVER (PARTITION BY h.memory_id
		                               ORDER BY h.recorded_at DESC, h.rowid DESC) AS rn
		    FROM memory_history h
		    WHERE h.recorded_at <= ?
		      AND h.phase IN ('`+phaseSupersede+`', '`+phaseUnsupersede+`')
		      AND `+historyScope+`
		),
		versioned AS (
		    SELECT v.memory_id, v.project_id, v.recorded_at, v.phase, v.content,
		           v.category, v.importance, v.resolved_at, v.source,
		           CASE WHEN c.phase = '`+phaseSupersede+`' THEN c.related_id END AS superseded_by
		    FROM newest v
		    LEFT JOIN claim c ON c.memory_id = v.memory_id AND c.rn = 1
		    WHERE v.rn = 1 AND v.phase <> '`+phaseDelete+`'
		),
		-- The unversioned rows: the in-scope memories that existed at T and have
		-- NO version recorded at or before it. Both halves of this are time-bounded,
		-- which is the whole content of the test. An unbounded NOT EXISTS answered a
		-- different question — "does this memory have history at all" — and a
		-- pre-v17 memory that the lifecycle touched after the upgrade has history
		-- (a resolve, a supersede, a delete, a reflection reuse: only UpdateMemory
		-- files a baseline, and every other first write records the state it read),
		-- so the memory was in neither half of the read and VANISHED from an answer
		-- it belongs to. Left unsaid, a vanished memory is indistinguishable from a
		-- memory that was never there.
		--
		-- The second half reaches the tombstones, because those have no row left to
		-- select from: a delete takes the memory, so a pre-v17 memory deleted after T
		-- has neither a live row nor a version at or before T, and reporting the gap
		-- needs the tombstone's own project id. Its text is not used — a delete row
		-- is dated D and says what the memory held at D, which is not a claim about
		-- T.
		--
		-- The two halves are NOT disjoint, and two things are needed for one memory
		-- to be counted once. First, "tombstoned" does not imply "not live": a
		-- snapshot restore reinstates the row under the id it recorded (and appends
		-- its own restore row), so a memory that was deleted and then restored is
		-- live, is in scope by created_at, and has no version at or before T — it
		-- matched both halves, and one memory was disclosed as two. Second, a memory
		-- can hold MORE THAN ONE tombstone (delete, restore, delete again), so the
		-- tombstone half is ranked per memory and keeps its newest row — the same
		-- (recorded_at DESC, rowid DESC) ranking the version set uses, because both
		-- are the same question: one row per memory. Either defect alone reaches
		-- CandidateSet.Unrecorded and every surface's count.
		unrecorded AS (
		    SELECT m.id AS memory_id, m.project_id, m.created_at
		    FROM memories m
		    WHERE m.created_at <= ?
		      AND NOT EXISTS (SELECT 1 FROM memory_history h
		                      WHERE h.memory_id = m.id AND h.recorded_at <= ?)
		      AND `+memoryScope+`
		    UNION ALL
		    SELECT memory_id, project_id, NULL FROM (
		        SELECT h.memory_id, h.project_id,
		               ROW_NUMBER() OVER (PARTITION BY h.memory_id
		                                  ORDER BY h.recorded_at DESC, h.rowid DESC) AS rn
		        FROM memory_history h
		        WHERE h.phase = '`+phaseDelete+`'
		          AND NOT EXISTS (SELECT 1 FROM memory_history v
		                          WHERE v.memory_id = h.memory_id AND v.recorded_at <= ?)
		          AND NOT EXISTS (SELECT 1 FROM memories m WHERE m.id = h.memory_id)
		          AND `+historyScope+`
		    ) WHERE rn = 1
		)
		SELECT memory_id, project_id, known, phase, recorded_at, content, category,
		       importance, resolved_at, source, superseded_by,
		       created_at, updated_at, tags, pinned, scope, valid_from, valid_until,
		       verified_at, access_count, last_accessed, agent, session_id,
		       source_ref, confidence
		FROM (
		    SELECT v.memory_id, v.project_id, 1 AS known, v.phase, v.recorded_at,
		           v.content, v.category, v.importance, v.resolved_at, v.source,
		           v.superseded_by,
		           m.created_at, m.updated_at, m.tags, m.pinned, m.scope,
		           m.valid_from, m.valid_until, m.verified_at, m.access_count,
		           m.last_accessed, m.agent, m.session_id, m.source_ref, m.confidence
		    FROM versioned v
		    LEFT JOIN memories m ON m.id = v.memory_id
		    UNION ALL
		    SELECT u.memory_id, u.project_id, 0, NULL, NULL,
		           NULL, NULL, NULL, NULL, NULL,
		           NULL,
		           u.created_at, m.updated_at, m.tags, m.pinned, m.scope,
		           m.valid_from, m.valid_until, m.verified_at, m.access_count,
		           m.last_accessed, m.agent, m.session_id, m.source_ref, m.confidence
		    FROM unrecorded u
		    LEFT JOIN memories m ON m.id = u.memory_id
		)`, asOfArgs(stamp, clauseArgs)...)
	if err != nil {
		return nil, fmt.Errorf("read memories as of %s: %w", stamp, err)
	}
	defer rows.Close() //nolint:errcheck

	set := &AsOfSet{AsOf: t}
	for rows.Next() {
		var (
			r                                            AsOfRow
			known                                        int
			phase, recordedAt, content, category, source sql.NullString
			importance                                   sql.NullFloat64
			resolvedAt, supersededBy                     sql.NullString
			createdAt, updatedAt, tagsJSON               sql.NullString
			pinned                                       sql.NullInt64
			scopeRaw, validFrom, validUntil, verifiedAt  sql.NullString
			accessCount                                  sql.NullInt64
			lastAccessed, agent, sessionID, sourceRef    sql.NullString
			confidence                                   sql.NullFloat64
		)
		if err := rows.Scan(
			&r.ID, &r.ProjectID, &known, &phase, &recordedAt, &content, &category,
			&importance, &resolvedAt, &source, &supersededBy,
			&createdAt, &updatedAt, &tagsJSON, &pinned, &scopeRaw,
			&validFrom, &validUntil, &verifiedAt, &accessCount,
			&lastAccessed, &agent, &sessionID, &sourceRef, &confidence,
		); err != nil {
			return nil, fmt.Errorf("scan memories as of %s: %w", stamp, err)
		}
		// than fresh, so an unreadable age can never win a ranking.
		r.CreatedAt = asOfCreatedAt(createdAt.String, recordedAt.String, t)
		r.VersionPhase = phase.String
		r.VersionRecordedAt = recordedAt.String
		r.SupersededBy = supersededBy.String
		r.UpdatedAt = updatedAt.String
		r.Pinned = pinned.Int64 == 1
		r.AccessCount = int(accessCount.Int64)
		if tagsJSON.Valid {
			var tags []string
			if err := json.Unmarshal([]byte(tagsJSON.String), &tags); err == nil {
				r.Tags = tags
			}
		}
		r.Scope = parseScope(scopeRaw)
		// --as-of opens the store read-only) must not name a column a store predating
		r.Importance = float32(importance.Float64)
		if confidence.Valid {
			v := confidence.Float64
			r.Confidence = &v
		}
		r.LastAccessed = optionalString(lastAccessed)
		r.Agent = optionalText(agent)
		r.SessionID = optionalText(sessionID)
		r.SourceRef = optionalText(sourceRef)
		r.ValidFrom = optionalString(validFrom)
		r.ValidUntil = optionalString(validUntil)
		r.VerifiedAt = optionalString(verifiedAt)

		if known == 0 {
			// Reported, never guessed. The state columns stay empty whatever the
			set.Unknown = append(set.Unknown, r)
			continue
		}
		r.Content = content.String
		r.Category = category.String
		r.Source = source.String
		if resolvedAt.Valid {
			v := resolvedAt.String
			r.ResolvedAt = &v
		}
		set.Rows = append(set.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memories as of %s: %w", stamp, err)
	}
	sortAsOfRows(set.Rows, t)
	sort.SliceStable(set.Unknown, func(i, j int) bool {
		if set.Unknown[i].CreatedAt != set.Unknown[j].CreatedAt {
			return set.Unknown[i].CreatedAt < set.Unknown[j].CreatedAt
		}
		return set.Unknown[i].ID < set.Unknown[j].ID
	})
	return set, nil
}

func asOfArgs(stamp string, scope []any) []any {
	args := make([]any, 0, 7+3*len(scope))
	args = append(args, stamp) // version set: the instant
	args = append(args, scope...)
	args = append(args, stamp) // supersede claim: the instant
	args = append(args, scope...)
	args = append(args, stamp, stamp) // unversioned live rows: created_at, then history
	args = append(args, scope...)
	args = append(args, stamp) // unversioned tombstones: the history bound
	args = append(args, scope...)
	return args
}

// The row's own created_at is the right answer whenever it can give one: it is
func asOfCreatedAt(createdAt, versionRecordedAt string, at time.Time) string {
	stamp := parseCreatedAt(createdAt)
	if !stamp.IsZero() && !stamp.After(at) {
		return createdAt
	}
	if versionRecordedAt == "" {
		return createdAt
	}
	return versionRecordedAt
}

func optionalString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

func optionalText(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func qualifyScopeClause(clause, table string) string {
	if clause == "1 = 1" {
		return clause
	}
	return strings.ReplaceAll(clause, "project_id", table+".project_id")
}

// have done then, made by a table that never recorded it. The cost is that a
func sortAsOfRows(rows []AsOfRow, t time.Time) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		as := float64(a.Importance) * DecayFactor(a.Category, a.Content, RetentionProject, a.Pinned, ageDays(a.CreatedAt, t))
		bs := float64(b.Importance) * DecayFactor(b.Category, b.Content, RetentionProject, b.Pinned, ageDays(b.CreatedAt, t))
		if as != bs {
			return as > bs
		}
		if a.Importance != b.Importance {
			return a.Importance > b.Importance
		}
		if a.CreatedAt != b.CreatedAt {
			return a.CreatedAt > b.CreatedAt
		}
		return a.ID < b.ID
	})
}

// MemoriesAsOf reads the memory set this store's project held at t.
// A Store method because every retrieval surface goes through a Store; the
func (s *Store) MemoriesAsOf(ctx context.Context, projectID string, t time.Time) (*AsOfSet, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return ReadMemoriesAsOf(ctx, s.queryDB(), ProjectScoped, projectID, t)
}

// MemoriesAsOfAll is MemoriesAsOf with no project predicate, for a
// cross-project historical read. The set is the union of every project's, and a
func (s *Store) MemoriesAsOfAll(ctx context.Context, t time.Time) (*AsOfSet, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return ReadMemoriesAsOf(ctx, s.queryDB(), AllProjects, "", t)
}
