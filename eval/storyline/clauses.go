package main

import (
	"fmt"
	"regexp"
	"strings"
)

type clause struct {
	text     string
	rejected bool
}

var (
	urlRe        = regexp.MustCompile(`(?i)https?://\S+`)
	boundary     = regexp.MustCompile(`(?i)(\s+instead of\s+|\s+rather than\s+)|(\s+but\s+)|[.!?]+(?:\s+|$)|[,;()—–\n]+|:\s+`)
	rejectRe     = regexp.MustCompile(`(?i)\b(not|no|never|cannot|unused|\w+n['’]t|instead of|rather than|no longer|wrong|incorrect|ignored|ignores|ignore|dropped|removed|retired|gone|deprecated|replaced|superseded|obsolete|outdated|stale|expired|old|avoid)\b`)
	notRejection = regexp.MustCompile(`(?i)\b(?:don't|do not|doesn't|does not|won't|can't|cannot|didn't)\s+(?:know|forget|remember|think|recall|care|mind|omit|skip)\b|\b(?:i|we)(?:\s+(?:\w+ly|just|also|still|even|yet|always|ever|quite))*\s+(?:do not|don't|won't|can't|didn't)(?:\s+(?:\w+ly|just|also|still|even|yet|always|ever|quite))*\s+(?:have|see)\b|\bnot sure\b|\bno idea\b`)
)

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
