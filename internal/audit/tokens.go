package audit

import (
	"hash/fnv"
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

// Fingerprint is the sidecar's token key: a 64-bit FNV-1a hash of one
// lower-cased word, as sixteen hex characters.
//
// A HASH rather than the word itself, and that is a constraint rather than an
// optimisation. The sidecar is a file on disk for the length of a session, and it
// has to hold something; what it must not hold is what the agent said. A hash of
// a word cannot be read back as that word, so the file cannot become a transcript
// somebody later finds — which is the same reasoning that puts the query's digest
// in retrieval_record rather than the query.
//
// FNV-1a is a non-cryptographic hash chosen for being three lines and having no
// table to keep. The security property here is one-directionalness, not
// collision resistance: a collision merges two words into one token, which can
// only ever ADD a match (a false "used"), never hide one, and 64 bits makes that
// remote for the vocabulary of a session.
func Fingerprint(word string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.ToLower(word)))
	return hex64(h.Sum64())
}

// hex64 renders a hash the way the sidecar and both readers agree on: sixteen
// lower-case hex characters, zero-padded. Fixed width matters because the file is
// parsed positionally.
func hex64(v uint64) string {
	const digits = "0123456789abcdef"
	var buf [16]byte
	for i := 15; i >= 0; i-- {
		buf[i] = digits[v&0xf]
		v >>= 4
	}
	return string(buf[:])
}

// unhex64 is Fingerprint's inverse, and it REFUSES anything that is not sixteen
// hex characters. A truncated or padded field would silently become a different
// token — a token nothing in the session ever matched — so a malformed line is
// an error the reader reports rather than a value it invents.
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

// DistinctTokens is the fingerprints of text's words, in first-seen order.
//
// Order is the memory's own, not a sorted or frequency-ordered list, because a
// caller's rule over it ("half of the first eight") has to be a statement about
// the memory's opening, and a set with no order could not make one. Deduplicated
// so a memory that repeats a word is not weighted by the repetition.
//
// It is a function of the TEXT and nothing else — no store, no clock, no
// configuration — which is what lets the transcript side fingerprint a word and
// this side fingerprint a memory with the same code and get the same answer.
func DistinctTokens(text string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, word := range splitWords(text) {
		if len(word) < minTokenLen || stopWords[word] {
			continue
		}
		fp := Fingerprint(word)
		if seen[fp] {
			continue
		}
		seen[fp] = true
		out = append(out, fp)
	}
	return out
}

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
		// splitWords lower-cases, so a 32-character hex run arrives here as 32
		// lower-case characters; the length test is over the word as written.
		if len(word) == idLen && isHex(word) {
			out = append(out, strings.ToUpper(word))
		}
	}
	return out
}

// isHex reports whether s is hex, which is what distinguishes an id-shaped token
// from any other 32-character word.
func isHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// negationCues are the phrases that mark a sentence as denying something.
//
// A list, deliberately, and the reason is the failure mode it avoids: a
// general-purpose model would be a better judge of contradiction than a cue list,
// and it would also cost a subscription-billed call per verdict, store text a
// model saw on a machine the operator did not choose, and be unavailable on every
// host with no harness configured. #646 asks for a heuristic, and the honest
// consequence is that "contradicted" means "the agent's own words, in the same
// sentence as this memory's wording, carried one of these" — not that Ghost knows
// the agent was wrong.
//
// The list holds PHRASES rather than single words on purpose. A bare "not" would
// fire on "this is not unrelated to the sweep", which is a mention. Each cue is a
// construction whose presence in one sentence makes that sentence a denial.
var negationCues = []string{
	"no longer", "not ", "n't ", "never ", "instead of", "rather than",
	"correction", "actually", "wrong", "obsolete", "outdated", "deprecated",
	"superseded", "stale", "incorrect", "false",
}

// HasNegationCue reports whether a single sentence denies something.
func HasNegationCue(segment string) bool {
	// Padded so a cue that ends in a space ("not ") can be found at either end of
	// the segment without the padding changing what the cue is.
	padded := " " + strings.ToLower(segment) + " "
	for _, cue := range negationCues {
		if strings.Contains(padded, cue) {
			return true
		}
	}
	return false
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
