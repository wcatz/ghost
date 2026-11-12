package assemble

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

const ftsRankFloor = 3

// The reason vocabulary. It is closed: a result that does not fit a reason here
// has a bug, not a new cause, and adding one is a contract change rather than a
const (
	reasonNoMemories             = "no_memories"
	reasonNotApplicable          = "not_applicable"
	reasonNoCandidates           = "no_candidates"
	reasonRetrievalFailed        = "retrieval_failed"
	reasonVectorUnavailable      = "vector_backend_unavailable"
	reasonAllInvalid             = "all_invalid"
	reasonAllOutOfCategory       = "all_out_of_category"
	reasonAllOutOfRetention      = "all_out_of_retention"
	reasonAllOutOfScope          = "all_out_of_scope"
	reasonAllDedupDropped        = "all_dedup_dropped"
	reasonNearDuplicate          = "near_duplicate"
	reasonContradictionSeparated = "contradiction_separated"
	reasonDiversityDeferred      = "diversity_deferred"
	reasonDiversityBackfilled    = "diversity_backfilled"
	reasonAllDiversity           = "all_diversity_capped"
	reasonAllOverBudget          = "all_over_budget"
	reasonBelowFloor             = "below_floor"
	reasonFloorMet               = "floor_met"
	reasonNoFloorArm             = "no_floor_arm"
	reasonBudgetDropped          = "response_budget"
	reasonRelevanceCutoff        = "relevance_cutoff"
	reasonLegConsensus           = "leg_consensus"
	reasonNothingClearedBar      = "nothing_cleared_the_bar"
	reasonRetrievalPartial       = "retrieval_partial"
)

func floorsOf(req Request) Floors {
	return Floors{
		FTSRankMax:   ftsRankFloor,
		VectorCosine: req.AbstainCosine,
		VectorArmOn:  req.AbstainCosine > 0,
	}
}

func (p *pipeline) satisfiesFloor(id string, f Floors) bool {
	sig, ok := p.trace.Signals[id]
	if !ok {
		return false
	}
	if sig.FTSRank >= 0 && sig.FTSRank <= f.FTSRankMax {
		return true
	}
	return f.VectorArmOn && sig.VectorScore >= 0 && sig.VectorScore >= float64(f.VectorCosine)
}

func (p *pipeline) legInPlay(name string) bool {
	leg := p.set.Legs[name]
	return leg.Applicable && leg.Attempted && leg.Available
}

func (p *pipeline) vectorLegInPlay() bool { return p.legInPlay("vector") }

func (p *pipeline) legFailed(name string) bool {
	leg := p.set.Legs[name]
	return leg.Applicable && leg.Attempted && !leg.Available
}

func (p *pipeline) legNeverRan(name string) bool {
	leg := p.set.Legs[name]
	return leg.Applicable && !leg.Attempted
}

func (p *pipeline) retrievalPartial() bool {
	return p.legFailed("fts") || p.legFailed("vector")
}

func (p *pipeline) emptyReason() string {
	if len(p.set.Rows) > 0 {
		if p.droppedBy[stageResponseFit] > 0 {
			return reasonAllOverBudget
		}
		return p.removalReason()
	}
	if p.set.ValidityExcluded > 0 {
		return reasonAllInvalid
	}
	if p.legNeverRan("vector") && !p.legFailed("fts") &&
		(p.passive || memory.HasFTSTerm(p.req.Query)) {
		return reasonVectorUnavailable
	}
	if p.legFailed("fts") || p.legFailed("vector") {
		return reasonRetrievalFailed
	}
	if p.passive {
		return reasonNoMemories
	}
	return reasonNoCandidates
}

func (p *pipeline) removalReason() string {
	if p.noAnswer != nil && p.droppedBy[stageBudget] == 0 {
		return reasonNothingClearedBar
	}
	stage, reason := p.dominantRemoval()
	if stage == "" {
		return reasonNoCandidates
	}
	return reason
}

func (p *pipeline) dominantRemoval() (string, string) {
	type cause struct {
		stage, reason string
		count         int
	}
	candidates := []cause{
		{stage: stageValidity, reason: reasonAllInvalid},
		{stage: stagePredicates, reason: p.predicateReason()},
		{stage: stageDedup, reason: reasonAllDedupDropped},
		{stage: stageDiversity, reason: reasonAllDiversity},
		{stage: stageBudget, reason: reasonAllOverBudget},
	}
	best := cause{}
	for _, c := range candidates {
		c.count = p.droppedBy[c.stage]
		if c.count > best.count {
			best = c
		}
	}
	if best.reason == "" {
		return "", ""
	}
	return best.stage, best.reason
}

func (p *pipeline) predicateReason() string {
	switch {
	case p.droppedBy[dropCategory] > 0 && p.droppedBy[dropCategory] >= p.droppedBy[dropRetention] && p.droppedBy[dropCategory] >= p.droppedBy[dropScope]:
		return reasonAllOutOfCategory
	case p.droppedBy[dropRetention] > 0 && p.droppedBy[dropRetention] >= p.droppedBy[dropScope]:
		return reasonAllOutOfRetention
	case p.droppedBy[dropScope] > 0:
		return reasonAllOutOfScope
	}
	return ""
}

func (p *pipeline) armValues() (ftsApplied, vectorApplied bool) {
	for _, it := range p.items {
		sig, ok := p.trace.Signals[it.ID]
		if !ok {
			continue
		}
		if sig.FTSRank >= 0 {
			ftsApplied = true
		}
		if sig.VectorScore != -1 {
			vectorApplied = true
		}
	}
	return ftsApplied, vectorApplied
}

func (p *pipeline) verdict() (Outcome, string) {
	if len(p.items) == 0 {
		p.trace.Floors.FTSApplied = false
		return OutcomeEmpty, p.emptyReason()
	}
	if p.retrievalPartial() {
		return OutcomeAnswerable, reasonRetrievalPartial
	}
	ftsApplied, vectorValue := p.armValues()
	p.trace.Floors.FTSApplied = ftsApplied
	if p.passive {
		p.trace.Floors.FTSApplied = false
		p.trace.Floors.VectorApplied = false
		return OutcomeAnswerable, reasonNotApplicable
	}
	if !ftsApplied && (!p.trace.Floors.VectorArmOn || !vectorValue) {
		return OutcomeAnswerable, reasonNoFloorArm
	}
	for _, it := range p.items {
		if p.satisfiesFloor(it.ID, p.trace.Floors) {
			return OutcomeAnswerable, reasonFloorMet
		}
	}
	return OutcomeWeak, reasonBelowFloor
}

func (p *pipeline) absenceNote() string {
	var advice []string
	if p.req.Budget.MaxItems > 0 {
		advice = append(advice, "widen the limit")
	}
	if len(p.req.Scope) > 0 {
		advice = append(advice, "drop the scope filter")
	}
	if p.req.Category != "" {
		advice = append(advice, "drop the category filter")
	}
	if p.req.Retention != "" {
		advice = append(advice, "drop the retention filter")
	}
	note := "Ghost memory: no match within the searched window"
	if len(advice) > 0 {
		note += " \u2014 " + strings.Join(advice, " or ")
	}
	return note + ". This is not evidence that nothing exists."
}

func (p *pipeline) coverageComplete() bool {
	for _, name := range []string{"fts", "vector"} {
		leg := p.set.Legs[name]
		if !leg.Applicable {
			continue
		}
		if name == "vector" && !leg.Attempted && !p.passive &&
			!memory.HasFTSTerm(p.req.Query) {
			continue
		}
		if !leg.Attempted || !leg.Available || leg.Truncated || !leg.CoverageComplete {
			return false
		}
	}
	return true
}

const perStageNote = "The note below breaks the removals down per stage."

func (p *pipeline) breakdownNote() string {
	if !p.breakdownLeads() {
		return ""
	}
	return formatNote("%d candidate rows were removed and none reached the answer: %s",
		len(p.dropped), p.removalBreakdown())
}

func (p *pipeline) stageNote() string {
	if !p.breakdownLeads() {
		return ""
	}
	notes := p.notes()
	if len(notes) == 0 || notes[0] != p.breakdownNote() {
		return ""
	}
	return " " + perStageNote
}

func (p *pipeline) abstention(outcome Outcome, reason string) string {
	switch outcome {
	case OutcomeWeak:
		return "These matches are weak: no returned row cleared the relevance floor, so do not rely on them for " +
			"this question — verify against the source before acting."
	case OutcomeEmpty:
		switch reason {
		case reasonNoCandidates:
			if !p.coverageComplete() {
				return p.absenceNote()
			}
			return "No matching memories found."
		case reasonRetrievalFailed:
			return "This search is incomplete: a retrieval leg failed, so it is unknown whether anything matches. " +
				"The answer is not an absence — retry, or read the log."
		case reasonVectorUnavailable:
			return "This search is incomplete: the vector leg could not run, so only the keyword leg ran and " +
				"it may be less complete than a hybrid one. The query was not wrong — the answer is " +
				"incomplete, not absent."
		case reasonNoMemories:
			return "No memories in the over-fetched window: the rows this block was assembled from came back empty, " +
				"so nothing is injected. That describes the window, not the store — a store with memories behind a " +
				"narrower scope, an expired validity window or a category this session did not read would look the same."
		case reasonAllInvalid:
			if p.passive {
				return passiveAllInvalidSentence
			}
			return "No sufficiently trustworthy memory found: the candidates this search found were withheld as out " +
				"of date, their validity windows having closed or not yet opened." + p.stageNote() + " The query was " +
				"not wrong — the answer is withheld, not absent."
		case reasonNothingClearedBar:
			return fmt.Sprintf("No memory answers this: nothing cleared the bar. The best vector match scored cosine "+
				"%.3f, below the %.3f no-answer bar (context.no_answer_cosine), so the %d candidate(s) found "+
				"were withheld rather than shown. The query was not wrong \u2014 the store holds nothing close "+
				"enough to answer it; set the bar to 0 to see the rows anyway.%s",
				p.noAnswer.top, p.noAnswer.bar, p.noAnswer.withheld, p.stageNote())
		case reasonAllOutOfCategory:
			return "No sufficiently trustworthy memory found: nothing found passed the category filter." +
				p.stageNote()
		case reasonAllOutOfRetention:
			return "No sufficiently trustworthy memory found: nothing found was in the requested retention tier " +
				"(" + p.req.Retention + ")." + p.stageNote()
		case reasonAllOutOfScope:
			if p.passive {
				return "No sufficiently trustworthy memory found: nothing found matched the requested scope."
			}
			return "No sufficiently trustworthy memory found: nothing found matched the requested scope." +
				p.stageNote()
		case reasonAllOverBudget:
			if p.droppedBy[stageResponseFit] > 0 {
				return fmt.Sprintf("No sufficiently trustworthy memory found: the candidates this search found "+
					"were cut by the response budget of %d bytes, which no search argument changes \u2014 ask for "+
					"fewer results.%s", p.req.Budget.MaxBytes, p.stageNote())
			}
			if p.dominantBudgetBound() == boundSliceBytes {
				return "No sufficiently trustworthy memory found: a per-bucket CONTENT byte cap cut the " +
					"candidates this search found. Raising the row limit would not help \u2014 it admits rows this " +
					"cap cuts again \u2014 so raise the bucket's byte cap, or ask for shorter memories." +
					p.stageNote()
			}
			return "No sufficiently trustworthy memory found: the item budget cut the candidates this search " +
				"found. Raise the limit to see them." + p.stageNote()
		}
		return "No sufficiently trustworthy memory found: every candidate this search found was removed before the " +
			"answer was assembled. " + p.removalBreakdown() + "."
	}
	return ""
}

func (p *pipeline) dominantBudgetBound() string {
	best, bestN := "", 0
	for _, bound := range []string{boundSliceItems, boundSliceBytes, boundTotalItems} {
		if n := p.droppedByBound[bound]; n > bestN || (n == bestN && n > 0 && bound == boundSliceBytes) {
			best, bestN = bound, n
		}
	}
	return best
}

func (p *pipeline) qualifierBlock() string {
	if len(p.qualifiers) == 0 {
		return ""
	}
	return strings.Join(p.qualifiers, "\n") + "\n\n"
}

func cosineField(v float32) string {
	if fixed := strconv.FormatFloat(float64(v), 'f', 3, 32); fixed != "0.000" || v == 0 {
		return fixed
	}
	return strconv.FormatFloat(float64(v), 'g', -1, 32)
}

func (p *pipeline) machineLine(outcome Outcome, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[ghost:outcome=%s reason=%s", outcome, reason)
	if p.trace.Floors.FTSApplied {
		fmt.Fprintf(&b, " floor_fts_rank=%d", p.trace.Floors.FTSRankMax)
	} else {
		b.WriteString(" floor_fts_rank=not_applied")
	}
	switch {
	case p.passive:
		b.WriteString(" abstain_cosine=not_applied")
	case p.trace.Floors.VectorApplied:
		fmt.Fprintf(&b, " abstain_cosine=%s", cosineField(p.trace.Floors.VectorCosine))
	case p.trace.Floors.VectorArmOn:
		b.WriteString(" abstain_cosine=not_applied")
	default:
		b.WriteString(" abstain_cosine=off")
	}
	fmt.Fprintf(&b, " candidates=%d admitted=%d legs=%s tokens_est=%d",
		len(p.set.Rows), len(p.items), p.legSummary(), totalTokens(p.items))
	if p.retrievalPartial() {
		b.WriteString(" retrieval_partial")
	}
	b.WriteString("]")
	return b.String()
}

func (p *pipeline) legSummary() string {
	parts := make([]string, 0, 2)
	for _, name := range []string{"fts", "vector"} {
		leg := p.set.Legs[name]
		state := "absent"
		switch {
		case leg.Applicable && leg.Attempted && leg.Available:
			state = "ok"
		case leg.Applicable && leg.Attempted:
			state = "failed"
		case leg.Applicable:
			state = "not_run"
		}
		parts = append(parts, name+":"+state)
	}
	return strings.Join(parts, ",")
}

func totalTokens(items []Item) int {
	n := 0
	for _, it := range items {
		n += it.Tokens
	}
	return n
}

func filterCaveat(category, retention string, scope map[string]string) string {
	var filters []string
	if category != "" {
		filters = append(filters, "category")
	}
	if retention != "" {
		filters = append(filters, "retention")
	}
	if len(scope) > 0 {
		filters = append(filters, "scope")
	}
	if len(filters) == 0 {
		return ""
	}

	which := "the " + filters[0] + " filter"
	verb := "was"
	if len(filters) > 1 {
		which = "the " + strings.Join(filters, " and ") + " filters"
		verb = "were"
	}
	next := "raise the limit"
	if category != "" || retention != "" {
		next += " or use ghost_memories_list for exhaustive browsing"
	}
	return "(Note: " + which + " " + verb + " applied to a finite search window, so further matches may exist beyond the retrieved candidates — " + next + ".)"
}

func assemblerNotes(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	for _, note := range notes {
		b.WriteString("\n(Note: ")
		b.WriteString(note)
		b.WriteString(")")
	}
	return b.String()
}

func (p *pipeline) failedLegs() string {
	var failed []string
	for _, name := range []string{"fts", "vector"} {
		if p.legFailed(name) {
			failed = append(failed, name+" leg: "+p.set.Legs[name].Err)
		}
	}
	return strings.Join(failed, "; ")
}

func (p *pipeline) response(res Result) string {
	var b strings.Builder
	b.WriteString(p.qualifierBlock())
	switch {
	case len(res.Items) == 0:
		b.WriteString(res.Abstention)
		b.WriteString(p.filterCaveat())
		b.WriteString(assemblerNotes(res.Notes))
	case res.Outcome == OutcomeWeak:
		for _, it := range res.Items {
			b.WriteString(it.Line())
			b.WriteString("\n")
		}
		b.WriteString("\n")
		b.WriteString(res.Abstention)
		b.WriteString(p.filterCaveat())
	default:
		for _, it := range res.Items {
			b.WriteString(it.Line())
			b.WriteString("\n")
		}
		if legs := p.failedLegs(); legs != "" {
			b.WriteString("\nWarning: this answer is incomplete — one retrieval leg failed, so matches it would have found are missing (")
			b.WriteString(legs)
			b.WriteString(").\n")
		}
		b.WriteString(p.filterCaveat())
	}
	b.WriteString(p.responseCutNotice())
	if res.Machine != "" {
		b.WriteString("\n" + res.Machine + "\n")
	}
	return b.String()
}

// CutNotice is the parenthesised sentence every response the byte budget cuts
// ends its body with — the last line of a listing, and for a search answer the
func CutNotice(cut, budget int, advice string) string {
	verb := "rows were"
	if cut == 1 {
		verb = "row was"
	}
	return fmt.Sprintf("(%d %s cut from the end of this answer to stay within the %d-byte response "+
		"budget, which bounds the answer and not what is stored. %s)", cut, verb, budget, advice)
}

const searchFitAdvice = "Ask for fewer results (a lower `limit`) or narrow the query to see the rest."

func (p *pipeline) responseCutNotice() string {
	if p.passive || len(p.items) == 0 {
		return ""
	}
	cut := p.droppedBy[stageResponseFit]
	if p.firstKept {
		return "\n\n" + p.firstKeptNotice()
	}
	if cut <= 0 {
		return ""
	}
	return "\n\n" + CutNotice(cut, p.req.Budget.MaxBytes, searchFitAdvice)
}

// FirstKept describes the one over-budget answer a byte budget still returns:
// its protected row — the first row, or the first pinned row — is kept whole
type FirstKept struct {
	Cut      int    // rows cut before the protected one
	Budget   int    // the response budget the answer was fitted to
	Which    string // "first row" or "first pinned row"
	Where    string // where the cut rows came from, e.g. "from the end of this answer,"
	Tool     string // the kind of call, e.g. "search" or "listing"
	RowAlone bool
}

// FirstKeptNotice is the parenthesised sentence for a FirstKept answer. It is
// exported for the reason CutNotice is: the assembler's response-fit post-pass
func FirstKeptNotice(k FirstKept) string {
	lead := "The " + k.Which
	if k.Cut > 0 {
		verb := "rows were"
		if k.Cut == 1 {
			verb = "row was"
		}
		lead = fmt.Sprintf("%d %s cut %s and the %s", k.Cut, verb, k.Where, k.Which)
	}
	if k.RowAlone {
		return fmt.Sprintf("(%s alone is larger than the %d-byte response budget, so it is shown in full "+
			"rather than dropped; no %s argument makes one row smaller.)", lead, k.Budget, k.Tool)
	}
	return fmt.Sprintf("(%s fits the %d-byte response budget, but not with the rest of this answer around "+
		"it, so it is shown in full over the budget rather than dropped.)", lead, k.Budget)
}

func (p *pipeline) firstKeptNotice() string {
	return FirstKeptNotice(FirstKept{
		Cut:      p.droppedBy[stageResponseFit],
		Budget:   p.req.Budget.MaxBytes,
		Which:    "first row",
		Where:    "from the end of this answer,",
		Tool:     "search",
		RowAlone: len(p.items) > 0 && len(p.items[0].Line()) > p.req.Budget.MaxBytes,
	})
}

func (p *pipeline) filterCaveat() string {
	if len(p.items) > 0 && (p.req.Budget.MaxItems <= 0 || len(p.items) >= p.req.Budget.MaxItems) {
		return ""
	}
	if p.droppedBy[stageResponseFit] > 0 {
		return ""
	}
	caveat := filterCaveat(p.req.Category, p.req.Retention, p.req.Scope)
	if caveat == "" {
		return ""
	}
	return "\n\n" + caveat
}

func (p *pipeline) fitResponse(base Result) (Result, error) {
	p.trace.Floors.VectorApplied = p.trace.Floors.VectorArmOn && p.vectorLegInPlay()

	in := len(p.items)
	res := base
	dropped := []string(nil)
	var fitNotes []string
	prev := p.notes()
	var impossible bool

	measure := func(items []Item) int {
		p.markConflicts()
		p.markSuperseded()
		p.markMaybeReplaced()
		res.Items = p.items
		res.Notes = p.notes()
		res.Tokens = totalTokens(p.items)
		res.Outcome, res.Reason = p.verdict()
		res.Abstention = p.abstention(res.Outcome, res.Reason)
		res.Machine = p.machineLine(res.Outcome, res.Reason)
		res.Response = p.response(res)
		res.Bytes = len(res.Response)

		fitNotes = append(fitNotes, vanished(prev, res.Notes)...)
		prev = res.Notes
		measured := res.Bytes
		if p.req.Budget.Measure != nil {
			measured = p.req.Budget.Measure(items, p.trace)
		}
		return measured
	}

	cut := func(items []Item, at int) []Item {
		lowest := p.items[at]
		p.items = append(p.items[:at:at], p.items[at+1:]...)
		if len(p.rows) > at {
			p.rows = append(p.rows[:at:at], p.rows[at+1:]...)
		}
		p.dropped[lowest.ID] = reasonBudgetDropped
		p.droppedBy[stageResponseFit]++
		p.trace.decide(lowest.ID, lowest.ProjectID, stageResponseFit, reasonBudgetDropped, lowest.Score)
		dropped = append(dropped, lowest.ID)
		return p.items
	}

	giveWay := func() bool {
		switch {
		case p.req.Budget.Measure != nil:
			return false
		case len(res.Notes) > 0:
			p.noteCut++
			return true
		default:
			impossible = true
			return false
		}
	}

	kept, outcome := FitItems(p.items, p.req.Budget, measure, p.allCutFraming, cut, giveWay, p.req.Budget.Measure == nil)
	p.items = kept

	switch outcome {
	case FitFramingIsTheExcess:
		p.trace.record(stageResponseFit, in, len(p.items), dropped, fitNotes...)
		p.trace.Notes = res.Notes
		return res, nil
	case FitLastRowKept:
		p.firstKept = true
		res.Response = p.response(res)
		res.Bytes = len(res.Response)
		p.trace.record(stageResponseFit, in, len(p.items), dropped, fitNotes...)
		p.trace.Notes = res.Notes
		return res, nil
	case FitNoRowsLeft:
		if impossible {
			return Result{}, ErrResponseBudgetExceeded
		}
		p.trace.record(stageResponseFit, in, len(p.items), dropped, fitNotes...)
		p.trace.Notes = res.Notes
		return res, nil
	}

	p.trace.record(stageResponseFit, in, len(p.items), dropped, fitNotes...)
	p.trace.Notes = res.Notes
	return res, nil
}

func (p *pipeline) allCutFraming() int {
	if p.req.Budget.Measure == nil || p.req.Budget.FramingCeiling <= 0 || len(p.items) == 0 {
		return 0
	}
	return p.req.Budget.Measure(nil, p.allCutTrace())
}

func (p *pipeline) allCutTrace() *Trace {
	t := *p.trace
	t.Decisions = append(make([]Decision, 0, len(p.trace.Decisions)+len(p.items)), p.trace.Decisions...)
	for _, it := range p.items {
		t.decide(it.ID, it.ProjectID, stageResponseFit, reasonBudgetDropped, it.Score)
	}
	return &t
}

func vanished(before, after []string) []string {
	if len(before) == 0 {
		return nil
	}
	kept := make(map[string]bool, len(after))
	for _, n := range after {
		kept[n] = true
	}
	var gone []string
	for _, n := range before {
		if !kept[n] {
			gone = append(gone, n)
		}
	}
	return gone
}
