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

	"github.com/wcatz/ghost/internal/memory"
)

// Outcome is which bucket one retrieved memory fell in.
//
// A closed set of four, and it is a type rather than a bare string so a verdict
// cannot be built from anything else by accident. The values are what the store
// holds, which is why they are lower case with underscores: the column is read
// back by an operator's SQL as often as by this package.
//
// The values themselves are declared in internal/memory, next to the column this
// package writes and the store-wide reader reads back. That direction is the only
// one available — this package already imports internal/memory, so it cannot be
// imported back — and it means the comparison cannot spell a bucket differently
// from the aggregate that counts it. It spelled one differently once: "superseded"
// against a stored "superseded_in_session", counted in Scored and in no bucket.
type Outcome string

const (
	// OutcomeUsed: the agent's own words named the memory or repeated enough of
	// its distinctive wording to be a restatement rather than a coincidence.
	OutcomeUsed Outcome = memory.VerdictOutcomeUsed
	// OutcomeIgnored: nothing the agent wrote mentions it. NOT a usefulness score
	// — see the file comment.
	OutcomeIgnored Outcome = memory.VerdictOutcomeIgnored
	// OutcomeSuperseded: the agent saved Ghost the same knowledge in this session,
	// which is a separate finding from a contradiction: the memory is not wrong,
	// it is out of date with what the agent now knows.
	OutcomeSuperseded Outcome = memory.VerdictOutcomeSuperseded
	// OutcomeContradicted: the agent denied this memory, in the same sentence as
	// the wording or the id it names.
	OutcomeContradicted Outcome = memory.VerdictOutcomeContradicted
)

// AllOutcomes is every outcome the comparer can produce, in the order the four buckets
// are named in the figures.
//
// It is the list a reader holds the comparer's vocabulary to, and it exists because Go
// cannot enumerate constants: a fifth Outcome added above and not here would be a bucket
// this build writes and no reader of the column counts. Both halves of that are checked
// rather than trusted — TestTheOutcomeVocabularyIsOneListSpelledOnce parses the const
// block above and fails if it and this list disagree in either direction, and
// TestTheStoreWideAggregateMapsEveryOutcomeTheComparerCanStore drives the store-wide
// aggregate with each value here and fails if one lands in no bucket.
var AllOutcomes = []Outcome{OutcomeUsed, OutcomeIgnored, OutcomeSuperseded, OutcomeContradicted}

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
// distinctive wording to count as having used it — inside ONE turn.
//
// The unit is the turn and not the session (#932). A restatement is local: the
// agent that used a memory said its words together, in the turn it used them in,
// and judging the union of everything written after the call let domain
// vocabulary spread over forty turns clear a bar no single turn came near —
// reported to the operator as a memory the agent had used when no instant of the
// session was about it. The negation arm had been local from the start, for the
// same reason (see negSegment), and this brings the token arm to the same
// question. A view cut by Since carries the turns at or after its cutoff, so a
// turn written before the call cannot answer for one written after it.
//
// Both directions of the threshold are deliberate. Too high and a faithful
// paraphrase of a long memory is reported as ignored — a false negative an
// operator reads as "this memory is worthless", which is the harm this audit
// exists to prevent. Too low and a transcript that happens to mention "the
// lockfile" claims every memory about lockfiles. The floor and the fraction are
// the compromise, and both are asserted in the tests rather than only here.
func (s *Signals) matches(toks []string) bool {
	for _, tn := range s.turns {
		// An unplaced turn is carried, never evidence: the scanner could not say
		// when it was written, so it cannot be after any call.
		if tn.at <= 0 {
			continue
		}
		if clearsTokenBar(sharedTokens(tn.fps, toks), len(toks)) {
			return true
		}
	}
	return false
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
// Three requirements, all STRONGER than the token arm, because contradiction
// outranks every other verdict — a false contradiction reports ordinary use as a
// memory the agent found wrong, which is the first thing an operator acts on.
//
//  1. The denial must be in the SAME SEGMENT as the memory (see segments). A cue
//     in one sentence cannot satisfy an id named in another, so a document that
//     negates one memory and quotes three others does not contradict all four.
//
//  2. The cue must be BOUND to the memory — beside the id, or beside the words of
//     the memory it is denying (boundPositions and boundToCue, in tokens.go). The
//     two arms differ on purpose: a cue binds to an id only within a CLAUSE, so a
//     citation after a comma is not a denial, while it binds to the memory's own
//     wording across punctuation, because a colon after "that is wrong" introduces
//     the restatement rather than a new assertion about something else.
//     "Per memory <id>, that applies" names an id and agrees with it, and "Per
//     <id>, I'll ignore the formatting" names it and agrees with it again: a cue
//     ANYWHERE in a sentence is not a denial of the memory that sentence mentions,
//     it is a denial of whatever the cue is next to (#854).
//
//  3. The denial must be about this memory's OWN WORDING, on the arm that reads
//     wording: the sentence must share the SAME token bar the `used` arm uses
//     (>=3 distinct fingerprints AND >= a third of the memory's tokens), not a
//     lower threshold. Two shared tokens is too loose: a memory about "cache
//     lockfile directory" would be contradicted by any sentence mentioning two of
//     those three words in a denial context, even when the denial is about
//     something else entirely.
//
// Requirement 2 is a tightening of both arms and requirement 3 is unchanged: a
// cue alone is never enough, and neither is a shared word without a cue.
func (s *Signals) contradicts(toks []string, memoryID string) bool {
	for _, seg := range s.negated {
		// The id arm: a cue in this sentence bound to the id itself, so the
		// sentence needs neither the memory's wording nor any particular wording
		// beyond the words either side of the cue.
		if seg.denies(memoryID) {
			return true
		}
		// The fingerprint arm: the same threshold the `used` arm uses, and one of
		// the words clearing it bound to the cue.
		if seg.deniesByWording(toks) {
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
