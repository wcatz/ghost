package audit

import (
	"encoding/hex"
	"slices"
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
func splitWords(text string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
	return out
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

// cueGap is how many WORDS may stand between a cue and the id it denies, which is
// the id arm's whole binding rule.
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
const cueGap = 1

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
// Skipping the words between is the point, and the skip is sound rather than
// generous: splitWords drops punctuation, so a clause boundary is not a word and
// cannot separate a cue from what follows it. "that is wrong — the opencode plugin
// materializes its transcript" and "that is wrong: the v20 migration runs" are the
// denial-then-restatement shape, and in both the cue's object is the next word
// with a token in it. What stops the sentence this rule exists for is that the
// words in between are words — "Per <id>, I'll ignore the formatting" reaches its
// object across a subject and a possessive, not across punctuation.
//
// The id arm does NOT use this: an id is 32 characters and so always survives the
// token filter, which would let this skip straight over a subject clause and bind
// "ignore" to any id the sentence named. Ids are bound by cueGap instead, where
// the words in between are counted rather than skipped.
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
		for i := c.start; i <= c.end; i++ {
			mark(i)
		}
		for i := c.start - 1; i >= 0; i-- {
			if isDistinctive(words[i]) {
				mark(i)
				break
			}
		}
		for i := c.end + 1; i < len(words); i++ {
			if isDistinctive(words[i]) {
				mark(i)
				break
			}
		}
	}
	sort.Ints(out)
	return out
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
func cueSpans(words []string) []cueSpan {
	var out []cueSpan
	for _, cue := range negationCueWords {
		for i := 0; i+len(cue) <= len(words); i++ {
			if slices.Equal(words[i:i+len(cue)], cue) {
				out = append(out, cueSpan{start: i, end: i + len(cue) - 1})
			}
		}
	}
	return out
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
func boundToCue(i int, cues []cueSpan) bool {
	for _, c := range cues {
		if i >= c.start-cueGap && i <= c.end+cueGap {
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
	return len(cueSpans(splitWords(segment))) > 0
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
