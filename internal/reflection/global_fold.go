package reflection

import (
	"sort"
	"strings"
	"unicode"

	"github.com/wcatz/ghost/internal/memory"
)

// GlobalFoldCluster is one group of near-duplicate _global rows: the row that
// stays and the rows folded into it.
type GlobalFoldCluster struct {
	Survivor memory.Memory
	Folded   []memory.Memory
}

// negatesText reports whether content carries a negation: never, not, no,
// cannot, or an n't contraction. Two rows that agree on everything but this are
// opposite instructions, and the token rule scores them as near-identical
// ("always run the full test suite" against "never run the full test suite"
// differ by one stopword-sized word), so the fold keeps them apart.
func negatesText(content string) bool {
	lower := strings.ReplaceAll(strings.ToLower(content), "’", "'")
	for _, w := range strings.FieldsFunc(lower, func(r rune) bool {
		return !unicode.IsLetter(r) && r != '\''
	}) {
		switch w {
		case "never", "not", "no", "cannot", "nor", "without":
			return true
		}
		if strings.HasSuffix(w, "n't") {
			return true
		}
	}
	return false
}

// samePolarity is the pairwise guard the _global fold adds to the project rule.
func samePolarity(a, b memory.Memory) bool {
	return negatesText(a.Content) == negatesText(b.Content)
}

// containsAll reports whether every token of b is in a.
func containsAll(a, b map[string]bool) bool {
	for t := range b {
		if !a[t] {
			return false
		}
	}
	return true
}

// chooseSurvivor picks the member whose text stays. The member that contains the
// others wins (its token set is a superset of every other member's), so folding
// never drops a specific the older wording carried; when no member contains all
// the rest, or several do, the longest content wins, as SQLiteConsolidator keeps
// the longest text. The newest row breaks a tie, then the id.
//
// ReplaceNonManual then claims the OLDEST stored row with identical text (the
// first of its same-category rows in created_at, id order), so for byte-identical
// members the survivor named here is that row, and the dry run names the row
// that will actually be kept.
func chooseSurvivor(members []memory.Memory) memory.Memory {
	toks := make([]map[string]bool, len(members))
	for i, m := range members {
		toks[i] = tokenize(m.Content)
	}
	contains := make([]bool, len(members))
	anyContains := false
	for i := range members {
		contains[i] = true
		for j := range members {
			if i != j && !containsAll(toks[i], toks[j]) {
				contains[i] = false
				break
			}
		}
		anyContains = anyContains || contains[i]
	}
	best := -1
	for i, m := range members {
		if anyContains && !contains[i] {
			continue
		}
		if best < 0 || longerOrNewer(m, members[best]) {
			best = i
		}
	}
	chosen := members[best]
	// The row ReplaceNonManual will reuse for this text: among the rows holding
	// it, one in the survivor's category first, then the oldest.
	var kept *memory.Memory
	for i := range members {
		m := &members[i]
		if m.Content != chosen.Content {
			continue
		}
		if kept == nil {
			kept = m
			continue
		}
		mSame, kSame := m.Category == chosen.Category, kept.Category == chosen.Category
		if mSame != kSame {
			if mSame {
				kept = m
			}
		} else if olderRow(*m, *kept) {
			kept = m
		}
	}
	return *kept
}

func longerOrNewer(a, b memory.Memory) bool {
	if len(a.Content) != len(b.Content) {
		return len(a.Content) > len(b.Content)
	}
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt > b.CreatedAt
	}
	return a.ID > b.ID
}

func olderRow(a, b memory.Memory) bool {
	if a.CreatedAt != b.CreatedAt {
		return a.CreatedAt < b.CreatedAt
	}
	return a.ID < b.ID
}

// PlanGlobalFold finds the near-duplicate clusters among the _global rows a
// consolidation may touch and builds the replacement set that folds them. It is
// pure: no store, no model, the same rows always give the same plan.
//
// mems must already be limited to what ReplaceNonManual may replace (not
// pinned, resolved, manual, builtin or persistent), which is what makes "a
// pinned row is never touched" true here: such a row is never in the input, so
// it is never a survivor, a fold target or a row whose content is rewritten.
//
// "Near-duplicate" is duplicateClusters, the rule the SQLite tier applies to a
// project, not a second one, plus one guard: rows of opposite polarity (one
// negated, one not) never cluster. The survivor is chooseSurvivor's pick, its
// text kept verbatim so its embedding and links stay with it; it takes the
// highest importance in the cluster and the union of its tags.
//
// rows is the complete replacement set for ReplaceNonManual: every input row not
// in a cluster is restated as it is (a verbatim re-emission writes nothing), and
// each cluster contributes its survivor with ReplacesIDs naming every member, so
// the folded rows' delete history names the survivor and their evidence is
// carried onto it. Nil when there is nothing to fold, so a caller cannot replace
// a corpus it has no reason to touch.
func PlanGlobalFold(mems []memory.Memory) (clusters []GlobalFoldCluster, rows []memory.Memory) {
	var out []memory.Memory
	for _, idx := range duplicateClustersWhere(mems, samePolarity) {
		if len(idx) == 1 {
			m := mems[idx[0]]
			out = append(out, memory.Memory{
				ProjectID:  "_global",
				Category:   m.Category,
				Content:    m.Content,
				Importance: m.Importance,
				Tags:       m.Tags,
				Source:     m.Source,
			})
			continue
		}
		members := make([]memory.Memory, len(idx))
		for i, j := range idx {
			members[i] = mems[j]
		}
		survivor := chooseSurvivor(members)
		sort.SliceStable(members, func(a, b int) bool { return longerOrNewer(members[a], members[b]) })
		merged := survivor
		tagSet := map[string]bool{}
		ids := []string{survivor.ID}
		var folded []memory.Memory
		for _, m := range members {
			if m.ID != survivor.ID {
				ids = append(ids, m.ID)
				folded = append(folded, m)
			}
			if m.Importance > merged.Importance {
				merged.Importance = m.Importance
			}
			for _, t := range m.Tags {
				tagSet[t] = true
			}
		}
		merged.Tags = make([]string, 0, len(tagSet))
		for t := range tagSet {
			merged.Tags = append(merged.Tags, t)
		}
		sort.Strings(merged.Tags)
		clusters = append(clusters, GlobalFoldCluster{Survivor: survivor, Folded: folded})
		out = append(out, memory.Memory{
			ProjectID:   "_global",
			Category:    survivor.Category,
			Content:     survivor.Content,
			Importance:  merged.Importance,
			Tags:        merged.Tags,
			Source:      survivor.Source,
			ReplacesIDs: ids,
		})
	}
	if len(clusters) == 0 {
		return nil, nil
	}
	return clusters, out
}
