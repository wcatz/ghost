package memory

import "strings"

func retires(newer, older string) bool {
	of := valueFields(older)
	ov := make(map[string]bool, len(of))
	for _, f := range of {
		if f.value {
			ov[f.word] = true
		}
	}
	return retiresFields(valueFields(newer), of, ov)
}

func retiresFields(newer, olderFields []valueField, olderValues map[string]bool) bool {
	if len(olderValues) == 0 || hasRetirementPhrase(olderFields) {
		return false
	}
	for start := 0; start < len(newer); {
		end := start + 1
		for end < len(newer) && newer[end].sentence == newer[start].sentence {
			end++
		}
		if retiringSentence(newer[start:end], olderValues) {
			return true
		}
		start = end
	}
	return false
}

func retiringSentence(s []valueField, olderValues map[string]bool) bool {
	inPhrase := make([]bool, len(s))
	found := false
	for i := range s {
		if n := retirementPhraseAt(s, i); n > 0 {
			found = true
			for k := i; k < i+n; k++ {
				inPhrase[k] = true
			}
		}
	}
	if !found {
		return false
	}
	shared := false
	for i, f := range s {
		if inPhrase[i] {
			continue
		}
		w := strings.ReplaceAll(f.word, "’", "'")
		if retirementPlanWords[w] || retirementNegations[w] {
			return false
		}
		if f.value && olderValues[f.word] {
			shared = true
		}
	}
	return shared
}

func hasRetirementPhrase(fields []valueField) bool {
	for i := range fields {
		if retirementPhraseAt(fields, i) > 0 {
			return true
		}
	}
	return false
}

func retirementPhraseAt(fields []valueField, i int) int {
	best := 0
	for _, p := range retirementPhrases {
		if len(p) <= best || i+len(p) > len(fields) {
			continue
		}
		match := true
		for k, w := range p {
			if fields[i+k].word != w {
				match = false
				break
			}
		}
		if match {
			best = len(p)
		}
	}
	return best
}

var retirementPhrases = [][]string{
	{"was", "removed"}, {"were", "removed"}, {"been", "removed"},
	{"was", "deleted"}, {"were", "deleted"}, {"been", "deleted"},
	{"was", "dropped"}, {"were", "dropped"}, {"been", "dropped"},
	{"is", "gone"}, {"are", "gone"},
	{"no", "longer"},
	{"not", "used", "anymore"},
	{"decommissioned"}, {"deprecated"}, {"retired"},
}

var retirementPlanWords = map[string]bool{
	"once": true, "will": true, "would": true, "should": true, "could": true,
	"can": true, "may": true, "might": true, "must": true, "shall": true,
	"if": true, "unless": true, "when": true, "whenever": true, "after": true,
	"until": true, "plan": true, "plans": true, "planned": true, "planning": true,
	"going": true, "todo": true, "soon": true, "eventually": true, "later": true,
	"propose": true, "proposed": true, "proposal": true, "intend": true, "intends": true,
	"won't": true, "can't": true, "shouldn't": true, "wouldn't": true,
}

var retirementNegations = map[string]bool{
	"not": true, "never": true, "isn't": true, "wasn't": true, "aren't": true,
	"weren't": true, "hasn't": true, "haven't": true, "hadn't": true,
	"don't": true, "doesn't": true, "didn't": true,
}
