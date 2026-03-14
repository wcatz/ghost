package tool

import (
	"crypto/sha256"
	"fmt"
)

// projectIDFromPath derives a stable project ID from the absolute path.
func projectIDFromPath(path string) string {
	h := sha256.Sum256([]byte(path))
	return fmt.Sprintf("%x", h[:6]) // 12 hex chars
}
