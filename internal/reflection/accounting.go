package reflection

import "github.com/wcatz/ghost/internal/memory"

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

// InputAccounting is every input id of one consolidation, in the buckets the
// report prints. The five fields that name ids and the two counts partition the
// input set: Merges, Refusals, Rewrites and Drops name the ids an operation
// touched, Kept and Passed are the counts of the ids a keep named and of the ids
// nothing named, and Absent is the ids no surviving output carries.
type InputAccounting struct {
	// Inputs is the number of distinct input ids, which is what the report's
	// total is and what the buckets have to add up to.
	Inputs   int
	Merges   []AccountMerge
	Refusals []Refusal
	Rewrites []AccountRewrite
	Drops    []AccountDrop
	// Kept are the ids an explicit `keep` named and the result still carries.
	Kept []string
	// Passed are the ids no operation named and the result still carries — the
	// pass-through, which for the LLM tier is every input the harness ignored and
	// for the SQLite tier is every input it did not absorb.
	Passed []string
	// Absent are the ids no operation named and no surviving output carries.
	// Nothing in the report protects them: ReplaceNonManual deletes every
	// replaceable row the emitted set does not account for, so these are the rows
	// this round removes. They are a small bucket on the LLM path (a post-filter
	// removed the row that carried them) and the whole of a SQLite tier's
	// absorptions, which name no ids and so are in no other bucket.
	Absent []string
}

// AccountInputs partitions the result's input ids into the buckets a report
// prints. It is pure: the same input and result always account the same way, and
// nothing here decides anything — the drop guard has already run, and `guarded`
// is its verdict, carried so a line can say what became of the ids it names.
//
// It describes the RESULT, which is what the report is about: whether a row
// survived is a question about result.Memories, and the content cap the caller
// applies on its way to the store is reported on its own line.
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
		switch content := byID[key].Content; {
		case !present[content]:
			acc.Absent = append(acc.Absent, byID[key].ID)
		case kept[key]:
			acc.Kept = append(acc.Kept, byID[key].ID)
		default:
			acc.Passed = append(acc.Passed, byID[key].ID)
		}
	}
	return acc
}
