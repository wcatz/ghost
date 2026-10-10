package main

import (
	"fmt"
	"regexp"
	"strings"
)

// The answer grade reads CLAUSES, not sentences and not word windows. A rejecting
// word ("not", "instead of", "retired") only means something about the spelling
// it shares a clause with, so the answer is cut into clauses first and a
// rejection never crosses a boundary: "Idempotency-Key (not X-Acme-Dedupe-Token)"
// rejects the token it is about, and "I don't have project info, but the standard
// header is X" does not reject X.
//
// A clause ends at a sentence end, a newline, one of , ; ( ) — –, a colon
// followed by a space, or the word "but". Two boundaries carry a rejection across
// the cut they make, because that is what the words mean:
//
//   - "instead of" and "rather than" reject the clause AFTER them ("use X
//     instead of Y" rejects Y and leaves X);
//   - a clause after "but" that holds a rejecting word and none of the check's
//     spellings rejects the clause BEFORE it ("Y would be the usual choice, but
//     it is not honoured here").

type clause struct {
	text     string
	rejected bool
}

var (
	// urlRe is a URL; only its host is kept for the avoid grade, so a spelling that
	// sits in a URL's path ("docs.example/Idempotency-Key") is a link, not a use.
	urlRe = regexp.MustCompile(`(?i)https?://\S+`)
	// boundary cuts clauses; the captured group, when it is one of the directional
	// words, says which way the rejection runs.
	boundary = regexp.MustCompile(`(?i)(\s+instead of\s+|\s+rather than\s+)|(\s+but\s+)|[.!?]+(?:\s+|$)|[,;()—–\n]+|:\s+`)
	// rejectRe is the rejection vocabulary, applied to the whole clause: a
	// rejecting word anywhere in the clause rejects every spelling in it.
	rejectRe = regexp.MustCompile(`(?i)\b(not|no|never|don't|doesn't|didn't|won't|isn't|aren't|wasn't|instead of|rather than|no longer|wrong|incorrect|ignored|ignores|ignore|dropped|removed|retired|gone|deprecated|replaced|superseded|obsolete|outdated|stale|expired|old|avoid)\b`)
	// notRejection is a negation that is not about the spelling: a statement about
	// the speaker ("I don't know whether X is required", "don't forget X") rather
	// than a rejection of X. It is removed before rejectRe is applied.
	notRejection = regexp.MustCompile(`(?i)\b(?:don't|do not|doesn't|does not|won't|can't|cannot|didn't)\s+(?:know|forget|remember|think|have|see|recall|care|mind|omit|skip)\b|\bnot sure\b|\bno idea\b`)
)

// hostOnly replaces each URL with its host, keeping trailing punctuation as
// text so a sentence-final "." still ends its sentence.
func hostOnly(s string) string {
	return urlRe.ReplaceAllStringFunc(s, func(u string) string {
		trail := ""
		for len(u) > 0 && strings.ContainsRune(".,;:!?)", rune(u[len(u)-1])) {
			trail = string(u[len(u)-1]) + trail
			u = u[:len(u)-1]
		}
		rest := u[strings.Index(u, "://")+3:]
		if k := strings.IndexByte(rest, '/'); k >= 0 {
			rest = rest[:k]
		}
		return rest + trail
	})
}

// clausesOf cuts an answer into clauses (lower-cased) and applies the two
// directional rules. spellings are the check's own spellings, used to tell a
// "but" clause that merely rejects from one that makes its own claim.
func clausesOf(answer string, spellings []string) []clause {
	var out []clause
	pending := false // the next clause is rejected by a leading "instead of"
	butNext := false // the next clause follows "but"
	rest := strings.ToLower(answer)
	for len(rest) > 0 {
		loc := boundary.FindStringSubmatchIndex(rest)
		var text string
		var directional, but bool
		if loc == nil {
			text, rest = rest, ""
		} else {
			text = rest[:loc[0]]
			directional = loc[2] >= 0
			but = loc[4] >= 0
			rest = rest[loc[1]:]
		}
		if strings.TrimSpace(text) != "" {
			c := clause{text: strings.TrimSpace(text), rejected: pending || rejects(text)}
			if butNext && c.rejected && !holdsAny(c.text, spellings) && len(out) > 0 {
				out[len(out)-1].rejected = true
			}
			out = append(out, c)
		}
		pending = directional
		butNext = but
	}
	return out
}

func rejects(text string) bool {
	return rejectRe.MatchString(notRejection.ReplaceAllString(text, " "))
}

func holdsAny(text string, spellings []string) bool {
	for _, a := range spellings {
		if containsToken(text, strings.ToLower(a)) {
			return true
		}
	}
	return false
}

// answerCarries is the primary grade: one of the check's spellings appears, as a
// whole token (6432 is not found in 16432), in at least one clause that does not
// reject it. It reads the answer and never the block, so a session handed the
// claim and ignoring it fails, and a session that never saw it and guessed it
// right passes — which is why the script may not contain the spelling
// (Validate).
func answerCarries(sess Session, c AnswerCheck) Check {
	name := "answer-carries:" + sessionName(sess.Index) + ":" + c.Name
	for _, cl := range clausesOf(sess.Answer, c.Any) {
		for _, a := range c.Any {
			if !cl.rejected && containsToken(cl.text, strings.ToLower(a)) {
				return Check{Name: name, Passed: true, Detail: fmt.Sprintf("the answer carries %q", a)}
			}
		}
	}
	return Check{Name: name, Passed: false, Detail: fmt.Sprintf("the answer carries none of %q outside a rejecting clause", c.Any)}
}

// answerAvoids is the negative answer grade: the answer must not USE the stale,
// expired or mistaken claim, which means a spelling must not appear in any clause
// that does not reject it. A spelling inside a URL's path is a link and is
// ignored; its host is not.
//
// When the check names Needs, an answer that mentions the spelling AT ALL
// without carrying one of the Needs spellings (outside a rejecting clause) fails
// too: naming the natural wrong answer while not supplying the right one is not
// a pass.
//
// It is a deliberately narrow reading: verbatim, deterministic, and blind to a
// paraphrase, which is what the judge column is for. Its known misgrades are
// listed in docs/benchmarks.md.
func answerAvoids(sess Session, c AnswerCheck) Check {
	name := "answer-avoids:" + sessionName(sess.Index) + ":" + c.Name
	var mentioned string
	for _, cl := range clausesOf(hostOnly(sess.Answer), c.Any) {
		for _, a := range c.Any {
			if !containsToken(cl.text, strings.ToLower(a)) {
				continue
			}
			mentioned = a
			if !cl.rejected {
				return Check{Name: name, Passed: false, Detail: fmt.Sprintf(
					"the answer uses %q in a clause that does not reject it: %s", a, oneLine(cl.text))}
			}
		}
	}
	if mentioned != "" && len(c.Needs) > 0 {
		for _, n := range c.Needs {
			if answerCarries(sess, AnswerCheck{Name: c.Name, Any: []string{n}}).Passed {
				return Check{Name: name, Passed: true, Detail: fmt.Sprintf("%q is rejected and %q is carried", mentioned, n)}
			}
		}
		return Check{Name: name, Passed: false, Detail: fmt.Sprintf(
			"the answer names %q without carrying any of %q", mentioned, c.Needs)}
	}
	return Check{Name: name, Passed: true, Detail: fmt.Sprintf("the answer does not use %q", c.Any)}
}
