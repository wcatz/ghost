package memory

import (
	"crypto/sha256"
	"encoding/hex"
)

// ContentHashVersion is the version prefix mixed into every content hash, and
// bumping it changes every digest: every stored verdict would then mismatch the
const ContentHashVersion = "v3"

// ContentHash is sha256(ContentHashVersion || "\x00" || content), hex-encoded —
// the digest of the TEXT alone, so a tag, importance, scope or `verified` edit
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(ContentHashVersion + "\x00" + content))
	return hex.EncodeToString(sum[:])
}
