package audit

import (
	"hash/fnv"
	"os"
	"strings"
	"testing"
)

// TestTheTokensAreKeyedRatherThanHashed is the test the old comment could not
// have survived.
//
// The first version of this hashed each word with plain FNV-1a and wrote in the
// source that "a hash of a word cannot be read back as that word". That is
// false. An unkeyed hash is a pure function of its input, so a reader holding a
// word list computes the hash of every candidate and reads the file as plain
// text — and for this project the word list is already sitting in the database
// the comparison itself reads. The sidecar lives in the OS temp directory, which
// every process on the machine can read, so the file was the transcript in
// disguise and the disguise was free to undo.
//
// The guarantee the design actually offers is narrower and this states it: with
// the key, the same word always gives the same token (so the two processes agree);
// without it, no word list reproduces the file.
func TestTheTokensAreKeyedRatherThanHashed(t *testing.T) {
	// Ordinary English, chosen because it is exactly what a word list contains.
	words := []string{
		"transcript", "directory", "memory", "session", "plugin",
		"messages", "watching", "overwrite", "concurrent", "installed",
	}

	s := newTestSignals(t)
	s.AddProse(strings.Join(words, " and the ") + " and running")

	path, err := WriteSidecar(t.TempDir(), s)
	if err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the sidecar back: %v", err)
	}
	body := string(raw)

	for _, w := range words {
		if strings.Contains(body, unkeyedFNV(w)) {
			t.Errorf("the sidecar carries the plain FNV-1a of %q, so a word list "+
				"reproduces it exactly", w)
		}
	}

	// The other half, which is the half that makes the first half matter: a
	// reader WITHOUT the key cannot produce a token the file holds, so a sidecar
	// and a memory fingerprinted under different keys share nothing. That is what
	// stops this being a confidentiality claim with no teeth — it is also the
	// reason a mismatched key degrades to a skipped audit rather than a wrong
	// one.
	other := mustHasher([]byte("a completely different install key"))
	underOther := other.distinctTokens(splitWords(strings.Join(words, " ")))
	for _, fp := range underOther {
		if strings.Contains(body, fp) {
			t.Errorf("token %s is the same under a different key, so the key is not in the token", fp)
		}
	}

	// And under the RIGHT key it is stable, because the whole comparison depends
	// on the hook and the detached child agreeing.
	again := mustHasher(testKey)
	if again.Fingerprint("transcript") != testHasher.Fingerprint("transcript") {
		t.Error("the same key gave two different tokens for the same word; the two sides could not agree")
	}
}

// TestAnUnkeyedSignalsProducesNothingRatherThanUnkeyedTokens: the degradation
// story has to be safe, and "safe" here is a specific shape rather than a
// sentiment.
//
// A caller that could not read the install key (a store that has never recorded
// a retrieval has no key file) used to be offered two options: hash anyway, or
// record nothing. It hashes into NOTHING: a zero Signals records no tokens and
// is Empty, so the hook writes no sidecar and says nothing. The alternative — an
// unkeyed file written by a caller that merely forgot a key — is silent, and
// silent is the failure mode here.
//
// Ids are still recorded, because an id is not a word: it is Ghost's own
// identifier, in the file in the clear, and the identifier arm needs it there. The
// leak this whole type closes is about the agent's VOCABULARY, and an id reveals
// nothing about what the agent said.
func TestAnUnkeyedSignalsProducesNothingRatherThanUnkeyedTokens(t *testing.T) {
	var zero Signals
	zero.AddProse("the transcript under mkdtemp holds D20E133860CC4AFE38B485AD5371BA59")
	zero.AddSaveArgs("a save about the lockfile directory")
	zero.AddToolArgs("a tool call about the cache directory")

	if len(zero.prose) != 0 || len(zero.saves) != 0 || len(zero.negated) != 0 {
		t.Errorf("an unkeyed Signals recorded %d prose, %d save and %d negation token(s); "+
			"it must record none", len(zero.prose), len(zero.saves), len(zero.negated))
	}
	if !zero.Empty() {
		t.Error("an unkeyed Signals is not Empty, so the hook would try to hand a child " +
			"evidence it cannot compare")
	}
	if _, err := WriteSidecar(t.TempDir(), &zero); err == nil {
		t.Error("WriteSidecar accepted an unkeyed Signals; the one way to put an unkeyed " +
			"sidecar on disk is to not be able to build one")
	}
	if _, err := New(nil); err == nil {
		t.Error("New(nil) returned a Signals; an absent key must be refused, not defaulted")
	}
	if _, err := New([]byte("short")); err == nil {
		t.Error("New accepted a 5-byte key")
	}
}

// TestTheHookAndTheChildMustSignWithTheSameKey: the two ends of the sidecar are
// separate processes, and a mismatch is not a degraded audit — it is a confident
// one.
//
// The file's tokens are literals, so reading it back under the wrong key changes
// nothing about the file. What changes is the OTHER side: CompareAgainst derives
// a memory's tokens with the hasher it holds, and a child holding a different key
// from the hook would therefore compare a memory fingerprinted under one key
// against a transcript fingerprinted under another, find no common token, and file
// every kept memory as "ignored". That is a report of total silence produced by a
// store that was perfectly fine, and it is why both ends resolve the key from the
// same file and why ReadSidecar takes the hasher as a parameter instead of
// building one.
func TestTheHookAndTheChildMustSignWithTheSameKey(t *testing.T) {
	dir := t.TempDir()

	// The hook: signed with the install key.
	writer := newTestSignals(t)
	writer.AddProse("the opencode plugin materializes its transcript under mkdtemp, " +
		"which is what " + testMemoryID + " says")
	path, err := WriteSidecar(dir, writer)
	if err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}

	// The child, under the SAME key: the memory the agent restated is judged used.
	same, err := ReadSidecar(path, mustHasher(testKey))
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	if !same.HasID(testMemoryID) {
		t.Error("the id did not survive the round trip")
	}
	v, ok := CompareAgainst(same, Judged{MemoryID: testMemoryID, Content: memContent})
	if !ok {
		t.Fatal("CompareAgainst refused to judge")
	}
	if v.Outcome != OutcomeUsed {
		t.Errorf("outcome under the SAME key = %s, want used", v.Outcome)
	}

	// The child, under a DIFFERENT key: the file still parses and the id still
	// crosses, but the memory's own tokens cannot meet the transcript's, so the
	// token arm goes silent. This is the shape of the failure, and it is why a
	// mismatched key has to be a skipped audit rather than a comparison.
	other := mustHasher([]byte("a completely different install key"))
	diff, err := ReadSidecar(path, other)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	if len(diff.prose) != len(writer.prose) {
		t.Errorf("the file's token set changed size when read under another key: %d vs %d; "+
			"the tokens are literals and reading must not rewrite them",
			len(diff.prose), len(writer.prose))
	}
	if diff.matches(other.DistinctTokens(memContent)) {
		t.Error("a memory matched signals from a different key; the key is not reaching " +
			"the comparison")
	}
}

// unkeyedFNV is the OLD token function, reproduced here so the test can assert
// its absence rather than the presence of the new one. Asserting only that the
// new token is present would pass just as happily if both were written, and the
// defect was a file carrying the unkeyed one.
//
// It renders the eight digest bytes hex64's way rather than the whole sum: hex64
// keeps the field's width honest, and a width-16 field is what isIDField reads.
func unkeyedFNV(word string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(strings.ToLower(word)))
	return hex64(h.Sum(nil)[:8])
}
