package assemble

// FitOutcome is what the fit rule settled on, so the caller can decide what an
// answer that still does not fit is: the search envelope refuses one, a caller
// with its own render keeps what it has.
type FitOutcome int

const (
	// FitFits: the block fits the cap.
	FitFits FitOutcome = iota
	// FitFramingIsTheExcess: the framing alone is over the ceiling, so the
	// rows are kept rather than cut for nothing.
	FitFramingIsTheExcess
	// FitLastRowKept: the last row was kept whole (the search envelope's
	// rule).
	FitLastRowKept
	// FitNoRowsLeft: there was no row left to give up.
	FitNoRowsLeft
)

// FitItems is the ONE fit rule for a block of rows: the response-fit post-pass
// in Run runs it, and so does the session-start block's historical read, whose
// rows are not the assembler's but which must be fitted by the same rule.
//
// The caller owns its rows and everything a drop changes; the helper owns the
// rule:
//
//   - the lowest-ranked row goes first, and an UNPINNED row before any pinned
//     one (Budget.KeepPinned — a pin is a slot guarantee, so the cap takes it
//     last);
//   - the block is measured again after every drop, because the framing grows
//     with every cut and a cap measured against a stale framing is the bug
//     this pass exists to avoid;
//   - a framing already over the ceiling (Budget.FramingCeiling) keeps the
//     rows rather than cutting them for nothing, and the question is asked
//     ONCE, from the framing as it stands with every row cut;
//   - with keepLastRow, the last row is never given up (the search envelope's
//     rule: an answer that had rows keeps its one row whole, over the budget,
//     with a notice saying so).
//
// The rows are the caller's, in the order the block renders them. measure
// renders the block for the rows it is given and returns its size in bytes;
// allCutFraming returns the size of the framing as it stands with every row
// cut; cut drops the row at an index and records it; giveWay is asked when
// there is no row left to drop and reports whether the caller has something
// else to give way (the search envelope's notes). A nil giveWay means nothing
// else can give way.
//
// It returns the rows that are left and what the rule settled on.
//
// IT IS THE ONE IMPLEMENTATION: two copies of a fit are two fits, and the
// second one is the one nobody updates.
func FitItems(items []Item, budget Budget, measure func([]Item) int, allCutFraming func() int, cut func([]Item, int) []Item, giveWay func() bool, keepLastRow bool) ([]Item, FitOutcome) {
	// Whether the framing alone is the excess, and whether that has been asked
	// yet. It is asked ONCE, on the first over-cap iteration, and never
	// re-asked: the answer does not depend on how many rows have been cut.
	asked, excess := false, false

	for {
		// The block is measured first, and always: the caller's render is what
		// the cap is about, and a block that was never rendered cannot be
		// fitted (nor answered).
		measured := measure(items)
		if budget.MaxBytes <= 0 || measured <= budget.MaxBytes {
			return items, FitFits
		}
		if !asked {
			asked = true
			excess = budget.FramingCeiling > 0 && allCutFraming() >= budget.FramingCeiling
		}
		if excess {
			return items, FitFramingIsTheExcess
		}
		if keepLastRow && len(items) == 1 {
			return items, FitLastRowKept
		}
		if len(items) == 0 {
			// Nothing left to give way: the caller either has something else
			// (the search envelope's notes) or the answer does not fit and
			// never will.
			if giveWay != nil && giveWay() {
				continue
			}
			return items, FitNoRowsLeft
		}
		// The lowest-ranked row, and an UNPINNED one before any pinned one:
		// a pin is a slot guarantee, so the cap takes it last.
		at := len(items) - 1
		if budget.KeepPinned {
			for i := at; i >= 0; i-- {
				if !items[i].Pinned {
					at = i
					break
				}
			}
		}
		items = cut(items, at)
	}
}
