package memory

import (
	"strings"
	"time"
)

const (
	ftsBucketWidth = "1099511627776" // 2^40
	ftsRowidMask   = "1099511627775" // 2^40 - 1: the memory's rowid within its bucket
)

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

func ftsSearchProjects(projectID string) []string {
	return []string{projectID, GlobalProjectID}
}

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

func ftsSearchStatement(projectID, match string, limit int, now time.Time, allProjects bool) (string, []any) {

	if allProjects || projectID == GlobalProjectID {
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

	buildArm := func(bucket string) (string, []any) {
		src, args := ftsMatchSource(match, true, bucket)
		where := "m.project_id = ?"
		args = append(args, bucket)
		if !now.IsZero() {
			where += " AND " + validityMatchesSQLFor("m.")
			stamp := now.UTC().Format(stampLayoutForSQL)
			args = append(args, stamp, stamp)
		}
		args = append(args, limit)
		return `
		SELECT ` + memoryColumnsPrefixed + `, f.frank AS frank, m.rowid AS mrowid
		FROM ` + src + `
		CROSS JOIN memories m ON m.rowid = f.mrowid
		WHERE ` + where + `
		ORDER BY f.frank, m.importance DESC, m.rowid
		LIMIT ?`, args
	}

	projArm, projArgs := buildArm(projectID)
	globArm, globArgs := buildArm(GlobalProjectID)
	args := append([]any{}, projArgs...)
	args = append(args, globArgs...)

	return `
		SELECT ` + memoryColumns + `
		FROM (
			SELECT * FROM (` + projArm + `)
			UNION ALL
			SELECT * FROM (` + globArm + `)
		)
		ORDER BY frank, importance DESC, mrowid
	`, args
}

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
