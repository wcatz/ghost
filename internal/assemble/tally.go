package assemble

// passiveAllInvalidSentence is the ONE sentence for a passive result whose rows
// were all withheld as out of date. It is shared between `abstention` (which
// renders it on the search-free surfaces) and the session-start block's
// wholly-withheld note (BucketTally.WithheldNote), and it has to be one
// constant because the two are the same statement about the same cause: a
// reader who sees one sentence on a tool call and a different one at session
// start would conclude the block was lying about one of them.
const passiveAllInvalidSentence = "No sufficiently trustworthy memory found: the candidates this block was assembled from were " +
	"withheld as out of date, their validity windows having closed or not yet opened." +
	" The block was not empty before that — the answer is withheld, not absent."

// WithheldPointer is the instruction appended to a withholding note: the rows
// are still in the store, marked with the window they carry, and
// ghost_memories_list shows them. It is exported so every surface that answers
// "every row was withheld" points at the same tool with the same bytes — the
// session-start block and ghost_project_context both append it, and two
// renderings of one pointer are two promises.
const WithheldPointer = " Call ghost_memories_list to see them, still marked with the window they carry."

// BucketTally is one bucket's fate as a reader of the block needs it, counted
// from the trace: how many rows were shown, how many the budget ranked out,
// how many a stage withheld, and — when any were withheld — the dominant cause.
//
// The split is the whole point of the type, and the distinction it draws is the
// one issue #897 was filed about: a session-start block that counts every
// unshown row as "not shown, ranked by a composite score" attributes a stage's
// withholding to the ranking. "Ranked out" is the cap's doing and carries the
// ranking's authority; "withheld" is a stage refusing a row it saw, which the
// ranking never reviewed.
type BucketTally struct {
	Shown     int
	RankedOut int
	Withheld  int
	// Reason is the withheld rows' dominant cause, first-seen on a tie. It is
	// the one cause WithheldNote names, so the sentence a wholly-withheld block
	// renders matches what actually withheld the rows.
	Reason string
}

// Window is the number of rows the assembler's window held for this bucket:
// everything the stages saw, shown or not. It is the "of M total" the block's
// header divides by, and taking it from the trace rather than a second COUNT
// is what keeps the header consistent with the rows the block is made of.
func (t BucketTally) Window() int {
	return t.Shown + t.RankedOut + t.Withheld
}

// CountsFor tallies one bucket's fate from a trace. `shown` is the caller's own
// admitted slice length (the trace records only the rows the stages decided on,
// and a kept row without a decision is not a "shown" it can count); RankedOut
// counts the budget's cuts, Withheld every other drop.
//
// A nil trace is a caller that never assembled (the historical renderer), and
// it gets the shown half and zeros: nothing was ranked out or withheld because
// no stage ran.
func CountsFor(trace *Trace, bucket string, shown int) BucketTally {
	t := BucketTally{Shown: shown}
	if trace == nil {
		return t
	}
	// The withheld causes counted in first-seen order, so the tie-break can
	// name the reason a reader of Decisions would have met first rather than
	// the last one to arrive.
	counts := make(map[string]int)
	order := make([]string, 0, 4)
	for _, d := range trace.Decisions {
		if d.ProjectID != bucket || d.Kept {
			continue
		}
		switch d.Stage {
		case stageBudget, stageResponseFit:
			t.RankedOut++
		default:
			t.Withheld++
			if _, seen := counts[d.Reason]; !seen {
				order = append(order, d.Reason)
			}
			counts[d.Reason]++
		}
	}
	best, bestN := "", 0
	for _, r := range order {
		if n := counts[r]; n > bestN {
			best, bestN = r, n
		}
	}
	t.Reason = best
	return t
}

// WithheldNote is the sentence for a block whose project half was entirely
// withheld: the cause, in the assembler's words, and the pointer to the rows
// that still carry it. It answers only that one question — a block that shows
// rows has its header accounting for the withheld half, and one with nothing
// withheld has nothing to explain — so anything else gets "".
func (t BucketTally) WithheldNote() string {
	if t.Shown != 0 || t.Withheld == 0 {
		return ""
	}
	return withheldSentence(t.Reason) + WithheldPointer
}

// withheldSentence maps a withheld cause to its sentence. The two validity
// states share the out-of-date wording the passive abstention uses; scope is
// the other withholding a session start can hit; anything else keeps the
// withholding honest without inventing a cause the tally did not observe.
func withheldSentence(reason string) string {
	switch reason {
	case validityExpired, validityFuture:
		return passiveAllInvalidSentence
	case "scope_contradiction":
		return "No sufficiently trustworthy memory found: nothing found matched the requested scope."
	default:
		return "No sufficiently trustworthy memory found: the candidates this block was assembled from were " +
			"withheld before the answer was assembled."
	}
}
