package reflection

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
// fraction of its tokens appears in some single output memory. Containment
// (input→output), not Jaccard: the question is whether the input's substance
// survives anywhere, not whether the rewrite is fully explained by one source.
// Deliberately lenient — a false positive costs a healthy merge whose input is
// then also re-added verbatim (a duplicate row), while an outright deletion
// lands far below the threshold (an unrelated survivor shares only a stray
// token or two).
const dropContainmentThreshold = 0.45

// AuditGuardedDrops returns every input memory that has no token-overlap
// survivor among the result memories, whatever its category. Every category is
// under the drop guard (#549): it began as an allowlist of operational
// knowledge — gotchas, dependency pins, workflow conventions and user
// preferences (issue #337, eval-cycle finding F3) — but an architecture or
// decision memory dropped by an unattended consolidation is just as
// unrecoverable, and nothing else covers it (agent saves carry source='mcp', so
// `manual` is only ever written for pinned seeds). Deleting an unreferenced
// input therefore takes an explicit --allow-drops.
//
// Uses the package's tokenize (numeric-retaining, stopword-filtered) so merged
// rewrites that preserve substance — including ports and versions — are
// recognized as survivors and are not re-added alongside the merge.
func AuditGuardedDrops(input ReflectionInput, result ReflectionResult) []DroppedGuarded {
	outTokens := make([]map[string]bool, 0, len(result.Memories))
	for _, m := range result.Memories {
		outTokens = append(outTokens, tokenize(m.Content))
	}

	var drops []DroppedGuarded
	for _, in := range input.ExistingMemories {
		inTokens := tokenize(in.Content)
		if len(inTokens) == 0 {
			continue
		}
		if hasCloseSurvivor(inTokens, outTokens) {
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

func hasCloseSurvivor(inTokens map[string]bool, outTokens []map[string]bool) bool {
	for _, out := range outTokens {
		if len(out) == 0 {
			continue
		}
		found := 0
		for tok := range inTokens {
			if out[tok] {
				found++
			}
		}
		if float64(found)/float64(len(inTokens)) >= dropContainmentThreshold {
			return true
		}
	}
	return false
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
