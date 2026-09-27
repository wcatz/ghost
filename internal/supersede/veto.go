package supersede

import (
	"regexp"

	"github.com/wcatz/ghost/internal/resolve"
)

// retirementPattern is one named, case-insensitive, word-bounded signal that a
// note names a rule as retired or changed. The name is what VetoSupersede
// returns as the reason a pair was allowed through the veto and reach the
// classifier, so a log line can say which signal stood in the way of the
// free decision.
type retirementPattern struct {
	pattern string
	re      *regexp.Regexp
}

// retireMarkers are the ways a note says an older rule no longer binds. They are
// the second half of the veto, and the reason a false veto is cheap: a rule the
// newer note never retires is not a stale fact, it is a standing rule, and the
// only thing that can retire one is a note saying it was retired. The list is
// deliberately broad for that reason — a false NEGATIVE here lets the veto
// stand on a genuine supersession, which costs recall, and every marker below
// costs at most one pair's worth of that.
var retireMarkers = []retirementPattern{
	{"no longer", regexp.MustCompile(`(?i)\bno longer\b`)},
	{"retired", regexp.MustCompile(`(?i)\bretire[ds]?\b`)},
	{"removed", regexp.MustCompile(`(?i)\bremove[ds]?\b`)},
	{"deprecated", regexp.MustCompile(`(?i)\bdeprecat(?:ed|ion|ing)\b`)},
	{"obsolete", regexp.MustCompile(`(?i)\bobsolete\b`)},
	{"replaced", regexp.MustCompile(`(?i)\breplac(?:e|es|ed|ing|ement)\b`)},
	{"superseded", regexp.MustCompile(`(?i)\bsupersed(?:e|es|ed|ing)\b`)},
	{"dropped", regexp.MustCompile(`(?i)\bdrop(?:ped|s|ping)\b`)},
	// A rule that was relaxed, loosened, lifted or waived is still a rule the
	// newer note changed, and "must now" is a rule the newer note rewrote.
	{"relaxed", regexp.MustCompile(`(?i)\brelax(?:ed|es|ing|ation)\b`)},
	{"loosened", regexp.MustCompile(`(?i)\bloosen(?:ed|s|ing)\b`)},
	{"lifted", regexp.MustCompile(`(?i)\blift(?:ed|s|ing)\b`)},
	{"waived", regexp.MustCompile(`(?i)\bwaiv(?:ed|es|ing)\b`)},
	{"must now", regexp.MustCompile(`(?i)\bmust now\b`)},
	{"not required", regexp.MustCompile(`(?i)\bnot (?:be )?required\b`)},
	{"exception", regexp.MustCompile(`(?i)\bexception to\b`)},
}

// VetoSupersede reports whether a candidate pair is settled as "no supersedes
// edge" without asking anything. It fires on one signal and one only: the OLDER
// note states a standing rule, and the NEWER note does not name that rule as
// retired or changed. The second half is what makes the first safe — a newer
// note that says "the no-merge rule is retired", "X is no longer required" or
// "use Y instead of X" is a real supersession of an imperative, and it reaches
// the classifier, which judges whether the retirement is of the SAME rule.
//
// A rule is not a fact that goes out of date, and the imperative vocabulary is
// resolve's own (VetoKeepImperative) rather than a copy: the same word has to
// protect a memory from being buried by resolve and from being demoted here, or
// one pass's list would grow while the other's did not. The open markers resolve
// also vetoes on are deliberately absent — "still open" is a claim about the
// state of the world, which a later note can genuinely overturn, while an
// imperative is a rule and only a note that names it changes it.
//
// The direction of the error is the same one resolve's veto takes. A false veto
// leaves a stale note ranked and visible, and a later pass can still link it; a
// missed one buries a rule an agent would otherwise follow, and nothing in the
// ordinary pass will look at it again.
func VetoSupersede(c Candidate) (reason string, vetoed bool) {
	imperative, _ := resolve.VetoKeepImperative(c.OlderContent)
	if imperative == "" {
		return "", false
	}
	if namesRetirement(c.NewerContent) {
		return "", false
	}
	return "older note states a rule (" + imperative + ") the newer note does not retire", true
}

// namesRetirement reports whether the newer note names any rule as retired or
// changed. Only the note's own text is read, and only as a signal that the
// question is worth a classifier call: whether the retirement covers the SAME
// rule the older note stated is the semantic judgement this veto deliberately
// does not make.
func namesRetirement(content string) bool {
	for _, p := range retireMarkers {
		if p.re.MatchString(content) {
			return true
		}
	}
	return false
}
