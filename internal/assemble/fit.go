package assemble

// FitOutcome describes how the fit rule settled.
type FitOutcome int

const (
	FitFits FitOutcome = iota
	FitFramingIsTheExcess
	FitLastRowKept
	FitNoRowsLeft
)

// FitItems fits a block of rows to Budget.MaxBytes using the caller's measure, framing, and cut callbacks.
func FitItems(items []Item, budget Budget, measure func([]Item) int, allCutFraming func() int, cut func([]Item, int) []Item, giveWay func() bool, keepLastRow bool) ([]Item, FitOutcome) {
	asked, excess := false, false

	for {
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
			if giveWay != nil && giveWay() {
				continue
			}
			return items, FitNoRowsLeft
		}
		at := fitCutIndex(items, budget)
		if at < 0 {
			return items, FitNoRowsLeft
		}
		items = cut(items, at)
	}
}

func fitCutIndex(items []Item, budget Budget) int {
	r := reserves{
		all: budget.ReserveByProject, allCounts: reserveCounts(items, budget.ReserveByProject, false),
		pinned: budget.ReservePinnedByProject, pinnedCounts: reserveCounts(items, budget.ReservePinnedByProject, true),
	}
	for _, respect := range []bool{true, false} {
		rr := r
		if !respect {
			rr = reserves{}
		}
		if budget.KeepPinned {
			if at := findCutCandidate(items, rr, false); at >= 0 {
				return at
			}
		}
		if at := findCutCandidate(items, rr, true); at >= 0 {
			return at
		}
	}
	return -1
}

type reserves struct {
	all, allCounts       map[string]int
	pinned, pinnedCounts map[string]int
}

func (r reserves) protects(it Item) bool {
	if min, ok := r.all[it.ProjectID]; ok && r.allCounts[it.ProjectID] <= min {
		return true
	}
	if it.Pinned {
		if min, ok := r.pinned[it.ProjectID]; ok && r.pinnedCounts[it.ProjectID] <= min {
			return true
		}
	}
	return false
}

func reserveCounts(items []Item, reserve map[string]int, pinnedOnly bool) map[string]int {
	if len(reserve) == 0 {
		return nil
	}
	counts := make(map[string]int, len(reserve))
	for _, it := range items {
		if pinnedOnly && !it.Pinned {
			continue
		}
		if _, ok := reserve[it.ProjectID]; ok {
			counts[it.ProjectID]++
		}
	}
	return counts
}

func findCutCandidate(items []Item, r reserves, allowPinned bool) int {
	for i := len(items) - 1; i >= 0; i-- {
		if !allowPinned && items[i].Pinned {
			continue
		}
		if r.protects(items[i]) {
			continue
		}
		return i
	}
	return -1
}
