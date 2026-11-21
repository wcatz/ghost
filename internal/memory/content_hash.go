package memory

import (
	"crypto/sha256"
	"encoding/hex"
)

// ContentHashVersion is the prefix mixed into every content hash. Bumping it
// changes every digest, withdrawing stored verdicts and re-asking cached KEEPs —
// survivable because both err toward silence and work, but a deliberate act with
// that cost. Its value matches the old resolve keep-cache prefix so moving the
// function here did not change a stored digest.
const ContentHashVersion = "v3"

// ContentHash is sha256(ContentHashVersion || "\x00" || content), hex-encoded —
// the digest of the TEXT alone, so a tag, importance, scope or `verified` edit
// leaves it standing while a rewrite moves it.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(ContentHashVersion + "\x00" + content))
	return hex.EncodeToString(sum[:])
}
