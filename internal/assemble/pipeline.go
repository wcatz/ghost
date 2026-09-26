package assemble

import (
	"sort"

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
	kept := p.rows[:0]
	for _, c := range p.rows {
		v := readValidity(c, p.req.Now)
		sig := p.signal(c)
		sig.ValidityState = v.state
		for _, raw := range v.unparseable {
			// A value nobody can read is not a claim, and it is not "valid"
			// either. It is reported so a caller can see the row's claim is
			// unreadable rather than absent.
			p.noteBuf = append(p.noteBuf, formatNote("validity_unparseable: row %s has a validity value Ghost cannot read (%q), treated as unset", shortID(c.ID), raw))
			p.trace.decide(c.ID, stageValidity, "validity_unparseable", c.Score)
		}
		if v.state == validityExpired || v.state == validityFuture {
			dropped = append(dropped, c.ID)
			p.dropped[c.ID] = v.state
			p.droppedBy[stageValidity]++
			p.trace.decide(c.ID, stageValidity, v.state, c.Score)
			continue
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

// provenanceWeight is stage 4's multiplier, as the trace records it: pinned, so
// confidence is copied rather than scored. Confidence is writable today and may
// be non-NULL on existing rows, so a multiplier that changed the order would
// change results with no measured justification behind it. Shipping the stage
// with the decision recorded is what makes a later change a one-line, measured
// one — a single weight applied where ProvenanceContribution is computed.
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
	switch p.set.EdgesStatus.Status {
	case "err":
		notes = append(notes, formatNote("edges_unavailable: the link lookup failed (%s), so conflict handling had no edges to read", p.set.EdgesStatus.Err))
	case "unavailable":
		notes = append(notes, "edges_unavailable: no link joins two of these candidates")
	}
	// A contradiction pair is recorded, never removed.
	for _, e := range p.set.Edges {
		if e.Relation == "contradicts" {
			notes = append(notes, formatNote("contradicts pair recorded, not separated: %s and %s both remain in the block", shortID(e.From), shortID(e.To)))
		}
	}
	p.noteBuf = append(p.noteBuf, notes...)
	p.trace.record(stageConflicts, in, len(p.rows), dropped, false, notes...)
}

// runDedup is stage 6. The retriever reorders the window by supersede and
// near-duplicate edges already, and the source policy for dropping a demoted
// loser belongs to the session-start surface, which is not on this seam yet. The
// stage is a pass-through in v1 and says so in the trace rather than pretending
// to have deduplicated.
func runDedup(p *pipeline) {
	p.trace.record(stageDedup, len(p.rows), len(p.rows), nil, false,
		"near-duplicate reordering is applied by the retriever over the window; no source policy drops losers on this surface yet")
}

// runDiversity is stage 7: a per-bucket quota, off by default until it is
// measured. Recorded as a no-op so a reader can see the stage ran and changed
// nothing, rather than inferring it was skipped.
func runDiversity(p *pipeline) {
	p.trace.record(stageDiversity, len(p.rows), len(p.rows), nil, false,
		"diversity is off by default: no measured per-bucket quota")
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

// notes is the bounded diagnostic note list, stage notes first.
func (p *pipeline) notes() []string {
	all := append([]string(nil), p.noteBuf...)
	if len(p.rows) == 0 && len(p.dropped) > 0 {
		all = append(all, formatNote("%d candidate rows were removed by the assembler's filters; none reached the answer", len(p.dropped)))
	}
	return boundNotes(all, p.req.Budget.MaxNoteBytes, p.req.Budget.MaxNotesBytes)
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
	switch {
	case p.droppedBy[stageValidity] == len(p.set.Rows):
		return "all_invalid"
	case p.req.Category != "" && p.droppedBy[stagePredicates] == len(p.set.Rows):
		return "all_out_of_category"
	case len(p.req.Scope) > 0 && p.droppedBy[stagePredicates] == len(p.set.Rows):
		return "all_out_of_scope"
	case p.droppedBy[stageBudget] == len(p.set.Rows):
		return "all_over_budget"
	}
	// Rows existed, none survived, and no single stage accounts for all of
	// them: the filters together emptied the set. The category verdict is named
	// first because it is the narrower of the two.
	if p.droppedBy[stagePredicates] > 0 {
		if p.req.Category != "" {
			return "all_out_of_category"
		}
		return "all_out_of_scope"
	}
	return "all_invalid"
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
