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
// fraction of its tokens appears in the output it is compared against.
// Containment (input→output), not Jaccard: the question is whether the input's
// substance survives anywhere, not whether the rewrite is fully explained by one
// source.
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
// Two exceptions, both witnessed rather than inferred.
//
// The first is a supersession the harness stated: result.Supersessions holds the
// ids it dropped as superseded by another input id, together with the text that
// successor carries into the result. Re-adding such a row is how a memory
// reading "three issues are still open" survived beside "the three open issues
// have all been fixed". The witness has to be present, though: a successor one
// of the result's own post-filters removed (dropForeignProjectMemories deletes a
// memory naming a project the input corpus never mentioned) is no successor, so
// the exemption lapses and the input goes back under the audit. An obsolete drop
// names no successor at all and always goes under the audit, because Ghost
// cannot check the claim.
//
// The second is which set of outputs an input is compared against. An input the
// response ADDRESSED — kept, merged or rewritten, per result.AddressedIDs — is
// measured against the union of every output, because a merge may carry an
// input's substance across a survivor plus its siblings, and scoring it against
// any one of them re-adds it verbatim beside the merge that just absorbed it,
// which is the duplicate class #639 measured across every project. An input the
// response never named is measured against a SINGLE output, exactly as before.
// That asymmetry is deliberate and it is the whole reason the union is not
// applied to everything: under this contract omission is routine (the prompt
// tells the harness an unnamed id is kept verbatim), and a corpus-wide union
// turns "this memory's words also occur somewhere else" into a deletion nobody
// asked for, on no --allow-drops and with no warning — the unattended loss
// #337/#549 exist to prevent. The SQLite fallback tier addresses no ids, so
// every one of its inputs is measured the strict way.
//
// Uses the package's tokenize (numeric-retaining, stopword-filtered) so merged
// rewrites that preserve substance — including ports and versions — are
// recognized as survivors and are not re-added alongside the merge.
func AuditGuardedDrops(input ReflectionInput, result ReflectionResult) []DroppedGuarded {
	outTokens := make([]map[string]bool, 0, len(result.Memories))
	union := make(map[string]bool)
	present := make(map[string]bool, len(result.Memories))
	for _, m := range result.Memories {
		tokens := tokenize(m.Content)
		outTokens = append(outTokens, tokens)
		for tok := range tokens {
			union[tok] = true
		}
		present[m.Content] = true
	}

	superseded := make(map[string]bool, len(result.Supersessions))
	for _, s := range result.Supersessions {
		if s.TargetText != "" && present[s.TargetText] {
			superseded[memIDKey(s.DroppedID)] = true
		}
	}
	addressed := make(map[string]bool, len(result.AddressedIDs))
	for _, id := range result.AddressedIDs {
		addressed[memIDKey(id)] = true
	}

	var drops []DroppedGuarded
	for _, in := range input.ExistingMemories {
		key := memIDKey(in.ID)
		if superseded[key] {
			continue
		}
		inTokens := tokenize(in.Content)
		if len(inTokens) == 0 {
			continue
		}
		if addressed[key] {
			if hasCloseSurvivor(inTokens, union) {
				continue
			}
		} else if hasCloseSurvivorInAny(inTokens, outTokens) {
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

// hasCloseSurvivor scores an input against one output's token set.
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

// hasCloseSurvivorInAny scores an input against each output separately and
// accepts the first that explains enough of it. This is the test an input
// deserves when nothing in the response accounted for it: the guard is asking
// "did the model absorb this, or forget it", and only one survivor claiming the
// memory's substance can answer yes.
func hasCloseSurvivorInAny(inTokens map[string]bool, outTokens []map[string]bool) bool {
	for _, out := range outTokens {
		if len(out) == 0 {
			continue
		}
		if hasCloseSurvivor(inTokens, out) {
			return true
		}
	}
	return false
}

// memIDKey normalizes a stored or emitted id for comparison. Ids are ULIDs, but
// a model that lower-cases one still means the memory it was shown, and the
// comparison must not depend on which side of the contract the spelling came
// from.
func memIDKey(id string) string {
	return strings.ToUpper(strings.TrimSpace(id))
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
