package audit

import (
	"os"
	"testing"
	"time"
)

// testKey is the per-install key every test in this package signs with.
//
// A literal rather than a real key, deliberately, and the point of the constant
// is that it is NOT a secret: these tests are about the comparison's rules, and a
// test that had to provision an install key could only assert the same thing more
// slowly. What they must still pin is that the tokens are a function of THIS key
// — which TestTheTokensAreKeyedRatherThanHashed does, by computing a fingerprint
// under a second key and refusing to match it.
var testKey = []byte("ghost-audit-test-key-not-a-secret")

// testHasher is testKey's hasher, built once. A test that needs to vary the key
// builds its own.
var testHasher = mustHasher(testKey)

func mustHasher(key []byte) Hasher {
	h, err := NewHasher(key)
	if err != nil {
		// Unreachable for the literals in this package's tests, and panicking
		// rather than returning is right: a test whose key is unusable has
		// nothing to say about fingerprints.
		panic("audit: test key rejected: " + err.Error())
	}
	return h
}

// newTestSignals is the Signals every test here starts from, keyed.
func newTestSignals(t *testing.T) *Signals {
	t.Helper()
	return NewWithHasher(testHasher)
}

// testTokens is DistinctTokens under the test key, for a test that wants a
// memory's own tokens.
func testTokens(text string) []string { return testHasher.DistinctTokens(text) }

// The three helpers below are file plumbing shared by the sidecar tests: two
// write/read a path and one makes a file look old enough to sweep. They are here
// rather than inline because each is used by more than one test, and a test that
// copied a `os.Chtimes` call into its own body would be asserting against a file
// it had aged with a different spelling of the same three lines.

func writeFileString(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

func readFileString(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// touchOlderThan moves a file's modification time into the past, which is how the
// sweep test gets a file that LOOKS stale without waiting a day for one to
// become so.
func touchOlderThan(path string, age time.Duration) error {
	when := time.Now().Add(-age)
	return os.Chtimes(path, when, when)
}
