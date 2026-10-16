package memory

import "strings"

// retires reports whether newer RETIRES older: it says older's subject is gone,
// with nothing in its place. "SendGrid was removed from parcel-notify:
// internal/mail is deleted …" retires "Order emails go out through SendGrid:
// internal/mail wraps the sendgrid-go v3 client …".
//
// The value guard (statesDifferentValue) cannot see this shape. It refuses a
// fold when EACH side holds a value token the other lacks, and a retirement
// names only what is gone, so it holds no value the retired claim lacks. The
// same-category lookup folded it: linked as a 'duplicate' of the claim it
// retires, which it strengthened, so the write that recorded a removal made the
// removed dependency rank higher. On the read side the replacement guess
// (lexicalReplacements) needs the same value swap, so the retired claim led the
// next session start, unmarked.
//
// The rule is lexical and deterministic. newer retires older when ONE SENTENCE
// of newer holds
//
//   - a retirement phrase (retirementPhrases), matched as whole words in order;
//   - a value token older also holds (valueTokens, the anchor sharesValue
//     uses): the phrase is about older's subject, not about something else the
//     note mentions. "Fixed the ingestHandler startup. The duplicate log line
//     was removed." names ingestHandler in one sentence and removes something
//     in another, and is no retirement of an ingestHandler note;
//   - no plan or condition word (retirementPlanWords): "the LEGACY_AUTH flag is
//     no longer needed once SSO ships" is a plan, and the flag is current;
//   - no negation outside the phrase (retirementNegations): "SENDGRID_API_KEY is
//     not deprecated";
//
// and older itself holds NO retirement phrase anywhere. Two notes that both
// say "no longer" are two statements about a state already changed, and the
// text cannot say which retires which; and a retirement re-saved in a second
// wording must still fold into the first. That test reads the phrase alone,
// with no anchor or plan test, so it is the stricter of the two readings.
//
// KNOWN LIMITS, the safe direction for all of them: a retirement in the active
// voice ("We removed SendGrid", "dropped Memcached") is not detected, because
// the bare verbs are left off the list (see retirementPhrases); a retirement
// that names its subject only in plain lowercase words, or only by a
// capitalised word at a sentence start ("Redis was removed"), shares no value
// token and is not detected, which is the value guard's own plain-word limit;
// and a sentence
// that also carries a plan word, a negation, or the subject in another
// sentence is not read as one. A missed retirement is two rows a supersede or
// reflect pass can still relate; a false one refuses a fold (two rows instead
// of one) and, on the read side, marks a live claim as maybe replaced without
// withholding it.
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

// retiresFields is retires over texts already read by valueFields, so
// lexicalReplacements reads each row once. olderValues is olderFields' value
// set.
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

// retiringSentence reports whether one sentence of a note retires a claim whose
// value tokens are olderValues: a retirement phrase, a value token shared with
// the claim, and no plan word and no negation outside the phrase.
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

// hasRetirementPhrase reports whether any retirement phrase occurs in fields,
// in any sentence and whatever surrounds it.
func hasRetirementPhrase(fields []valueField) bool {
	for i := range fields {
		if retirementPhraseAt(fields, i) > 0 {
			return true
		}
	}
	return false
}

// retirementPhraseAt is the length in words of the longest retirement phrase
// that starts at fields[i], or 0.
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

// retirementPhrases is the closed list of English phrases that say a thing is
// gone, matched as whole lowercased words in order (punctuation around a word
// is trimmed, so "removed," is "removed").
//
// The verbs appear in their PAST passive forms only ("was removed", "been
// deleted"). The present passive describes something a system keeps doing: "a
// job is removed from river_job on success", "the session key is deleted on
// logout" are live behaviour that shares the claim's identifiers, and a bare
// "removed"/"deleted"/"dropped" is a change-log verb with any object at all
// ("removed the duplicate log line", "dropped packets"). "no longer", "is gone",
// "not used anymore" and the three participles that only ever describe a state
// of retirement ("decommissioned", "deprecated", "retired") are kept as they
// stand. "deprecated" is kept although a deprecated thing may still exist: the
// note says it is not the current way, the line it applies to is marked and
// never withheld, and the reader sees both.
var retirementPhrases = [][]string{
	{"was", "removed"}, {"were", "removed"}, {"been", "removed"},
	{"was", "deleted"}, {"were", "deleted"}, {"been", "deleted"},
	{"was", "dropped"}, {"were", "dropped"}, {"been", "dropped"},
	{"is", "gone"}, {"are", "gone"},
	{"no", "longer"},
	{"not", "used", "anymore"},
	{"decommissioned"}, {"deprecated"}, {"retired"},
}

// retirementPlanWords mark a sentence as a plan or a condition rather than a
// report: "will no longer", "is no longer needed once X ships", "can be
// removed after the cutover", "when the TTL expires". A real retirement written
// with one of them ("After the migration, SendGrid was removed") is missed,
// which is the safe direction.
var retirementPlanWords = map[string]bool{
	"once": true, "will": true, "would": true, "should": true, "could": true,
	"can": true, "may": true, "might": true, "must": true, "shall": true,
	"if": true, "unless": true, "when": true, "whenever": true, "after": true,
	"until": true, "plan": true, "plans": true, "planned": true, "planning": true,
	"going": true, "todo": true, "soon": true, "eventually": true, "later": true,
	"propose": true, "proposed": true, "proposal": true, "intend": true, "intends": true,
	"won't": true, "can't": true, "shouldn't": true, "wouldn't": true,
}

// retirementNegations turn a retirement phrase around: "is not deprecated",
// "was never removed". A negation that is part of a phrase ("not used
// anymore") does not count against it.
var retirementNegations = map[string]bool{
	"not": true, "never": true, "isn't": true, "wasn't": true, "aren't": true,
	"weren't": true, "hasn't": true, "haven't": true, "hadn't": true,
	"don't": true, "doesn't": true, "didn't": true,
}
