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
	// Beyond is how many of RankedOut were never fetched: eligible rows past the
	// over-fetch's LIMIT, which the ranking cut before any stage saw them. It is
	// set by CountedAgainst and is already part of RankedOut.
	Beyond int
	// Deduped is how many rows the retriever fetched and then removed as
	// near-duplicate losers (a bucket policy's DropDemotedLosers). Counted by
	// CountsFor from the trace's stage 6 decisions, which are the one record of
	// them; they are the policy's doing, not the ranking's cut, and not part of
	// RankedOut or Withheld.
	Deduped int
	// Excluded is how many of Withheld never entered the window: rows the fetch's
	// own validity predicate removed before the LIMIT, so no stage ever decided
	// on them and the trace holds nothing for them. Set by CountedAgainst.
	Excluded int
	// PinnedCut is how many pinned rows the bucket's cap left out, because the
	// pinned rows alone exceeded it. It is a SUBSET of RankedOut (a cut row is a
	// ranked-out one), never added to Total, and it is what lets a header say the
	// pin's slot guarantee ran out rather than leave those rows to read as ranking.
	// Set by CountsFor from the trace.
	PinnedCut int
	// ByteCut is how many of RankedOut the response-fit post-pass removed to
	// bring the block under its byte cap (stage 8, reason budget_dropped). It is a
	// SUBSET of RankedOut and never added to Total, kept apart so a header can say
	// those rows were cut for size and not outranked by the composite score.
	ByteCut int
	// Reason is the withheld rows' dominant cause, first-seen on a tie. It is
	// the one cause WithheldNote names, so the sentence a wholly-withheld block
	// renders matches what actually withheld the rows.
	Reason string
}

// Window is the number of rows the retrieval's over-fetched window held for
// this bucket: everything the stages saw, shown or not. It is NOT the number of
// rows the store holds; Total is that.
func (t BucketTally) Window() int {
	return t.Shown + t.RankedOut + t.Withheld + t.Deduped - t.Beyond - t.Excluded
}

// Total is every eligible row in the bucket: the window's rows plus the rows
// beyond it. It is the "of M total" the block's header divides by, and it
// answers for the store (rows passing the retrieval's own SQL predicates), so
// a project holding sixty live rows reads "of 60", not "of 45".
func (t BucketTally) Total() int {
	return t.Shown + t.RankedOut + t.Withheld + t.Deduped
}

// CountedAgainst folds in what the store holds for the bucket: `eligible` rows
// counted over the same population the retrieval's fetch draws from, of which
// `excluded` were removed by the fetch's own validity predicate before its LIMIT
// (so no stage saw them), and the window's `overFetch` limit.
//
//   - excluded rows are withheld, and are counted once: they are inside
//     `eligible` and in no trace decision.
//   - the rest of the eligible rows past the over-fetch were cut by the ranking
//     before any stage saw them, so they are ranked out (Beyond).
//   - near-duplicate losers are NOT derived here: the trace records each one
//     (stage 6) and CountsFor has already counted them, so Deduped has one source.
//     A row inside the limit that the trace never recorded (a write raced the two
//     reads) is left uncounted rather than guessed into a fate.
//
// A negative eligible count means the count could not be read and changes
// nothing; a count smaller than what the tally already holds (a write raced the
// two reads) never shrinks the tally.
func (t BucketTally) CountedAgainst(eligible, excluded, overFetch int) BucketTally {
	if eligible < 0 || excluded < 0 || excluded > eligible || eligible < t.Total() {
		return t
	}
	inWindow := eligible - excluded
	fetched := inWindow
	if overFetch > 0 && overFetch < inWindow {
		fetched = overFetch
	}
	if beyond := inWindow - fetched; beyond > 0 {
		t.RankedOut += beyond
		t.Beyond += beyond
	}
	if excluded > 0 {
		t.Withheld += excluded
		t.Excluded += excluded
		if t.Reason == "" {
			// A predicate-withheld row has no decision to name its cause; the two
			// validity states share one sentence, so either names it.
			t.Reason = validityExpired
		}
	}
	return t
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
	t.PinnedCut = trace.PinnedCut[bucket]
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
		case stageDedup:
			// The retriever's removal of a near-duplicate loser: neither the
			// ranking's cut nor a stage refusing the row's content.
			t.Deduped++
		case stageBudget, stageResponseFit:
			t.RankedOut++
			if d.Stage == stageResponseFit {
				t.ByteCut++
			}
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
// rows has its header accounting for the withheld and ranked-out halves, and one
// with nothing withheld has nothing to explain — so anything else gets "". A
// bucket with rows the ranking cut INSIDE the window is not wholly withheld; rows
// merely beyond the window do not change the answer, because the assembler
// answers about the window (ghost_project_context says the same).
//
// The sentence is the assembler's own abstention for the cause, not a table of
// this type's: see withheldSentence.
func (t BucketTally) WithheldNote() string {
	if t.Shown != 0 || t.Withheld == 0 || t.RankedOut-t.Beyond != 0 {
		return ""
	}
	return withheldSentence(t.Reason) + WithheldPointer
}

// EmptyNote is the one note for a result whose rows were found and withheld, so
// the whole block came back empty: the assembler's own abstention plus the
// pointer to the rows that still carry their window. "" for an answer and for
// `no_memories`, an empty window that may describe absence. ghost_project_context
// and the session-start block both print it, so they say the same sentence for
// the same state.
func EmptyNote(res Result) string {
	if res.Outcome != OutcomeEmpty || res.Reason == ReasonNoMemories {
		return ""
	}
	note := res.Abstention
	if note == "" {
		// Unreachable while every reason renders a sentence; the fallback states
		// the fact in the fewest words that cannot be wrong, rather than
		// reintroducing the census's lie.
		return "Ghost holds memories for this project, but none of them is current."
	}
	return note + WithheldPointer
}

// withheldSentence is the assembler's own abstention for a withheld bucket's
// dominant cause. There is no sentence table here: the cause is mapped to the
// reason `abstention` renders and the sentence is asked of it, so the two cannot
// drift. Only a cause the assembler has no passive sentence for falls to the
// generic line, which claims nothing the tally did not observe.
func withheldSentence(reason string) string {
	p := &pipeline{passive: true}
	switch reason {
	case validityExpired, validityFuture:
		return p.abstention(OutcomeEmpty, reasonAllInvalid)
	case "scope_contradiction":
		return p.abstention(OutcomeEmpty, reasonAllOutOfScope)
	default:
		return "No sufficiently trustworthy memory found: the candidates this block was assembled from were " +
			"withheld before the answer was assembled."
	}
}
