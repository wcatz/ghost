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

var opposedStances = [][2]string{
	{"use", "avoid"}, {"enable", "disable"}, {"allow", "deny"}, {"allow", "forbid"}, {"always", "avoid"},
}

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

func swapTokens(content string) map[string]bool {
	t := tokenize(content)
	for _, w := range strings.FieldsFunc(strings.ToLower(content), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if w == "on" {
			t["on"] = true
		}
	}
	return t
}

func swapped(a, b string) bool {
	ta, tb := swapTokens(a), swapTokens(b)
	onlyA, onlyB := 0, 0
	for t := range ta {
		if !tb[t] {
			onlyA++
		}
	}
	for t := range tb {
		if !ta[t] {
			onlyB++
		}
	}
	return onlyA > 0 && onlyB > 0 && min(onlyA, onlyB) <= 2
}

func samePolarity(a, b memory.Memory) bool {
	if negatesText(a.Content) != negatesText(b.Content) || swapped(a.Content, b.Content) {
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

func containsAll(a, b map[string]bool) bool {
	for t := range b {
		if !a[t] {
			return false
		}
	}
	return true
}

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
