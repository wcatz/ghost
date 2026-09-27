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
	// runs before stage 8, and a block that stage 8 then trims must not be
	// described as if it were whole: contradictPairs is filtered against the final
	// admitted set, and the rest are dropped when nothing was admitted, because
	// both are the last things the pipeline knows.
	blockNotes []string
	// contradictPairs are the 'contradicts' pairs stage 5 recorded, keyed
	// unordered and once each. notes() renders them after the window closes,
	// against the rows the answer actually holds, so "both remain in the block"
	// cannot outlive one of them. The stage's own copy of the fact goes to the
	// trace rather than here: a note true at stage 5 is not always true of the
	// answer, and one sentence cannot serve both readers.
	contradictPairs [][2]string
	// windowDisclosure is the note explaining a window that is the pipeline's
	// ceiling rather than the caller's. It is held here because it is set before
	// the stages run and belongs to the stage that acts on it: stage 8 is what
	// enforces whatever byte bound the caller gave, so its record is where a
	// reader looking at the trace will find it.
	windowDisclosure string
	// retrievalFailures are the retrieval's own statements — a leg that errored,
	// a link lookup that failed. They are facts about the retrieval rather than
	// about the block, so they lead the notes ahead of the conflict chatter: a
	// list of pairs must not be able to squeeze out the one sentence that says
	// the answer is not an absence.
	retrievalFailures []string
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
			// quoteData, because this is stored text on its way to a tool
			// answer: a portable artifact is explicitly untrusted input, and
			// the validity triple is writable through ImportMemory and
			// RestoreSnapshot. %q escapes a delimiter without delimiting it, so
			// a value carrying one could close the data block and continue as
			// instruction. Delimited like every other stored text in an answer.
			p.noteBuf = append(p.noteBuf, formatNote("validity_unparseable: row %s has a validity value Ghost cannot read (%s), treated as unset", shortID(c.ID), quoteData(raw)))
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
			p.droppedBy[dropCategory]++
			p.trace.decide(c.ID, stagePredicates, "category_mismatch", c.Score)
			continue
		case !sig.ScopeMatched:
			dropped = append(dropped, c.ID)
			p.dropped[c.ID] = "scope_contradiction"
			p.droppedBy[stagePredicates]++
			p.droppedBy[dropScope]++
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
// The two stage-3 verdicts are counted under their own keys as well as under
// stagePredicates, so a reason can name the filter that emptied the set rather than
// the stage that contained it. They are not stages: nothing runs them.
const (
	dropCategory = "predicate:category"
	dropScope    = "predicate:scope"
)

const provenanceWeight = "1.0"

// maxRenderedConflictPairs bounds how many contradicting pairs the answer names.
// The pairs are a record of a stage that deliberately changes nothing, so a
// handful is enough for a reader to know the block contains them; a graph with
// dozens must not be able to spend the note budget that the leg-failure
// disclosure and the per-row diagnostics need.
const maxRenderedConflictPairs = 5

// The cap above bounds Result.Notes only. The trace keeps one note per
// contradicting edge, so a dense conflict graph is still O(edges) there. That is
// deliberate for now — the trace exists for diagnosis and nothing projects it in
// this version — and it is the first thing to revisit when explain does.

// runProvenance is stage 4: the weight is pinned and the decision is recorded
// for every row, so a caller can already see what a future multiplier would act
// on. Both contributions are zero while the weight is 1.0, which is what makes a
// seeded confidence value unable to change the order.
//
// The evidence counts are recorded here too, because this is the stage that owns
// what a memory is supported BY. They come from the retriever's candidate rather
// than from the store -- the assembler may reach the store through one read, and
// that read already carries them (#673) -- and they are recorded, not weighed: a
// memory no evidence names is not demoted for it, and a memory three agents
// reported is not promoted.
func runProvenance(p *pipeline) {
	for id, sig := range p.trace.Signals {
		sig.Confidence = p.confidenceOf(id)
		sig.ConfidenceContribution = 0
		sig.ProvenanceWeight = provenanceWeight
		sig.ProvenanceContribution = 0
		sig.Evidence = p.evidenceOf(id)
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
	// The trace's own copy of the stage's statements, kept apart from the ones
	// the answer will show: a note true at this stage is not always true of the
	// answer, and reporting both to the same reader says the same fact twice.
	var stageNotes []string
	// Everything below is a statement about the block, so nothing is said when
	// there is no block: "no link joins two of these candidates" is a claim
	// about a set that was never retrieved, and an empty answer is explained by
	// the removal breakdown instead.
	// A failed lookup is a statement about the retrieval, not about the block, so
	// it is reported whatever was admitted. It is also the whole reason EdgeStatus
	// distinguishes "err" from "unavailable": a failed lookup and a store with no
	// edges are different facts, and collapsing them loses the diagnosis.
	if p.set.EdgesStatus.Status == "err" {
		p.retrievalFailures = append(p.retrievalFailures, formatNote("edges_unavailable: the link lookup failed (%s), so conflict handling had no edges to read", p.set.EdgesStatus.Err))
	}
	// A failed lookup is excluded from the scan as well as reported: it read no
	// edges, so a pair sentence would assert something about rows this stage never
	// saw. The production store already returns none with the error, but the
	// retriever is an interface, and the rule is cheaper to state than to assume.
	if len(p.items) > 0 && p.set.EdgesStatus.Status != "err" {
		admitted := make(map[string]bool, len(p.items))
		for _, it := range p.items {
			admitted[it.ID] = true
		}
		// "unavailable" means the read found nothing, and that is a claim about
		// the whole candidate set only when one query covered it. The read is
		// chunked, and a caller whose window exceeds the chunk is legitimate here
		// — the window is not capped at the ceiling — so at more than one chunk a
		// pair across a query boundary was never read, and saying these candidates
		// do not contradict each other would be the opposite of what is known. The
		// count decides the sentence.
		switch {
		case p.set.EdgesStatus.Status == "unavailable" && p.set.EdgesStatus.Chunks > 1:
			notes = append(notes, formatNote("edges_partial: the link lookup read these candidates in %d queries, so a contradicting pair across a query boundary was not read — this block is not known to be free of one", p.set.EdgesStatus.Chunks))
		case p.set.EdgesStatus.Status == "unavailable":
			notes = append(notes, "edges_unavailable: no link joins two of these candidates")
		}
		// A contradiction pair is recorded, never removed. The pair is kept, not
		// rendered into the user-facing notes: stage 8 can still cut one endpoint,
		// and notes() renders it against the rows the answer finally holds. The
		// stage record below gets its own statement, which is true of this stage
		// — the two audiences get different sentences, not the same one twice.
		//
		// memory_links is keyed on (source, target, relation) and normalises the
		// order only for the symmetric 'related' relation, so a contradicts pair
		// may be stored either way round and both rows are legal. They are one
		// fact, so they are keyed unordered and once here: reporting both would
		// name the same pair twice and inflate the count notes() prints.
		seen := make(map[[2]string]bool, len(p.set.Edges))
		for _, e := range p.set.Edges {
			if e.Relation != "contradicts" || !admitted[e.From] || !admitted[e.To] {
				continue
			}
			pair := [2]string{e.From, e.To}
			if pair[0] > pair[1] {
				pair[0], pair[1] = pair[1], pair[0]
			}
			if seen[pair] {
				continue
			}
			seen[pair] = true
			p.contradictPairs = append(p.contradictPairs, pair)
			stageNotes = append(stageNotes, formatNote("contradicts pair recorded, not separated: %s and %s were both candidates at this stage", shortID(pair[0]), shortID(pair[1])))
		}
	}
	p.blockNotes = append(p.blockNotes, notes...)
	p.trace.record(stageConflicts, in, len(p.rows), dropped, false, append(append([]string(nil), stageNotes...), notes...)...)
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
	notes := []string(nil)
	if p.windowDisclosure != "" {
		notes = append(notes, p.windowDisclosure)
	}
	p.trace.record(stageBudget, in, len(rows), dropped, false, notes...)
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

// evidenceOf is confidenceOf's counterpart for the support counts. An id no row
// carries reads as no evidence, which is the honest zero: a signal recorded for a
// row this stage did not admit is not a claim about a memory.
func (p *pipeline) evidenceOf(id string) memory.EvidenceCounts {
	for i := range p.rows {
		if p.rows[i].ID == id {
			return p.rows[i].Evidence
		}
	}
	return memory.EvidenceCounts{}
}

// notes is the bounded diagnostic note list, in the order the reader needs it:
// the per-stage breakdown when the answer is empty, then the retrieval's own
// failures, then the statements about the block, then the per-row detail. Each
// group leads the one below it because bounding drops from the end, so the
// sentences that qualify the answer survive pressure and the per-row detail is
// what gets dropped.
//
// What each group reaches today. The breakdown, the retrieval failures and the
// per-row notes are what the search surface renders on an empty answer. The block
// statements are gated on an admitted row, and that surface renders no notes at
// all for a non-empty answer — the listing is the answer — so the block group is
// assembled and bounded here for a caller that projects Result.Notes (the notes
// envelope element, and the trace projection behind explain), and none of it is
// rendered by the search surface today. The machine line is not that caller: per
// the design it carries leg status and the retrieval_partial modifier, not notes.
func (p *pipeline) notes() []string {
	all := make([]string, 0, len(p.noteBuf)+len(p.blockNotes)+len(p.contradictPairs)+1)
	if len(p.rows) == 0 && len(p.dropped) > 0 {
		all = append(all, formatNote("%d candidate rows were removed and none reached the answer: %s",
			len(p.dropped), p.removalBreakdown()))
	}
	all = append(all, p.retrievalFailures...)
	if len(p.items) > 0 {
		all = append(all, p.blockNotes...)
		// Rendered here rather than at stage 5: the pair was recorded when both
		// endpoints were candidates, and the sentence says both are in the
		// answer, so it is only true of the rows the answer still holds. Capped,
		// because a graph with many contradicting pairs would otherwise spend the
		// whole note budget on conflicts and drop the rows' own diagnostics.
		admitted := make(map[string]bool, len(p.items))
		for _, it := range p.items {
			admitted[it.ID] = true
		}
		eligible := make([][2]string, 0, len(p.contradictPairs))
		for _, pair := range p.contradictPairs {
			if admitted[pair[0]] && admitted[pair[1]] {
				eligible = append(eligible, pair)
			}
		}
		// The count leads the pairs it qualifies. Bounding truncates from the
		// end, so a count placed after them is the first thing dropped under
		// pressure — and then the answer presents a capped list as the whole
		// block. A count, not a pointer: the stage record holds every pair, but
		// nothing projects it to a caller in this version, so promising it would
		// send an agent looking for something it cannot see.
		if held := len(eligible) - maxRenderedConflictPairs; held > 0 {
			all = append(all, formatNote("%d further contradicting pairs are in this block", held))
		}
		for i, pair := range eligible {
			if i == maxRenderedConflictPairs {
				break
			}
			all = append(all, formatNote("contradicts pair recorded, not separated: %s and %s both remain in the block", shortID(pair[0]), shortID(pair[1])))
		}
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

// predicateReason names the stage-3 verdict that removed the rows, or "" when
// neither filter is set. The two verdicts are counted apart, because they answer
// different questions for the caller: a category that removed nothing while the
// scope removed everything is a scope problem, and a reason naming the category
// sends the reader to change the wrong filter. A tie names the category, which is
// the narrower of the two and the one a caller is likelier to have set by accident.
func (p *pipeline) predicateReason() string {
	switch {
	case p.droppedBy[dropCategory] > 0 && p.droppedBy[dropCategory] >= p.droppedBy[dropScope]:
		return "all_out_of_category"
	case p.droppedBy[dropScope] > 0:
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
