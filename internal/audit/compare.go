package audit

// The comparison: what Ghost retrieved, against what the agent did with it.
//
// Four buckets, in the precedence order below, and every one of them is a claim
// about the agent's OWN words — never about what it was shown, and never about
// whether the memory was any good. That second exclusion is #648's job and the
// reason "ignored" is a bucket and not a score: an audit that ranked memories
// would be reporting an opinion about the corpus on the strength of one
// transcript, and an operator reading it as advice about which memories to delete
// would be acting on a heuristic that never saw the session it is judging.
//
// Precedence is the load-bearing decision. A session that used a memory and then
// said it was wrong has the more urgent finding on it, so a contradiction is
// filed first; and a memory the agent used is not superseded by a save that
// mentions it afterwards, so use outranks the save restatement. The order is
// therefore: contradiction, identifier, token overlap, save restatement, and
// anything that matches nothing at all is ignored.

import (
	"fmt"
)

// Outcome is which bucket one retrieved memory fell in.
//
// A closed set of four, and it is a type rather than a bare string so a verdict
// cannot be built from anything else by accident. The values are what the store
// holds, which is why they are lower case with underscores: the column is read
// back by an operator's SQL as often as by this package.
type Outcome string

const (
	// OutcomeUsed: the agent's own words named the memory or repeated enough of
	// its distinctive wording to be a restatement rather than a coincidence.
	OutcomeUsed Outcome = "used"
	// OutcomeIgnored: nothing the agent wrote mentions it. NOT a usefulness score
	// — see the file comment.
	OutcomeIgnored Outcome = "ignored"
	// OutcomeSuperseded: the agent saved Ghost the same knowledge in this session,
	// which is a separate finding from a contradiction: the memory is not wrong,
	// it is out of date with what the agent now knows.
	OutcomeSuperseded Outcome = "superseded_in_session"
	// OutcomeContradicted: the agent denied this memory, in the same sentence as
	// the wording or the id it names.
	OutcomeContradicted Outcome = "contradicted"
)

// Signal is what PROVED a positive verdict, drawn from a closed set of three.
//
// It exists because "used" is not one claim. An id named in the agent's prose is
// as strong an indication as this heuristic can produce; a token overlap is a
// bar cleared, and a reader is entitled to know which of the two it was. It is
// empty on ignored, superseded and contradicted verdicts, because on those it
// records what was NOT found.
type Signal string

const (
	// SignalIdentifier: the agent named the memory's id.
	SignalIdentifier Signal = "identifier"
	// SignalToken: enough of the memory's distinctive wording was repeated.
	SignalToken Signal = "token"
	// SignalNegation: a denial cue in the same sentence as the memory's wording
	// or its id.
	SignalNegation Signal = "negation"
)

// Judged is one retrieved memory, with the content the comparison needs.
//
// Content is here and on no persisted type: it is read from the store, compared
// against fingerprints, and dropped. What leaves this function is a Verdict, and
// Verdict has no field text could go in.
type Judged struct {
	MemoryID string
	Content  string
}

// Verdict is one memory's fate in one session, and it is the whole vocabulary
// that reaches the store.
//
// Three fields, none of them text: an id, a bucket, and what proved it. The
// constraint is structural rather than a matter of discipline here — there is no
// field a sentence could be put in — so a later writer cannot widen it by
// forgetting a rule.
type Verdict struct {
	MemoryID string
	Outcome  Outcome
	Signal   Signal
}

// tokenFloor is the fewest matched fingerprints the token arm will accept, and
// the fraction is tokenFraction.
//
// Both, because either alone is wrong in a direction that matters. A fraction
// alone lets three matched words claim a memory of ninety — a coincidence, and a
// false "used" is the one error this audit cannot afford, because a memory filed
// as used is a memory nobody looks at again. An absolute floor alone is the same
// mistake scaled: three matched words out of a three-word memory is the whole
// memory, while three out of ninety is noise. So a memory is claimed when the
// agent repeated at least a third of its distinctive words AND at least three of
// them.
//
// The rule is pinned by TestCompareTokenArmNeedsEnoughOfTheMemory and
// TestCompareTokenArmNeedsEveryTokenOfAShortMemory rather than only stated here,
// because both directions of it are arithmetic and arithmetic drifts.
//
// There is deliberately no fourth constant: the negation arm uses the SAME two
// thresholds as the token arm. A lower bar for contradiction was the original
// design and it was wrong — contradiction outranks every other verdict, so a
// loose bar there reports ordinary use as a finding the operator would act on.
const (
	tokenFloor    = 3
	tokenFraction = 3
)

// Compare judges every retrieved memory against the signals, and returns one
// verdict per memory it could judge.
//
// A Judged with no id is skipped rather than given a verdict: there is nothing to
// file the verdict under and nothing to count it against, so a verdict about it
// could not be stored, reported, or purged. The count of those is the caller's
// to notice if it ever matters — it does not, because the store's own write
// refuses such a row too.
func Compare(s *Signals, judged []Judged) []Verdict {
	out := make([]Verdict, 0, len(judged))
	for _, j := range judged {
		v, ok := CompareAgainst(s, j)
		if !ok {
			continue
		}
		out = append(out, v)
	}
	return out
}

// CompareAgainst judges one memory, and reports whether it could be judged at
// all.
//
// The ok result is false only for a Judged with no MemoryID; every other input
// has a verdict, and "ignored" is what a memory with no content to match on
// gets rather than an error, because the store has memories with empty content
// and failing the whole comparison over one of them would lose the verdicts for
// every memory beside it.
func CompareAgainst(s *Signals, j Judged) (Verdict, bool) {
	if j.MemoryID == "" {
		return Verdict{}, false
	}
	toks := s.h.DistinctTokens(j.Content)

	switch {
	case s.contradicts(toks, j.MemoryID):
		return Verdict{MemoryID: j.MemoryID, Outcome: OutcomeContradicted, Signal: SignalNegation}, true
	case s.HasID(j.MemoryID):
		return Verdict{MemoryID: j.MemoryID, Outcome: OutcomeUsed, Signal: SignalIdentifier}, true
	case s.matches(toks):
		return Verdict{MemoryID: j.MemoryID, Outcome: OutcomeUsed, Signal: SignalToken}, true
	case s.matchesSaves(toks):
		return Verdict{MemoryID: j.MemoryID, Outcome: OutcomeSuperseded}, true
	}
	return Verdict{MemoryID: j.MemoryID, Outcome: OutcomeIgnored}, true
}

// matches reports whether the agent's own words repeat enough of a memory's
// distinctive wording to count as having used it.
//
// Both directions of the threshold are deliberate. Too high and a faithful
// paraphrase of a long memory is reported as ignored — a false negative an
// operator reads as "this memory is worthless", which is the harm this audit
// exists to prevent. Too low and a transcript that happens to mention "the
// lockfile" claims every memory about lockfiles. The floor and the fraction are
// the compromise, and both are asserted in the tests rather than only here.
func (s *Signals) matches(toks []string) bool {
	return clearsTokenBar(sharedTokens(s.prose, toks), len(toks))
}

// clearsTokenBar is the threshold, in one place, because the two arms that use it
// have to agree and a rule written twice is a rule that will be written twice
// differently.
func clearsTokenBar(matched, total int) bool {
	return matched >= tokenFloor && matched*tokenFraction >= total
}

// matchesSaves is matches over the words of GHOST SAVE's arguments.
//
// A save restating a memory is the agent declaring the same knowledge again,
// which is a finding about the memory's currency rather than about whether the
// agent read it — so it is a different bucket, kept out of prose for that reason
// (see AddSaveArgs). It uses the same threshold, because the evidence is the same
// shape: the agent's own words, about the same subject.
func (s *Signals) matchesSaves(toks []string) bool {
	return clearsTokenBar(sharedTokens(s.saves, toks), len(toks))
}

// contradicts reports whether the agent denied THIS memory, in one sentence.
//
// Two requirements, both STRONGER than the token arm, because contradiction
// outranks every other verdict — a false contradiction reports ordinary use as a
// memory the agent found wrong.
//
//  1. The denial must be about this memory. The cue lives in a sentence; the
//     sentence must share at least the SAME token bar the `used` arm uses (>=3
//     distinct fingerprints AND >= 3/4 of the memory's tokens), not a lower
//     threshold. Two shared tokens is too loose: a memory about "cache lockfile
//     directory" would be contradicted by any sentence mentioning two of those
//     three words in a denial context, even when the denial is about something
//     else entirely.
//
//  2. The id arm requires the cue to be present in a sentence naming the memory.
//     Without the cue, naming an id is merely USING it (the identifier arm), not
//     denying it. Requiring the cue prevents "per memory <id>, that applies" from
//     being read as contradiction.
func (s *Signals) contradicts(toks []string, memoryID string) bool {
	for _, seg := range s.negated {
		// The id arm: the sentence must both name the id AND carry a denial cue
		// (seg.fps and seg.ids were only populated when HasNegationCue was true).
		if seg.idsNamed(memoryID) {
			return true
		}
		// The fingerprint arm: the same threshold the `used` arm uses.
		if clearsTokenBar(sharedTokens(seg.fps, toks), len(toks)) {
			return true
		}
	}
	return false
}

// sharedTokens counts how many of a memory's fingerprints the transcript also
// holds.
//
// Built over the MEMORY's token set, which is the small side — a memory is a
// sentence or two — and scanned against the transcript's, which is every
// distinctive word in a session. The asymmetry is what keeps a run cheap: the
// transcript's fingerprint set is built ONCE per comparison and reused for every
// memory, so a fifty-call window costs one set rather than fifty, and the inner
// loop is a map lookup per token of a short list.
//
// Neither side holds duplicates by construction (DistinctTokens deduplicates,
// and addFingerprints does the same on the read path), so a count here is a
// count of DISTINCT shared words.
func sharedTokens(transcript, memory []string) int {
	if len(memory) == 0 {
		return 0
	}
	have := make(map[string]bool, len(transcript))
	for _, fp := range transcript {
		have[fp] = true
	}
	matched := 0
	for _, fp := range memory {
		if have[fp] {
			matched++
		}
	}
	return matched
}

// String renders a verdict for a log line, and carries no content because a
// verdict has none.
func (v Verdict) String() string {
	if v.Signal == "" {
		return fmt.Sprintf("%s %s", v.MemoryID, v.Outcome)
	}
	return fmt.Sprintf("%s %s (%s)", v.MemoryID, v.Outcome, v.Signal)
}
