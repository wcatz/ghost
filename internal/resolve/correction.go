// Correction-aware demotion: resolve walks two deterministic, free signals —
package resolve

import (
	"regexp"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

var (
	subjectTokenRe = regexp.MustCompile(`[a-z0-9_./-]{4,}`)

	subjectStopwords = map[string]bool{
		"that": true, "with": true, "this": true, "from": true, "being": true,
		"have": true, "been": true, "were": true, "will": true, "there": true,
		"also": true, "into": true, "which": true, "they": true, "each": true,
		"their": true, "about": true, "after": true, "before": true, "these": true,
		"those": true, "should": true, "could": true, "would": true, "within": true,
		"while": true, "every": true, "through": true, "during": true, "between": true,
		"because": true, "memory": true, "memories": true, "note": true, "notes": true,
		"2026": true, "09": true,
		"the": true, "and": true, "was": true, "for": true, "are": true, "but": true,
		"not": true, "you": true, "all": true, "can": true, "had": true, "her": true,
		"one": true, "our": true, "out": true, "has": true, "his": true, "via": true,
		"than": true, "only": true, "make": true, "made": true, "just": true,
		"then": true, "more": true, "most": true, "some": true, "any": true,
		"less": true, "does": true, "over": true, "very": true, "when": true,
		"what": true, "where": true, "who": true, "whom": true, "why": true,
		"how": true, "both": true,
	}

	correctionMarkers = []string{
		"correction", "correction/resolution", "corrects the",
		"is already fixed", "already fixed on main", "no pr is needed",
		"do not open a", "withdrawn", "retracted", "supersedes the",
	}

	correctionRareDF = 4

	correctionMinSharedRare = 3
)

func subjectTermSet(content string) map[string]bool {
	s := strings.ToLower(content)
	out := make(map[string]bool)
	for _, m := range subjectTokenRe.FindAllString(s, -1) {
		if !subjectStopwords[m] {
			out[m] = true
		}
	}
	return out
}

func isCorrection(content string) bool {
	lc := strings.ToLower(content)
	for _, m := range correctionMarkers {
		if strings.Contains(lc, m) {
			return true
		}
	}
	return false
}

func isOlder(a, b memory.Memory) bool {
	if a.UpdatedAt != b.UpdatedAt {
		return a.UpdatedAt < b.UpdatedAt
	}
	return a.ID < b.ID
}

// Pairing is one correction and the older, prefilter-passing candidate it
type Pairing struct {
	Correction memory.Memory
	Target     memory.Memory
}

func correctionPairingsFrom(loaded, cands, corrections []memory.Memory) []Pairing {
	if len(cands) == 0 {
		return nil
	}

	termSets := make(map[string]map[string]bool, len(loaded)+len(cands)+len(corrections))
	for _, m := range cands {
		termSets[m.ID] = subjectTermSet(m.Content)
	}
	for _, m := range corrections {
		if _, ok := termSets[m.ID]; !ok {
			termSets[m.ID] = subjectTermSet(m.Content)
		}
	}
	df := make(map[string]int)
	for _, m := range loaded {
		terms, ok := termSets[m.ID]
		if !ok {
			terms = subjectTermSet(m.Content)
			termSets[m.ID] = terms
		}
		for t := range terms {
			df[t]++
		}
	}
	rareFor := func(id string) map[string]bool {
		r := make(map[string]bool)
		for t := range termSets[id] {
			if df[t] <= correctionRareDF {
				r[t] = true
			}
		}
		return r
	}

	candRare := make(map[string]map[string]bool, len(cands))
	for _, m := range cands {
		candRare[m.ID] = rareFor(m.ID)
	}

	var correctionMems []memory.Memory
	for _, m := range corrections {
		if isCorrection(m.Content) {
			correctionMems = append(correctionMems, m)
		}
	}
	if len(correctionMems) == 0 {
		return nil
	}

	var out []Pairing
	for _, c := range correctionMems {
		cRare := rareFor(c.ID)
		for _, m := range cands {
			if m.ID == c.ID {
				continue
			}
			if !isOlder(m, c) {
				continue
			}
			shared := 0
			for t := range cRare {
				if candRare[m.ID][t] {
					shared++
				}
			}
			if shared >= correctionMinSharedRare {
				out = append(out, Pairing{Correction: c, Target: m})
			}
		}
	}
	return out
}
