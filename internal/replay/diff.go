// Package replay turns the real search calls in audit_log into a retrieval
// regression gate: it runs each recorded query against a pinned snapshot with
// two builds and reports where their ranked result sets disagree.
package replay

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/assemble"
)

// RankShift is one id that kept its place in both result sets but changed rank.
// Ranks are one-based; zero means the id was not in that result set at all.
type RankShift struct {
	ID       string
	Baseline int
	Current  int
}

// Diff is the per-query result-set comparison.
type Diff struct {
	BaselineTop   string
	CurrentTop    string
	TopHitChanged bool
	Overlap       int
	TopK          int
	Jaccard       float64
	Shifts        []RankShift
	Added         []string
	Removed       []string
}

// ComputeDiff compares the top-k windows of two ranked id lists. Ranks are read
// from the full lists so a row that moved into or out of the window still has
// an honest rank; membership is decided only by the window.
func ComputeDiff(baseline, current []string, topK int) Diff {
	if topK <= 0 {
		topK = len(baseline)
		if len(current) > topK {
			topK = len(current)
		}
	}
	baseTop, currTop := topN(baseline, topK), topN(current, topK)
	baseSet, currSet := setOf(baseTop), setOf(currTop)
	baseRank, currRank := rankOf(baseline), rankOf(current)

	d := Diff{TopK: topK, BaselineTop: firstOrEmpty(baseline), CurrentTop: firstOrEmpty(current)}
	d.TopHitChanged = d.BaselineTop != d.CurrentTop

	seen := map[string]bool{}
	for id := range baseSet {
		seen[id] = true
	}
	for id := range currSet {
		seen[id] = true
	}
	for id := range seen {
		br, cr := baseRank[id], currRank[id]
		switch {
		case br == 0 && cr > 0:
			d.Added = append(d.Added, id)
		case cr == 0 && br > 0:
			d.Removed = append(d.Removed, id)
		case br > 0 && cr > 0 && br != cr:
			d.Shifts = append(d.Shifts, RankShift{ID: id, Baseline: br, Current: cr})
		}
		if baseSet[id] && currSet[id] {
			d.Overlap++
		}
	}
	sort.Strings(d.Added)
	sort.Strings(d.Removed)
	sort.Slice(d.Shifts, func(i, j int) bool { return d.Shifts[i].ID < d.Shifts[j].ID })
	union := len(baseSet) + len(currSet) - d.Overlap
	if union > 0 {
		d.Jaccard = float64(d.Overlap) / float64(union)
	}
	return d
}

func topN(ids []string, n int) []string {
	if n >= len(ids) {
		return ids
	}
	return ids[:n]
}

func setOf(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func rankOf(ids []string) map[string]int {
	out := make(map[string]int, len(ids))
	for i, id := range ids {
		if _, exists := out[id]; !exists {
			out[id] = i + 1
		}
	}
	return out
}

func firstOrEmpty(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

// Result is one recorded query's comparison, or the error that stopped it.
type Result struct {
	Query Query
	Diff  Diff
	Err   error
}

// FormatReport renders the per-query diffs and a one-line summary. Query text
// is caller text, so it goes through the shared data renderer; ids go through
// the shared token renderer, so a stored id cannot forge a line of the report.
func FormatReport(results []Result) string {
	var b strings.Builder
	changed := 0
	for i, r := range results {
		fmt.Fprintf(&b, "query %d: %s (project %s, %s)\n", i+1,
			assemble.Data(r.Query.Query), assemble.Label(r.Query.ProjectID), assemble.Label(r.Query.Source))
		if r.Err != nil {
			fmt.Fprintf(&b, "  could not replay: %v\n", r.Err)
			continue
		}
		fmt.Fprintf(&b, "  top-%d overlap %d/%d (jaccard %.3f)\n", r.Diff.TopK, r.Diff.Overlap, r.Diff.TopK, r.Diff.Jaccard)
		if r.Diff.TopHitChanged {
			changed++
			fmt.Fprintf(&b, "  changed top hit: %s -> %s\n", renderID(r.Diff.BaselineTop), renderID(r.Diff.CurrentTop))
		}
		if len(r.Diff.Shifts) > 0 {
			b.WriteString("  rank shifts: ")
			for i, s := range r.Diff.Shifts {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "%s %d->%d", renderID(s.ID), s.Baseline, s.Current)
			}
			b.WriteString("\n")
		}
		if len(r.Diff.Added) > 0 || len(r.Diff.Removed) > 0 {
			fmt.Fprintf(&b, "  membership: +%s -%s\n", renderIDs(r.Diff.Added), renderIDs(r.Diff.Removed))
		}
	}
	fmt.Fprintf(&b, "%d quer%s replayed, %d changed top hit%s\n",
		len(results), plural(len(results), "y", "ies"), changed, plural(changed, "", "s"))
	return b.String()
}

func plural(n int, singular, many string) string {
	if n == 1 {
		return singular
	}
	return many
}

func renderID(id string) string {
	if id == "" {
		return "(none)"
	}
	return assemble.Token(id)
}

func renderIDs(ids []string) string {
	if len(ids) == 0 {
		return "(none)"
	}
	rendered := make([]string, 0, len(ids))
	for _, id := range ids {
		rendered = append(rendered, renderID(id))
	}
	return strings.Join(rendered, ",")
}
