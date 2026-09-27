package reflection

import (
	"strings"
)

// DroppedGuarded is an input memory, in any category, that has no close
// survivor in the consolidation output. Importance and Tags are carried so the
// memory can be re-inserted verbatim by RetainGuardedDrops without losing its
// weight or labels.
type DroppedGuarded struct {
	Category   string
	Content    string
	Importance float32
	Tags       []string
}

// dropContainmentThreshold: an input memory counts as survived when this
// fraction of its tokens appears anywhere in the consolidation output.
// Containment (input→output), not Jaccard: the question is whether the input's
// substance survives anywhere, not whether the rewrite is fully explained by one
// source.
//
// Measured against the UNION of the output memories rather than any single one
// (#639). A merge that splits one input's substance across two survivors is the
// normal shape of consolidation, and the old per-memory test scored it below the
// bar and re-added the input verbatim beside the merge that had just replaced
// it — the paraphrase-duplicates that made goduckbot and mini-gun the two
// worst-graded projects in the maintenance benchmark.
//
// Deliberately lenient — a false positive costs a healthy merge whose input is
// then also re-added verbatim (a duplicate row), while an outright deletion
// lands far below the threshold (an unrelated survivor shares only a stray token
// or two).
const dropContainmentThreshold = 0.45

// AuditGuardedDrops returns every input memory that has no token-overlap
// survivor among the result memories, whatever its category. Every category is
// under the drop guard (#549): it began as an allowlist of operational
// knowledge — gotchas, dependency pins, workflow conventions and user
// preferences (issue #337, eval-cycle finding F3) — but an architecture or
// decision memory dropped by an unattended consolidation is just as
// unrecoverable, and the `manual` source that reflection preserves covers none
// of it: seeds are written 'builtin' and agent saves 'mcp', so nothing an
// agent writes is ever excluded by it. Deleting an unreferenced input
// therefore takes an explicit --allow-drops.
//
// The one exception is an input the harness named and explained:
// result.SupersededIDs holds the ids it dropped as superseded by another input
// id that the same response carries forward (#639). That is a witnessed
// decision, not an omission — re-adding the stale row beside its own successor
// is how a memory reading "three issues are still open" survived next to "the
// three open issues have all been fixed". An obsolete drop names no successor
// and stays under the audit, because Ghost cannot check the claim.
//
// Uses the package's tokenize (numeric-retaining, stopword-filtered) so merged
// rewrites that preserve substance — including ports and versions — are
// recognized as survivors and are not re-added alongside the merge.
func AuditGuardedDrops(input ReflectionInput, result ReflectionResult) []DroppedGuarded {
	union := make(map[string]bool)
	for _, m := range result.Memories {
		for tok := range tokenize(m.Content) {
			union[tok] = true
		}
	}
	superseded := make(map[string]bool, len(result.SupersededIDs))
	for _, id := range result.SupersededIDs {
		superseded[strings.ToUpper(strings.TrimSpace(id))] = true
	}

	var drops []DroppedGuarded
	for _, in := range input.ExistingMemories {
		if superseded[strings.ToUpper(strings.TrimSpace(in.ID))] {
			continue
		}
		inTokens := tokenize(in.Content)
		if len(inTokens) == 0 {
			continue
		}
		if hasCloseSurvivor(inTokens, union) {
			continue
		}
		drops = append(drops, DroppedGuarded{
			Category:   in.Category,
			Content:    in.Content,
			Importance: in.Importance,
			Tags:       in.Tags,
		})
	}
	return drops
}

func hasCloseSurvivor(inTokens map[string]bool, outTokens map[string]bool) bool {
	if len(outTokens) == 0 {
		return false
	}
	found := 0
	for tok := range inTokens {
		if outTokens[tok] {
			found++
		}
	}
	return float64(found)/float64(len(inTokens)) >= dropContainmentThreshold
}

// RetainGuardedDrops turns every flagged drop back into a result memory so it
// can be carried forward verbatim. Refusing to apply (the pre-#458
// behaviour) preserved fidelity but meant a project whose consolidator dropped
// memories was never consolidated at all — measured 1 success in 13
// attempts on a 220-memory project. Re-inserting the dropped memories keeps
// the zero-loss invariant AND lets the rest of the consolidation apply. Because
// the content is byte-identical to the stored memory, ReplaceNonManual's
// exact-content reuse matches it to its existing row, so its embedding, links,
// and access stats survive (see #452).
func RetainGuardedDrops(drops []DroppedGuarded) []ReflectMemory {
	retained := make([]ReflectMemory, 0, len(drops))
	for _, d := range drops {
		retained = append(retained, ReflectMemory{
			Category:   d.Category,
			Content:    d.Content,
			Importance: d.Importance,
			Tags:       d.Tags,
		})
	}
	return retained
}
