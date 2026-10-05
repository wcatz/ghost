package audit

import (
	"encoding/hex"
	"sort"
	"strings"
	"unicode"
)

// #646 part 2: what the agent DID with what Ghost retrieved.
//
// The comparison is a heuristic and says so on every surface that reports it.
// What makes it trustworthy is not cleverness but the line it refuses to cross:
// only the agent's OWN words count as evidence. A transcript holds both what the
// agent said and what Ghost handed it, and the injected block repeats the
// memory's wording verbatim — so a comparison that read the whole file would
// report every retrieval as used, whatever the agent did, and the audit would be
// worthless in the exact way it was built to detect.
//
// So the transcript side of this (internal/hostevent) reads ASSISTANT-authored
// regions only: prose, tool-call arguments, and the arguments of Ghost's own save
// tools. The memory side is a store read that is never persisted. Nothing here
// ever holds a transcript, a query, or a memory's content once a verdict is
// decided.

// minTokenLen is the shortest word a token fingerprint is taken over. Below it a
// "token" is a fragment — "the", "a", "is", a two-letter argument name — and
// matching on those measures how often English and a shell agree, not whether a
// memory was read.
const minTokenLen = 4

// stopWords are the words that survive minTokenLen and still carry no signal.
// Kept as a list rather than a rule because a rule ("anything a corpus of prose
// uses often") needs a corpus, and the corpus that matters changes with the
// memory: "under" is a stopword in a sentence about filesystems and a
// distinctive token in one about layout.
var stopWords = map[string]bool{
	"about": true, "after": true, "again": true, "against": true, "along": true,
	"already": true, "also": true, "another": true, "around": true, "because": true,
	"been": true, "before": true, "being": true, "below": true, "between": true,
	"both": true, "came": true, "can": true, "come": true, "could": true,
	"does": true, "doing": true, "done": true, "down": true, "during": true,
	"each": true, "either": true, "else": true, "even": true, "ever": true,
	"every": true, "from": true, "further": true, "have": true, "having": true,
	"here": true, "however": true, "into": true, "itself": true, "just": true,
	"like": true, "made": true, "make": true, "many": true, "more": true,
	"most": true, "much": true, "must": true, "only": true, "other": true,
	"over": true, "same": true, "shall": true, "should": true, "since": true,
	"some": true, "still": true, "such": true, "than": true, "that": true,
	"their": true, "them": true, "then": true, "there": true, "these": true,
	"they": true, "this": true, "those": true, "through": true, "thus": true,
	"under": true, "until": true, "used": true, "using": true, "very": true,
	"were": true, "what": true, "when": true, "where": true, "which": true,
	"while": true, "will": true, "with": true, "within": true, "would": true,
	"your": true,
}

// hex64 renders a hash the way the sidecar and both readers agree on: sixteen
// lower-case hex characters, zero-padded. Fixed width matters because the file is
// parsed positionally.
//
// It is a RENDERER, not the hash: the hash itself is Hasher.Fingerprint, and
// keeping the two apart matters because a render is the one part of a token
// anybody may recompute from the file, while the value it renders is the part
// that took a key.
func hex64(sum []byte) string {
	var buf [16]byte
	n := hex.Encode(buf[:], sum)
	// A shorter digest would leave the tail zero-padded, which is a DIFFERENT
	// sixteen-character field rather than a shorter one — the file is parsed by
	// width, so padding would turn a truncation into a plausible token that
	// matches nothing. Truncating instead keeps the field's meaning honest.
	return string(buf[:n])
}

// unhex64 REFUSES anything that is not sixteen lower-case hex characters. A
// truncated, padded or upper-cased field would silently become a different
// token — a token nothing in the session ever matched — so a malformed line is
// an error the reader reports rather than a value it invents.
//
// Lower-case only, and deliberately: an id is upper-case, so the two vocabularies
// stay disjoint and isIDField can tell them apart by case alone. Widening this to
// A-F would be the same defect class isHex had — see parseNegSegment.
func unhex64(s string) (string, bool) {
	if len(s) != 16 {
		return "", false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return s, true
}

// DistinctTokens lives on Hasher, in hasher.go: a token is a function of the
// text AND the install key, so there is no text-only spelling of it to reach for
// by accident. This file is the tokenizer both sides share.

// splitWords is the ONE tokenizer, used by both sides of the comparison.
//
// Letters and digits, lower-cased, on every boundary. The asymmetry that matters
// is what is DROPPED rather than what is kept: "rm-rfs" is one word, not three,
// so an agent writing about a memory's "rm -rfs" wording cannot match it — which
// is the conservative direction, since a missed match is an honest "ignored"
// while a spurious one is a false "used".
//
// It DROPS punctuation, which is what makes "Per <id>, I'll ignore the
// formatting" and "The build is stale, the memory <id> applies" the same list of
// words — and the second of those is a use filed as a contradiction, because the
// binding rule counts the words between a cue and an id and cannot see that a
// comma is among them. So the tokenizer that does the binding carries the clause
// each word falls in as well, and this is the half of it with no use for one —
// which is every caller except the negation arm, since the token and id arms want
// words and nothing else.
func splitWords(text string) []string {
	words, _ := splitClauses(text)
	return words
}

// splitClauses is splitWords plus, for each word, the CLAUSE it falls in.
//
// A parallel slice rather than a token that keeps its punctuation, because the
// punctuation is not something a cue or a memory token should ever match: what
// the binding needs to know is only whether a boundary FELL between two words,
// and attaching the glyph to a word would put "is," and "is" in the same
// vocabulary.
//
// Clause ids are assigned in order and only ever increase, so one comparison —
// are two positions in the same clause — is the whole of what asks whether a
// clause boundary separates them.
//
// The boundaries themselves are named in clauseBoundary rather than inferred from
// "not a letter and not a digit", because the difference between a boundary and
// ordinary punctuation is the difference between "the build is stale, the memory
// <id> applies" and "the memory <id> (see below) applies", and the second is a
// citation of the memory while the first is a denial of something else that
// happens to cite one.
func splitClauses(text string) ([]string, []int) {
	var out []string
	var clauses []int
	var cur strings.Builder
	clause := 0
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			clauses = append(clauses, clause)
			cur.Reset()
		}
	}
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			cur.WriteRune(unicode.ToLower(r))
		case clauseBoundary(r):
			// Flushed first, so the boundary falls BETWEEN two words rather than
			// inside one: splitWords drops a hyphen in "rm-rfs" the same way it
			// drops a comma, and the two tokens must stay two tokens here.
			flush()
			clause++
		default:
			flush()
		}
	}
	flush()
	return out, clauses
}

// clauseBoundary reports whether r ends a clause.
//
// The set is a list and not a rule over unicode categories because the marks that
// matter here are the ones an agent TYPES as a boundary, and a category test
// cannot tell them from the ones it would sweep in with them: a period inside a
// decimal or a colon inside "https:" is punctuation without ending anything.
//
// Auditable against sentenceSplit, which is the other place a boundary is named.
// ';' is in BOTH lists, and on the in-package path AddProse it is unreachable:
// segments splits a sentence at ';' before splitClauses sees one, so the only text
// that carries a ';' into this function is a caller's own through the exported
// HasNegationCue. It is kept because the two lists answer different questions —
// sentenceSplit decides where one SEGMENT ends, this decides whether two words are
// in the same CLAUSE — and a reader who has to check the overlap to know which
// marks are reachable cannot audit either list.
//
// A paragraph break and a newline end a SEGMENT rather than a clause, and are
// absent here for that reason rather than by omission: segments already splits on
// them, so a boundary of that kind is a segment boundary and a cue cannot be in
// one segment while the words it is about are in another (see contradicts).
//
// The list is what the comparison is entitled to read as a boundary, and it is
// short on purpose. Each entry is a mark English puts between one assertion and
// another, and the binding rule's whole claim is that a cue on one side of one of
// them is not denying what the other side names — "The build is stale, the memory
// <id> applies" is a denial of the build that CITES a memory, and the citation is
// written in exactly the closed set the filler allowance spans.
//
// The dash is here for the same reason the comma is, and in particular the em and
// en forms an agent actually types rather than the ASCII hyphen alone.
//
// A dash being a boundary costs something, and it is named rather than discovered
// a year later: "ignore the memory-entry <id>" splits into two closed-set words
// at the hyphen and so is not bound any more. A hyphenated compound standing
// between a cue and an id is not a construction an agent writes, and the cost is
// a denial that goes unfiled, which is the direction cueGap already chose.
//
// NOT parentheses, quotes or an ellipsis. Those bracket a clause rather than
// ending one, and the marking they do is the cue's own: a denial in brackets is
// still a denial, and a citation in brackets is still a citation. Reading them as
// boundaries would blank "the advice (from the note <id>) is obsolete" on the
// strength of a shape that says nothing about what is being denied.
func clauseBoundary(r rune) bool {
	switch r {
	case ',', ':', ';', '—', '–', '-':
		return true
	}
	return false
}

// idLen is the length of the ids Ghost mints: hex(randomblob(16)), upper case.
//
// A LENGTH rather than a pattern, because the point is not to identify Ghost's
// ids — it is to notice that a transcript names a memory at all, and a host that
// mints ids differently still works. 32 hex characters is the run every observed
// adapter's rendering of a memory id produces, and anything shorter is far more
// likely to be a git SHA or a hash the agent computed than an id Ghost issued.
const idLen = 32

// memoryIDs is the id-shaped tokens in text, upper-cased.
//
// Upper-cased on both sides of the comparison: ids are hex, and a host or a model
// that lower-cases one has still named the same memory. A hex id has exactly one
// case-insensitive spelling, so this cannot merge two memories into one.
func memoryIDs(text string) []string {
	var out []string
	for _, word := range splitWords(text) {
		if id, ok := memoryIDWord(word); ok {
			out = append(out, id)
		}
	}
	return out
}

// memoryIDWord is one id-shaped word and its canonical spelling, so the shape rule
// is written once: memoryIDs asks it of a whole text, and the negation arm asks
// it of the single words a cue is bound to (#854), which is a different question
// about the same shape.
func memoryIDWord(word string) (string, bool) {
	// splitWords lower-cases, so a 32-character hex run arrives here as 32
	// lower-case characters; the length test is over the word as written.
	if len(word) == idLen && isHex(word) {
		return strings.ToUpper(word), true
	}
	return "", false
}

// isHex reports whether s is hex, which is what distinguishes an id-shaped token
// from any other 32-character word.
func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// negationCues are explicit denial constructions. Each one must deny a CLAIM —
// not merely contain a negative-sounding word.
//
// "not ", "n't " and "never " are NOT cues here: they fire on "I will not touch
// the cache lockfile directory today" (an instruction to the agent) and on
// "that is not unrelated to the sweep" (a mention, not a denial), producing a
// contradicted verdict about a memory the agent plainly used. "actually",
// "correction" and "instead of" are likewise rejected: they fire on "Per memory
// <id>, actually that applies here" (agreement) and on ordinary rewording.
//
// The only cues kept are constructions where a denial of some specific claim is
// grammatically required: the copula pair ("is wrong", "is incorrect",
// "is false", "is not true"), temporal invalidation ("no longer", "is
// outdated", "is obsolete", "is stale"), and explicit withdrawal ("ignore",
// "disregard"). A sentence containing one of these is denying something, and
// the contradicts arm then requires that the denial be ABOUT this memory.
//
// Each entry is a run of WORDS and is matched AS ONE (#854). Matched as plain
// substrings — which is what this list was, and what two of its entries could
// not survive being — a cue fires inside a word that merely contains it:
// "ignored", "ignores" and "ignored files" all contain "ignore", and "falsehood"
// contains "is false". An agent naming an id and then talking about its own
// ignored files agrees with the memory and was filed as having found it wrong.
//
// The matching is therefore over splitWords, the ONE tokenizer both sides of the
// comparison already use: a cue is a run of CONSECUTIVE words, so nothing shorter
// than the cue and nothing longer than the cue satisfies it. Both properties are
// pinned per entry by TestACueIsMatchedAsWholeWords, because the defect was
// EVERY entry and a test that asserted only "some cue still matches" would have
// passed with all fifteen still substring-matched.
//
// "Consecutive" counts closed-set words as absent (cueFillers, #860): "is now
// obsolete" is "is obsolete" with an adverb in it, and an agent writes the first
// as readily as the second. That tolerance is bounded at one word, it is a
// membership test rather than a distance (see cueRunFillers), and it is INTERNAL
// to a cue: a filler ahead of the cue is part of the distance to what the cue
// denies, and a boundary inside one is a boundary in the sentence — see cueRun.
var negationCues = []string{
	"is wrong", "is incorrect", "is false", "is not true", "is not correct",
	"is obsolete", "is outdated", "is stale", "is deprecated", "is superseded",
	"no longer applies", "no longer true", "no longer correct",
	"disregard", "ignore",
}

// negationCueWords is negationCues as the word runs they are matched as.
//
// Derived once rather than split per sentence: a comparison scans a whole
// transcript, and the split is a pure function of a list that cannot change at
// run time.
var negationCueWords = func() [][]string {
	out := make([][]string, 0, len(negationCues))
	for _, cue := range negationCues {
		out = append(out, splitWords(cue))
	}
	return out
}()

// cueGap is how many ARBITRARY words may stand between a cue and the id it
// denies, which is the id arm's whole binding rule.
//
// A gap rather than exact adjacency because both constructions a denial takes put
// a word in between: "ignore memory <id>" has a noun between the cue and the id,
// and "<id> is wrong" has the determiners between the id and the cue. One is where
// it has to stop, because that is the sentence this rule exists for — "Per <id>,
// I'll ignore the formatting" names the memory, agrees with it, and then uses the
// word "ignore" about the agent's own prose, two words away. English puts a
// subject between a comma and its clause's verb, so the distance holds for "I will
// ignore" and "I'll ignore" alike rather than turning on how a contraction happens
// to tokenise.
//
// One word is the whole of the rule on this side, and it is deliberately tight: a
// denial it misses is a contradiction that is not filed, which is the honest
// direction (see splitWords), while one it admits is ordinary use reported to an
// operator as a finding.
//
// Not the WHOLE rule on this side, though, and the second half is a different kind
// of check: the count is bounded twice over, by cueFillerGap over cueFillers and by
// a clause boundary that no count can express (see boundToCue). A reader who comes
// here for the binding rule and stops at the constant has the width and not the
// boundary, and the width alone reads "the memory <id> applies" as a denial of the
// memory because it is preceded by "the" and "memory".
//
// The count is of a SLICE of the words between them, so the constant cannot mean
// adjacency by accident again: it was once written as a pair of position bounds
// (c.start-cueGap-1 and c.end+cueGap+1), where the extra 1 on each side is the
// cue's own edge and dropping it reads as "one word may separate them" while
// admitting none — which silently lost "ignore memory <id>", the construction this
// constant exists for, while the comment still claimed it (#858). A slice has no
// such edge to get wrong.
// TestTheGapIsAWordGapAndNotAdjacency holds the sentence rather than the formula.
const cueGap = 1

// cueFillers is the CLOSED SET of words a cue may be separated from what it denies
// by, or separated from its own next word by, and it is the whole of #860.
//
// A set rather than a distance, because the distance is not the thing that is
// wrong. One word was narrow enough to be right and narrower than English: "Memory
// <id> is now obsolete", "disregard the memory <id>" and "ignore the advice in
// <id>" are all denials, and each puts a determiner, a noun or a preposition
// between the cue and the memory. Widening cueGap instead would trade those misses
// for the false contradiction #858 exists to prevent, because the sentence that
// rule is FOR — "Per <id>, I'll ignore the formatting" — also reaches an id across
// two ordinary words.
//
// What the set holds is the point, so it is worth saying why each entry earns its
// place and what the test at the edge of it is:
//
//   - the determiners an agent uses for a memory: the, this, that, a, an,
//   - the nouns Ghost's own vocabulary is written in, so "ignore the note <id>"
//     reads as the denial it is: memory, note, entry, advice,
//   - the one preposition that binds a cue to a thing it is about: in,
//   - and the two adverbs an agent reaches for mid-sentence: now, also.
//
// Everything an agent can put between a cue and a memory that is NOT here is the
// safe direction: the denial goes unfound, which is the miss this arm is allowed
// to make. What the set must not contain is a SUBJECT or a VERB, because those are
// what a sentence's own clause is made of: "per", "my", "formatter", "covers".
//
// The guard #858 exists for is safe on this side by that, not by any property of
// its own — "I'll" is a subject and it does NOT reopen it only because splitWords
// tokenises it to two words, so "Per <id>, I now ignore the formatting" is the
// same sentence with the contraction expanded and is pinned as a negative too.
//
// Membership and not proximity: every word between them must be in this set, so a
// gap of the allowance's width containing ONE word outside it is not an allowance
// at all ("ignore the linter advice in <id>"). That is the edge
// TestTheGapWidensOnlyAcrossAClosedSetOfWords pins from both sides, along with the
// width itself.
var cueFillers = map[string]bool{
	"a": true, "an": true, "the": true, "this": true, "that": true,
	"memory": true, "note": true, "entry": true, "advice": true,
	"in": true, "now": true, "also": true,
}

// cueFillerGap is how many closed-set words may stand between a cue and the id it
// denies. Three, not two, and the sentence that fixes it is "ignore the advice in
// <id>" — a determiner, the noun an agent actually reaches for when it means the
// memory, and a preposition. Two admits "disregard the memory <id>" and still
// misses that one, and a miss here is a contradiction that is not filed.
//
// The bound is not slack either: a run of four closed-set words is beyond it
// ("ignore the memory note in <id>"), so the set is a set and not an unlimited
// reach over whatever happens to be nearby.
const cueFillerGap = 3

// cueRunFillers is how many closed-set words may stand INSIDE a cue's own run of
// words — "is now obsolete" for the cue "is obsolete".
//
// One, which is all the real constructions need: an agent interrupts a denial with
// an adverb, not with a clause. It is a different constant from cueFillerGap
// because it is a different question — what separates two words of ONE cue, rather
// than a cue and the thing it denies — and a cue run is the whole of what makes a
// sentence a negation SEGMENT, so the wider bound would be widening the list of
// sentences this package considers denials at all.
const cueRunFillers = 1

// allCueFillers reports whether every one of these words is a closed-set word.
//
// All of them rather than most: a gap the allowance reaches has to BE the set, so
// one word outside it ends the reach. "ignore the formatter, <id>" is two words
// long and inside the width, and it must stay unbound — the cue is about the
// formatter, which is exactly what #858's binding rule exists to notice.
func allCueFillers(words []string) bool {
	for _, w := range words {
		if !cueFillers[w] {
			return false
		}
	}
	return true
}

// isDistinctive is whether a word is one this package would fingerprint.
//
// The length floor and the stopword list, which is the same pair distinctTokens
// applies — and it is named rather than inlined because the binding below has to
// skip exactly the words a fingerprint skips. A cue is found over RAW words (its
// own words are mostly too short or too common to be tokens: "is wrong" is one
// token), so what it binds to is found over the words that survive as tokens.
func isDistinctive(word string) bool {
	return len(word) >= minTokenLen && !stopWords[word]
}

// boundPositions is where a cue's object is, in the sentence's word positions: the
// cue's OWN words plus, on each side, the nearest word a fingerprint would keep.
//
// Skipping the words between is the point, and the skip crosses punctuation
// WITHOUT looking at it, which is what makes it sound on this arm rather than
// merely convenient: a clause boundary is not a word, so a cue and the restatement
// on the far side of a colon are adjacent here. "that is wrong — the opencode
// plugin materializes its transcript" and "that is wrong: the v20 migration runs"
// are the denial-then-restatement shape, and in both the cue's object is the next
// word with a token in it. What stops the sentence this rule exists for is that the
// words in between are words — "Per <id>, I'll ignore the formatting" reaches its
// object across a subject and a possessive, not across punctuation.
//
// The id arm does NOT use this, and the reason is the same sentence read two ways.
// An id is 32 characters and so always survives the token filter, which would let
// this skip straight over a subject clause and bind "ignore" to any id the sentence
// named. Ids are bound by boundToCue instead, which COUNTS the words in between
// and also refuses to cross a clause boundary at all — so this arm's skip and the
// id arm's count disagree about punctuation by design, and the disagreement is the
// rule: what a cue is about here is the memory's own WORDING, which a restatement
// on the far side of a colon IS, and what a cue is about there is a NAME, which
// the clause after a comma is free to introduce and walk away from.
//
// On this arm the same skip DOES put an id beside a cue, and that is harmless
// rather than by luck: an id is never one of a memory's tokens (addWords keeps ids
// out of the token set, and the token arm's floor of three is what depends on
// that), so an id in cueFps can never match anything the comparison asks about.
func boundPositions(words []string, cues []cueSpan) []int {
	var out []int
	seen := make(map[int]bool, len(cues)*3)
	mark := func(i int) {
		if i < 0 || i >= len(words) || seen[i] {
			return
		}
		seen[i] = true
		out = append(out, i)
	}
	for _, c := range cues {
		// Only what lies BESIDE the cue, never the cue's own words: they are
		// ordinary tokens ("ignore", "stale", "wrong", "superseded" all clear
		// minTokenLen and none is a stopword), so recording them lets a memory whose
		// wording happens to contain one of them satisfy the binding while the cue
		// is bound to something else entirely — the agent denied the changelog and
		// quoted the memory, and the memory was filed contradicted for agreeing
		// (TestTheCuesOwnWordsAreNotWhatACueIsBoundTo).
		for i := c.start - 1; i >= 0; i-- {
			if isDistinctive(words[i]) && !insideCue(i, cues) {
				mark(i)
				break
			}
		}
		for i := c.end + 1; i < len(words); i++ {
			if isDistinctive(words[i]) && !insideCue(i, cues) {
				mark(i)
				break
			}
		}
	}
	sort.Ints(out)
	return out
}

// insideCue reports whether the word at position i is part of one of these cues.
//
// Distinct from boundToCue, which asks how far a cue reaches: this asks only
// whether the position is a cue's own text, so a sideward skip can step over a
// second cue in the same sentence ("the changelog is wrong, ignore the docs") and
// land on the word that cue is actually about.
func insideCue(i int, cues []cueSpan) bool {
	for _, c := range cues {
		if i >= c.start && i <= c.end {
			return true
		}
	}
	return false
}

// cueSpan is where one cue sits in a sentence: the range of word positions it
// occupies, inclusive at both ends.
//
// Positions rather than text, because the sentence has already been split and the
// binding rule is about DISTANCE between two things in it.
type cueSpan struct {
	start int
	end   int
}

// cueSpans reports every denial cue in words.
//
// Every occurrence and every cue, so the binding below is the union of what they
// each deny rather than the first one's guess: a sentence can carry a cue per
// memory id, which is the case a document listing several retractions takes.
func cueSpans(words []string, clauses []int) []cueSpan {
	var out []cueSpan
	for _, cue := range negationCueWords {
		for i := 0; i+len(cue) <= len(words); i++ {
			if end, ok := cueRun(words, cue, clauses, i); ok {
				out = append(out, cueSpan{start: i, end: end})
			}
		}
	}
	return out
}

// cueRun matches one cue's words starting at position i and reports where the run
// ended, allowing up to cueRunFillers closed-set words between its words.
//
// The words themselves still have to match WHOLE and in order — this walks the
// cue one word at a time and never skips a word that is not in the closed set, so
// it cannot match "is wrong" inside "xis wrongy" any more than the exact form
// could, and it cannot match a cue with a filler beyond the bound ("is very
// obsolete"). The tolerance is on the words BETWEEN a cue's own words, which is
// the whole of #860's claim: a closed-set word cannot hide a cue, it only fails to
// hide one.
//
// NEVER before the first word, which is what keeps the tolerance from becoming a
// second way to widen cueGap: a filler ahead of the cue is part of the distance
// the id arm measures, so consuming it would move the cue's leading edge onto the
// filler and reach one word further than the constants allow
// (TestANAdjacentFillerIsStillApartOfTheDistance).
//
// Reported as a span rather than a match so the binding downstream counts the same
// positions this function matched, and a filler INSIDE a run counts as inside the
// cue. The same clause test applies inside a run as outside one: a cue is a run of
// consecutive words, and two consecutive words either side of a comma are two
// assertions, so a cue matching them would be a span whose own extent the id arm
// has no single clause to compare against.
//
// A filler INSIDE a run counts as inside the cue, and that is what it is — the
// cue's own extent — but it has a consequence worth naming, because it is the one
// place this tolerance can lose a genuine denial: if
// the memory's own distinctive word is the filler ("that entry is obsolete" for a
// memory about "the entry that records weekly releases"), insideCue tells the
// sideward skip to step over it and the binding never reaches a word the memory
// holds. The alternative — treating an interior filler as outside the cue — lets
// the skip stop ON it instead, which is a false contradiction on any memory whose
// wording shares a cue-adjacent closed-set word. Both words are in Ghost's own
// vocabulary, so neither error is avoidable by choosing a set that is narrower;
// the one taken here is the one that reports a denial as `used`, which is the
// honest direction (see cueGap).
func cueRun(words, cue []string, clauses []int, i int) (int, bool) {
	pos := i
	for k := 0; k < len(cue); k++ {
		if k > 0 {
			// Interior only, and never before the FIRST word. A filler ahead of the
			// cue is a word BETWEEN the cue and whatever the cue is about, and
			// consuming it here would move the cue's leading edge onto the filler:
			// "Per <id>, I now ignore the formatting" would gain a span starting at
			// "now", the id would then be one word from THAT instead of from "ignore",
			// and an agent agreeing with a memory would be filed as having found it
			// wrong -- the false contradiction this whole rule exists to stop, reached
			// through the tolerance meant to prevent it. So the leading position is an
			// exact match or nothing.
			for skipped := 0; pos < len(words) && words[pos] != cue[k]; skipped++ {
				// The filler must be in the SAME clause as the cue's first word,
				// which is what keeps a cue a run of consecutive words rather than
				// a run with a boundary inside it: "is, the obsolete" is two
				// assertions, and a cue matching it would be a span the id arm has
				// no single clause to compare.
				if skipped >= cueRunFillers || !cueFillers[words[pos]] || clauses[pos] != clauses[i] {
					break
				}
				pos++
			}
		}
		// Every position the run touches has to be in the cue's FIRST word's
		// clause, including the matched one. The matched word is the case that
		// matters: without it a cue run straddles a boundary — "is the, obsolete"
		// matches, because the filler before the comma is still in the first clause
		// — and a span whose own words sit in two clauses leaves boundToCue nothing
		// to compare an id against, since it can only ask about the clause of the
		// cue's first word.
		if pos >= len(words) || words[pos] != cue[k] || clauses[pos] != clauses[i] {
			return 0, false
		}
		pos++
	}
	return pos - 1, true
}

// boundToCue reports whether the word at position i is what a cue in this
// sentence denies, by counting the words between them.
//
// This is the ID arm's binding rule and only that arm's, because an id is a
// 32-character token that survives any filter — a rule that skipped to the
// nearest word which could be a fingerprint would step straight over the subject
// clause in "Per <id>, I'll ignore the formatting" and bind the cue to the id
// anyway, which is the false contradiction #854 is about. The cue is at the other
// end of it: this is not "is there a cue somewhere in this sentence" — the
// segment already established that, and answering it a second time without the
// distance is what filed an agent's agreement as a contradiction.
//
// The count is of a SLICE of the words strictly between, which is what makes the
// two constants readable: up to cueGap of them however they are spelled, or up to
// cueFillerGap of them if every one is in the closed set (cueFillers, #860). A
// longer gap of arbitrary words is the false contradiction, and a gap of the
// allowance's width with a word outside the set in it is the same sentence with an
// extra noun in it.
//
// **AND A CLAUSE BOUNDARY BETWEEN THEM ENDS THE BINDING**, checked before the
// count because it is not a distance at all — the words on either side of it can
// be zero and the binding still dies. It is a separate rule rather than a wider
// condition on the count because the two failures are different: the count is what
// keeps a cue off a subject clause, and this is what keeps it off the NEXT
// assertion. "The build is stale, the memory <id> applies" denies the build and
// cites a memory, and every word between the cue and the id is in cueFillers —
// that is what makes it a citation — so the count alone binds it. And the fix
// cannot be "one fewer filler", because "The CI is deprecated, also <id> covers
// lockfiles" and "Disregard this, <id> is accurate" each reach the id across a
// SINGLE arbitrary word: the comma is all that stops them, and it was already
// doing so on a gap of one.
//
// Symmetric on purpose: the clause of a cue's first word and of the id must be
// the same whichever of them the sentence writes first, so "Memory <id> is wrong"
// binds and "Memory <id>, that is wrong" does not. Which is not a distinction an
// agent intends — both read the same aloud — and it is the correct direction to
// be wrong in: the second is filed as `used` rather than reported to an operator
// as "the agent found this memory wrong".
//
// Only the ID arm. The fingerprints are bound by boundPositions, which SKIPS
// punctuation deliberately, because "that is wrong: <the memory's wording>" is a
// denial followed by its restatement and the words the cue is about are on the far
// side of the colon (see boundPositions). That shape and this one are the same
// punctuation read two ways, and they differ in what the cue is attached to: a
// restatement IS the memory's own wording, so the skip landing on it is the
// binding working, while an id is a name for the memory, which is what the words
// around it are free to introduce and walk away from.
func boundToCue(i int, words []string, clauses []int, cues []cueSpan) bool {
	for _, c := range cues {
		if clauses[i] != clauses[c.start] {
			continue
		}
		// A position inside a cue is not a position between one, and the empty
		// slice is the honest answer: an id cannot be one of a cue's own English
		// words, so this branch exists only so that a position the cue already
		// covers is not treated as being far away from it.
		var between []string
		switch {
		case i < c.start:
			between = words[i+1 : c.start]
		case i > c.end:
			between = words[c.end+1 : i]
		}
		if len(between) <= cueGap {
			return true
		}
		if len(between) <= cueFillerGap && allCueFillers(between) {
			return true
		}
	}
	return false
}

// HasNegationCue reports whether a single sentence denies something.
//
// A denial of SOMETHING, not of a particular memory: what it denies is decided per
// memory, against that memory's id and its own wording, by the arms in compare.go.
func HasNegationCue(segment string) bool {
	words, clauses := splitClauses(segment)
	return len(cueSpans(words, clauses)) > 0
}

// sentenceSplit is where one sentence ends. Sentence-final punctuation and the
// newline a tool result is written on; NOT a space, so "e.g." and a version
// number do not manufacture a boundary.
var sentenceSplit = func(r rune) bool {
	switch r {
	case '.', '!', '?', '\n', ';':
		return true
	}
	return false
}

// segments splits the agent's prose into sentences.
//
// Sentence-granular rather than message-granular, and the reason is the whole
// contradicted bucket: a cue is attributed to a memory only when BOTH the cue and
// the memory's wording are in the same segment. A single document that negates
// one memory and quotes three others would otherwise contradict all four.
func segments(text string) []string {
	var out []string
	start := 0
	for i, r := range text {
		if !sentenceSplit(r) {
			continue
		}
		if seg := text[start:i]; seg != "" {
			out = append(out, seg)
		}
		start = i + len(string(r))
	}
	if seg := text[start:]; seg != "" {
		out = append(out, seg)
	}
	return out
}
