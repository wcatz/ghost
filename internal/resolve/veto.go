// Deterministic KEEP vetoes: the cheap, free half of resolve's KEEP bias.
//
// Issue #640 measured the ordinary pass on a real database: roughly one resolve
// in three buried durable knowledge, and the notes that were buried shared two
// signals the classifier had no instruction to protect — a standing imperative
// ("NEVER run …", "must …", "do NOT …") and an open marker ("NOT YET FIXED",
// "OUTSTANDING", "still stale", "unresolved", "TODO"). A memory carrying either
// is KEEP on its face: it states a rule or an unfinished problem, so dropping it
// from session-start injection is the exact harm the pass is supposed to avoid.
//
// The veto is deliberately one-directional. A false veto costs one noisy memory
// in the ranked surface — visible, searchable, one wasted injection slot — while
// a missed veto costs a rule an agent will break or work it will repeat. So the
// lists below are broad rather than clever, and this file is the single gate:
// both the resolve pass (Run) and the repair pass (Reassess) call VetoKeep, and
// it exports nothing narrower, so a future consumer such as the context
// assembler inherits the same decision rather than re-deriving it.
package resolve

import "regexp"

// vetoPattern is one named, case-insensitive, word-bounded KEEP signal. The name
// is what VetoKeep returns as its reason, so a log line or a CLI run can say
// which rule kept a note out of the classifier.
type vetoPattern struct {
	pattern string
	re      *regexp.Regexp
}

var (
	// keepVetoImperatives mark a standing rule or instruction. These memories
	// are the ones an agent breaks when they are missing, which is why
	// "always" and "required" count as much as "never" does.
	keepVetoImperatives = []vetoPattern{
		{"never", regexp.MustCompile(`(?i)\bnever\b`)},
		{"do not", regexp.MustCompile(`(?i)\bdo not\b`)},
		// Both apostrophe forms: note text is LLM- and editor-written, and the
		// typographic U+2019 is at least as common as the ASCII one.
		{"don't", regexp.MustCompile(`(?i)\bdo(?:n[’'])t\b`)},
		{"must", regexp.MustCompile(`(?i)\bmust\b`)},
		{"always", regexp.MustCompile(`(?i)\balways\b`)},
		{"required", regexp.MustCompile(`(?i)\brequired\b`)},
	}

	// keepVetoOpenMarkers mark unfinished work. A note that says a problem is
	// still open is by definition not resolved evidence, whatever else it
	// contains — the issue's "Fixed (PR #240): … NOT YET FIXED" shape lands
	// here.
	keepVetoOpenMarkers = []vetoPattern{
		{"not yet", regexp.MustCompile(`(?i)\bnot yet\b`)},
		{"outstanding", regexp.MustCompile(`(?i)\boutstanding\b`)},
		{"still pending", regexp.MustCompile(`(?i)\bstill pending\b`)},
		{"still open", regexp.MustCompile(`(?i)\bstill open\b`)},
		{"still stale", regexp.MustCompile(`(?i)\bstill stale\b`)},
		{"unresolved", regexp.MustCompile(`(?i)\bunresolved\b`)},
		{"todo", regexp.MustCompile(`(?i)\btodo\b`)},
	}
)

// VetoKeep reports whether content carries a deterministic KEEP signal, and
// which pattern fired. A vetoed memory is KEEP with no harness call at all: it
// is not classified, and it is not written into the KEEP cache either, because
// the veto is recomputed free on every pass and a cache entry would only add a
// stale row to reason about.
//
// The veto guards the classifier, not the two free deterministic demotions in
// Run (the supersedes-edge piggyback and correction pairing). Those act on
// evidence resolve did not ask about — a link supersede's own classifier already
// adjudicated, and a correction's own text — and widening the veto to cover them
// is a separate, deliberate change.
func VetoKeep(content string) (reason string, vetoed bool) {
	if reason, vetoed := VetoKeepImperative(content); vetoed {
		return reason, true
	}
	for _, p := range keepVetoOpenMarkers {
		if p.re.MatchString(content) {
			return p.pattern, true
		}
	}
	return "", false
}

// VetoKeepImperative reports whether content states a standing rule or
// instruction, and which pattern fired — the imperative half of VetoKeep, without
// the open markers.
//
// It is exported because internal/supersede reads the SAME signal for the
// opposite decision (#686): an older note that states a rule is not a stale fact
// to be retired just because a newer, similar note exists, so supersede vetoes
// the edge and asks nothing. One vocabulary has to decide both, or a word added
// here to protect a memory from being buried would be missing from the list that
// protects it from being demoted. The open markers stay out of it deliberately:
// "still open" says a problem is unresolved, which is a claim about the state of
// the world and can be genuinely superseded, while an imperative is a rule and a
// rule is only retired by a note that says so.
func VetoKeepImperative(content string) (reason string, vetoed bool) {
	for _, p := range keepVetoImperatives {
		if p.re.MatchString(content) {
			return p.pattern, true
		}
	}
	return "", false
}
