package assemble

const passiveAllInvalidSentence = "No sufficiently trustworthy memory found: the candidates this block was assembled from were " +
	"withheld as out of date, their validity windows having closed or not yet opened." +
	" The block was not empty before that — the answer is withheld, not absent."

// WithheldPointer is the instruction appended to a withholding note.
const WithheldPointer = " Call ghost_memories_list to see them, still marked with the window they carry."

// BucketTally summarizes one bucket's fate in the assembled block.
type BucketTally struct {
	Shown     int
	RankedOut int
	Withheld  int
	Beyond    int
	Deduped   int
	Excluded  int
	PinnedCut int
	ByteCut   int
	Reason    string
	AtLeast   bool
}

// Window returns the number of rows in the retrieval window for this bucket.
func (t BucketTally) Window() int {
	return t.Shown + t.RankedOut + t.Withheld + t.Deduped - t.Beyond - t.Excluded
}

// Total returns the total eligible rows for this bucket.
func (t BucketTally) Total() int {
	return t.Shown + t.RankedOut + t.Withheld + t.Deduped
}

// CountedAgainst folds store counts into the tally.
func (t BucketTally) CountedAgainst(eligible, excluded, overFetch int) BucketTally {
	got, _ := t.countedAgainst(eligible, excluded, overFetch)
	return got
}

// CountedAtLeast folds capped store counts into the tally and marks it as a lower bound.
func (t BucketTally) CountedAtLeast(eligible, excluded, overFetch int) BucketTally {
	got, ok := t.countedAgainst(eligible, excluded, overFetch)
	got.AtLeast = ok
	return got
}

func (t BucketTally) countedAgainst(eligible, excluded, overFetch int) (BucketTally, bool) {
	if eligible < 0 || excluded < 0 || excluded > eligible || eligible < t.Total() {
		return t, false
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
			t.Reason = validityExpired
		}
	}
	return t, true
}

// CountsFor tallies one bucket's fate from a trace.
func CountsFor(trace *Trace, bucket string, shown int) BucketTally {
	t := BucketTally{Shown: shown}
	if trace == nil {
		return t
	}
	t.PinnedCut = trace.PinnedCut[bucket]
	counts := make(map[string]int)
	order := make([]string, 0, 4)
	for _, d := range trace.Decisions {
		if d.ProjectID != bucket || d.Kept {
			continue
		}
		switch d.Stage {
		case stageDedup:
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

// WithheldNote returns the note for a block whose project bucket was entirely withheld.
func (t BucketTally) WithheldNote() string {
	if t.Shown != 0 || t.Withheld == 0 || t.RankedOut-t.Beyond != 0 {
		return ""
	}
	if t.Reason == ReasonHostInstructions {
		return withheldSentence(t.Reason) + " Call ghost_memories_list to see them."
	}
	return withheldSentence(t.Reason) + WithheldPointer
}

// EmptyNote returns the note for an empty result whose rows were found and withheld.
func EmptyNote(res Result) string {
	if res.Outcome != OutcomeEmpty || res.Reason == ReasonNoMemories {
		return ""
	}
	note := res.Abstention
	if note == "" {
		return "Ghost holds memories for this project, but none of them is current."
	}
	return note + WithheldPointer
}

func withheldSentence(reason string) string {
	p := &pipeline{passive: true}
	switch reason {
	case validityExpired, validityFuture:
		return p.abstention(OutcomeEmpty, reasonAllInvalid)
	case "scope_contradiction":
		return p.abstention(OutcomeEmpty, reasonAllOutOfScope)
	case ReasonHostInstructions:
		return "Every candidate in this block restates a rule from the instruction files this host already loaded, so none is repeated here."
	default:
		return "No sufficiently trustworthy memory found: the candidates this block was assembled from were " +
			"withheld before the answer was assembled."
	}
}
