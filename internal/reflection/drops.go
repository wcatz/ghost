package reflection

import (
	"strings"

	"github.com/wcatz/ghost/internal/memory"
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
// One exemption, and it is witnessed rather than inferred. result.Replacements
// names the input ids a response disposed of together with the text that took
// their place — a rewrite's own new text, or the emitted text of the successor a
// supersession named. A rewrite and a supersession are the same KIND of claim —
// "this row no longer needs carrying, that text says it instead" — so they share
// one list: re-adding such a row would put it back beside the text that replaced
// it, which is a duplicate rather than a save, and re-adding it is how a memory
// reading "three issues are still open" survived beside "the three open issues
// have all been fixed".
//
// They are not equally TRUSTED, though, and that distinction is the rest of this
// paragraph. A rewrite's witness is the model's own text for that same id, and
// the grounding check has already tied it to that row by rejecting a rewrite
// that introduces an identifier absent from its sources, so presence is enough.
// A supersession's witness is some OTHER carried-forward id, and nothing
// constrains it to be about the memory being disposed of — the parser's only rule
// is that the target survives the response. So a supersession is honoured only
// when its witness is RELATED to the row it disposes of, scored at the same
// containment the rest of the guard uses (#549).
//
// The witness has to be there too, for either kind. A filter that runs over the
// result after the operations are resolved can remove the replacing text —
// dropForeignProjectMemories deletes a memory naming a project the input corpus
// never mentioned, and a rewrite or a merge is exactly such a memory — and an
// exemption trusted on the id alone then disposes of a row with nothing in its
// place, silently, with nothing left for --allow-drops to act on. A claim whose
// text is gone lapses, and the input goes back under the ordinary audit. An
// obsolete drop names no successor and never gets here at all, so the token audit
// governs it.
//
// What the guard no longer has to do is the biggest part. An input the response
// never named is emitted verbatim by executeOps (#639), so there is nothing to
// rescue: before that, an unnamed memory was emitted by nobody and survived only
// if this guard FAILED to recognise a survivor, which made a false positive in
// the "absorbed" direction a silent deletion with no warning and no
// --allow-drops. An id the model never mentioned needs no inference at all.
//
// Which output set an input is compared against is the remaining choice. A MERGED
// source (result.Merges) is measured against the text of ITS OWN merge, since
// that is the only witness that can say whether the merge carried it: a merge
// source is consumed by its merge, so its own text is never in the result, and
// the parser rejects an id claimed twice, so a sibling cannot be there either.
// It is measured against nothing at all when the merge is not in the result, and
// falls back to the strict per-output test. Everything else is measured against a
// single output. The SQLite fallback names no ids, so every one of its inputs is
// measured the strict way.
//
// Uses the package's tokenize (numeric-retaining, stopword-filtered) so merged
// rewrites that preserve substance — including ports and versions — are
// recognized as survivors and are not re-added alongside the merge.
func AuditGuardedDrops(input ReflectionInput, result ReflectionResult) []DroppedGuarded {
	outTokens := make([]map[string]bool, 0, len(result.Memories))
	present := make(map[string]bool, len(result.Memories))
	for _, m := range result.Memories {
		tokens := tokenize(m.Content)
		outTokens = append(outTokens, tokens)
		present[m.Content] = true
	}

	// The disposed input's own text, so a supersession's claim can be checked for
	// being ABOUT the memory it disposes of rather than only for having a witness.
	byID := make(map[string]memory.Memory, len(input.ExistingMemories))
	for _, in := range input.ExistingMemories {
		byID[memIDKey(in.ID)] = in
	}

	// A disposition is honoured when its witness is PRESENT, and — for a
	// supersession, where the witness is some OTHER row — only when it is also
	// RELATED (#549). Presence alone was the whole test, so a response could
	// dispose of any memory at all by naming any carried-forward id as its
	// successor: the parser's only rule is that the target is carried forward, so
	// naming a neighbour is a valid operation, and nothing downstream compared
	// the two texts. The result was a deletion with no warning, no --allow-drops
	// and a zero exit status — the loss this guard exists to prevent, reached
	// through the one path the guard had exempted.
	//
	// A rewrite is not in that class and is not re-checked here. Its witness is
	// the model's own text for that same id, already tied to that row by the
	// grounding check, which rejects a rewrite introducing an identifier absent
	// from its sources and asks for `keep` instead. The unconstrained assertion
	// is specifically "this other row says it better".
	//
	// Relatedness is the same containment the rest of the guard uses, at the
	// same threshold, so "the same fact" means one thing in this file. A
	// supersession that genuinely rewords the fact in wholly different words
	// scores low and its stale row is re-added beside the replacement — a visible
	// duplicate, which is the direction this check must fail. The alternative
	// failure is a silent deletion.
	replaced := make(map[string]bool, len(result.Replacements))
	for _, r := range result.Replacements {
		if r.Text == "" || !present[r.Text] {
			continue
		}
		if r.Supersession {
			disposed, ok := byID[memIDKey(r.ID)]
			if !ok {
				continue
			}
			if !hasCloseSurvivor(tokenize(disposed.Content), tokenize(r.Text)) {
				continue
			}
		}
		replaced[memIDKey(r.ID)] = true
	}
	// A merged source is scored against the text of ITS OWN merge, not against
	// the union of every output (#549). The union was sound before the
	// pass-through existed, when the union was a handful of survivors; now every
	// id the response never named is emitted verbatim, so the union of a real
	// result is the whole project's vocabulary, and a source whose substance its
	// merge discarded passes containment on the strength of whatever unrelated
	// memory happens to share its words. That is the same class of loss the
	// pass-through was written to remove — an unrelated survivor sharing 45% of
	// the absorbed memory's tokens — reintroduced through the merge branch.
	//
	// The merge's own text is the whole witness available: a merge source is
	// consumed by its merge, so its own text is never in the result, and the
	// parser rejects an id claimed by two operations, so a sibling cannot be in
	// the result either. Scoring against the merge's siblings could only LOWER
	// the bar — they add their own words to a set the merge's text already
	// dominates — and it never rescues a merge whose own text is too thin, which
	// is the case the guard exists for.
	//
	// A merge a post-filter removed has no witness and is absent from this map,
	// so its sources fall back to the strict per-output test, which is the right
	// question once there is no merge to be spread across.
	mergedText := make(map[string]map[string]bool, len(result.Merges))
	for _, m := range result.Merges {
		if m.Text == "" || !present[m.Text] {
			continue
		}
		tokens := tokenize(m.Text)
		for _, id := range m.IDs {
			mergedText[memIDKey(id)] = tokens
		}
	}

	var drops []DroppedGuarded
	for _, in := range input.ExistingMemories {
		key := memIDKey(in.ID)
		if replaced[key] {
			continue
		}
		inTokens := tokenize(in.Content)
		if len(inTokens) == 0 {
			continue
		}
		if mergeTokens, ok := mergedText[key]; ok {
			if hasCloseSurvivor(inTokens, mergeTokens) {
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
