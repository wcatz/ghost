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

// memRow is one memory as it exists in the store, for set-level reflect
// grading.
type memRow struct {
	ID      string
	Content string
}

// fetchMemRows reads the project's current memories from the scratch DB.
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

// normContent canonicalizes content for matching: lowercase, whitespace
// collapsed to single spaces.
func normContent(s string) string {
	return strings.TrimSpace(wsRe.ReplaceAllString(strings.ToLower(s), " "))
}

// jaccard scores token-set overlap of two contents, 0..1. Tokenization
// mirrors memory.tokenizeContent: lowercase, split on non-alphanumerics,
// drop single-char words — so merged rewrites that preserve substance still
// score as survivors.
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
	// descendantThreshold: a row this similar to an original cluster member is
	// treated as that member's descendant — reflect rewrites merged content,
	// so exact matching is impossible by design.
	descendantThreshold = 0.55
	// survivalThreshold: a distractor with no row at least this similar was
	// dropped entirely.
	survivalThreshold = 0.65
)

// MergeResult reports one merge group's post-reflect outcome.
type MergeResult struct {
	Group     string // group id
	Status    string // collapsed | partial | lost
	Survivors string // member keys with a close surviving row
	Detail    string
}

// ReflectReport is the set-level grading of the reflect stage.
type ReflectReport struct {
	Before             int
	After              int
	Groups             []MergeResult
	DroppedDistractors []string // corpus keys with no near row after reflect
	DroppedImportant   int      // merge-group members lost with no descendant at all
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

// gradeReflect compares pre- and post-reflect store state against the corpus:
// each merge group should collapse to exactly one close survivor row;
// unannotated distractors must survive intact enough to recognize.
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
					groupRowIDs[r.ID] = true // same row may match several members
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

	// Distractor survival: entries with no annotation at all. Supersede-pair
	// NEWER members are excluded — they are pipeline participants (graded by
	// the supersede stage and reflect's old-member drop), not stability
	// fixtures, and reflect is free to rewrite them.
	targets := map[string]bool{}
	for _, e := range entries {
		if e.ExpectedSupersededBy != "" {
			targets[e.ExpectedSupersededBy] = true
		}
	}
	for _, e := range entries {
		if e.ExpectedResolved || e.ExpectedSupersededBy != "" || e.MergeGroup != "" || targets[e.Key] {
			continue // graded elsewhere or above
		}
		if bestJaccard(e.Content, rowsAfter) < survivalThreshold {
			rep.DroppedDistractors = append(rep.DroppedDistractors, e.Key)
		}
	}
	return rep
}
