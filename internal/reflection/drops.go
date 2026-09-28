package reflection

import (
	"strings"
)

// DroppedGuarded is an input memory, in any category, that has no close
// survivor in the consolidation output. Importance and Tags are carried so the
// memory can be re-inserted verbatim by RetainGuardedDrops without losing its
// weight or labels.
type DroppedGuarded struct {
	// ID is the input memory's own id. The guard is asked a question about
	// memories and answers with their text, but a reader of a dry run needs to
	// know WHICH row the verdict was about: the operation that named the id is
	// on its own line in the report, and the verdict has to be annotatable on
	// that line (#684).
	ID         string
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
// THERE IS NO EXEMPTION. Not for a rewrite and not for a supersession: an
// unattended reflect never deletes a memory on the model's say-so alone.
//
// This used to carve out one, and the carve-out was the hole. `drop X reason:
// superseded by Y` disposed of X whenever Y's text was in the result, with
// nothing checking that Y had anything to do with X — the parser's only rule is
// that the target survives the response, so naming a neighbour is a valid
// operation, and two ordinary deployment notes in the same project paired
// because they sat adjacent in the prompt is all it took. Requiring the witness
// to be RELATED closed that, and then turned out to be redundant: the witness
// text is itself an output, so a row at 45% containment against it also passes
// the per-output scan below. The whole branch was dead code, and removing it
// leaves the suite green.
//
// The rule is KEEP-biased, deliberately. A stale row that is kept is
// REPAIRABLE: the lifecycle runs resolve and supersede right after reflect, and
// demoting a row that is genuinely stale is exactly their job. A deleted row is
// not repairable, and on the unattended path there is nobody watching to notice.
// So the failure direction here is a duplicate — the old text back beside its
// replacement — never a silent deletion.
//
// The cost, named: the "three issues are still open" class can come back as a
// kept stale row when the successor is reworded below the containment bar, and
// stays visible until resolve or supersede demote it. A corpus of those grows
// the input the next pass has to read. That is the trade, and it is the right way
// round for a command that rewrites a memory store with no human in the loop.
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
// falls back to the strict per-output test. Everything else — an explicit
// `obsolete` drop, a rewrite, a supersession, and every input of the offline
// SQLite tier, which names no ids — is measured against a single output.
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
			ID:         in.ID,
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

// memIDKey normalizes a stored or emitted id for comparison. A stored id is 32 hex
// characters (`hex(randomblob(16))` — ULID-shaped in that it is a 128-bit random value
// rendered in hex, though the ULID time prefix is not there), but
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
