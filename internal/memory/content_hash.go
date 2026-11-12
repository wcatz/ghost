package memory

import (
	"crypto/sha256"
	"encoding/hex"
)

// ContentHashVersion is the version prefix mixed into every content hash, and
// bumping it changes every digest: every stored verdict would then mismatch the
// text it judged and be WITHDRAWN, and every cached KEEP would be re-asked. Both
// err toward silence and toward work rather than toward a wrong answer, which is
// why the bump is survivable — but it is a deliberate act with that cost, not a
// refresh. It is the value internal/resolve's keep-cache historically used
// ("v3"), so moving the function here did not move a single stored digest:
// resolve's cache keys and the verdict stamps this build writes are
// byte-identical to what they were.
const ContentHashVersion = "v3"

// ContentHash is sha256(ContentHashVersion || "\x00" || content), hex-encoded —
// the digest of the TEXT alone, so a tag, importance, scope or `verified` edit
// leaves it standing while a rewrite moves it.
func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(ContentHashVersion + "\x00" + content))
	return hex.EncodeToString(sum[:])
}
