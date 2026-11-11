package reflection

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// MaxConsolidationInput is the maximum number of consolidatable memories one reflect run rewrites.
const MaxConsolidationInput = 2000

// InputSignature fingerprints consolidation input for --skip-unchanged.
func InputSignature(mems []memory.Memory) string {
	lines := make([]string, 0, len(mems))
	for _, m := range mems {
		lines = append(lines, fmt.Sprintf("%s|%s|%s|%.1f|%s|%s|%s",
			m.ID, m.UpdatedAt, m.Category, m.Importance, m.Source,
			strings.Join(m.Tags, ","), m.Content))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}
