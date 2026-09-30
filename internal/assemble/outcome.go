package assemble

import (
	"fmt"
	"strconv"
	"strings"
)

// Decision 3: abstention is an outcome, not an empty list. Every assembled block
// carries a verdict — answerable, weak or empty — with a reason from a closed
// vocabulary, because "nothing resembles this question", "something was found
// and withheld" and "the best match is weak" are three different instructions
// and a bare empty list renders all three as the same sentence.
//
// The verdict is derived from the trace and the admitted rows. It is never a
// second query: a relevance judgement that re-ran retrieval would be a ranking
// path the stages do not describe, and it could disagree with the one they did.

// ftsRankFloor is Arm A: the worst keyword rank that still counts as a strong
// match. Rank 0 is the best the keyword leg found, so 0..3 covers the head of
// the result and nothing behind it. It is a BM25 rank and not an RRF score
// because rank stays meaningful when the retrieval depth changes, while a fused
// score is a function of the window's size.
const ftsRankFloor = 3

// The reason vocabulary. It is closed: a result that does not fit a reason here
// has a bug, not a new cause, and adding one is a contract change rather than a
// patch. The empty reasons name a stage; the non-empty ones name the state of
// the floor.
const (
	// Empty results, named by the stage that emptied the set.
	// reasonNoMemories is the passive empty reason, and it is deliberately NOT a
	// synonym for no_candidates. A passive retrieval read an over-fetched window
	// and found nothing in it; it never counted the store, so it cannot say the
	// store holds no memories. The sentence says what was actually observed.
	reasonNoMemories = "no_memories"
	// reasonNotApplicable is the answerable reason for a passive block: the honest
	// report of a result that was never judged against a floor, and distinct from
	// reasonNoFloorArm on purpose — that one says an arm held a value and the floor
	// was never configured, while this one says there was no query to be relevant
	// to and therefore no arm to hold anything.
	reasonNotApplicable     = "not_applicable"
	reasonNoCandidates      = "no_candidates"
	reasonRetrievalFailed   = "retrieval_failed"
	reasonVectorUnavailable = "vector_backend_unavailable"
	reasonAllInvalid        = "all_invalid"
	reasonAllOutOfCategory  = "all_out_of_category"
	// reasonAllOutOfRetention is the third stage-3 verdict, added with the tier
	// filter itself. The vocabulary above is closed on purpose, so adding one is a
	// contract change rather than a patch: a caller branching on the reason now has
	// a value it has never seen. That is the cheaper of the two failures — the
	// alternative was folding a tier mismatch into all_out_of_category, which
	// would tell a reader who filtered by retention to go and change their
	// category.
	reasonAllOutOfRetention = "all_out_of_retention"
	reasonAllOutOfScope     = "all_out_of_scope"
	reasonAllDedupDropped   = "all_dedup_dropped"
	reasonAllDiversity      = "all_diversity_capped"
	reasonAllOverBudget     = "all_over_budget"
	// Results that admitted rows.
	reasonBelowFloor    = "below_floor"
	reasonFloorMet      = "floor_met"
	reasonNoFloorArm    = "no_floor_arm"
	reasonBudgetDropped = "response_budget"
	// reasonRetrievalPartial is the answerable verdict when a leg failed. It is
	// the same token the machine line's modifier uses, and the two are not
	// redundant: the reason says why the answer is answerable (no floor verdict
	// was possible), while the modifier is a property of the retrieval that stays
	// true whatever the outcome — an empty result whose rows were all excluded
	// carries the stage reason, and only the modifier says the retrieval was
	// also incomplete.
	reasonRetrievalPartial = "retrieval_partial"
)

// floors are the thresholds this request will judge against. Arm A is always
// on; Arm B is the caller's cosine and is off unless it is set, because a
// threshold nobody measured must not be shipped as a default.
func floorsOf(req Request) Floors {
	return Floors{
		FTSRankMax:   ftsRankFloor,
		VectorCosine: req.AbstainCosine,
		VectorArmOn:  req.AbstainCosine > 0,
	}
}

// satisfiesFloor reports whether one admitted row clears an arm.
//
// The vector leg's ATTEMPT, not its result count, decides whether the vector arm
// is part of the judgement: a leg that ran and returned nothing is exactly the
// case the cosine arm exists to detect, so both arms apply. A leg that never ran
// leaves nothing to detect with, which is why the caller gates the whole floor
// on vectorLegInPlay rather than reading a per-row score.
func (p *pipeline) satisfiesFloor(id string, f Floors) bool {
	sig, ok := p.trace.Signals[id]
	if !ok {
		return false
	}
	if sig.FTSRank >= 0 && sig.FTSRank <= f.FTSRankMax {
		return true
	}
	// A score is only comparable when the vector leg actually produced one. The -1
	// sentinel means the leg did not retrieve the row, and a threshold applied to
	// it is a comparison nobody made: `0 >= 0.6` is false for a reason that has
	// nothing to do with how similar the memory is.
	return f.VectorArmOn && sig.VectorScore >= 0 && sig.VectorScore >= float64(f.VectorCosine)
}

// legInPlay reports whether a leg could have judged this result: it was
// applicable, it ran, and it answered. Everything else — never applicable, never
// attempted, or errored — leaves the question it exists to answer unasked, and a
// result cannot be called weak for failing a comparison that was never made.
func (p *pipeline) legInPlay(name string) bool {
	leg := p.set.Legs[name]
	return leg.Applicable && leg.Attempted && leg.Available
}

// vectorLegInPlay is the vector leg's own question: whether a semantic strength
// verdict was possible at all.
func (p *pipeline) vectorLegInPlay() bool { return p.legInPlay("vector") }

// legFailed reports whether an applicable leg ran and could not answer. It is
// what makes a retrieval partial, and the reason a survivor is never
// `below_floor`: no floor verdict can be made from a path that failed.
func (p *pipeline) legFailed(name string) bool {
	leg := p.set.Legs[name]
	return leg.Applicable && leg.Attempted && !leg.Available
}

// legNeverRan reports whether an applicable leg was never attempted, so it
// neither answered nor broke. It is a different fact from legFailed and the
// caller has to keep them apart: a leg that never ran is a machine with nothing
// to run it (no embedder, or a query it could not embed), which is a
// configuration the caller fixes once, while a leg that ran and broke is an
// incident they retry. `memory.Candidates` marks a leg Attempted only when it
// produced a query vector, so the tool's own embed failure lands here too.
func (p *pipeline) legNeverRan(name string) bool {
	leg := p.set.Legs[name]
	return leg.Applicable && !leg.Attempted
}

// retrievalPartial reports whether the answer is built from a partial retrieval.
// It is a modifier on the machine line and a note in the prose, never a reason:
// "this answer may be incomplete" and "this answer is not good enough" are
// different claims, and merging them would let an outage read as a verdict.
func (p *pipeline) retrievalPartial() bool {
	return p.legFailed("fts") || p.legFailed("vector")
}

// emptyReason is the reason for a result that admitted no row. The stage that
// emptied the set names it, and stage 1's own failures are named before any
// stage is consulted: an absent row is only evidence of absence when the search
// that failed to find it actually ran.
func (p *pipeline) emptyReason() string {
	if len(p.set.Rows) > 0 {
		// The response-fit post-pass runs after every stage, so when it removed
		// the last rows it is the cause the caller has to act on: the block was
		// assembled and then did not fit. A stage that removed more rows overall
		// is not what emptied it — naming that one would send the caller to a
		// filter that was never the reason there is no answer.
		if p.droppedBy[stageResponseFit] > 0 {
			return reasonAllOverBudget
		}
		return p.removalReason()
	}
	// The two retrieval reasons are events, not synonyms, and the ORDER of the
	// checks is the whole point. `vector_backend_unavailable` is a machine with no
	// embedder to run the leg — the keyword search really was all that ran, and
	// that is what its sentence says, because the vector leg never started. A leg
	// that started and broke is `retrieval_failed` instead, whose sentence tells
	// the caller to retry: reporting that as a missing backend would tell an
	// operator whose embedder answered that they have no embedder, which is the
	// same conflation `verdict()` refuses further down. A second failed leg makes
	// it the general case, and says so.
	if p.legNeverRan("vector") && !p.legFailed("fts") {
		return reasonVectorUnavailable
	}
	if p.legFailed("fts") || p.legFailed("vector") {
		return reasonRetrievalFailed
	}
	if p.passive {
		// The last line, and the only one the passive path changes. Reaching here
		// means no stage removed anything and no leg fact explains the emptiness, so
		// the window itself is what was empty — and `no_candidates` would claim a
		// QUERY matched nothing, which is a sentence about a search that never ran.
		// A passive empty is `no_memories`.
		//
		// It sits BELOW the two leg checks deliberately, which is what the block
		// above says in its own terms: a stage that removed rows is the cause the
		// caller can act on, and a leg fact is only reached when no stage did. The
		// two leg reasons are real facts on a passive set — a retriever can report a
		// leg applicable-but-not-run — so they are kept, not duplicated above.
		return reasonNoMemories
	}
	return reasonNoCandidates
}

// removalReason is the reason for a set the stages emptied. The reason set is
// closed, so a set emptied by more than one stage carries only one label and it
// has to be the cause that accounts for the rows: a single expired row must not
// claim a set the item budget actually emptied, because the caller's next move
// differs — one looks for a date problem, the other raises the limit.
func (p *pipeline) removalReason() string {
	stage, reason := p.dominantRemoval()
	if stage == "" {
		return reasonNoCandidates
	}
	return reason
}

// dominantRemoval is the stage responsible for most of the removed rows, and
// the reason that goes with it. Stage order breaks ties, so the answer does not
// depend on map iteration. The response-fit post-pass is absent from the list
// because it runs after this and has its own reason: a set it emptied was
// emptied by the byte cap and nothing else.
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

// predicateReason names the stage-3 verdict that removed the rows, or "" when no
// filter is set. The three verdicts are counted apart because they answer
// different questions for the caller: a category that removed nothing while the
// tier removed everything is a tier problem, and a reason naming the category
// sends the reader to change the wrong filter. A tie names the narrowest predicate
// that removed anything, in the order category, retention, scope — the tier is
// narrower than the scope for the same reason the category is, and all three are
// likelier to have been set by accident than deliberately.
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

// armValues reports which arms have a VALUE to compare on this result, not which
// legs ran. A leg that answered can still have retrieved nothing — a semantic
// query sharing no words with the corpus leaves every row at the -1 "this leg did
// not retrieve it" sentinel — and a threshold applied to a sentinel is a
// comparison nobody made, which is how a result with no judgement at all came back
// `weak`/`below_floor` against a floor the corpus never had a chance to miss.
func (p *pipeline) armValues() (ftsApplied, vectorApplied bool) {
	for _, it := range p.items {
		sig, ok := p.trace.Signals[it.ID]
		if !ok {
			continue
		}
		if sig.FTSRank >= 0 {
			ftsApplied = true
		}
		if sig.VectorScore >= 0 {
			vectorApplied = true
		}
	}
	return ftsApplied, vectorApplied
}

// verdict is the outcome and its reason, in that order, because a reason without
// an outcome is a stage's bookkeeping and an outcome without a reason is the
// bare list this replaces.
func (p *pipeline) verdict() (Outcome, string) {
	if len(p.items) == 0 {
		// No row survived, so no arm compared one — and the field has to say so
		// rather than keeping whatever an earlier fit-pass iteration left in it,
		// since the pass re-renders after every drop and a block that ends empty
		// was judged on its last row a moment ago.
		p.trace.Floors.FTSApplied = false
		return OutcomeEmpty, p.emptyReason()
	}
	// A failed leg comes FIRST, and the order is the point. Both checks below
	// suppress the floor for the same reason — no verdict needs every applicable
	// leg's input — but they answer different questions about it. A leg that ran
	// and failed is an INCIDENT and the arm reports nothing, while a leg that
	// never ran is a machine with no embedder, a configuration state.
	// `no_floor_arm` below covers the case where no arm held a value; reporting a
	// vector-leg condition here would be the same conflation in the other
	// direction, telling an operator with a working embedder and a broken search
	// that they have no embedder.
	if p.retrievalPartial() {
		return OutcomeAnswerable, reasonRetrievalPartial
	}
	// Which arms have anything to judge, which is not the same question as which
	// legs ran: a leg that answered and retrieved nothing leaves the sentinel on
	// every row. With neither arm holding a value there is no floor verdict to
	// make in either direction, and `below_floor` would be a claim about the
	// corpus that no comparison supports.
	ftsApplied, vectorValue := p.armValues()
	p.trace.Floors.FTSApplied = ftsApplied
	// A passive block is never `weak`, and the check comes before the floor because
	// the floor never applied to it: there was no query, so neither an FTS rank nor
	// a cosine exists to compare. Reporting `below_floor` would tell the caller its
	// memories were judged and found wanting, which is a claim about a question
	// this surface was never asked.
	//
	// Both arms are cleared, not just the keyword one: `fitResponse` derives
	// VectorApplied from the leg status and the configured arm, so a retriever
	// reporting the vector leg `ok` on a passive request would otherwise leave the
	// trace claiming a cosine applied to a block the machine line is simultaneously
	// reporting `not_applied` for. The CONFIGURED arm and the threshold are left
	// alone, because they are facts about the request rather than about the verdict,
	// and a reader has to be able to tell "not applied" from "not configured".
	if p.passive {
		p.trace.Floors.FTSApplied = false
		p.trace.Floors.VectorApplied = false
		return OutcomeAnswerable, reasonNotApplicable
	}
	if !ftsApplied && (!p.trace.Floors.VectorArmOn || !vectorValue) {
		return OutcomeAnswerable, reasonNoFloorArm
	}
	// The keyword arm is judged on its own merits, with or without a vector leg.
	// Reporting the vector leg's condition in place of a keyword judgement was the
	// same error in the other direction: on a machine with no embedder the top
	// keyword hit is rank 0, so arm A fires and the answer IS judged, and a reason
	// saying it was not tells the caller to distrust a row the assembler cleared.
	// The leg's state is on the line (`legs=`, and `abstain_cosine=not_applied`),
	// which is where a fact about retrieval belongs.
	for _, it := range p.items {
		if p.satisfiesFloor(it.ID, p.trace.Floors) {
			return OutcomeAnswerable, reasonFloorMet
		}
	}
	// Nothing cleared, and the keyword arm held values to compare — so this is a
	// relevance verdict about the corpus, not an artefact of the vector leg being
	// unavailable. It is reported as such either way, and the line says which arm
	// ran.
	return OutcomeWeak, reasonBelowFloor
}

// absenceNote replaces the absence sentence wherever coverage is not complete
// or a leg was cut off at the window. It exists because a windowed search can
// never prove a negative: the leg stopped at the number of rows it was asked
// for, so "nothing matched" is a claim about the window dressed as a claim
// about the store. The note says exactly that.
//
// Its ADVICE names each knob the request actually set and no other, tested the
// same way the rest of the seam tests a scope: a non-nil but EMPTY scope map is
// a filter the request did not set, and it is the shape a JSON `{}` decodes to.
// The item limit is on the shipped tool path in every case (the handler always
// sends one, 10 by default), so the note an unfiltered caller gets advises
// widening that and says nothing about a filter it never passed.
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

// coverageComplete reports whether every applicable leg can support an absence
// claim: it ran, it answered, it was not cut off at the window, and it vouched
// for its own coverage.
//
// The fourth condition is the one that costs, and Run does not work around it:
// `LegStatus.CoverageComplete` stays false on the vector leg until its
// expected/indexed/unembedded counts are reconciled, and a row with no embedding
// is invisible to that leg's own scan, so nothing it read can say anything about
// the rows it never saw. A leg that cannot support a coverage claim must not make
// one. A non-applicable leg is not a failure and is not consulted.
func (p *pipeline) coverageComplete() bool {
	for _, name := range []string{"fts", "vector"} {
		leg := p.set.Legs[name]
		if !leg.Applicable {
			continue
		}
		if !leg.Attempted || !leg.Available || leg.Truncated || !leg.CoverageComplete {
			return false
		}
	}
	return true
}

// perStageNote is the pointer to the per-stage breakdown, and it goes on any
// sentence whose reason is the DOMINANT removal, because a dominant cause is not
// the only one. `dominantRemoval` picks the stage that removed the most rows, so
// two stages can empty a set and the reason names one of them; the breakdown note
// is what makes the sentence checkable, and a reader who stopped at the cause
// would otherwise take one stage's rows as the whole story. It sits directly
// after the cause it qualifies, so the reassurance that may follow still reads
// last. No sentence carrying it claims every row failed the same way.
const perStageNote = "The note below breaks the removals down per stage."

// breakdownNote is the per-stage removal breakdown, which leads the note list
// whenever the block is empty. notes() and stageNote() build it from here so the
// pointer's test and the note it points at cannot disagree about the text.
func (p *pipeline) breakdownNote() string {
	if !p.breakdownLeads() {
		return ""
	}
	return formatNote("%d candidate rows were removed and none reached the answer: %s",
		len(p.dropped), p.removalBreakdown())
}

// stageNote is the pointer above, returned only while the note it names is in
// the answer being rendered. The fit pass is allowed to cut notes and the
// breakdown is the last one it reaches, so under a tight cap an answer can be
// left carrying the sentence with nothing below it — which is the checkability
// the sentence exists to promise, gone while the claim to it remains. The
// pointer leaves with its note; the cause it qualifies does not leave at all,
// because the caller still has to know which cap emptied the set.
//
// The test is whether the note SURVIVED, which is not the same question as how
// many were cut: notes() has already applied the tail cut, so comparing noteCut
// against its length asks whether twice the cut count reached the original
// length, and fires at half the notes — dropping the pointer off an answer that
// still carries the note it promises.
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

// abstention is the human sentence for a non-answerable result. An answerable
// one gets nothing: a caveat on a result that withheld no row teaches the
// caller to discount the whole surface.
//
// The wording separates the three empty cases that look alike and are not. Rows
// that were found and then withheld are not an absence — the query was fine and
// the answer was suppressed — so they say the rows were withheld, not that
// nothing matched. A search whose applicable legs all vouched for their coverage
// may say nothing matched. One whose coverage is incomplete, or whose leg filled
// its window, may not, and gets the window note instead.
func (p *pipeline) abstention(outcome Outcome, reason string) string {
	switch outcome {
	case OutcomeWeak:
		// The rows are still shown: this is a warning about them, not a
		// withholding, and the caller can judge a weak candidate for itself.
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
			// "Could not run", not "was unavailable": the leg status knows the
			// vector leg never started and not why, and a machine that refused one
			// query is not a machine with no embedder. `legs=vector:not_run` and
			// the docs carry the two possible causes.
			//
			// And the incompleteness framing, not the withholding one. This reason
			// is only reachable when NO candidate row came back at all, so "no
			// sufficiently trustworthy memory found" would tell the reader that
			// trustworthy memories were rejected — the exclusion wording its row-less
			// sibling `retrieval_failed` deliberately avoids, and the wording the
			// coverage-gate tests fail a row-less answer for carrying.
			return "This search is incomplete: the vector leg could not run, so only the keyword leg ran and " +
				"it may be less complete than a hybrid one. The query was not wrong — the answer is " +
				"incomplete, not absent."
		case reasonNoMemories:
			// Not "no matching memories found": a passive retrieval matched nothing
			// because nothing was asked, and the block it read is a window, not a
			// census. The sentence names the observation it can actually make, which
			// is the only claim the evidence supports.
			return "No memories in the over-fetched window: the rows this block was assembled from came back empty, " +
				"so nothing is injected. That describes the window, not the store — a store with memories behind a " +
				"narrower scope, an expired validity window or a category this session did not read would look the same."
		case reasonAllInvalid:
			if p.passive {
				// The SHARED exclusion wording is kept, deliberately: an empty result
				// caused by exclusions says "no sufficiently trustworthy memory
				// found" rather than "no matching memories", on every surface, and
				// the distinction is the reason all_invalid is its own reason. What
				// does not survive the move to a queryless surface is the two halves
				// that talk about a SEARCH: a passive block was assembled from a
				// window nobody queried, and the closing clause would tell a reader
				// "the query was not wrong" about a request that made none. Both are
				// rewritten rather than the sentence being replaced, so the phrase
				// every other exclusion reason uses stays the phrase this one uses.
				//
				// And `stageNote()` is DROPPED here, which is the third thing that
				// does not survive. It splices "The note below breaks the removals
				// down per stage", and `Result.Abstention` is bytes a CALLER
				// renders: `p.response` writes `assemblerNotes(res.Notes)` after it,
				// so the search half keeps a true promise, but
				// `ghost_project_context`, the project-context resource and the
				// `recall_project` prompt return this string as the ENTIRE answer
				// and render no notes, so the promise pointed at a breakdown that was
				// not in the payload. The assembler does not own the rendering and
				// cannot keep the promise, so the passive sentence drops it rather
				// than every passive caller being made to render diagnostics to
				// satisfy a sentence. What carries the reason without the notes is
				// the clause above it: "withheld as out of date, their validity
				// windows having closed or not yet opened".
				return "No sufficiently trustworthy memory found: the candidates this block was assembled from were " +
					"withheld as out of date, their validity windows having closed or not yet opened." +
					" The block was not empty before that — the answer is withheld, not absent."
			}
			return "No sufficiently trustworthy memory found: the candidates this search found were withheld as out " +
				"of date, their validity windows having closed or not yet opened." + p.stageNote() + " The query was " +
				"not wrong — the answer is withheld, not absent."
		case reasonAllOutOfCategory:
			return "No sufficiently trustworthy memory found: nothing found passed the category filter." +
				p.stageNote()
		case reasonAllOutOfRetention:
			return "No sufficiently trustworthy memory found: nothing found was in the requested retention tier " +
				"(" + p.req.Retention + ")." + p.stageNote()
		case reasonAllOutOfScope:
			return "No sufficiently trustworthy memory found: nothing found matched the requested scope." +
				p.stageNote()
		case reasonAllOverBudget:
			// Two stages produce this reason and only one of them is the response
			// cap, so the sentence has to say which. Quoting Budget.MaxBytes
			// unconditionally names a budget the caller never set whenever stage 8
			// emptied the set instead (MaxBytes 0), and tells it to fix a filter it
			// never passed.
			if p.droppedBy[stageResponseFit] > 0 {
				// The byte cap is server-side: no tool argument reaches it, so
				// advice to raise a limit would send the caller after a knob it
				// does not have. Naming the number and the action it does have is
				// the whole difference between a next step and a shrug.
				//
				// perStageNote applies here as it does to the other four. This
				// branch is reached whenever the pass emptied the block, which
				// says nothing about what emptied it before: a set validity
				// halved still renders the breakdown note naming both stages, and
				// the sentence above it must not read as the whole story.
				return fmt.Sprintf("No sufficiently trustworthy memory found: the candidates this search found "+
					"were cut by the response budget of %d bytes, which no search argument changes \u2014 ask for "+
					"fewer results.%s", p.req.Budget.MaxBytes, p.stageNote())
			}
			// A content-byte cap is a different remedy from a row count, and the
			// sentence has to name the one that applies: raising the row limit for
			// a byte-capped slice admits rows the byte cap cuts again, so the
			// answer comes back identical and the caller has learned nothing.
			if p.dominantBudgetBound() == boundSliceBytes {
				return "No sufficiently trustworthy memory found: a per-bucket CONTENT byte cap cut the " +
					"candidates this search found. Raising the row limit would not help \u2014 it admits rows this " +
					"cap cuts again \u2014 so raise the bucket's byte cap, or ask for shorter memories." +
					p.stageNote()
			}
			return "No sufficiently trustworthy memory found: the item budget cut the candidates this search " +
				"found. Raise the limit to see them." + p.stageNote()
		}
		// The dedup and diversity reasons have no sentence yet: their stages are
		// pass-throughs, so the copy would be unreachable and would drift from
		// the stage that eventually produces it. The generic sentence is true of
		// both, and the per-stage breakdown note beside it says which stage ran.
		return "No sufficiently trustworthy memory found: every candidate this search found was removed before the " +
			"answer was assembled. " + p.removalBreakdown() + "."
	}
	return ""
}

// dominantBudgetBound is which of stage 8's three caps removed the most rows.
// It is only consulted once stage 8 is known to have emptied the set, and the
// byte cap wins a tie because it is the stricter of the two a row can break.
func (p *pipeline) dominantBudgetBound() string {
	best, bestN := "", 0
	for _, bound := range []string{boundSliceItems, boundSliceBytes, boundTotalItems} {
		if n := p.droppedByBound[bound]; n > bestN || (n == bestN && n > 0 && bound == boundSliceBytes) {
			best, bestN = bound, n
		}
	}
	return best
}

// qualifierBlock renders the qualifiers as a block that leads the response. It
// used to be the MCP surface's job, which meant the search tool prepended ~800
// bytes of disclosure to a render the response-fit post-pass had already trimmed
// to a cap — so a 16000-byte answer shipped at ~16800. Result.Qualifiers is still
// the field a caller reads; this is where it is printed.
func (p *pipeline) qualifierBlock() string {
	if len(p.qualifiers) == 0 {
		return ""
	}
	return strings.Join(p.qualifiers, "\n") + "\n\n"
}

// machineLine is the one trailing machine-readable line of the response. A
// caller that has to read prose to learn whether to trust an answer is a caller
// that will eventually trust the wrong one, and the fields it carries are the
// ones that decide that: the verdict, its reason, the thresholds that produced
// it, what retrieval returned, and the leg status.
//
// abstain_cosine carries three states, because a threshold and a threshold that
// ran are different facts: `off` (nobody configured one), `not_applied` (one was
// configured and no cosine could be compared — the vector leg never executed, or
// ran and failed; `reason=` and `legs=` say which), and the
// number, printed when the arm was on AND the leg ran — which is not the same as
// a row having been compared, since an empty result reads no cosine and a result
// whose first row clears the keyword arm never reaches one. The first two states
// both leave VectorCosine unreadable as evidence — 0 is a threshold every row
// clears — and collapsing them would tell a reader who set the key that it did
// nothing, or a reader who did not that a threshold exists.
// cosineField renders a configured cosine for the machine line.
//
// Three decimals is the field's width, and it is kept for every value it states
// exactly. A threshold below 0.0005 is the exception: at that width it prints
// `0.000`, which is the number a reader parses as "a threshold of zero" — a
// threshold every row clears, and the value the glossary hands to `off`. An arm
// in force that renders as the number `off` means is a line stating the opposite
// of what happened, so a value the fixed width would round away is rendered at
// its own precision instead. An irregular field width is the cheaper mistake.
func cosineField(v float32) string {
	if fixed := strconv.FormatFloat(float64(v), 'f', 3, 32); fixed != "0.000" || v == 0 {
		return fixed
	}
	return strconv.FormatFloat(float64(v), 'g', -1, 32)
}

func (p *pipeline) machineLine(outcome Outcome, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[ghost:outcome=%s reason=%s", outcome, reason)
	// The keyword arm gets the same three-way treatment as the cosine, because it
	// has the same failure: a threshold printed next to a verdict it did not
	// produce invites a reader to believe the judgement came from it. A retriever
	// that ranked nothing leaves every row at the -1 sentinel, so no keyword
	// comparison happened at all — and an empty result reads no rank either.
	if p.trace.Floors.FTSApplied {
		fmt.Fprintf(&b, " floor_fts_rank=%d", p.trace.Floors.FTSRankMax)
	} else {
		b.WriteString(" floor_fts_rank=not_applied")
	}
	switch {
	case p.passive:
		// FIRST, ahead of every other state, and that ordering is the point. A
		// passive block has no query, so the cosine arm has nothing to compare
		// against — and a retriever that reported the vector leg `ok` (or a caller
		// that set AbstainCosine) would otherwise print a number next to a verdict
		// that no cosine produced. A fourth state, and the reason the three below
		// are not enough here: `off` reports a CONFIGURATION to a reader who cannot
		// change one, because this surface has no vector arm whatever
		// `context.abstain_cosine` is set to.
		b.WriteString(" abstain_cosine=not_applied")
	case p.trace.Floors.VectorApplied:
		fmt.Fprintf(&b, " abstain_cosine=%s", cosineField(p.trace.Floors.VectorCosine))
	case p.trace.Floors.VectorArmOn:
		// Configured and never used. Printing the number here would read as
		// though the cosine cleared a row it was never compared against, which is
		// the same error as labelling a keyword hit weak because an embedder was
		// down. The reason beside it already says which leg did not run.
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

// legSummary is the leg status the line carries, in three states and not two.
//
// `absent` is a leg the request never asked for. `not_run` is a leg the request
// DID ask for that never executed — which is the ordinary state of a hybrid
// search on a machine with no embedder, or one whose embed call just failed.
// Collapsing those two into one word, as this did, tells a harness reading the
// line that the leg did not apply when in fact it applied and did not answer,
// and the two call for opposite responses: `absent` is nothing to do,
// `not_run` is a keyword-only search. `failed` stays separate from both, because
// a leg that ran and errored is an incident rather than a configuration.
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

// totalTokens is the block's token ESTIMATE: the sum of its items' own
// estimates, so a total computed here and a per-item figure shown to a caller
// cannot disagree by a rounding step.
func totalTokens(items []Item) int {
	n := 0
	for _, it := range items {
		n += it.Tokens
	}
	return n
}

// filterCaveat names the filters that can make a windowed result short and
// gives the caller a filter-appropriate next step. All three are applied before
// the final cut now, but the candidate pool they select from is still finite, so
// further matches may exist beyond it.
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
		// The exhaustive-browsing advice belongs to any row-level filter, not only
		// to the category: ghost_memories_list carries the same filters and is not
		// windowed, so it is the next step for a caller looking for rows of one
		// tier just as it is for one category.
		next += " or use ghost_memories_list for exhaustive browsing"
	}
	return "(Note: " + which + " " + verb + " applied to a finite search window, so further matches may exist beyond the retrieved candidates — " + next + ".)"
}

// assemblerNotes renders the bounded diagnostics for an empty answer, as a
// leading label so the lines below it are not mistaken for more memories.
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

// failedLegs names the retrieval legs that were applicable, ran and could not
// answer — the whole-sentence form of the machine line's `failed` state.
func (p *pipeline) failedLegs() string {
	var failed []string
	for _, name := range []string{"fts", "vector"} {
		if p.legFailed(name) {
			failed = append(failed, name+" leg: "+p.set.Legs[name].Err)
		}
	}
	return strings.Join(failed, "; ")
}

// response renders the complete search answer: the listing, whatever the verdict
// adds to it, and the machine line. It is the one place a response's bytes are
// counted, which is why the post-pass below measures this and no other function.
//
// A result that admitted rows shows them, whatever the verdict: `weak` is a
// warning, not a filter, and a caller that can see the weak candidates is the
// only one that can judge them. An empty result leads with its reason, because
// the leading sentence is the one a reader stops at, and "nothing matched" is
// only true of a set that was searched.
func (p *pipeline) response(res Result) string {
	var b strings.Builder
	// The qualifiers lead the block, and they are rendered HERE rather than by
	// the surface: they are bytes the caller receives, and a cap measured against
	// a render that excludes them is a cap on text nobody receives. A block
	// assembled at an instant reads as a block about the present unless the reader
	// is told otherwise, and the rows themselves carry nothing that says which it
	// is — so the note has to be the first thing a reader meets, and the fit pass
	// has to have measured it to get there.
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
	// The machine line is last in every case, so a reader working bottom-up
	// meets the verdict before the prose it qualifies.
	if res.Machine != "" {
		b.WriteString("\n" + res.Machine + "\n")
	}
	return b.String()
}

// filterCaveat is the windowed-answer hint, already wrapped in the blank line
// that separates a block from its own commentary. It is one function for every
// branch because a weak answer is exactly where the hint is worth most — a short
// AND untrustworthy result is the one a caller is most likely to read as the
// whole truth — and a per-branch copy is how it ended up on two of the three.
func (p *pipeline) filterCaveat() string {
	// An empty answer gets it unconditionally: the leading sentence there is a
	// verdict, and the hint is what keeps a filtered empty result from reading as
	// an absence claim. A non-empty one only when it came back shorter than the
	// caller asked for, because a window that filled is not obviously short.
	if len(p.items) > 0 && (p.req.Budget.MaxItems <= 0 || len(p.items) >= p.req.Budget.MaxItems) {
		return ""
	}
	// Never after a response-fit trim. That trim removed rows the window
	// admitted, so the window is not what made the answer short, and the caveat's
	// advice — raise the limit — admits more rows and makes the response larger,
	// which is the one move that cannot help. The machine line's `admitted=` still
	// carries the shortfall.
	if p.droppedBy[stageResponseFit] > 0 {
		return ""
	}
	caveat := filterCaveat(p.req.Category, p.req.Retention, p.req.Scope)
	if caveat == "" {
		return ""
	}
	return "\n\n" + caveat
}

// fitResponse is the response-fit post-pass: the seed is measured from the
// complete envelope, and while that render exceeds Budget.MaxBytes the
// lowest-ranked row is dropped, the outcome recomputed, the notes re-derived and
// bounded, and the response re-rendered. A fitting seed needs no iteration.
//
// The order of what gives way is fixed and is the point of the pass. Rows go
// first, from the bottom of the ranking, because a response that drops its best
// candidates to keep its worst has inverted the ranking the stages produced.
// Diagnostic notes go next, and only once no row is left, because they qualify
// the answer rather than being part of it. The verdict, its reason and the
// machine line are last, and if they still do not fit there is no answer to
// return: ErrResponseBudgetExceeded says the budget was impossible, which is a
// different fact from `empty` and one this package must not report as an
// absence.
func (p *pipeline) fitResponse(base Result) (Result, error) {
	// Whether the vector arm judged anything is a fact about the RUN, not the
	// request, so it is recorded here and not in newTrace, which only knows what
	// was asked for. It is set before the first render because the machine line
	// reads it: a projection built from the trace that read only VectorCosine
	// would report a threshold that cleared rows it was never compared against.
	p.trace.Floors.VectorApplied = p.trace.Floors.VectorArmOn && p.vectorLegInPlay()

	in := len(p.items)
	res := base
	dropped := []string(nil)
	var fitNotes []string
	prev := p.notes()

	for {
		// Re-derive everything a row or note drop can change, and nothing else.
		// The outcome is recomputed rather than carried: the row that cleared a
		// floor may be the row that was just dropped, and a verdict that
		// outlived its evidence is a claim about rows the answer no longer has.
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
		if p.req.Budget.MaxBytes <= 0 || res.Bytes <= p.req.Budget.MaxBytes {
			break
		}

		switch {
		case len(p.items) > 0:
			// p.rows alongside p.items, or notes()'s gate on an empty row set
			// stays false and the removal breakdown — the thing that makes an
			// empty answer's leading sentence checkable — disappears on exactly
			// the case this post-pass creates.
			lowest := p.items[len(p.items)-1]
			p.items = p.items[:len(p.items)-1]
			if n := len(p.rows); n > 0 {
				p.rows = p.rows[:n-1]
			}
			p.dropped[lowest.ID] = reasonBudgetDropped
			p.droppedBy[stageResponseFit]++
			p.trace.decide(lowest.ID, stageResponseFit, reasonBudgetDropped, lowest.Score)
			dropped = append(dropped, lowest.ID)
		case len(res.Notes) > 0:
			// Cut from the end, which is where boundNotes already puts the
			// per-row detail: the sentences that qualify the answer survive the
			// pressure and the detail is what goes.
			p.noteCut++
		default:
			return Result{}, ErrResponseBudgetExceeded
		}
	}

	p.trace.record(stageResponseFit, in, len(p.items), dropped, false, fitNotes...)
	p.trace.Notes = res.Notes
	return res, nil
}

// vanished returns the notes that were in before and are not in after — the
// ones the fit pass discarded, whether a row drop re-derived them away or a
// tail cut took them. They are recorded so a reader of the trace can tell a
// note that was never written from one the answer no longer carries.
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
