package memory

import (
	"strings"
	"time"
)

// The full-text index is bucketed by project (schema v28, see initSQL and
// migrateV28): an FTS row's rowid is the project's ordinal in
// memories_fts_projects times ftsBucketWidth plus the memory's own rowid. A
// project's rows are therefore one contiguous rowid range, and a MATCH carrying
// that range is answered by FTS5 for that project alone: the range is applied
// inside the virtual table's scan, so another project's matches are never
// enumerated, ranked, joined or sorted.
//
// The bucket width is 2^40, so a memory's rowid must stay below it; rowids are
// assigned as max+1 and nothing in Ghost writes an explicit one.
const (
	ftsBucketWidth = "1099511627776" // 2^40
	ftsRowidMask   = "1099511627775" // 2^40 - 1: the memory's rowid within its bucket
)

// ftsMatchSource is the FROM-clause source, aliased f, of the full-text matches
// of one MATCH query. It exposes f.mrowid (the memory's rowid, to join memories
// on) and, when withRank is set, f.frank (FTS5's rank, bm25).
//
// With projects named, each project is its own FTS5 scan bounded to its bucket,
// and a scope of two projects (a project and _global) is the UNION ALL of two
// bounded scans; a project named twice is scanned once. A project with no
// bucket has no memories, so its bound is NULL and its scan returns nothing.
// With no project named the scan is the whole index, which is what an
// all-projects search asks for.
//
// rank is the same bm25 value the unbounded scan computed for the row: bm25's
// statistics (row count, average length, each phrase's document frequency)
// are read from the whole index whichever range the scan is bounded to, and a
// row's tokens are what they always were.
func ftsMatchSource(match string, withRank bool, projects ...string) (string, []any) {
	cols := "rowid & " + ftsRowidMask + " AS mrowid"
	if withRank {
		cols += ", rank AS frank"
	}
	if len(projects) == 0 {
		return "(SELECT " + cols + " FROM memories_fts WHERE memories_fts MATCH ?) f", []any{match}
	}
	var arms []string
	var args []any
	seen := make(map[string]bool, len(projects))
	for _, p := range projects {
		if seen[p] {
			continue
		}
		seen[p] = true
		arms = append(arms, "SELECT "+cols+" FROM memories_fts WHERE memories_fts MATCH ?"+
			" AND rowid >= (SELECT ord * "+ftsBucketWidth+" FROM memories_fts_projects WHERE project_id = ?)"+
			" AND rowid < (SELECT (ord + 1) * "+ftsBucketWidth+" FROM memories_fts_projects WHERE project_id = ?)")
		args = append(args, match, p, p)
	}
	return "(" + strings.Join(arms, " UNION ALL ") + ") f", args
}

// ftsSearchProjects is the bucket list of a project-scoped keyword read: the
// project's own rows and _global's, the scope the statement's project predicate
// has always named.
func ftsSearchProjects(projectID string) []string {
	return []string{projectID, GlobalProjectID}
}

// dedupLookupStatement is one of the two duplicate lookups a save runs inside
// its write transaction (UpsertWithOptions): the same-category lookup, or with
// cross set the cross-category one. scopeClause and scopeArgs are the
// candidate exclusions the caller has already decided on.
//
// Both read the saving project's bucket alone, so the lookup's cost — and the
// write-lock hold it sits inside — follows the project's own matches rather
// than the store's. memories is reached by rowid from the FTS rows (CROSS JOIN
// fixes that order); the project predicate stays as the statement of intent
// and is a no-op on the bucket's rows. m.rowid is the last ORDER BY key because
// it is the order the store-wide scan delivered tied rows in (FTS rowid order,
// which was the memory's rowid), so ties fill the fifteen slots as they did.
func dedupLookupStatement(cross bool, scopeClause string, scopeArgs []any, projectID, category, ftsQuery string) (string, []any) {
	extra := ""
	categoryOp := "="
	if cross {
		categoryOp = "!="
		extra = `
		  AND m.resolved_at IS NULL
		  AND NOT EXISTS (
		      SELECT 1
		      FROM memory_links l
		      JOIN memories s ON s.id = l.source_id
		      WHERE l.target_id = m.id
		        AND l.relation = 'supersedes'
		        AND l.invalidated_at IS NULL
		        AND NOT ` + scopesConflictSQL("s.scope", "m.scope") + `
		  )`
	}
	source, args := ftsMatchSource(ftsQuery, true, projectID)
	query := `
		SELECT m.id, m.content
		FROM ` + source + `
		CROSS JOIN memories m ON m.rowid = f.mrowid
		WHERE m.project_id = ?
		  AND m.category ` + categoryOp + ` ?` + extra + `
		  ` + scopeClause + `
		ORDER BY f.frank, m.importance DESC, m.rowid
		LIMIT 15
	`
	args = append(args, projectID, category)
	args = append(args, scopeArgs...)
	return query, args
}

// ftsSearchStatement is the statement searchFTSValidating runs: a project's and
// _global's buckets, or with allProjects the whole index. A non-zero now adds
// the validity window predicate before the LIMIT (#1053); its placeholders are
// bound in its own text order: until, then from, both to the same
// whole-second-truncated instant. The tie order is the store-wide scan's, as in
// dedupLookupStatement.
func ftsSearchStatement(projectID, match string, limit int, now time.Time, allProjects bool) (string, []any) {
	var projects []string
	if !allProjects {
		projects = ftsSearchProjects(projectID)
	}
	source, args := ftsMatchSource(match, true, projects...)
	where := "1"
	if !allProjects {
		where = "(m.project_id = ? OR m.project_id = '_global')"
		args = append(args, projectID)
	}
	if !now.IsZero() {
		where += " AND " + validityMatchesSQLFor("m.")
		stamp := now.UTC().Format(stampLayoutForSQL)
		args = append(args, stamp, stamp)
	}
	args = append(args, limit)
	return `
		SELECT ` + memoryColumnsPrefixed + `
		FROM ` + source + `
		CROSS JOIN memories m ON m.rowid = f.mrowid
		WHERE ` + where + `
		ORDER BY f.frank, m.importance DESC, m.rowid
		LIMIT ?
	`, args
}

// keywordValidityExcludedStatement is the count keywordValidityExcluded runs:
// the keyword leg's own population (the same buckets, the same project
// predicate) less the rows the window predicate keeps. NOT (validityMatchesSQLFor(...))
// is "the predicate would have dropped this row"; its two placeholders are bound
// in the predicate's own text order (until, then from). It counts, so it asks
// FTS5 for no rank.
func keywordValidityExcludedStatement(project, match string, now time.Time, allProjects bool) (string, []any) {
	var projects []string
	if !allProjects {
		projects = ftsSearchProjects(project)
	}
	source, args := ftsMatchSource(match, false, projects...)
	where := "1"
	if !allProjects {
		where = "(m.project_id = ? OR m.project_id = '_global')"
		args = append(args, project)
	}
	stamp := now.UTC().Format(stampLayoutForSQL)
	args = append(args, stamp, stamp)
	return `SELECT COUNT(*) FROM ` + source + ` CROSS JOIN memories m ON m.rowid = f.mrowid WHERE ` + where +
		` AND NOT (` + validityMatchesSQLFor("m.") + `)`, args
}
