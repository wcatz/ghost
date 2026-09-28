package reflection

import (
	"sort"

	"github.com/wcatz/ghost/internal/memory"
)

// The input accounting (#684).
//
// A consolidation result says what the corpus BECAME; it did not say what
// happened to the rows it was given. `ghost reflect` therefore listed the
// resulting memories and nothing else, and an input a merge consumed simply
// disappeared from the report — a reviewer, or an independent judge reading a
// dry run, could not tell a merge from a loss. The applied run records the
// answer (each consumed input gets a `memory_history` delete row naming its
// successor) and the preview showed none of it, which is backwards: the preview
// is the point at which a person decides whether to pass --apply.
//
// This file is the part that knows the answer, and it is a pure function of the
// result so the same account is available to every reader of one. The invariant
// is the whole design: every input id lands in exactly ONE bucket, and the
// buckets are what the report prints — the operations that named an id on their
// own line, the two counts for the ids nothing named, and the ids no surviving
// output carries at all. A run that cannot be accounted for is a run nobody
// should have applied, and the one that used to be unauditable is the one that
// had nothing to audit.

// AccountMerge is one merge line: the ids it folded in, the text it produced,
// whether that text is one of the result's memories, and how many of its sources
// the drop guard flagged.
type AccountMerge struct {
	IDs  []string
	Text string
	// In is whether the merge's text is still one of the result's memories, which
	// a post-filter (contamination, fabrication) can remove after the operation
	// was accepted. The text compared is the one the tier produced, because that
	// is what the result holds: `ghost reflect` clamps the scope-split copies of
	// the memories, not the result's own slice, so clamping the key here would
	// miss a merge over the cap and report one that landed as lost. The
	// truncation itself is reported separately, on the line above this section.
	In          bool
	SourceBytes int
	// Guarded counts the sources the drop guard found no surviving output for.
	// A number rather than their ids, because the line already names them and a
	// report that quoted one id twice would break the one-account contract.
	Guarded int
}

// AccountRewrite is one rewrite line: the id it replaced, the text it wrote
// instead, and whether that text is one of the result's memories.
type AccountRewrite struct {
	ID      string
	Text    string
	In      bool
	Guarded bool
}

// AccountDrop is one drop line: the id the response disposed of, the reason it
// gave, the id it named as the replacement, and whether the drop guard found
// anything in the result carrying the row.
type AccountDrop struct {
	ID     string
	Reason string
	// Successor is empty for an `obsolete` drop, and otherwise the stored
	// spelling of the id that takes over — resolved here for the reason Drop
	// documents: the raw one is the response's, and upper-cased by the parser.
	Successor string
	Guarded   bool
}

// Why a row is gone. Two reasons, and the difference is the whole point: the
// first is a loss, the second is a deduplication, and a report that cannot tell
// them apart is the defect #684 is about.
const (
	// deletedNoCarrier: nothing in the result carries this row's text, so the
	// replace deletes it. A merge folded it away, a drop named it, a post-filter
	// removed the emission that was carrying it, or the SQLite tier absorbed a
	// near-duplicate.
	deletedNoCarrier = "no surviving output carries this row"
	// deletedDuplicate: the text IS in the result, and another input with the same
	// bytes is the stored row the reuse claims. Nothing is lost — the knowledge
	// is in the project either way — but this row is gone, and reporting it as
	// merely carried is how a caller ends up looking for a memory that is not
	// there under either of the two ids it was saved as.
	deletedDuplicate = "an identical row is reused in its place"
)

// AccountDeleted is one input row an apply removes, and why.
type AccountDeleted struct {
	ID     string
	Reason string
}

// InputAccounting is every input id of one consolidation, in the buckets the
// report prints. The five fields that name ids and the two counts partition the
// input set: Merges, Refusals, Rewrites and Drops name the ids an operation
// touched, Kept and Passed are the counts of the ids a keep named and of the ids
// nothing named, and Deleted is the rows an apply removes.
type InputAccounting struct {
	// Inputs is the number of distinct input ids, which is what the report's
	// total is and what the buckets have to add up to.
	Inputs   int
	Merges   []AccountMerge
	Refusals []Refusal
	Rewrites []AccountRewrite
	Drops    []AccountDrop
	// Kept are the ids an explicit `keep` named whose stored row the replace
	// reuses in place.
	Kept []string
	// Passed are the ids no operation named whose stored row the replace reuses in
	// place — the pass-through, which for the LLM tier is every input the harness
	// ignored and for the SQLite tier is every input it did not absorb.
	Passed []string
	// Deleted are the rows an apply removes, each with the reason. This is the
	// bucket that makes the section an AUDIT: the others say what survived, and
	// an id that is in none of them is a row an operator still believes they have.
	//
	// It is decided by the replace's own reuse pass rather than by whether the
	// row's text is in the result, because reuse is content-keyed and claims ONE
	// row per emission: two byte-identical inputs both have their text in the
	// result, and only one of them is still there afterwards. Keying on the text
	// called both of them carried, which is the same confusion in the opposite
	// direction — a row reported as kept that the apply deleted.
	Deleted []AccountDeleted
}

// claimCandidate is one stored row the project replace may still claim: the
// input id, and the two stored fields takeReusableRow's pick reads.
type claimCandidate struct {
	id       string
	category string
	created  string
}

// emission is one row the project replace is handed: the text and category it
// claims a stored row by.
type emission struct {
	content  string
	category string
}

// claimedRows mirrors the reuse pass in `ReplaceNonManual`: each emission claims
// one stored row with byte-identical content — the one in the same category,
// else the oldest — and every input row left in a bucket when the pass ends is
// deleted by the same transaction. So the answer to "is this row still here?" is
// a CLAIM, not a membership test on the text, and the two answers differ exactly
// when several rows carry the same bytes.
//
// The emission ORDER does not matter to the outcome, and that is worth knowing
// rather than assuming: two emissions can only compete for one row if they share
// its content, and then either the category match claims it or the bucket's first
// row does, so the row is claimed under both orders. That is also why
// `--promote-globals` is not a parameter here. A cross-project candidate is
// written to `_global` instead of the project, so a project emission list would
// be shorter — but an input's own pass-through is always project-scoped (a
// verbatim emission states no scope), so every input no operation named is
// accounted for by the project replace under both settings. A NAMED input is the
// one a promotion could change the fate of, and a named input is owned by its own
// line rather than by this bucket.
//
// The rule is duplicated rather than shared because the store's version runs
// against an open transaction and returns rows to the caller; a second
// implementation of "which same-content row survives" is only acceptable while
// something checks the two against each other on a real store, which
// TestReflectSummaryNamesTheRowAnIdenticalTwinReuses and its two siblings do —
// each asserts that the id the section reports as deleted is the id the store
// stopped holding.
func claimedRows(input []memory.Memory, emitted []emission) map[string]bool {
	reusable := make(map[string][]claimCandidate, len(input))
	for _, m := range input {
		reusable[m.Content] = append(reusable[m.Content], claimCandidate{
			id: m.ID, category: m.Category, created: m.CreatedAt,
		})
	}
	// The store's candidate query is `ORDER BY created_at, id` and takeReusableRow
	// takes the FIRST of what is left, so this order decides which same-content
	// row survives a deduplication. It is not the caller's read order — the
	// consolidator's input is whatever GetAll returned, which is by importance.
	for content, bucket := range reusable {
		sorted := append([]claimCandidate(nil), bucket...)
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].created != sorted[j].created {
				return sorted[i].created < sorted[j].created
			}
			return sorted[i].id < sorted[j].id
		})
		reusable[content] = sorted
	}
	claimed := make(map[string]bool, len(input))
	for _, e := range emitted {
		bucket := reusable[e.content]
		if len(bucket) == 0 {
			// A fresh insert: nothing to claim, and the emission's row is new.
			continue
		}
		pick := 0
		for i, c := range bucket {
			if c.category == e.category {
				pick = i
				break
			}
		}
		// memIDKey, because the walk below looks its claims up the same way and a
		// map keyed by the raw id would report every row as unclaimed. The stored
		// spelling is upper-case hex, so this is not visible on a real store — it
		// is the same normalise-on-both-sides rule memIDKey exists for, and a
		// hand-built input with a lower-case id is enough to break it.
		claimed[memIDKey(bucket[pick].id)] = true
		// The full slice expression, because the store's version does the same:
		// without it this appends into the bucket's own backing array.
		reusable[e.content] = append(bucket[:pick:pick], bucket[pick+1:]...)
	}
	return claimed
}

// AccountInputs partitions the result's input ids into the buckets a report
// prints. It is pure: the same input and result always account the same way, and
// nothing here decides anything — the drop guard has already run, and `guarded`
// is its verdict, carried so a line can say what became of the ids it names.
//
// It describes what an apply DOES with the rows, which is the only question that
// makes a bucket named "kept" or "passed through" worth reading. Two things it
// cannot see, both reported by the apply path itself: a proposal dropped at the
// write boundary for holding a credential is dropped after this section is
// printed (see `dropCredentialProposals`), and a row saved during the round trip
// is preserved untouched rather than claimed or deleted (see the `preserved`
// count `ApplyReflection` returns). Everything else follows from the result and
// the input.
func AccountInputs(input ReflectionInput, result ReflectionResult, guarded []DroppedGuarded) InputAccounting {
	present := make(map[string]bool, len(result.Memories))
	for _, m := range result.Memories {
		present[m.Content] = true
	}
	flagged := make(map[string]bool, len(guarded))
	for _, g := range guarded {
		flagged[memIDKey(g.ID)] = true
	}

	// The input by id, deduplicated. A repeated id is one row, and a report that
	// named it twice would claim an input the run was not given; the op parser
	// rejects an id claimed by two operations, so this is the only way a
	// duplicate reaches the accounting, and the total counts ids rather than
	// rows so that the buckets still add up to it.
	byID := make(map[string]memory.Memory, len(input.ExistingMemories))
	order := make([]string, 0, len(input.ExistingMemories))
	for _, in := range input.ExistingMemories {
		key := memIDKey(in.ID)
		if _, dup := byID[key]; dup {
			continue
		}
		byID[key] = in
		order = append(order, key)
	}
	// Every id a line prints is the STORED spelling, never the one the model
	// wrote. `trimIDLabel` strips the prompt's `id:` label and trims but does not
	// upper-case, and memIDKey exists precisely because a model that lower-cased
	// an id still means the row it was shown — so printing the raw spelling would
	// let a merge line show `01j8z…02` where a count line shows `01J8Z…02`, and
	// these ids are the keys the report asks an operator to look rows up by.
	// An id the input does not carry is passed through unchanged: the parser
	// refuses one, so a line can only reach that with a hand-built result, and
	// printing nothing there would be worse than printing what it was given.
	storedID := func(id string) string {
		if m, ok := byID[memIDKey(id)]; ok {
			return m.ID
		}
		return id
	}
	storedIDs := func(ids []string) []string {
		out := make([]string, 0, len(ids))
		for _, id := range ids {
			out = append(out, storedID(id))
		}
		return out
	}

	acc := InputAccounting{Inputs: len(order)}
	for _, r := range result.Refusals {
		r.IDs = storedIDs(r.IDs)
		acc.Refusals = append(acc.Refusals, r)
	}
	// The rows the replace will reuse, decided before the walk so a bucket can say
	// "still here" only of a row that is still here.
	rows := make([]memory.Memory, 0, len(order))
	for _, key := range order {
		rows = append(rows, byID[key])
	}
	emitted := make([]emission, 0, len(result.Memories))
	for _, m := range result.Memories {
		emitted = append(emitted, emission{content: m.Content, category: m.Category})
	}
	claimed := claimedRows(rows, emitted)

	// Which operation owns an id. The four lists are disjoint by the parser's own
	// rule — an id takes exactly one operation — except that a supersession is
	// recorded twice on purpose: Replacements carries the claim (the text the
	// successor carries, which stamps the delete row's related_id) and Drops
	// carries the reason. Drops claims the id first, so Replacements is read as
	// the rewrites it is after that.
	dropOf := make(map[string]AccountDrop, len(result.Drops))
	for _, d := range result.Drops {
		dropOf[memIDKey(d.ID)] = AccountDrop{
			ID:        storedID(d.ID),
			Reason:    d.Reason,
			Successor: storedID(d.Successor),
			Guarded:   flagged[memIDKey(d.ID)],
		}
	}
	rewriteOf := make(map[string]AccountRewrite, len(result.Replacements))
	for _, r := range result.Replacements {
		key := memIDKey(r.ID)
		if _, claimed := dropOf[key]; claimed {
			continue
		}
		rewriteOf[key] = AccountRewrite{ID: storedID(r.ID), Text: r.Text, In: present[r.Text], Guarded: flagged[key]}
	}
	refused := make(map[string]bool)
	for _, r := range result.Refusals {
		for _, id := range r.IDs {
			refused[memIDKey(id)] = true
		}
	}
	kept := make(map[string]bool, len(result.Kept))
	for _, id := range result.Kept {
		kept[memIDKey(id)] = true
	}
	// A merge is reported once and owns each of its sources, so the walk below
	// annotates the merge line rather than counting the source separately.
	mergeOf := make(map[string]int, len(result.Merges))
	for _, m := range result.Merges {
		line := AccountMerge{IDs: storedIDs(m.IDs), Text: m.Text, In: present[m.Text]}
		for _, id := range m.IDs {
			if src, ok := byID[memIDKey(id)]; ok {
				line.SourceBytes += len(src.Content)
			}
			mergeOf[memIDKey(id)] = len(acc.Merges)
		}
		acc.Merges = append(acc.Merges, line)
	}

	for _, key := range order {
		if idx, ok := mergeOf[key]; ok {
			if flagged[key] {
				acc.Merges[idx].Guarded++
			}
			continue
		}
		if rw, ok := rewriteOf[key]; ok {
			acc.Rewrites = append(acc.Rewrites, rw)
			continue
		}
		if d, ok := dropOf[key]; ok {
			acc.Drops = append(acc.Drops, d)
			continue
		}
		if refused[key] {
			// Named by a refusal, printed on the refusal's own line: the sources
			// were emitted unchanged, so this is neither a keep nor a pass-through
			// and counting it as either would misreport what the model asked for.
			continue
		}
		row := byID[key]
		if claimed[key] {
			// The row the reuse claimed: updated in place, its id, embedding,
			// links and age intact. That is what "carried through" has to mean.
			if kept[key] {
				acc.Kept = append(acc.Kept, row.ID)
			} else {
				acc.Passed = append(acc.Passed, row.ID)
			}
			continue
		}
		reason := deletedNoCarrier
		if present[row.Content] {
			// The text IS written to the project — by another row with the same
			// bytes, which is the one the reuse claimed. Nothing is lost, this row
			// is, and saying only "carried" would be the same confusion as calling a
			// loss a deduplication.
			reason = deletedDuplicate
		}
		acc.Deleted = append(acc.Deleted, AccountDeleted{ID: row.ID, Reason: reason})
	}
	return acc
}
