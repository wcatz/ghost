package memory

import (
	"sort"
	"time"
)

// searchTrace is the per-candidate record the ranking path writes as it ranks.
//
// It exists so explain mode can report what the ranking DID rather than
// recompute what it should have done. Before it, explain rebuilt the fused
// score by summing the leg weights itself, rebuilt the status factor from the
// hydrated row, rebuilt the scope verdict with a second ScopeMatches call, and
// rebuilt the window-scoped penalties with a second copy of both queries. Every
// one of those is a parallel re-derivation: it agrees with the ranking only for
// as long as nobody edits the ranking, and the day someone does, an agent
// debugging a bad result is handed a diagnosis of a search that did not happen.
//
// The trace is opt-in and rides on SearchParams, which every ranking stage
// already receives. A nil trace costs one nil check per stage and allocates
// nothing, so the production path is unchanged byte for byte — the difference
// this buys is that explain's numbers and the ranking's numbers are now the
// same numbers, not two computations that happen to match today.
//
// It is deliberately NOT a second decision path. Nothing reads the trace to
// decide anything; only explain reads it, and only to report.
type searchTrace struct {
	// now is the clock the ranking ordered by. Explain reads a row's age and
	// decay against THIS instant rather than its own, because two clocks in one
	// explanation is one more way for it to describe a search nobody ran.
	now time.Time
	// scopeKeys is the requested scope's key set, sorted, as the narrowing
	// compared it. Sorted because a reader (and a test) comparing two
	// explanations should not have to care about map order.
	scopeKeys []string
	// rows is keyed by memory id. A row is absent from it only when the
	// ranking path never scored it — the vector floor removed it first, which
	// is a recorded outcome, not a missing one.
	rows map[string]*tracedCandidate
}

// tracedCandidate is one candidate's standing as the ranking path left it.
type tracedCandidate struct {
	// FTSRank and VectorRank are 0-based within their leg, and -1 when the leg
	// did not retrieve the row. Rank 0 is a real first place, so an absent leg
	// must not look like one.
	FTSRank, VectorRank int
	// VectorScore is the cosine the vector leg measured, or -1 when that leg did
	// not score the row.
	VectorScore float64
	// Base is the fused score before status demotion: the RRF sum of the two
	// legs' weighted rank terms. Score is Base after the status factor, which is
	// the number window selection actually cut on.
	Base, Score float64
	// StatusFactor is the multiplicative resolved / project-scoped-_global
	// demotion the fusion applied to Base. 1.0 means it applied none.
	StatusFactor float64
	// ProjectMatch is whether the row belongs to the searched project. A
	// _global row is admitted by the leg's project predicate and is NOT a match:
	// it is a shared row, and StatusFactor is what demotes it.
	ProjectMatch bool
	// RowProject is the project the row belongs to, as the leg supplied it.
	RowProject string
	// ScopeMatched is the verdict scope narrowing reached for this candidate —
	// including for the candidates it dropped, which is the only way an
	// explanation can say WHY one was dropped by scope.
	//
	// It defaults TRUE, because ScopeMatches is vacuously true for an empty
	// request: with no scope asked for, nothing was excluded, and scopeEligiblePool
	// short-circuits before it examines a single candidate. Narrowing overwrites
	// it for every candidate whenever a scope WAS requested, so the default is
	// only ever read in the case where the true answer is "matched".
	ScopeMatched bool
	// KeywordReserved says the row was admitted by the keyword reservation
	// rather than by the score cut. TookSlotFrom names the row it displaced.
	KeywordReserved bool
	TookSlotFrom    string
	// DisplacedBy names the reserved row that took this row's window slot.
	DisplacedBy string
	// FloorDropped says the vector similarity floor removed the row before
	// fusion ever scored it, and FloorScore is the cosine that did it.
	FloorDropped bool
	FloorScore   float64
	// Decay and AgeDays are recorded for the rows decayRank ordered by. A row
	// the window cut carries the factor the path WOULD have applied at the
	// same clock, which is a statement about the row and not about a decision
	// (see explain.go).
	Decay, AgeDays float64
	// SupersedePenalty and NearDuplicatePenalty are the window-scoped counts
	// demoteResults applied, and SupersededBy / NearDuplicateOf name the
	// counterpart behind each. A count alone says the row moved; the counterpart
	// is the row a reader has to look at next, and the two are recorded from the
	// same edge set so the id named is the id that decided it.
	//
	// Both counts are 0 for a window row nothing demoted — the historical
	// reading of these fields, and 0 for a row outside the window too, so the
	// distinction is carried by the id lists rather than by a sentinel the
	// existing payload shape does not have room for.
	SupersedePenalty     int
	NearDuplicatePenalty int
	SupersededBy         []string
	NearDuplicateOf      []string
}

// newSearchTrace starts a trace and records the scope keys the narrowing will
// compare, so the trace is complete before any candidate is scored.
func newSearchTrace(scope map[string]string) *searchTrace {
	tr := &searchTrace{rows: map[string]*tracedCandidate{}}
	for k := range scope {
		tr.scopeKeys = append(tr.scopeKeys, k)
	}
	sort.Strings(tr.scopeKeys)
	return tr
}

// row returns the trace entry for id, creating it with the sentinels for
// "this leg did not retrieve the row" (-1), "no demotion was applied" (1.0)
// and "no scope contradicted this row" (true).
func (tr *searchTrace) row(id string) *tracedCandidate {
	if tr == nil {
		return nil
	}
	c, ok := tr.rows[id]
	if !ok {
		c = &tracedCandidate{
			FTSRank: -1, VectorRank: -1, VectorScore: -1,
			StatusFactor: 1.0, ScopeMatched: true,
		}
		tr.rows[id] = c
	}
	return c
}

// lookup returns the recorded entry for id, and whether one exists. A missing
// entry is a real outcome — the row never reached fusion — and callers must
// distinguish it from a recorded zero.
func (tr *searchTrace) lookup(id string) (*tracedCandidate, bool) {
	if tr == nil {
		return nil, false
	}
	c, ok := tr.rows[id]
	return c, ok
}

// clampScopeKeys bounds how many scope keys one explanation may name. The keys
// come from a caller, so the list is as unbounded as the scope object; the
// comparison itself is what the ranking used, and this only bounds how much of
// it is rendered. It mirrors the attribution cap: a list is cut with a count
// beside it, never silently.
func clampScopeKeys(keys []string) []string {
	const maxScopeKeys = 16
	if len(keys) <= maxScopeKeys {
		return keys
	}
	return keys[:maxScopeKeys]
}

// clampAttribution bounds how many counterpart ids one row may name. A
// near-duplicate cluster in a large store can name hundreds, and a supersede
// chain a dozen; the count beside the list already says how many there are, so
// cutting the rendered list loses no fact.
func clampAttribution(ids []string) []string {
	const maxAttributed = 8
	if len(ids) <= maxAttributed {
		return ids
	}
	return ids[:maxAttributed]
}
