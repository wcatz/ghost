package memory

// Historical retrieval: what a project's memory set was at an instant T
// (issue #647). The live tables keep one value per memory — the last one — so
// no query over them can answer "what did Ghost believe at T". memory_history
// can, because each of its rows is a VERSION rather than a diff: the state a
// memory held once the write named by its phase landed. The newest row at or
// before T is therefore the state at T, with no reconstruction and no
// inference involved anywhere in this file.

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
//
// Content, Category, Importance, ResolvedAt and Source come from the version row
// — the state the memory held once that write landed. ProjectID comes from the
// same row, because a memory that was promoted into _global after T was the
// project's at T, and a read that reported the project it holds NOW would place
// a memory in a bucket it was not in at the instant asked about.
//
// The rest of the embedded Memory is the CURRENT row. That is not an oversight
// and it is bounded: memory_history records exactly the five state columns
// above plus the event's other end, so tags, scope, pin, access count,
// provenance and the validity triple were never versioned. They are read from
// the row as it stands, and the doc on this type says so where a reader of the
// value will see it. A deleted memory has no row at all, so those fields are
// zero for it — the honest reading, since nothing records what they held.
//
// CreatedAt is the exception, and it is the one field here the decay measures an
// age from: it is the row's own when that column can answer, and the version
// row's recorded_at when it cannot (the row is gone, or its created_at is later
// than the instant asked about). See asOfCreatedAt for why, and docs/
// architecture.md's Historical retrieval section for the bias that would
// otherwise reach a listing.
type AsOfRow struct {
	Memory

	// VersionRecordedAt and VersionPhase name the write whose state this row
	// carries, so a reader can tell a memory that was merely saved before T from
	// one that was edited, resolved or rewritten then. The instant is the
	// history row's own recorded_at: the moment the write landed is the earliest
	// moment this state was true, and a row that changed twice in the same
	// second is ordered after both by rowid.
	VersionRecordedAt string
	VersionPhase      string

	// SupersededBy is the memory whose active `supersedes` edge claimed this one
	// at T, and "" when no claim was live. It is read from the supersede /
	// unsupersede SEQUENCE rather than from the version row, because a supersede
	// records a claim about a memory rather than a change to it: the target's own
	// state columns are identical whether the claim is live or withdrawn, and a
	// memory can be superseded, withdrawn and superseded again, so only the
	// sequence says which claim stood.
	SupersededBy string
}

// AsOfSet is one project's memory set as it stood at an instant.
//
// Rows is every memory that was live at T, tombstoned and not-yet-created
// memories already dropped, ordered the way a presentation surface wants them:
// the same composite the current read uses (DecayFactor over the historical
// category, importance, pin and age measured to T) with the same tie-break
// chain, so a historical block and a current one are ordered by one rule.
// Resolved memories are INCLUDED, because a memory that was known and then
// resolved was part of what the store knew; a surface that shows only live
// knowledge filters them itself, exactly as it does on the current read.
//
// Unknown is the honest half of the answer: the live rows in scope that have no
// recorded version at or before T. A memory written before schema v17 has none
// (migrateV17 backfills nothing), so the store cannot say what it said at T,
// and Unknown is where that fact is reported rather than guessed into Rows.
type AsOfSet struct {
	Rows    []AsOfRow
	Unknown []AsOfRow
	// AsOf is the instant the read was asked for, echoed back so a caller that
	// passed it through several layers can label its output from the set rather
	// than from the argument it happens to still be holding.
	AsOf time.Time
}

// UnknownNote is the sentence a surface must show when Unknown is non-empty, and
// "" when it is not. It is a method rather than a field so a caller cannot
// render a disclosure that is no longer true: the note is a statement about this
// set, and a set that is complete has none to make.
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
// "live" — the current read excludes resolved rows in SQL
// (GetTopMemories) and a historical read has to exclude them by the same rule
// or the two surfaces would disagree about what a project knows.
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

// asOfStampLayout is the format SQLite's datetime() writes, and therefore the
// format memory_history.recorded_at holds. The bound comparison in the read is
// a string comparison, which is only correct because both sides are this layout
// — so the Go side formats through it rather than reaching for RFC 3339, which
// would sort after every stored stamp and silently return the whole history.
const asOfStampLayout = "2006-01-02 15:04:05"

// asOfStamp renders an instant the way the history table stores one. A caller
// that passed a local time gets the UTC rendering, because datetime('now') is
// UTC and the comparison is lexicographic.
func asOfStamp(t time.Time) string { return t.UTC().Format(asOfStampLayout) }

// asOfScopeClause is the project predicate the read uses, per project mode, and
// the arguments that go with it. It is the same three-way distinction the
// retrieval legs make, and it exists as a function because the read applies the
// predicate to two different tables (memory_history.project_id for the versioned
// rows, memories.project_id for the unversioned ones) and the two must not be
// able to disagree about which memories are in scope.
func asOfScopeClause(mode ProjectMode, projectID string) (string, []any) {
	switch mode {
	case AllProjects:
		return "1 = 1", nil
	case GlobalOnly:
		// The legs spell _global out rather than relying on an empty project id
		// matching nothing, so every mode issues one statement shape and no row
		// can be pulled in by an empty project id.
		return "project_id = ?", []any{GlobalProjectID}
	default:
		return "(project_id = ? OR project_id = ?)", []any{projectID, GlobalProjectID}
	}
}

// ReadMemoriesAsOf reads the memory set a project held at t, from any handle —
// a Store's snapshot transaction, its own handle, or the read-only connection
// the session hook opens. It is a package function rather than a method for the
// same reason SupersedePenalties and DemotionPenalties are: two of the three
// callers hold no Store.
//
// One statement, not two. The versioned rows and the unversioned ones partition
// the in-scope set, and a pair of statements would leave a concurrent write
// free to land between them — which would either double-count a memory or hide
// a gap that had just closed. UNION ALL inside one statement cannot.
func ReadMemoriesAsOf(ctx context.Context, q Queryer, mode ProjectMode, projectID string, t time.Time) (*AsOfSet, error) {
	stamp := asOfStamp(t)
	clause, clauseArgs := asOfScopeClause(mode, projectID)
	historyScope := qualifyScopeClause(clause, "h")
	memoryScope := qualifyScopeClause(clause, "m")

	// The version set: the newest row at or before T for every in-scope memory
	// that had one, and the newest supersede/unsupersede row for each of those.
	// Both window functions partition by memory_id and rank by (recorded_at,
	// rowid): recorded_at alone is second-precision, so every write one
	// reflection makes shares a timestamp, and the newest among them would be a
	// tie-break rather than an answer. rowid is insertion order and is never
	// reused, so it is the honest second key.
	//
	// Scope is read from the history row's own project_id, not from the memory's:
	// a promotion moves the history rows with the row, so at T the history says
	// which project held the memory, and that is the question being asked.
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
		-- T. The two halves are disjoint (a tombstoned memory is not live), so the
		-- UNION cannot double-count.
		unrecorded AS (
		    SELECT m.id AS memory_id, m.project_id, m.created_at
		    FROM memories m
		    WHERE m.created_at <= ?
		      AND NOT EXISTS (SELECT 1 FROM memory_history h
		                      WHERE h.memory_id = m.id AND h.recorded_at <= ?)
		      AND `+memoryScope+`
		    UNION ALL
		    SELECT h.memory_id, h.project_id, NULL
		    FROM memory_history h
		    WHERE h.phase = '`+phaseDelete+`'
		      AND NOT EXISTS (SELECT 1 FROM memory_history v
		                      WHERE v.memory_id = h.memory_id AND v.recorded_at <= ?)
		      AND `+historyScope+`
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
		// created_at is nullable here and not on the live table: a tombstoned
		// memory's row is gone, so the join yields NULL for everything the
		// version row does not carry. That is the honest reading — nothing
		// records what the row held — and the decay treats it as ancient rather
		// than fresh, so an unreadable age can never win a ranking.
		r.CreatedAt = asOfCreatedAt(createdAt.String, recordedAt.String, t)
		r.VersionPhase = phase.String
		r.VersionRecordedAt = recordedAt.String
		r.SupersededBy = supersededBy.String
		// The current row's fields, and only from the current row: a tombstoned
		// memory's row is gone, so the join yields NULL for all of them and the
		// zero values below are the honest reading rather than a guess.
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
			// row holds NOW: today's text is an answer to a different question,
			// and a caller that wanted today's text has the current read.
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

// asOfArgs binds the statement's placeholders in the order they appear: the
// statement is four reads of the same scope and the same instant — the version
// set, the supersede claim, the unversioned live rows, the unversioned
// tombstones — so the arguments repeat with them.
//
// Written out rather than generated from the CTE list, because the order IS the
// statement's and a generated list would have to be kept in step with it by hand:
// binding a scope argument to an instant fails loudly, but binding one instant to
// another bound fails as a plausible answer. The unversioned live half is the one
// that takes two instants, because it bounds created_at by T and the "is there a
// version at or before T" check by T as well.
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

// asOfCreatedAt is the instant a memory's AGE is measured from at T.
//
// The row's own created_at is the right answer whenever it can give one: it is
// set on INSERT and never rewritten (a snapshot restore carries the snapshot's
// created_at back on both its UPDATE and its INSERT, which is what stopped a
// restore from making an old memory read as new). Two cases it cannot answer, and
// both fall back to the version row's own recorded_at — the last instant at which
// the state this read is returning was established, which is a fact about the past
// by construction, since the version row is at or before T by definition:
//
//   - The row is gone. A delete takes the memory and leaves the tombstone, so
//     every current column is NULL and an empty created_at parses as the zero
//     time: ageDays then reads as ~56,000 days and DecayFactor sits at its floor.
//     A memory deleted yesterday was therefore pushed to the bottom of every
//     listing covering the past year, on the strength of a column that says
//     nothing about it.
//   - created_at is LATER than T, which no current writer produces (an artifact
//     that omits it imports with created_at = the import instant, which is its own
//     version row's). It is refused rather than believed because ageDays clamps a
//     negative age at 0, so a future-dated column is the single most favourable
//     value a row can carry into a ranking of the past — a hand-edited store, or a
//     future writer, would silently win every capped listing it appeared in.
//
// A version row that is itself absent (the gap rows) leaves both empty, which is
// the honest reading for a row that is not in the set: nothing measures its age.
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

// optionalString reads a nullable text column into the *string the Memory type
// carries, and nil for a NULL column. NULL stays nil so "no claim" cannot be
// confused with a claim about the empty string — the same rule scanMemories
// applies, and the reason the validity columns are pointers at all.
func optionalString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

// optionalText is optionalString for the fields that read NULL as "unknown" and
// so collapse to the empty string. It is what scanMemories does inline for the
// same fields, and there is no meaningful empty value for an agent, session or
// reference.
func optionalText(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

// qualifyScopeClause rewrites a project predicate written against an
// unaliased `project_id` so it names one table. The read applies the same
// predicate to memory_history and to memories, and a clause that reached the
// wrong table would be a SQL error at best and a different answer at worst.
func qualifyScopeClause(clause, table string) string {
	if clause == "1 = 1" {
		return clause
	}
	return strings.ReplaceAll(clause, "project_id", table+".project_id")
}

// sortAsOfRows orders a set the way GetTopMemories orders the current one:
// composite score descending, then importance, then the newer memory, then id.
//
// The composite is DecayFactor over the HISTORICAL category, importance and pin
// with the age measured to T — the same rule, evaluated at the instant asked
// about. It is computed in Go rather than as a second SQL expression on the
// version columns because DecayFactor is the Go mirror of DecayRankingSQL and
// there is a parity test between those two; a third spelling of the decay
// formula would have no such test and would drift the first time the SQL
// constant changed.
func sortAsOfRows(rows []AsOfRow, t time.Time) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		as := float64(a.Importance) * DecayFactor(a.Category, a.Pinned, ageDays(a.CreatedAt, t))
		bs := float64(b.Importance) * DecayFactor(b.Category, b.Pinned, ageDays(b.CreatedAt, t))
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
//
// A Store method because every retrieval surface goes through a Store; the
// read-only handle the session hook opens uses ReadMemoriesAsOf directly, with
// the same arguments. No transaction is opened: the read is one statement, so
// there is no second query for a concurrent write to slip between, and taking a
// snapshot on the primary handle would hold the write lock for the whole read
// (see warnNoReadHandle).
func (s *Store) MemoriesAsOf(ctx context.Context, projectID string, t time.Time) (*AsOfSet, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return ReadMemoriesAsOf(ctx, s.queryDB(), ProjectScoped, projectID, t)
}

// MemoriesAsOfAll is MemoriesAsOf with no project predicate, for a
// cross-project historical read. The set is the union of every project's, and a
// version row's own project_id decides which bucket each memory falls in, so a
// memory that had already been promoted is counted under the bucket it was in at
// T.
func (s *Store) MemoriesAsOfAll(ctx context.Context, t time.Time) (*AsOfSet, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return ReadMemoriesAsOf(ctx, s.queryDB(), AllProjects, "", t)
}
