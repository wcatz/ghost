package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/wcatz/ghost/eval/cycle/corpus"
	"github.com/wcatz/ghost/internal/memory"
)

type memRow struct {
	ID      string
	Content string
}

func fetchMemRows(dbPath, projectName string) ([]memRow, error) {
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close() //nolint:errcheck
	rows, err := db.Query(
		`SELECT m.id, m.content FROM memories m JOIN projects p ON m.project_id=p.id `+
			`WHERE p.name=? OR p.id=? ORDER BY m.created_at, m.id`,
		projectName, projectName)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []memRow
	for rows.Next() {
		var r memRow
		if err := rows.Scan(&r.ID, &r.Content); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

var wsRe = regexp.MustCompile(`\s+`)

func normContent(s string) string {
	return strings.TrimSpace(wsRe.ReplaceAllString(strings.ToLower(s), " "))
}

func jaccard(a, b string) float64 {
	set := func(s string) map[string]bool {
		m := map[string]bool{}
		for _, tok := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}) {
			if len(tok) > 1 {
				m[tok] = true
			}
		}
		return m
	}
	sa, sb := set(a), set(b)
	if len(sa) == 0 || len(sb) == 0 {
		return 0
	}
	inter := 0
	for tok := range sa {
		if sb[tok] {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	return float64(inter) / float64(union)
}

const (
	descendantThreshold = 0.55
	survivalThreshold   = 0.65
)

type MergeResult struct {
	Group     string
	Status    string
	Survivors string
	Detail    string
}

type ReflectReport struct {
	Before             int
	After              int
	Groups             []MergeResult
	DroppedDistractors []string
	DroppedImportant   int
}

func bestJaccard(content string, rows []memRow) float64 {
	best := 0.0
	for _, r := range rows {
		if s := jaccard(content, r.Content); s > best {
			best = s
		}
	}
	return best
}

func gradeReflect(entries []corpus.Entry, rowsBefore, rowsAfter []memRow) ReflectReport {
	rep := ReflectReport{Before: len(rowsBefore), After: len(rowsAfter)}

	groups := map[string][]corpus.Entry{}
	for _, e := range entries {
		if e.MergeGroup != "" {
			groups[e.MergeGroup] = append(groups[e.MergeGroup], e)
		}
	}
	names := make([]string, 0, len(groups))
	for g := range groups {
		names = append(names, g)
	}
	sort.Strings(names)

	for _, g := range names {
		members := groups[g]
		var survivors []string
		groupRowIDs := map[string]bool{}
		for _, m := range members {
			matched := false
			for _, r := range rowsAfter {
				if jaccard(m.Content, r.Content) >= descendantThreshold {
					groupRowIDs[r.ID] = true
					matched = true
				}
			}
			if matched {
				survivors = append(survivors, m.Key)
			}
		}
		var status, detail string
		switch rows := len(groupRowIDs); rows {
		case 0:
			status = "lost"
			rep.DroppedImportant += len(members)
			detail = fmt.Sprintf("0/%d members have any close surviving row", len(members))
		case 1:
			status = "collapsed"
			detail = fmt.Sprintf("%d/%d members recognized, collapsed onto %d row", len(survivors), len(members), rows)
		default:
			status = "partial"
			detail = fmt.Sprintf("%d/%d members recognized across %d separate rows — no merge happened", len(survivors), len(members), rows)
		}
		rep.Groups = append(rep.Groups, MergeResult{
			Group: g, Status: status, Survivors: strings.Join(survivors, ","), Detail: detail,
		})
	}

	targets := map[string]bool{}
	for _, e := range entries {
		if e.ExpectedSupersededBy != "" {
			targets[e.ExpectedSupersededBy] = true
		}
	}
	for _, e := range entries {
		if e.ExpectedResolved || e.ExpectedSupersededBy != "" || e.MergeGroup != "" || targets[e.Key] {
			continue
		}
		if bestJaccard(e.Content, rowsAfter) < survivalThreshold {
			rep.DroppedDistractors = append(rep.DroppedDistractors, e.Key)
		}
	}
	return rep
}
