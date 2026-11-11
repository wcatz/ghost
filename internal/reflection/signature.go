package reflection

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// MaxConsolidationInput is the most consolidatable memories one `ghost reflect`
// rewrites. Every tier rewrites its whole input at once: a harness tier is sent
const MaxConsolidationInput = 2000

// InputSignature fingerprints the consolidation input so an unchanged corpus
// can skip a full-corpus LLM call (ghost reflect --skip-unchanged). Each line
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
