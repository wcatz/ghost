package reflection

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// InputSignature fingerprints the consolidation input so an unchanged corpus
// can skip a full-corpus LLM call (ghost reflect --skip-unchanged). It covers
// every prompt-rendered field except the deliberate exclusions below, so any
// prompt-visible mutation invalidates the gate.
//
// Deliberately excluded:
//   - learned_context: the consolidator's own output, so including it would
//     make every successful run invalidate its own gate;
//   - git commits: best-effort grounding that moves on every commit without
//     changing what the consolidator would produce;
//   - access counts: incremented by ordinary reads/injections, so including
//     them would make the signature change on sessions that saved nothing.
func InputSignature(mems []memory.Memory) string {
	lines := make([]string, 0, len(mems))
	for _, m := range mems {
		lines = append(lines, fmt.Sprintf("%s|%s|%s|%.2f|%s|%s|%s",
			m.ID, m.UpdatedAt, m.Category, m.Importance, m.Source,
			strings.Join(m.Tags, ","), m.Content))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}
