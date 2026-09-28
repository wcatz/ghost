package supersede

import (
	"regexp"

	"github.com/wcatz/ghost/internal/resolve"
)

// retireMarkers are the ways a note says an older rule no longer binds. They are
// the second half of the veto, and the reason a false veto is cheap: a rule the
// newer note never retires is not a stale fact, it is a standing rule, and the
// only thing that can retire one is a note saying it was retired. The list is
// deliberately broad for that reason — a false NEGATIVE here lets the veto
// stand on a genuine supersession, which costs recall, and every marker below
// costs at most one pair's worth of that. They are bare patterns rather than
// named ones because the only question asked of them is whether one matches:
// the reason a vetoed pair reports names the IMPERATIVE that fired, which is the
// signal a reader of lifecycle.log needs.
var retireMarkers = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bno longer\b`),
	// Every inflection, verbs AND nouns: a note that says it is "retiring" the
	// rule, or that "retirement of the no-merge rule starts next sprint", has
	// retired it as far as this veto is concerned. Each form here has a word in
	// TestNamesRetirementCoversEveryInflection, and that test exists because a
	// form that stops matching is silent — on the creation pass it costs a
	// supersession, and on the repair pass it costs an edge. It also exists
	// because the form that was missing was the BARE PRESENT TENSE on six of
	// these stems — drop, deprecate, relax, loosen, lift and waive. Those six
	// entries named only the past and the progressive, so they recognised the
	// note people write once the change has happened ("we dropped the rule") and
	// missed the note they write while it is current ("we drop the rule"), which
	// left the veto standing on genuine supersessions. A marker written as a bare
	// stem with no right boundary would fix that and cost the other direction,
	// matching "dropdown" and "relaxation", so the boundary is the load-bearing
	// half: every verb marker lists its forms explicitly and ends in \b.
	regexp.MustCompile(`(?i)\bretir(?:e|es|ed|ing|al|ement)\b`),
	regexp.MustCompile(`(?i)\bremov(?:e|es|ed|ing|al)\b`),
	regexp.MustCompile(`(?i)\bdeprecat(?:e|es|ed|ing|ion)\b`),
	regexp.MustCompile(`(?i)\bobsolete\b`),
	regexp.MustCompile(`(?i)\breplac(?:e|es|ed|ing|ement)\b`),
	regexp.MustCompile(`(?i)\bsupersed(?:e|es|ed|ing)\b`),
	// drop, relax, loosen and lift do not take a silent e — their bare form IS
	// the stem — so their inflection group is optional rather than a member. The
	// trailing \b is what keeps "dropdown", "liftoff" and "looseners" out.
	regexp.MustCompile(`(?i)\bdrop(?:s|ped|ping)?\b`),
	// A rule that was relaxed, loosened, lifted or waived is still a rule the
	// newer note changed, and "must now" is a rule the newer note rewrote.
	// relax is the one marker that deliberately does NOT carry its noun:
	// "relaxation" reads as often as a Grace period or a policy term as a
	// retirement, and a marker that fires on it lets a note that retires
	// nothing through the veto. It is the only narrowing in this list, and it
	// costs a false NEGATIVE — the one error the list above is deliberately
	// broad to suppress — which is why it is called out here rather than left
	// to be discovered. The near-miss half of
	// TestNamesRetirementCoversEveryInflection pins it.
	regexp.MustCompile(`(?i)\brelax(?:es|ed|ing)?\b`),
	regexp.MustCompile(`(?i)\bloosen(?:s|ed|ing)?\b`),
	regexp.MustCompile(`(?i)\blift(?:s|ed|ing)?\b`),
	regexp.MustCompile(`(?i)\bwaiv(?:e|es|ed|ing)\b`),
	regexp.MustCompile(`(?i)\bmust now\b`),
	regexp.MustCompile(`(?i)\bnot (?:be )?required\b`),
	regexp.MustCompile(`(?i)\bexception to\b`),
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
	for _, re := range retireMarkers {
		if re.MatchString(content) {
			return true
		}
	}
	return false
}
