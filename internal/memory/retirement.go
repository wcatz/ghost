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

// retiresClaim is the replacement-guess form of retiresFields. It asks only
// whether the newer text retires a value token the older text still asserts,
// without the save path's guard against an older text that itself carries a
// retirement phrase. That guard stops two "no longer" notes being folded into
// one another — a WRITE decision the replacement guess never makes — but it
// also stopped a regression from retiring the fix it names: the fix says the
// crash "no longer" happens, and the regression says the fix "no longer" holds.
//
// The check is a WINDOW around the retirement phrase rather than a whole
// sentence, for the same reason: a retirement note routinely names an unrelated
// "no" or "once" in a clause beside the one that retires the value ("a tenant
// has no usage rows ... the 4f2a9c1 change no longer holds"). A plan or negation
// word WITHIN the window still blocks the claim, so "no longer needed once SSO
// ships" stays a plan and never retires anything.
func retiresClaim(newer []valueField, olderValues map[string]bool) bool {
	if len(olderValues) == 0 {
		return false
	}
	const clauseWords = 8
	for i := range newer {
		n := retirementPhraseAt(newer, i)
		if n <= 0 {
			continue
		}
		start, end := i-clauseWords, i+n+clauseWords
		if start < 0 {
			start = 0
		}
		if end > len(newer) {
			end = len(newer)
		}
		shared := false
		blocked := false
		for k := start; k < end; k++ {
			w := strings.ReplaceAll(newer[k].word, "’", "'")
			if retirementPlanWords[w] || retirementNegations[w] {
				blocked = true
				break
			}
			if k >= i && k < i+n {
				continue
			}
			if newer[k].value && olderValues[newer[k].word] {
				shared = true
			}
		}
		if !blocked && shared {
			return true
		}
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
