package audit

// The KEYED token hasher.
//
// #646 part 2's sidecar is a file on disk in the OS temp directory, for the
// length of a session, holding one hash per distinctive word the agent used. The
// first version of this hashed with plain FNV-1a and its comment claimed a hash
// "cannot be read back as that word". That claim was false, and cheaply so: an
// unkeyed 64-bit hash of a lower-cased word is a pure function of that word, so
// anybody holding a word list — which for English is a few kilobytes, and for
// this project's own vocabulary is the memories in the very database the
// comparison reads — computes every candidate in seconds and matches it against
// the file. The sidecar therefore leaked the agent's distinctive vocabulary to
// whoever found it, and a file in the shared temp directory is a file another
// process on the machine can read.
//
// So the hash is an HMAC under the SAME per-install key that retrieval_record's
// query digest uses, and there is no unkeyed fallback anywhere. A caller that
// cannot get the key writes no sidecar at all: an absent sidecar costs one turn's
// audit, while an unkeyed one costs the property the sidecar was for. That
// asymmetry is the design, and it is why NewHasher refuses an empty key rather
// than accepting one.
//
// The key lives in a 0600 file beside ghost.db rather than in the database, so it
// is not in a `ghost backup` (a VACUUM INTO of the db), not in a portable export,
// and not in a store handed to another machine. It is read through
// config.DataDirPath, so a run that must not touch the real data directory does
// not — it reads the sandbox's. A sidecar written under a sandbox key is not
// comparable against a real store, which is correct: it was never going to be.
//
// 64 bits of HMAC-SHA256 is a truncation, and it is deliberate. Collision
// resistance is not the property at stake — a collision merges two words into one
// token, which can only ADD a match (a false "used"), never hide one. What a
// dictionary attack needs is the ability to compute a candidate, and under a key
// the machine's other processes do not hold, that is gone.

import (
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strings"
)

// errNoKey is a Hasher built without a key, refused rather than defaulted.
var errNoKey = errors.New("audit: a token hasher needs the per-install key")

// minKeyBytes is the shortest key NewHasher accepts.
//
// Not the 32 the key file holds. A test needs a key it can write down, and a
// caller that supplied a short one has still supplied a secret — the property
// being bought is that the value is not public, not that it is long. The one
// thing that would defeat it entirely is the empty key, and that is refused.
const minKeyBytes = 8

// Hasher turns a word into the sidecar's token, under a key.
//
// A value rather than a pointer so it can live in a Signals by value, and a
// distinct type rather than a bare []byte so a hasher and a key cannot be
// swapped for one another by accident.
type Hasher struct {
	key []byte
}

// NewHasher returns a Hasher over key, and refuses a key too short to be one.
//
// The refusal matters: the alternative reading — an empty key, an unkeyed hash —
// is precisely the leak this type exists to close, so it is an error rather than
// a silent downgrade. Callers degrade on it by writing nothing.
func NewHasher(key []byte) (Hasher, error) {
	if len(key) < minKeyBytes {
		return Hasher{}, errNoKey
	}
	// Copied rather than aliased, because the caller may reuse or zero its buffer
	// and a hasher holding a reference to it would stop being stable mid-session —
	// which reads as a comparison that matched nothing rather than as an error.
	key = append([]byte(nil), key...)
	return Hasher{key: key}, nil
}

// Fingerprint is the sidecar's token for one word: the first eight bytes of
// HMAC-SHA256(key, lower(word)), as sixteen lower-case hex characters.
//
// Sixteen hex characters because the file is parsed positionally and a field's
// width is what tells a fingerprint from an id; lower-case because the id
// vocabulary is upper-case and the two must not overlap (see isIDField).
func (h Hasher) Fingerprint(word string) string {
	mac := hmac.New(sha256.New, h.key)
	mac.Write([]byte(strings.ToLower(word)))
	return hex64(mac.Sum(nil)[:8])
}

// DistinctTokens is Fingerprint over a text's distinctive words, in first-seen
// order.
//
// Order is the memory's own, not a sorted or frequency-ordered list, because a
// caller's rule over it ("half of the first eight") has to be a statement about
// the memory's opening, and a set with no order could not make one.
// Deduplicated so a memory that repeats a word is not weighted by the
// repetition.
//
// It is a function of the TEXT and the KEY and nothing else — no store, no
// clock, no configuration — which is what lets the transcript side fingerprint a
// word and this side fingerprint a memory with the same code and get the same
// answer, and what makes a sidecar written by one process readable by another.
func (h Hasher) DistinctTokens(text string) []string {
	return h.distinctTokens(splitWords(text))
}

// distinctTokens is DistinctTokens over words the caller has already split, so
// the segment path does not split a sentence twice.
//
// It returns NOTHING for a hasher with no key, and that is the degradation the
// whole type is built around. HMAC will happily compute a digest under an empty
// key, so the zero Hasher would otherwise produce a file of tokens that are
// uniform and perfectly reversible — an unkeyed hash wearing a keyed one's
// clothes, and silent about it. Refusing here instead means a caller that could
// not read the install key has produced no evidence, which reads downstream as
// "this scan found nothing" and costs one turn's audit. The alternative costs the
// property the sidecar exists for, on a turn nobody was watching.
func (h Hasher) distinctTokens(words []string) []string {
	if !h.HasKey() {
		return nil
	}
	seen := make(map[string]bool, len(words))
	out := make([]string, 0, len(words))
	for _, word := range words {
		if len(word) < minTokenLen || stopWords[word] {
			continue
		}
		fp := h.Fingerprint(word)
		if !seen[fp] {
			seen[fp] = true
			out = append(out, fp)
		}
	}
	return out
}

// HasKey reports whether this hasher was built over a key.
//
// The zero Hasher answers false, and every use of it produces NOTHING rather
// than an unkeyed token — which is why a zero Signals is Empty and a sidecar
// written from one is refused. That combination is the whole degradation story
// for a caller that could not read the install key: it produces no evidence,
// rather than evidence anyone can reverse.
func (h Hasher) HasKey() bool { return len(h.key) > 0 }
