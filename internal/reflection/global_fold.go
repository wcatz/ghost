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

// stanceWord maps the forms of the opposed verbs the fold knows to one stem.
var stanceWord = map[string]string{
	"use": "use", "uses": "use", "using": "use",
	"avoid": "avoid", "avoids": "avoid", "avoiding": "avoid",
	"enable": "enable", "enables": "enable", "enabled": "enable", "enabling": "enable",
	"disable": "disable", "disables": "disable", "disabled": "disable", "disabling": "disable",
	"allow": "allow", "allows": "allow", "allowed": "allow", "allowing": "allow",
	"deny": "deny", "denies": "deny", "denied": "deny",
	"forbid": "forbid", "forbids": "forbid", "forbidden": "forbid",
	"always": "always",
}

// opposedStances are the pairs of stems that, split across two rows, make them
// opposite instructions that the token rule would otherwise call duplicates.
var opposedStances = [][2]string{
	{"use", "avoid"}, {"enable", "disable"}, {"allow", "deny"}, {"allow", "forbid"}, {"always", "avoid"},
}

// stanceInfo is what the guard reads from a row: its stance stems and the object
// of "prefer" (the first word after it that is not a connective), if any.
func stanceInfo(content string) (stems map[string]bool, preferred string) {
	stems = map[string]bool{}
	words := strings.FieldsFunc(strings.ToLower(content), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for i, w := range words {
		if st, ok := stanceWord[w]; ok {
			stems[st] = true
		}
		switch w {
		case "prefer", "prefers", "preferred", "preferring":
			for _, n := range words[i+1:] {
				if n == "to" || n == "using" || n == "use" || n == "the" || n == "a" || n == "an" {
					continue
				}
				if preferred == "" {
					preferred = n
				}
				break
			}
		}
	}
	return stems, preferred
}

// samePolarity is the pairwise guard the _global fold adds to the project rule:
// two rows never cluster when one is negated and the other is not, when they take
// opposed stances (use/avoid, enable/disable, allow/deny or forbid, always/avoid),
// or when both prefer something and the preferred things differ. It is a
// conservative list, not a classifier: a miss costs a duplicate that stays, a
// false cluster would delete the opposite of an instruction.
func samePolarity(a, b memory.Memory) bool {
	if negatesText(a.Content) != negatesText(b.Content) {
		return false
	}
	sa, pa := stanceInfo(a.Content)
	sb, pb := stanceInfo(b.Content)
	for _, p := range opposedStances {
		if (sa[p[0]] && sb[p[1]]) || (sa[p[1]] && sb[p[0]]) {
			return false
		}
	}
	return pa == pb || pa == "" || pb == ""
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
// consolidation may touch. It is pure: no store, no model, the same rows always
// give the same plan, and nil when there is nothing to fold.
//
// mems should already be limited to what a fold may touch (reflection-written,
// not pinned, resolved or persistent); the store re-checks each row when the
// fold is applied.
//
// "Near-duplicate" is duplicateClusters, the rule the SQLite tier applies to a
// project, not a second one, plus the guards in samePolarity: rows of opposite
// polarity or stance never cluster. The survivor is chooseSurvivor's pick; the
// store keeps its text verbatim and gives it the highest importance and the
// union of the tags in the cluster.
func PlanGlobalFold(mems []memory.Memory) []GlobalFoldCluster {
	var clusters []GlobalFoldCluster
	for _, idx := range duplicateClustersWhere(mems, samePolarity) {
		if len(idx) == 1 {
			continue
		}
		members := make([]memory.Memory, len(idx))
		for i, j := range idx {
			members[i] = mems[j]
		}
		survivor := chooseSurvivor(members)
		sort.SliceStable(members, func(a, b int) bool { return longerOrNewer(members[a], members[b]) })
		var folded []memory.Memory
		for _, m := range members {
			if m.ID != survivor.ID {
				folded = append(folded, m)
			}
		}
		clusters = append(clusters, GlobalFoldCluster{Survivor: survivor, Folded: folded})
	}
	return clusters
}
