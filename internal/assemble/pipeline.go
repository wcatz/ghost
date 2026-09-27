package assemble

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// pipeline is the working state the stages share. Stages run in the order of
// the stages slice and communicate only through it, so a new stage cannot
// quietly depend on one that happens to run before it.
type pipeline struct {
	req   Request
	mode  memory.ProjectMode
	set   *memory.CandidateSet
	trace *Trace

	// rows is the surviving candidate set, in rank order.
	rows []memory.Candidate
	// blockNotes are statements about the assembled block — its conflicts, its
	// dedup and diversity state. They are held apart from noteBuf because stage 5
	// runs before stage 8, and a block that stage 8 then empties must not be
	// described as if it existed: the notes are folded in only when something
	// was actually admitted, which is the last thing the pipeline knows.
	blockNotes []string
	// items mirrors rows, materialised once so rendering and the trace read
	// the same values.
	items []Item
	// droppedBy counts how many rows each stage removed, which is how an
	// empty result names the stage responsible.
	droppedBy map[string]int
	noteBuf   []string
	// dropped is every id any stage removed, for the notes.
	dropped map[string]string
}

// stage is one step of the ordered pipeline. A stage that changes nothing in
// v1 still runs and still records its counts: "this filter was applied and
// removed no rows" is a different statement from "this filter was not reached".
type stage struct {
	name string
	run  func(*pipeline)
}

// stages is the pipeline, in order. Every filter and every decision that can
// affect membership runs before the final window closure in stage 8.
//
// Stage 1 (retrieve) is not in this list: it is the one stage that needs the
// Retriever, so Run performs it before the pipeline starts. Everything after it
// is a pure function of the candidate set, which is what makes stages 2-8
// testable without a database.
var stages = []stage{
	{name: stageValidity, run: runValidity},
	{name: stagePredicates, run: runPredicates},
	{name: stageProvenance, run: runProvenance},
	{name: stageConflicts, run: runConflicts},
	{name: stageDedup, run: runDedup},
	{name: stageDiversity, run: runDiversity},
	{name: stageBudget, run: runBudget},
	{name: stageRender, run: runRender},
}

// runValidity is stage 2: drop a row whose validity window has closed or has not
// opened, and record the state of every row that survives. verified_at is a
// flag, not a predicate — a live row nobody has re-verified is still true as far
// as the store knows, and hiding it would be a claim the data does not support.
func runValidity(p *pipeline) {
	in := len(p.rows)
	var dropped []string
	// A fresh slice, not p.rows[:0]: the candidate set is the retriever's
	// return value and the contract says it is the widened untrimmed result, so
	// compacting into its backing array would leave the caller holding
	// duplicated, stale rows. A later stage or a caller with the same set would
	// read corruption instead of the retrieval that produced it.
	kept := make([]memory.Candidate, 0, len(p.rows))
	for _, c := range p.rows {
		v := readValidity(c, p.req.Now)
		sig := p.signal(c)
		sig.ValidityState = v.state
		for _, raw := range v.unparseable {
			// A value nobody can read is not a claim, and it is not "valid"
			// either. It is reported so a caller can see the row's claim is
			// unreadable rather than absent — as a note, which is true of the row
			// whether it survives or not.
			p.noteBuf = append(p.noteBuf, formatNote("validity_unparseable: row %s has a validity value Ghost cannot read (%q), treated as unset", shortID(c.ID), raw))
		}
		if v.state == validityExpired || v.state == validityFuture {
			dropped = append(dropped, c.ID)
			p.dropped[c.ID] = v.state
			p.droppedBy[stageValidity]++
			p.trace.decide(c.ID, stageValidity, v.state, c.Score)
			continue
		}
		if len(v.unparseable) > 0 {
			// Kept, not excluded — and only on this path. A row can be both
			// expired and carry an unreadable value, and then it has one fate
			// (dropped); recording a kept decision for it too would leave two
			// contradictory entries for the same row at the same stage, which is
			// the one thing the Decision record is documented not to hold.
			p.trace.keep(c.ID, stageValidity, "validity_unparseable", c.Score)
		}
		it := itemOf(c)
		it.ValidityState = v.state
		it.ValidFrom = parseStampPtr(c.ValidFrom)
		it.ValidUntil = parseStampPtr(c.ValidUntil)
		it.VerifiedAt = parseStampPtr(c.VerifiedAt)
		kept = append(kept, c)
		p.items = append(p.items, it)
		p.trace.Signals[c.ID] = sig
	}
	p.rows = kept
	p.trace.record(stageValidity, in, len(kept), dropped, false)
}

// runPredicates is stage 3: the category and scope verdicts, applied over the
// widened candidate set — before the window closes, which is the whole point of
// the seam. A predicate that ran after closure could only ever remove from the
// answer, never add to it, so a matching row the window cut was invisible and
// the tool reported absence while the memory existed.
//
// Project membership is enforced by the retrieval SQL; it is recorded per row
// as a verdict, never applied as a second drop. Silently dropping a row the legs
// returned would hide a storage bug as a filtering decision.
func runPredicates(p *pipeline) {
	in := len(p.rows)
	var dropped []string
	kept := make([]memory.Candidate, 0, len(p.rows))
	items := make([]Item, 0, len(p.rows))
	for i, c := range p.rows {
		it := p.items[i]
		sig := p.trace.Signals[c.ID]
		sig.ProjectMatch = !BucketUnexpected(c.ProjectID, p.req.ProjectID) || p.mode == memory.AllProjects
		sig.ScopeKeysCompared = scopeKeys(p.req.Scope)
		sig.ScopeMatched = !ScopeContradicts(c.Scope, p.req.Scope)
		p.trace.Signals[c.ID] = sig

		switch {
		case p.req.Category != "" && c.Category != p.req.Category:
			dropped = append(dropped, c.ID)
			p.dropped[c.ID] = "category_mismatch"
			p.droppedBy[stagePredicates]++
			p.trace.decide(c.ID, stagePredicates, "category_mismatch", c.Score)
			continue
		case !sig.ScopeMatched:
			dropped = append(dropped, c.ID)
			p.dropped[c.ID] = "scope_contradiction"
			p.droppedBy[stagePredicates]++
			p.trace.decide(c.ID, stagePredicates, "scope_contradiction", c.Score)
			continue
		}
		kept = append(kept, c)
		items = append(items, it)
	}
	p.rows, p.items = kept, items
	p.trace.record(stagePredicates, in, len(kept), dropped, false)
}

// provenanceWeight is stage 4's weight, as the trace records it, and only that:
// no score is multiplied by it in v1. Confidence is writable today and may be
// non-NULL on existing rows, so a weight that changed the order would change
// results with no measured justification behind it. Shipping the stage with the
// decision recorded is what makes a later change measurable — but it is not a
// one-line change: a real multiplier needs a float here, applied where
// ProvenanceContribution and ConfidenceContribution are computed.
const provenanceWeight = "1.0"

// runProvenance is stage 4: the weight is pinned and the decision is recorded
// for every row, so a caller can already see what a future multiplier would act
// on. Both contributions are zero while the weight is 1.0, which is what makes a
// seeded confidence value unable to change the order.
func runProvenance(p *pipeline) {
	for id, sig := range p.trace.Signals {
		sig.Confidence = p.confidenceOf(id)
		sig.ConfidenceContribution = 0
		sig.ProvenanceWeight = provenanceWeight
		sig.ProvenanceContribution = 0
		p.trace.Signals[id] = sig
	}
	p.trace.record(stageProvenance, len(p.rows), len(p.rows), nil, false,
		"provenance weight is pinned at 1.0: no measured threshold justifies scoring confidence yet")
}

// runConflicts is stage 5. Supersede and contradiction handling belong here, and
// the retriever's window already carries the supersede demotion it applies
// today. `contradicts` is recorded rather than acted on: the product contract is
// that a contradicted row survives while a duplicate restatement sinks, so
// removing the weaker endpoint would reverse tested behaviour. A v1 pass-through
// keeps the stage in the pipeline, in order, without changing membership.
func runConflicts(p *pipeline) {
	in := len(p.rows)
	var dropped []string
	var notes []string
	// Everything below is a statement about the block, so nothing is said when
	// there is no block: "no link joins two of these candidates" is a claim
	// about a set that was never retrieved, and an empty answer is explained by
	// the removal breakdown instead. A contradicts note additionally checks that
	// both endpoints survived, because the edge set covers every candidate while
	// the block holds only the rows that made it through.
	if len(p.items) > 0 {
		admitted := make(map[string]bool, len(p.items))
		for _, it := range p.items {
			admitted[it.ID] = true
		}
		// Whether the block survives stage 8 is not known yet, so these are held
		// as block notes rather than emitted: an empty answer must not claim two
		// rows "both remain in the block".
		switch p.set.EdgesStatus.Status {
		case "err":
			notes = append(notes, formatNote("edges_unavailable: the link lookup failed (%s), so conflict handling had no edges to read", p.set.EdgesStatus.Err))
		case "unavailable":
			notes = append(notes, "edges_unavailable: no link joins two of these candidates")
		}
		// A contradiction pair is recorded, never removed.
		for _, e := range p.set.Edges {
			if e.Relation == "contradicts" && admitted[e.From] && admitted[e.To] {
				notes = append(notes, formatNote("contradicts pair recorded, not separated: %s and %s both remain in the block", shortID(e.From), shortID(e.To)))
			}
		}
	}
	p.blockNotes = append(p.blockNotes, notes...)
	p.trace.record(stageConflicts, in, len(p.rows), dropped, false, notes...)
}

// runDedup is stage 6. The retriever reorders the window by supersede and
// near-duplicate edges already, and the source policy for dropping a demoted
// loser belongs to the session-start surface, which is not on this seam yet. The
// stage is a pass-through in v1 and says so in the trace rather than pretending
// to have deduplicated.
func runDedup(p *pipeline) {
	p.blockNotes = append(p.blockNotes,
		"near-duplicate reordering is applied by the retriever over the window; no source policy drops losers on this surface yet")
	p.trace.record(stageDedup, len(p.rows), len(p.rows), nil, false,
		"near-duplicate reordering is applied by the retriever over the window; no source policy drops losers on this surface yet")
}

// runDiversity is stage 7: a per-bucket quota, off by default until it is
// measured. Recorded as a no-op so a reader can see the stage ran and changed
// nothing, rather than inferring it was skipped.
func runDiversity(p *pipeline) {
	note := "diversity is off by default: no measured per-bucket quota"
	p.blockNotes = append(p.blockNotes, note)
	p.trace.record(stageDiversity, len(p.rows), len(p.rows), nil, false, note)
}

// runBudget is stage 8: the final closure. The order the retriever returned is
// authoritative and is preserved — it carries the keyword reservation, status
// demotion, decay and both demotions, none of which can be recovered from a
// single score, so re-sorting here would undo them. What this stage owns is
// membership: the per-bucket slice caps, then the total cap, both hard.
func runBudget(p *pipeline) {
	in := len(p.rows)
	rows, items, dropped := p.rows, p.items, []string(nil)

	// Per-item presentation clamp first: a clamped item is shorter, so it can
	// only ever help the caps below.
	for i := range items {
		if s := p.sliceFor(items[i].Bucket); s != nil && s.ClampBytes > 0 {
			clamped := clampBytes(items[i].Content, s.ClampBytes)
			items[i].Content = clamped
			items[i].Bytes = len(clamped)
		}
	}

	// Per-bucket caps. A bucket with no slice is unbounded, which is what makes
	// a single-slice request mean "this bucket, this many rows".
	if len(p.req.Budget.Slices) > 0 {
		count := map[string]int{}
		bytes := map[string]int{}
		keepRow := make([]bool, len(rows))
		for i, it := range items {
			s := p.sliceFor(it.Bucket)
			if s == nil {
				keepRow[i] = true
				continue
			}
			overItems := s.MaxItems > 0 && count[it.Bucket] >= s.MaxItems
			overBytes := s.MaxBytes > 0 && bytes[it.Bucket]+it.Bytes > s.MaxBytes
			if overItems || overBytes {
				continue
			}
			keepRow[i] = true
			count[it.Bucket]++
			bytes[it.Bucket] += it.Bytes
		}
		rows, items, dropped = trim(rows, items, keepRow, dropped, p, "slice_budget")
	}

	// The total cap, applied across buckets.
	if p.req.Budget.MaxItems > 0 && len(rows) > p.req.Budget.MaxItems {
		keepRow := make([]bool, len(rows))
		for i := range p.req.Budget.MaxItems {
			keepRow[i] = true
		}
		rows, items, dropped = trim(rows, items, keepRow, dropped, p, "budget")
	}
	if p.req.Budget.MaxBytes > 0 {
		used, keepRow := 0, make([]bool, len(items))
		for i, it := range items {
			if used+it.Bytes > p.req.Budget.MaxBytes {
				break
			}
			used += it.Bytes
			keepRow[i] = true
		}
		rows, items, dropped = trim(rows, items, keepRow, dropped, p, "budget")
	}

	p.rows, p.items = rows, items
	p.trace.record(stageBudget, in, len(rows), dropped, false)
}

// runRender is stage 9: the shared item renderer. Each surface keeps its own
// framing and field order around Line(); what is shared is the item line, so the
// same memory reads the same way in search output and in an injected block. The
// response-fit post-pass, which needs the framing to measure a complete
// response, is not part of this stage — it runs after the outcome.
func runRender(p *pipeline) {
	p.trace.record(stageRender, len(p.rows), len(p.items), nil, false)
}

// trim drops the rows keepRow marks false, recording each one.
func trim(rows []memory.Candidate, items []Item, keepRow []bool, dropped []string, p *pipeline, reason string) ([]memory.Candidate, []Item, []string) {
	keptRows := make([]memory.Candidate, 0, len(rows))
	keptItems := make([]Item, 0, len(items))
	for i := range rows {
		if !keepRow[i] {
			dropped = append(dropped, rows[i].ID)
			p.dropped[rows[i].ID] = reason
			p.droppedBy[stageBudget]++
			p.trace.decide(rows[i].ID, stageBudget, reason, rows[i].Score)
			continue
		}
		keptRows = append(keptRows, rows[i])
		keptItems = append(keptItems, items[i])
	}
	return keptRows, keptItems, dropped
}

// sliceFor returns the slice policy for a bucket, or nil when the bucket is
// unbounded.
func (p *pipeline) sliceFor(bucket string) *Slice {
	for i := range p.req.Budget.Slices {
		if p.req.Budget.Slices[i].Bucket == bucket {
			return &p.req.Budget.Slices[i]
		}
	}
	return nil
}

// signal returns a row's starting signal set, built from the retriever's facts.
func (p *pipeline) signal(c memory.Candidate) Signals {
	return Signals{
		FTSRank:      c.FTSRank,
		VectorRank:   c.VectorRank,
		VectorScore:  c.VectorScore,
		Base:         c.Base,
		DecayFactor:  c.Decay,
		AgeDays:      c.AgeDays,
		CreatedAt:    parseStamp(c.CreatedAt),
		ProjectMatch: true,
		ScopeMatched: true,
	}
}

func (p *pipeline) confidenceOf(id string) *float64 {
	for i := range p.rows {
		if p.rows[i].ID == id {
			return p.rows[i].Confidence
		}
	}
	return nil
}

// notes is the bounded diagnostic note list, stage notes first. An empty result
// also gets the per-stage breakdown, because a closed reason set can name only
// one cause and a caller who was told "withheld as out of date" while the budget
// cut the rest needs to see that both happened.
func (p *pipeline) notes() []string {
	all := make([]string, 0, len(p.noteBuf)+1)
	// First, not last. The breakdown qualifies the closed reason the answer leads
	// with, and bounding the list drops from the end — so leading with it means
	// pressure discards per-row detail instead of the one sentence that makes the
	// label checkable.
	if len(p.rows) == 0 && len(p.dropped) > 0 {
		all = append(all, formatNote("%d candidate rows were removed and none reached the answer: %s",
			len(p.dropped), p.removalBreakdown()))
	}
	if len(p.items) > 0 {
		all = append(all, p.blockNotes...)
	}
	all = append(all, p.noteBuf...)
	return boundNotes(all, p.req.Budget.MaxNoteBytes, p.req.Budget.MaxNotesBytes)
}

// removalBreakdown is the per-stage removal count in pipeline order.
func (p *pipeline) removalBreakdown() string {
	order := []string{stageValidity, stagePredicates, stageProvenance, stageConflicts, stageDedup, stageDiversity, stageBudget}
	parts := make([]string, 0, len(order))
	for _, stage := range order {
		if n := p.droppedBy[stage]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", stage, n))
		}
	}
	if len(parts) == 0 {
		return "no stage recorded a removal"
	}
	return strings.Join(parts, ", ")
}

// outcome and reason are Decision 3's rules, restricted to what v1 can decide.
// The relevance floor arrives with abstention, so an admitted row is answerable
// and `weak` is not yet reachable; every empty result still names the stage that
// emptied it, which is the part a caller acts on.
func (p *pipeline) reason() string {
	if len(p.items) > 0 {
		return ""
	}
	if len(p.set.Rows) == 0 {
		if p.set.Legs["fts"].Err != "" || p.set.Legs["vector"].Err != "" {
			return "retrieval_failed"
		}
		return "no_candidates"
	}
	// The reason set is closed, so a set emptied by more than one stage can
	// carry only one label, and the label has to be the cause that accounts for
	// the rows. Picking the first stage that removed anything would let a single
	// expired row claim a set that the item budget actually emptied, and the
	// caller's next move differs: one looks for a date problem, the other raises
	// the limit. Ties go to the earlier stage, which keeps "the first matching
	// stage supplies the reason" true for the cases where the stages do not
	// compete.
	stage, reason := p.dominantRemoval()
	if stage == "" {
		return "no_candidates"
	}
	return reason
}

// dominantRemoval is the stage responsible for most of the removed rows, and the
// reason that goes with it. Stage order breaks ties, so the result does not
// depend on map iteration.
func (p *pipeline) dominantRemoval() (string, string) {
	type cause struct {
		stage, reason string
		count         int
	}
	candidates := []cause{
		{stage: stageValidity, reason: "all_invalid"},
		{stage: stagePredicates, reason: p.predicateReason()},
		{stage: stageDedup, reason: "all_dedup_dropped"},
		{stage: stageDiversity, reason: "all_diversity_capped"},
		{stage: stageBudget, reason: "all_over_budget"},
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

// predicateReason names the narrower of the two stage-3 verdicts, or "" when
// neither filter is set.
func (p *pipeline) predicateReason() string {
	switch {
	case p.req.Category != "":
		return "all_out_of_category"
	case len(p.req.Scope) > 0:
		return "all_out_of_scope"
	}
	return ""
}

func (p *pipeline) outcome(reason string) Outcome {
	if len(p.items) > 0 {
		return OutcomeAnswerable
	}
	return OutcomeEmpty
}

// scopeKeys lists the scope keys a request asked about, sorted for a stable
// trace.
func scopeKeys(scope map[string]string) []string {
	if len(scope) == 0 {
		return nil
	}
	keys := make([]string, 0, len(scope))
	for k := range scope {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// shortID truncates an id for a note, so a trace line stays readable.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
