package assemble

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/memref"
)

// pipeline is the working state the stages share. Stages run in the order of
// the stages slice and communicate only through it, so a new stage cannot
// quietly depend on one that happens to run before it.
type pipeline struct {
	req   Request
	mode  memory.ProjectMode
	set   *memory.CandidateSet
	trace *Trace

	// passive is a fact about the REQUEST, bound once at Run and read by the
	// stages that must behave differently without a query. It is derived from
	// the query's absence rather than from Source, because the consequences
	// follow from the absence: there is no leg rank, no cosine, and no relevance
	// verdict available, and a Source-keyed branch would let a future passive
	// source be judged against a floor that never applied to it.
	passive bool

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
	// against the rows the answer actually holds: a separated pair has exactly
	// one endpoint admitted, and the sentence names that side kept and the other
	// withheld, so a pair whose winner the budget later cut is not described as
	// if both were in the block. The stage's own copy of the fact goes to the
	// trace rather than here: a note true at stage 5 is not always true of the
	// answer, and one sentence cannot serve both readers.
	contradictPairs [][2]string
	// separations are the withheld rows stage 5 recorded, each with the kept rows
	// it was dropped against. separationKept reads them so explain names a row the
	// withheld memory really contradicts.
	separations []contradictionSeparation
	// conflictPartners maps each kept row to the rows it directly contradicts that
	// were withheld, in rank order. markConflicts reads it for the conflicts_with
	// marker. It is edge-derived, not separation-derived: a kept row can contradict
	// a row that was dropped against a DIFFERENT kept row (a chain's far end
	// contradicts the middle, which the near end dropped), and the marker would
	// under-name if it were built from the separations alone.
	conflictPartners map[string][]string
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
	// qualifiers are the statements that change what the block means, so they
	// are held apart from the notes: a surface renders them on every answer,
	// including an empty one, and a note list is bounded from the end.
	qualifiers []string
	// items mirrors rows, materialised once so rendering and the trace read
	// the same values.
	items []Item
	// droppedBy counts how many rows each stage removed, which is how an
	// empty result names the stage responsible.
	droppedBy map[string]int
	// droppedByBound counts the same removals by WHICH cap in stage 8 cut the
	// row, because the caps have different remedies: a row-count cap is fixed by
	// raising the limit and a content-byte cap is not. One map for the stage and
	// one for the bound, since a stage can empty a set in more than one way and
	// the sentence has to name the one that accounts for the rows.
	droppedByBound map[string]int
	noteBuf        []string
	// dropped is every id any stage removed, for the notes.
	dropped map[string]string
	// losers is the near-duplicate losers stage 6 recorded, by id: the rows the
	// retriever removed, with the ids they lost to. explain reads it to report
	// them as not included with near_duplicate_of set.
	losers map[string]memory.DroppedLoser
	// deferred is each row stage 7 moved behind the window, with the category
	// that filled its share and the share itself. It is the reason explain
	// renders for a row outside the window, and it is kept apart from `dropped`
	// because the token alone cannot name the cap a row hit.
	deferred map[string]diversityDeferral
	// noteCut is how many notes the response-fit post-pass has taken off the end
	// of the bounded list. It lives here rather than in the post-pass so notes()
	// stays the one function that produces the list: a second derivation would be
	// a second statement of which notes exist, and the two would disagree the
	// first time a row drop re-derived them.
	noteCut int
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
//
// On a historical (as_of) request it is the one stage that decides nothing: the
// window is the live row's, so no verdict drawn from it is a verdict about the
// instant, and the row is kept with its bounds rendered unjudged. Everything the
// stage would have concluded is replaced by the disclosure in qualifiersFor.
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
		// A historical (as_of) request draws NO verdict from the window (#910).
		// memory_history records no validity, so the window an as_of row carries
		// is the live row's, and judging it against T would answer with bounds
		// the row did not necessarily hold then: that is what dropped a row whose
		// window has closed or has not opened NOW even though nothing says it
		// had at T. The empty state is ValidityLabel's documented "the values and
		// no verdict" rendering (there is nothing to say about a window nobody
		// can place), and memory.ValidityWithheld holds nothing back on it, so
		// the row survives here exactly as a row with no window does. The borrow
		// is stated instead by memory.AsOfValidityNote, which qualifiersFor
		// appends to every historical block.
		if p.req.AsOf != nil {
			v.state = ""
		}
		sig := p.signal(c)
		sig.ValidityState = v.state
		for _, raw := range v.unparseable {
			// A value nobody can read is not a claim, and it is not "valid"
			// either. It is reported so a caller can see the row's claim is
			// unreadable rather than absent — as a note, which is true of the row
			// whether it survives or not.
			// Data, because this is stored text on its way to a tool
			// answer: a portable artifact is explicitly untrusted input, and
			// the validity triple is writable through ImportMemory and
			// RestoreSnapshot. %q escapes a delimiter without delimiting it, so
			// a value carrying one could close the data block and continue as
			// instruction. Delimited like every other stored text in an answer.
			p.noteBuf = append(p.noteBuf, formatNote("validity_unparseable: row %s has a validity value Ghost cannot read (%s), treated as unset", ShortID(c.ID), Data(raw)))
		}
		if memory.ValidityWithheld(v.state) {
			dropped = append(dropped, c.ID)
			p.dropped[c.ID] = v.state
			p.droppedBy[stageValidity]++
			p.trace.decide(c.ID, c.ProjectID, stageValidity, v.state, c.Score)
			continue
		}
		if len(v.unparseable) > 0 {
			// Kept, not excluded — and only on this path. A row can be both
			// expired and carry an unreadable value, and then it has one fate
			// (dropped); recording a kept decision for it too would leave two
			// contradictory entries for the same row at the same stage, which is
			// the one thing the Decision record is documented not to hold.
			p.trace.keep(c.ID, c.ProjectID, stageValidity, "validity_unparseable", c.Score)
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
	p.trace.record(stageValidity, in, len(kept), dropped)
}

// runPredicates is stage 3: the category, retention-tier and scope verdicts, applied over the
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
			p.trace.decide(c.ID, c.ProjectID, stagePredicates, "category_mismatch", c.Score)
			continue
		case p.req.Retention != "" && c.Retention != p.req.Retention:
			dropped = append(dropped, c.ID)
			p.dropped[c.ID] = "retention_mismatch"
			p.droppedBy[stagePredicates]++
			p.droppedBy[dropRetention]++
			p.trace.decide(c.ID, c.ProjectID, stagePredicates, "retention_mismatch", c.Score)
			continue
		case !sig.ScopeMatched:
			dropped = append(dropped, c.ID)
			p.dropped[c.ID] = "scope_contradiction"
			p.droppedBy[stagePredicates]++
			p.droppedBy[dropScope]++
			p.trace.decide(c.ID, c.ProjectID, stagePredicates, "scope_contradiction", c.Score)
			continue
		}
		kept = append(kept, c)
		items = append(items, it)
	}
	p.rows, p.items = kept, items
	p.trace.record(stagePredicates, in, len(kept), dropped)
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
	dropCategory  = "predicate:category"
	dropRetention = "predicate:retention"
	dropScope     = "predicate:scope"
)

const provenanceWeight = "1.0"

// maxRenderedConflictPairs bounds how many separated pairs the answer names.
// Each pair names a row stage 5 withheld, so a handful is enough for a reader to
// know the block was thinned and which rows went; a graph with dozens must not be
// able to spend the note budget that the leg-failure disclosure and the per-row
// diagnostics need.
const maxRenderedConflictPairs = 5

// The cap above bounds Result.Notes only. The trace keeps one note per withheld
// loser, so a dense conflict graph is still O(rows) there. That is deliberate for
// now — the trace exists for diagnosis, and the explain projection reads decisions
// and signals rather than these per-separation notes — and it is the first thing
// to revisit if a projection ever renders them.

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
	p.trace.record(stageProvenance, len(p.rows), len(p.rows), nil,
		"provenance weight is pinned at 1.0: no measured threshold justifies scoring confidence yet")
}

// contradictionSeparation is one row stage 5 withheld and the kept rows it was
// dropped against — the kept rows it directly contradicts, rank-ordered. There
// is usually one, but a row can contradict two kept rows that do not contradict
// each other, and then it is lost to both.
type contradictionSeparation struct {
	withheld string
	kept     []string
}

// runConflicts is stage 5. Supersede handling belongs here and the retriever's
// window already carries the supersede demotion it applies today. `contradicts`
// is SEPARATED here (#925): a row that contradicts a row the stage already kept is
// withheld against that kept row. The keep priority is stated in order in
// docs/architecture.md: pinned beats unpinned; a later verified_at beats an
// earlier or absent one; a later updated_at (created_at when updated_at is unset)
// beats an earlier one; and an exact tie falls to the rank the window already
// holds. Relevance rank decides nothing until every other key has tied, because a
// row that ranks higher is not therefore the row that is true.
//
// The rule is GREEDY over keep priority, not per connected component. Order the
// rows that have a live, non-scope-exempt contradicts edge by that priority and
// walk it: keep a row unless it has a contradicts edge to a row already kept,
// otherwise drop it against the kept rows it directly contradicts. What a shape
// keeps follows from which row wins keep priority, not from the shape: a chain
// A-B-C whose winner is an end keeps both ends and drops only the middle (the far
// end's sole edge is to the middle, which is itself dropped, so it contradicts
// nothing kept), and one whose winner is the middle keeps the middle alone; a
// triangle — three rows that all contradict each other — keeps one winner; a
// star keeps its centre alone when the centre wins, and otherwise every leaf
// with the centre dropped. The component rule this replaces winnowed a whole component to one row,
// which over-dropped: a chain's far end was withheld with nothing it contradicts
// left in the block, and the winner's conflicts_with named a row it had no edge
// to. The withheld rows are dropped here with Decision.Against naming the kept
// rows they directly contradict, each kept line names the rows it directly
// contradicts that were withheld (bounded by ConflictsLabel), and the recorded
// pairs are what notes() renders against the rows the answer finally holds.
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
		scopes := make(map[string]map[string]string, len(p.items))
		candByID := make(map[string]memory.Candidate, len(p.rows))
		rank := make(map[string]int, len(p.rows))
		for i, c := range p.rows {
			candByID[c.ID] = c
			rank[c.ID] = i
		}
		for _, it := range p.items {
			admitted[it.ID] = true
			scopes[it.ID] = it.Scope
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
		// memory_links is keyed on (source, target, relation) and normalises the
		// order only for the symmetric 'related' relation, so a contradicts pair
		// may be stored either way round and both rows are legal. They are one
		// fact, so they are keyed unordered and once here: recording both would
		// name the same pair twice in the answer and inflate its count.
		seen := make(map[[2]string]bool, len(p.set.Edges))
		adj := make(map[string][]string, len(p.items))
		for _, e := range p.set.Edges {
			if e.Relation != "contradicts" || !admitted[e.From] || !admitted[e.To] {
				continue
			}
			// Two rows that name different values for a shared scope key are two
			// true claims about two places, so the edge is not a conflict. Every
			// other reader of a link applies this same rule (memory.ScopesConflict),
			// and such a pair is neither separated, marked nor noted. The exemption
			// is per-edge, as every other reader states it, and the greedy walk
			// below still resolves a mixed-scope shape (#945): with A naming
			// environment=production contradicting an unscoped B contradicting C
			// naming environment=development, and no A-C edge, a scoped winner
			// stands beside the other scoped row and withholds only the middle it
			// directly contradicts, while an unscoped B that wins withholds both
			// scoped rows against itself. The two scope-conflicting rows are never
			// separated against each other, because a row is withheld only
			// against a kept row it has a direct contradicts edge to.
			if memory.ScopesConflict(scopes[e.From], scopes[e.To]) {
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
			adj[e.From] = append(adj[e.From], e.To)
			adj[e.To] = append(adj[e.To], e.From)
		}
		// Greedy by keep priority. Order the rows that have at least one live,
		// non-scope-exempt contradicts edge by the documented priority and walk
		// that order: keep a row unless it has a contradicts edge to a row already
		// kept, otherwise drop it against the kept rows it directly contradicts.
		// The order is a total one — the priority keys decide, and the rank the
		// window holds breaks a full tie — so the walk is deterministic.
		involved := make([]string, 0, len(adj))
		for id := range adj {
			involved = append(involved, id)
		}
		sort.Slice(involved, func(i, j int) bool {
			a, b := involved[i], involved[j]
			if contradictionBeats(candByID[a], candByID[b]) {
				return true
			}
			if contradictionBeats(candByID[b], candByID[a]) {
				return false
			}
			return rank[a] < rank[b]
		})
		keptSet := make(map[string]bool, len(involved))
		withheld := make(map[string]bool, len(dropped))
		for _, id := range involved {
			var against []string
			for _, nb := range adj[id] {
				if keptSet[nb] {
					against = append(against, nb)
				}
			}
			if len(against) == 0 {
				keptSet[id] = true
				continue
			}
			sort.Slice(against, func(i, j int) bool { return rank[against[i]] < rank[against[j]] })
			dropped = append(dropped, id)
			withheld[id] = true
			p.dropped[id] = reasonContradictionSeparated
			p.droppedBy[stageConflicts]++
			p.separations = append(p.separations, contradictionSeparation{withheld: id, kept: against})
			p.trace.Decisions = append(p.trace.Decisions, Decision{
				ID:        id,
				ProjectID: candByID[id].ProjectID,
				Stage:     stageConflicts,
				Reason:    reasonContradictionSeparated,
				Before:    candByID[id].Score,
				Against:   against,
			})
			for _, k := range against {
				stageNotes = append(stageNotes, formatNote(
					"contradicts pair recorded and separated: %s and %s were both candidates at this stage; %s kept, %s withheld",
					ShortID(k), ShortID(id), ShortID(k), ShortID(id)))
			}
		}
		// Build the kept→withheld marker from the edges: each kept row names the
		// rows it directly contradicts that were withheld, in rank order. This is
		// not derivable from the separations — a kept row can contradict a row
		// dropped against a different kept row (a chain's far end contradicts the
		// middle, which the near end dropped).
		p.conflictPartners = make(map[string][]string, len(keptSet))
		for _, id := range involved {
			if withheld[id] {
				continue
			}
			var partners []string
			for _, nb := range adj[id] {
				if withheld[nb] {
					partners = append(partners, nb)
				}
			}
			if len(partners) > 0 {
				sort.Slice(partners, func(i, j int) bool { return rank[partners[i]] < rank[partners[j]] })
				p.conflictPartners[id] = partners
			}
		}
		// Remove the withheld rows, preserving rank order. A fresh slice, not
		// p.rows[:0]: the candidate set is the retriever's return value and the
		// contract says it is the widened untrimmed result, so compacting into
		// its backing array would leave the caller holding stale rows.
		if len(dropped) > 0 {
			keptRows := make([]memory.Candidate, 0, len(p.rows)-len(dropped))
			keptItems := make([]Item, 0, len(p.items)-len(dropped))
			for i, c := range p.rows {
				if withheld[c.ID] {
					continue
				}
				keptRows = append(keptRows, c)
				keptItems = append(keptItems, p.items[i])
			}
			p.rows, p.items = keptRows, keptItems
		}
	}
	p.blockNotes = append(p.blockNotes, notes...)
	p.trace.record(stageConflicts, in, len(p.rows), dropped, append(append([]string(nil), stageNotes...), notes...)...)
}

// contradictionBeats reports whether a should win the contradiction tie-break
// against b. The keys are compared in the documented order and the first that
// differs decides; an exact tie reports false, which leaves b (the earlier-ranked
// row) as the winner.
func contradictionBeats(a, b memory.Candidate) bool {
	if a.Pinned != b.Pinned {
		return a.Pinned
	}
	if av, bv := contradictionVerifiedAt(a), contradictionVerifiedAt(b); !av.Equal(bv) {
		return av.After(bv)
	}
	if at, bt := contradictionStamp(a), contradictionStamp(b); !at.Equal(bt) {
		return at.After(bt)
	}
	return false
}

// contradictionVerifiedAt is a row's verified_at as a comparable instant. An
// absent stamp is the zero time, so any real stamp beats it — a stamp somebody
// checked beats a stamp nobody did.
func contradictionVerifiedAt(c memory.Candidate) time.Time {
	if c.VerifiedAt == nil {
		return time.Time{}
	}
	t, _ := memory.ParseStamp(*c.VerifiedAt)
	return t
}

// contradictionStamp is the freshness key: updated_at when the row has one, and
// created_at when it does not. An unparseable value is read as ancient, which is
// how the rest of the assembler treats a stamp it cannot read and stops a bad
// value from winning.
func contradictionStamp(c memory.Candidate) time.Time {
	if c.UpdatedAt != "" {
		return parseStamp(c.UpdatedAt)
	}
	return parseStamp(c.CreatedAt)
}

// runDedup is stage 6. The retriever reorders the window by supersede and
// near-duplicate edges already, and the policy for dropping a demoted loser is
// the CALLER's — `Slice.DropDemotedLosers`, which reaches the retriever through
// `passivePolicies` and which the session-start surface sets for `_global`.
//
// The retriever's removals are RECORDED here. A removed loser never enters
// CandidateSet.Rows, and the pairwise judgement was made over edges this pipeline
// never saw, so the stage cannot decide it; it takes the verdicts the retriever
// reported (CandidateSet.DroppedLosers) and files each as a drop at this stage
// with the ids it lost to. That is what puts the row in the trace, in the
// retrieval record, in the per-bucket tally and in explain: one source for all
// four. A set with no removals records the stage as a pass-through.
func runDedup(p *pipeline) {
	// The sentence is about what the RETRIEVER did, and a passive bucket can have
	// had losers removed rather than ranked last — so the old wording ("no source
	// policy drops losers on this surface yet") would be false for exactly the
	// surface it was written for. It is derived from the request rather than
	// asserted, because the request is where the policy is stated.
	note := "near-duplicate reordering is applied by the retriever over the window"
	// GATED ON THE MODE, then on the flag, and both gates are the point. A
	// query-mode request reaches no passive policy (`passivePolicies` returns
	// nil for a query) but its retriever now REMOVES the loser outright (#926),
	// so the old "no source policy drops losers on this surface yet" would be
	// false of exactly the surface it was written for — an operator told no
	// removal happens here would be looking at a window holding one row of each
	// pair. The sentence is stated as the MODE's behaviour rather than as a
	// per-row outcome: whether this particular window held a pair is the
	// per-row decisions below' business.
	if !p.passive {
		note = "near-duplicate losers are REMOVED by the retriever, so the answer holds one row of every pair the removal did not veto (a row stage 5 separated as a contradiction is already out of the block)"
	} else if p.dropsDemotedLosers() {
		// Stated as a POLICY: whether a row was removed is the per-row decisions'
		// business below, and a note that claimed a removal for every `_global`
		// slice that sets the flag would be a report about a prediction, on the
		// overwhelmingly common occasion that the window held no near-duplicate
		// edge at all.
		note += "; near-duplicate losers are REMOVED for the buckets whose policy asks for it, so the block holds one row of every pair the removal did not veto (a row stage 5 separated as a contradiction is already out of the block)"
	} else {
		note += "; no source policy drops losers on this surface yet"
	}
	p.blockNotes = append(p.blockNotes, note)

	in := len(p.rows)
	notes := []string{note}
	var dropped []string
	p.losers = make(map[string]memory.DroppedLoser, len(p.set.DroppedLosers))
	for _, l := range p.set.DroppedLosers {
		// A row already in the window is not a removed one: the set's own account
		// of it (kept or dropped by an earlier stage) stands, and a second verdict
		// would count one row twice.
		if _, dup := p.losers[l.ID]; dup || p.droppedAlready(l.ID) || p.inRows(l.ID) {
			continue
		}
		p.losers[l.ID] = l
		p.trace.Decisions = append(p.trace.Decisions, Decision{
			ID: l.ID, ProjectID: l.ProjectID, Stage: stageDedup, Reason: reasonNearDuplicate,
			Before: l.Score, Against: append([]string(nil), l.LostTo...),
		})
		p.dropped[l.ID] = reasonNearDuplicate
		p.droppedBy[stageDedup]++
		dropped = append(dropped, l.ID)
	}
	if len(dropped) > 0 {
		notes = append(notes, formatNote("%d near-duplicate loser(s) removed by the retriever; each is a decision at this stage naming the row it lost to", len(dropped)))
	}
	p.trace.record(stageDedup, in+len(dropped), in, dropped, notes...)
}

func (p *pipeline) droppedAlready(id string) bool {
	_, ok := p.dropped[id]
	return ok
}

func (p *pipeline) inRows(id string) bool {
	for _, c := range p.rows {
		if c.ID == id {
			return true
		}
	}
	return false
}

// dropsDemotedLosers reports whether any bucket in the request ASKS the retriever
// to remove near-duplicate losers rather than rank them last. It is about the
// request, not the result: the removal happened in the retriever, over a window
// whose edges this pipeline never saw, so only the policy can be reported here.
func (p *pipeline) dropsDemotedLosers() bool {
	for _, s := range p.req.Budget.Slices {
		if s.DropDemotedLosers {
			return true
		}
	}
	return false
}

// diversityShareWindow is one window the share divides: the candidate indices it
// covers, in rank order, the number of rows it admits and the most of them one
// category may take.
//
// A SLICED request carries one window per bucket that states an item cap, and
// that is not a refinement — it is the rule. Stage 8 caps each bucket over the
// rows IT admits, so a share that divided one window across every bucket would
// let one bucket's share evict another bucket's row: the eviction moves the row
// behind the shared window, and stage 8 then keeps it under its own bucket's cap,
// which leaves a deferral verdict on a row the answer renders and a header that
// says a row it is showing was withheld. Dividing per bucket makes the two stages
// agree about which rows are in play, because they then read the same cap for the
// same rows.
type diversityShareWindow struct {
	rows  []int
	slots int
	share int
}

// runDiversity is stage 7: a per-category SHARE of the window (#927), on PASSIVE
// reads only.
//
// A small digest is easy to fill with rows from one category while the
// next-ranked rows of every other category are cut, and a reader gets a block
// about one thing. The share is bounded so it cannot do that: inside a window no
// category may take more than half the slots, rounded up, and never fewer than
// one.
//
// WHY PASSIVE ONLY, AND WHY IT IS THE MODE AND NOT THE SOURCE. A query is a
// relevance question and the ranking is the whole answer to it: `ghost_memory_search`
// asked "what matches", so moving the row that matches behind another row because
// of what it is ABOUT is the stage answering a question nobody asked. A passive
// read asks no question at all — it hands a model everything worth knowing before
// a turn — and there breadth is the point, because a digest that is all one
// category teaches one thing. The gate is the request-bound `p.passive` (an empty
// `Query`, bound once at Run) rather than `Source`, for the reason the pipeline's
// own field gives: the consequences follow from the absence of a query, and a
// Source-keyed branch would let a future passive source be shared by a query-mode
// one. A query-mode request is a recorded pass-through here — no deferral, no
// backfill, no per-row verdict, no drop — so search is unchanged by this stage
// byte for byte.
//
// It is a DEFERRAL and never a deletion, and the five rules that follow from
// that are the whole design:
//
//   - The stage runs only when the candidates left after validity, conflicts and
//     dedup EXCEED a window. Everything that fits is left exactly as it was,
//     so a block that never had to choose is byte-for-byte the block it was.
//   - A row over the share is MOVED, in its existing relative order, to just
//     after the window, so the next-ranked rows of other categories take the
//     slots it vacated. Nothing is removed, so the window is never shrunk: if
//     the other categories cannot fill it, the deferred rows come back in their
//     original order until it is full again.
//   - A PINNED row is never deferred, and it still counts toward its category's
//     share. A pin is a slot guarantee (#936), and a stage that moved a pinned
//     row behind the window would take the guarantee back with a deferral.
//   - Nothing is reordered inside a category. The stage writes a permutation of
//     the ranking that keeps every category's relative order, so the retriever's
//     order — which carries the keyword reservation, the status demotion, decay
//     and both demotions — survives it.
//   - A verdict is filed only for a row the stage MOVED. A row it deferred and
//     then readmitted is recorded as considered, and a row stage 8 admits keeps
//     no drop verdict — `trim`, which is where membership is decided, withdraws
//     it, and a slice's byte cap is one shipped shape that gets there (see
//     `withdrawDeferral`).
func runDiversity(p *pipeline) {
	in := len(p.rows)
	if !p.passive {
		// A query-mode read: the stage is a recorded no-op, so a reader can see
		// it ran and declined rather than inferring it was skipped.
		note := "diversity shares the window on a passive read only: this request carries a " +
			"query, so the ranking decides it and the share would move the row that answered it, " +
			"so no row was deferred"
		p.blockNotes = append(p.blockNotes, note)
		p.trace.record(stageDiversity, in, in, nil, note)
		return
	}
	if len(p.items) != in {
		// rows and items are index-aligned by every stage above, and a stage
		// that permuted one without the other would render a row's content
		// beside another row's id. Nothing here can repair that, so the stage
		// declines to act and says which invariant it found broken.
		note := "diversity did not run: this block's candidate rows and their items " +
			"disagree in length, so no reordering was attempted"
		p.blockNotes = append(p.blockNotes, note)
		p.trace.record(stageDiversity, in, in, nil, note)
		return
	}

	windows := p.diversityWindows()
	acting := 0
	for _, w := range windows {
		if len(w.rows) > w.slots {
			acting++
		}
	}
	if acting == 0 {
		// No window to divide: either the block bounds no rows at all, or every
		// bucket's rows already fit under its own cap. The answer is the budget
		// stage's to bound, and the retrieval ceiling is NOT a substitute for a
		// window because it sizes the FETCH, not the block.
		total := p.admitCap()
		if total <= 0 {
			note := "diversity divides a window, and this request states no item cap: " +
				"the answer is bounded by bytes alone, so there is no window to divide"
			p.blockNotes = append(p.blockNotes, note)
			p.trace.record(stageDiversity, in, in, nil, note)
			return
		}
		share := diversityShare(total)
		// Two populations, and the sentence names the one it actually measured.
		// `in` is every candidate — including rows in buckets the windows skipped
		// — while `total` is the cap, so on a request whose slices carry only byte
		// caps, or one bucket with no item cap beside a capped one, `in` can
		// exceed `total` and "N candidates fit under it" would be false.
		note := fmt.Sprintf("diversity is a per-category share of the %d-row window, "+
			"no more than %d slots for any one category: ", total, share)
		if in <= total {
			note += fmt.Sprintf("%d candidates fit under it, so no row was deferred", in)
		} else {
			note += "no window overflowed, so no row was deferred"
		}
		p.blockNotes = append(p.blockNotes, note)
		p.trace.record(stageDiversity, in, in, nil, note)
		return
	}

	// Walk each window. `kept` is the set of rows that stay inside their window,
	// `moved` the rows the share took out of it, and `returned` the subset of
	// `kept` that came back because the other categories ran out of rows.
	kept := make(map[int]bool, in)
	moved := make(map[int]diversityDeferral, in)
	returned := make(map[int]bool, in)
	for _, w := range windows {
		if len(w.rows) <= w.slots {
			continue
		}
		k, back, def := p.diversityWalk(w)
		for _, i := range k {
			kept[i] = true
		}
		for _, i := range back {
			returned[i] = true
		}
		for _, i := range def {
			moved[i] = diversityDeferral{category: p.rows[i].Category, share: w.share, slots: w.slots}
		}
	}
	if p.deferred == nil {
		p.deferred = map[string]diversityDeferral{}
	}

	// Recorded from the ORIGINAL order — so a reader meets each verdict in rank
	// order, and the ids are the ones the ranking gave — and recorded BEFORE the
	// rows are permuted, because `moved` and `returned` are indexed by rank.
	// Recording after the permutation would file row N's verdict under row N's
	// NEW identity: a deferral verdict landing on a row the answer renders, which
	// is the header-honesty break the per-bucket window exists to prevent.
	stageNotes := make([]string, 0, len(moved))
	dropped := make([]string, 0, len(moved))
	for i := 0; i < in; i++ {
		c := p.rows[i]
		if returned[i] {
			p.trace.keep(c.ID, c.ProjectID, stageDiversity, reasonDiversityBackfilled, c.Score)
			continue
		}
		d, movedRow := moved[i]
		if !movedRow {
			continue
		}
		dropped = append(dropped, c.ID)
		p.dropped[c.ID] = reasonDiversityDeferred
		p.droppedBy[stageDiversity]++
		p.deferred[c.ID] = d
		p.trace.decide(c.ID, c.ProjectID, stageDiversity, reasonDiversityDeferred, c.Score)
		stageNotes = append(stageNotes, formatNote(
			"diversity deferred %s: category %s had taken the %d slots half its bucket's %d-row window allows",
			ShortID(c.ID), Token(c.Category), d.share, d.slots))
	}

	// The permutation itself: every row that stayed inside its window, IN RANK
	// ORDER, then every row that moved behind it, in rank order.
	//
	// Ordering each half by rank is what keeps every category's relative order.
	// The walk fills a window in rank order and then appends the rows that came
	// back, so a pinned row admitted from behind the window can outrank an
	// earlier-deferred row of its own category; ordering the kept set by rank
	// undoes exactly that and nothing else, because the SET of rows in the window
	// is unchanged and stage 8 admits the same rows either way.
	//
	// The move group is ordered by rank for the same reason, and that is what
	// keeps one bucket's reordering from disturbing another bucket: every bucket
	// keeps its own rows in its own relative order, so stage 8's per-bucket caps
	// admit the same rows they would have admitted unshared.
	//
	// Fresh slices, not p.rows[:0]: the candidate set is the retriever's return
	// value and the contract says it is the widened untrimmed result, so
	// compacting into its backing array would leave the caller holding
	// duplicated, stale rows.
	order := make([]int, 0, in)
	for i := 0; i < in; i++ {
		if kept[i] {
			order = append(order, i)
		}
	}
	for i := 0; i < in; i++ {
		if !kept[i] {
			order = append(order, i)
		}
	}
	rows := make([]memory.Candidate, len(order))
	items := make([]Item, len(order))
	for dst, src := range order {
		rows[dst] = p.rows[src]
		items[dst] = p.items[src]
	}
	p.rows, p.items = rows, items

	note := diversitySummaryNote(len(dropped))
	p.blockNotes = append(p.blockNotes, note)

	p.trace.record(stageDiversity, in, in, dropped, append(stageNotes, note)...)
}

// diversityWindows is the set of windows this request's share divides.
//
// A SLICED request gets one window per bucket that states an item cap, sized by
// that slice's cap, because that cap is the bound stage 8 enforces over the rows
// it admits — see diversityShareWindow. A request with no slices gets the single total
// window `itemBound` states. A bucket whose slice states no item cap is left out
// entirely: stage 8 never cuts its rows on count, so there is nothing for a share
// to make room for, and moving its rows would only promote rows the ranking had
// decided against.
//
// The windows are ordered by the first row they contain, so the stage's notes and
// its permutation are both deterministic — a map iteration order here would make
// the trace a property of the hash seed.
func (p *pipeline) diversityWindows() []diversityShareWindow {
	var out []diversityShareWindow
	seen := map[string]bool{}
	if len(p.req.Budget.Slices) > 0 {
		for i := range p.rows {
			bucket := p.capBucket(i)
			if seen[bucket] {
				continue
			}
			seen[bucket] = true
			s := p.sliceFor(bucket)
			if s == nil || s.MaxItems <= 0 {
				continue
			}
			out = append(out, diversityShareWindow{rows: p.diversityBucket(bucket), slots: s.MaxItems, share: diversityShare(s.MaxItems)})
		}
		return out
	}
	window := p.admitCap()
	if window <= 0 {
		return nil
	}
	all := make([]int, len(p.rows))
	for i := range all {
		all[i] = i
	}
	return []diversityShareWindow{{rows: all, slots: window, share: diversityShare(window)}}
}

// diversityBucket is every candidate in one bucket, in rank order.
func (p *pipeline) diversityBucket(bucket string) []int {
	var out []int
	for i := range p.rows {
		if p.capBucket(i) == bucket {
			out = append(out, i)
		}
	}
	return out
}

// diversityWalk divides one window into the rows that stay inside it, the rows it
// deferred and then readmitted, and the rows it left behind the window.
//
// The line between the last two is drawn at the WINDOW, not at the share. A
// deferral moves a row to just after the window, which for a row already behind
// the window would move it UP — promoting exactly the rows the ranking had
// decided against. So only a row the window was holding is deferred; one behind it
// keeps its place and can still be admitted if a slot is free, because the
// "next-ranked rows of other categories take the slots" is what a vacated slot is
// FOR. A store whose rows are all one category is the case this settles: the
// share is filled, nothing else can take the slots, so every deferred row comes
// back and the stage records no drop rather than re-describing rows the budget
// was already going to cut.
func (p *pipeline) diversityWalk(w diversityShareWindow) (keep, backfilled, deferred []int) {
	head := make([]int, 0, w.slots)
	counts := make(map[string]int, 8)
	for pos, i := range w.rows {
		c := p.rows[i]
		switch {
		case len(head) < w.slots && (c.Pinned || counts[c.Category] < w.share):
			head = append(head, i)
			counts[c.Category]++
		case pos < w.slots:
			deferred = append(deferred, i)
		default:
			// Already behind the window: a deferral would move it UP.
		}
	}
	// The other categories ran out of rows before the window was full, so the
	// deferred rows come back in their original order until it is. This is what
	// makes the stage unable to shorten an answer: every slot below the window
	// is either held or vacated, and a vacated one that is not needed still
	// leaves its row behind the window, so the window ends up holding exactly
	// `slots` rows whatever the share did to which of them they are.
	for _, i := range deferred {
		if len(head) >= w.slots {
			break
		}
		backfilled = append(backfilled, i)
		head = append(head, i)
	}
	return head, backfilled, deferred[len(backfilled):]
}

// diversityShare is the most slots of the window one category may take: half the
// window, ROUNDED UP, and never fewer than one. A window of one admits one row
// and that row has a category, so a share of zero would defer the only row the
// answer could hold — which is the same answer the window always gave, reached
// by breaking the guarantee instead of keeping it.
func diversityShare(window int) int {
	if window < 1 {
		return 0
	}
	return (window + 1) / 2
}

// diversitySummaryNote is stage 7's summary line. It reports only the count of
// rows the stage moved, because that is the number the withdrawal can correct: a
// deferral on a row the budget stage admits turns the count down, so the sentence
// is rewritten from the stage record rather than left claiming a deferral the
// trace no longer holds.
func diversitySummaryNote(n int) string {
	return fmt.Sprintf("diversity deferred %d candidates behind their bucket's window: no category may take "+
		"more than half the slots of its own window, so the rows it moved make room for the next-ranked "+
		"rows of other categories", n)
}

// diversityDeferral is one row stage 7 moved behind the window and why: the
// category that had filled its share, and the share itself. explain reads it so
// the sentence can name the cap a row hit rather than repeat the token.
type diversityDeferral struct {
	category string
	share    int
	// slots is the window the share was half of, so the sentence can name it.
	slots int
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
		if s := p.sliceFor(p.capBucket(i)); s != nil && s.ClampBytes > 0 {
			clamped := clampBytes(items[i].Content, s.ClampBytes)
			items[i].Content = clamped
			items[i].Bytes = len(clamped)
			// The token estimate is derived from the bytes, so a clamp that
			// shortened the content has to shorten the estimate too. Leaving it
			// would report a per-item cost the answer no longer pays.
			items[i].Tokens = tokenEstimate(items[i].Bytes)
		}
	}

	// Per-bucket caps. A bucket with no slice is unbounded, which is what makes
	// a single-slice request mean "this bucket, this many rows".
	if len(p.req.Budget.Slices) > 0 {
		count := map[string]int{}
		bytes := map[string]int{}
		keepRow := make([]bool, len(rows))
		for i, it := range items {
			bucket := p.capBucket(i)
			s := p.sliceFor(bucket)
			if s == nil {
				keepRow[i] = true
				continue
			}
			overItems := s.MaxItems > 0 && count[bucket] >= s.MaxItems
			overBytes := s.MaxBytes > 0 && bytes[bucket]+it.Bytes > s.MaxBytes
			if overItems || overBytes {
				// WHICH bound cut the row is the caller's next step, so it is
				// counted rather than inferred afterwards: raising the row limit
				// does nothing for a row the byte cap cut, and an answer that
				// says "raise the limit" for one sends the caller round the same
				// loop with a bigger number. A row over both counts as the byte
				// cap, which is the stricter of the two.
				if overBytes {
					p.droppedByBound[boundSliceBytes]++
				} else {
					p.droppedByBound[boundSliceItems]++
				}
				continue
			}
			keepRow[i] = true
			count[bucket]++
			bytes[bucket] += it.Bytes
		}
		rows, items, dropped = trim(rows, items, keepRow, dropped, p, "slice_budget")
	}

	// The total cap, applied across buckets.
	if p.req.Budget.MaxItems > 0 && len(rows) > p.req.Budget.MaxItems {
		p.droppedByBound[boundTotalItems] += len(rows) - p.req.Budget.MaxItems
		keepRow := make([]bool, len(rows))
		for i := range p.req.Budget.MaxItems {
			keepRow[i] = true
		}
		rows, items, dropped = trim(rows, items, keepRow, dropped, p, "budget")
	}
	// Budget.MaxBytes is NOT applied here. It bounds the complete rendered
	// response, which this stage cannot measure: it owns item membership and the
	// framing belongs to the renderer. The response-fit post-pass in outcome.go
	// trims against it, one row at a time, and records what it removed. Applying
	// it to item CONTENT as well would bound the same budget twice with two
	// different units, so the effective limit would depend on how much framing
	// a caller's rows happened to need — and a cap could silently cut a row
	// before the post-pass ever saw the response that did not fit. Slice.MaxBytes
	// is the item-content cap; this is not it.

	p.rows, p.items = rows, items
	notes := []string(nil)
	if p.windowDisclosure != "" {
		notes = append(notes, p.windowDisclosure)
	}
	p.trace.record(stageBudget, in, len(rows), dropped, notes...)
}

// runRender is stage 9: the shared item renderer. Each surface keeps its own
// framing and field order around Line(); what is shared is the item line, so the
// same memory reads the same way in search output and in an injected block. The
// response-fit post-pass, which needs the framing to measure a complete
// response, is not part of this stage — it runs after the outcome.
func runRender(p *pipeline) {
	p.trace.record(stageRender, len(p.rows), len(p.items), nil)
}

// trim drops the rows keepRow marks false, recording each one.
func trim(rows []memory.Candidate, items []Item, keepRow []bool, dropped []string, p *pipeline, reason string) ([]memory.Candidate, []Item, []string) {
	keptRows := make([]memory.Candidate, 0, len(rows))
	keptItems := make([]Item, 0, len(items))
	for i := range rows {
		if !keepRow[i] {
			dropped = append(dropped, rows[i].ID)
			// ONE verdict per row. A row stage 7 deferred is still in the
			// candidate order — a deferral moves a row behind the window, it
			// does not remove it — so the budget stage is what actually cuts it,
			// and the row the trace has already judged keeps the verdict that
			// first excluded it. Filing a second one would count one row twice:
			// in the per-stage breakdown, in the bucket tally the passive header
			// reads, and in the retrieval record. The row is still listed as
			// removed by this stage, because it was.
			//
			// Only a PASSIVE read can reach this with a stage 7 verdict: the share
			// is a passive-read rule, so a query-mode answer is cut by this stage
			// with no earlier verdict to keep.
			if _, decided := p.dropped[rows[i].ID]; !decided {
				p.dropped[rows[i].ID] = reason
				p.droppedBy[stageBudget]++
				if p.passive && rows[i].Pinned {
					// Keyed on the row's OWN project, the key the decision below and
					// CountsFor use, so the count is a subset of the same bucket's RankedOut
					// even when a union bucket admits `_global` rows.
					bucket := rows[i].ProjectID
					p.trace.addPinnedCut(bucket, 1)
				}
				p.trace.decide(rows[i].ID, rows[i].ProjectID, stageBudget, reason, rows[i].Score)
			}
			continue
		}
		keptRows = append(keptRows, rows[i])
		keptItems = append(keptItems, items[i])
	}
	// A deferral verdict on a row this stage KEPT would be a lie: the answer
	// renders the row, and the trace, `dropped`, `droppedBy`, the bucket tally
	// and the stage record would all say it was withheld.
	//
	// WHEN THIS IS REACHED. Stage 7 leaves a deferred row BEHIND the window, so
	// what normally cuts it is the rows in front of it. A slice's MaxItems does
	// exactly that. A slice's MaxBytes is the case that breaks the accounting: it
	// cuts rows by CONTENT while the item count is still under its cap, so it can
	// remove every row in front of a deferred one and leave the deferred rows —
	// which this stage then admits. `MaxItems` and `MaxBytes` on the same slice,
	// which a real caller can state, is enough. So the withdrawal is not
	// belt-and-braces against a future refactor; it runs on a shipped shape.
	for i := range rows {
		if !keepRow[i] {
			continue
		}
		p.withdrawDeferral(rows[i].ID)
	}
	return keptRows, keptItems, dropped
}

// withdrawDeferral reverses a stage-7 deferral for a row stage 8 admitted: the
// row keeps place in the answer and loses the verdict that excluded it, in the
// trace, in `dropped` and in the per-stage count.
func (p *pipeline) withdrawDeferral(id string) bool {
	if p.dropped[id] != reasonDiversityDeferred {
		return false
	}
	delete(p.dropped, id)
	delete(p.deferred, id)
	if n := p.droppedBy[stageDiversity]; n > 0 {
		p.droppedBy[stageDiversity] = n - 1
	}
	return p.trace.withdrawDeferral(id)
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

// capBucket is the bucket whose cap governs row i, which is NOT always the row's
// own project.
//
// It is the policy that FETCHED the row, because that is where the caller stated
// the bound. A slice with IncludeGlobal set reads one project's rows and
// `_global`'s under a single cap, and the `_global` rows among them carry their
// own project id — so keying the cap on the row would find no slice for them at
// all and leave them unbounded, turning "at most N rows" into "at most N project
// rows, plus however many globals happened to rank nearby". That is the whole
// reason the retriever reports which policy admitted a row.
//
// The fallback is the row's own bucket, which is what every row with no
// `FetchedBy` gets: a query-mode candidate, and a passive row read by a policy
// that admits only its own bucket (the session-start shape, where the two are the
// same thing anyway).
func (p *pipeline) capBucket(i int) string {
	if i < len(p.rows) && p.rows[i].FetchedBy != "" {
		return p.rows[i].FetchedBy
	}
	if i < len(p.items) {
		return p.items[i].Bucket
	}
	return ""
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
	if breakdown := p.breakdownNote(); breakdown != "" {
		all = append(all, breakdown)
	}
	all = append(all, p.retrievalFailures...)
	if len(p.items) > 0 {
		all = append(all, p.blockNotes...)
		// Rendered here rather than at stage 5: the pair was recorded when both
		// endpoints were candidates, and stage 5 separated it, so exactly one
		// endpoint is in the answer. The sentence names that side kept and the
		// other withheld, which is true of the rows the answer actually holds —
		// where the stage's own sentence, that both were candidates there, is not.
		// A pair whose winner the budget later cut is not described: neither side
		// is in the block, so "kept" would name a row the reader cannot see.
		// Capped, because a graph with many contradicting pairs would otherwise
		// spend the whole note budget on conflicts and drop the rows' own
		// diagnostics.
		admitted := make(map[string]bool, len(p.items))
		for _, it := range p.items {
			admitted[it.ID] = true
		}
		var eligible [][2]string
		for _, pair := range p.contradictPairs {
			kept, withheld := pair[0], pair[1]
			switch {
			case admitted[kept] && admitted[withheld]:
				// Both endpoints survived: not a separated pair, so nothing to
				// report. This cannot arise while separation always removes one
				// side, and is stated rather than assumed so a future stage that
				// leaves both is silent here instead of claiming a separation.
				continue
			case admitted[kept]:
			case admitted[withheld]:
				kept, withheld = withheld, kept
			default:
				// Neither endpoint is in the block: the pair is no claim about
				// the answer, and naming its winner would send the reader to a
				// row they cannot see.
				continue
			}
			eligible = append(eligible, [2]string{kept, withheld})
		}
		// The count leads the pairs it qualifies. Bounding truncates from the
		// end, so a count placed after them is the first thing dropped under
		// pressure — and then the answer presents a capped list as the whole
		// block. A count, not a pointer: the stage record holds every pair, but
		// neither the answer nor the explain payload renders it, so promising it
		// would send an agent looking for something it cannot see.
		if held := len(eligible) - maxRenderedConflictPairs; held > 0 {
			all = append(all, formatNote("%d further contradicting pairs are in this block", held))
		}
		for i, pair := range eligible {
			if i == maxRenderedConflictPairs {
				break
			}
			all = append(all, formatNote("contradicts pair recorded and separated: %s kept, %s withheld", ShortID(pair[0]), ShortID(pair[1])))
		}
	}
	all = append(all, p.noteBuf...)
	return p.cutNotes(boundNotes(all, p.req.Budget.MaxNoteBytes, p.req.Budget.MaxNotesBytes))
}

// markConflicts sets Item.ConflictsWith on every row stage 5 kept that a live
// `contradicts` edge joined to a withheld row: the kept line names the rows it
// directly contradicts that were withheld, with ConflictsLabel bounding how many
// render at once. It is the one place the marker is decided, so every surface
// that renders Item.Line inherits it.
//
// It runs from fitResponse, on every pass, and not at stage 5: the response-fit
// post-pass can drop a row after the stages are done, and a survivor must not
// keep naming a partner the reader no longer has. The withheld rows are already
// gone from p.items when this runs, so the marker is rebuilt from the edges
// stage 5 recorded (conflictPartners) rather than projected from the separations:
// a kept line names the rows it directly contradicts that were withheld, in rank
// order. The stored list is whole; ConflictsLabel is what bounds the rendered
// line. Nothing is removed or reordered here.
func (p *pipeline) markConflicts() {
	present := make(map[string]bool, len(p.items))
	for _, it := range p.items {
		present[it.ID] = true
	}
	for i := range p.items {
		p.items[i].ConflictsWith = nil
		partners, ok := p.conflictPartners[p.items[i].ID]
		if !ok {
			continue
		}
		for _, w := range partners {
			// The withheld side is out of the answer by construction; the guard
			// is a defensive one for a row that somehow survived.
			if !present[w] {
				p.items[i].ConflictsWith = append(p.items[i].ConflictsWith, w)
			}
		}
	}
}

// markSuperseded sets Item.SupersededBy on each pinned row of a passive read whose
// superseder is still in the answer. The retriever names the superseders (it owns
// the edge rules); this keeps only the ones the reader can see, and runs from
// fitResponse beside markConflicts for the same reason: a response-fit drop can
// remove the superseder after the stages are done.
func (p *pipeline) markSuperseded() {
	present := make(map[string]bool, len(p.items))
	for _, it := range p.items {
		present[it.ID] = true
	}
	named := make(map[string][]string)
	for _, c := range p.set.Rows {
		if len(c.SupersededBy) > 0 {
			named[c.ID] = c.SupersededBy
		}
	}
	for i := range p.items {
		p.items[i].SupersededBy = nil
		if !p.passive || !p.items[i].Pinned {
			continue
		}
		for _, id := range named[p.items[i].ID] {
			if present[id] {
				p.items[i].SupersededBy = append(p.items[i].SupersededBy, id)
			}
		}
	}
}

// breakdownLeads reports whether the per-stage removal breakdown heads the note
// list, which is the only place notes() puts it. It leads because the
// response-fit pass cuts notes from the TAIL, so the breakdown is the last one
// to go — and the last one an answer can lose.
func (p *pipeline) breakdownLeads() bool {
	return len(p.rows) == 0 && len(p.dropped) > 0
}

// cutNotes applies the response-fit post-pass's tail cut. It is separate from
// boundNotes because the two bound different things: boundNotes keeps the list
// inside its byte budget by truncating, while this removes whole notes the
// rendered response had no room for. Truncating here instead would leave a
// half-note in an answer that had already decided to drop it.
func (p *pipeline) cutNotes(notes []string) []string {
	if p.noteCut <= 0 {
		return notes
	}
	if p.noteCut >= len(notes) {
		return nil
	}
	return notes[:len(notes)-p.noteCut]
}

// removalBreakdown is the per-stage removal count in pipeline order.
func (p *pipeline) removalBreakdown() string {
	// response_fit is last because it runs last: it is a Run post-pass, not a
	// stage, and counting it separately is what lets a reader tell a set the
	// pipeline emptied from one the byte cap emptied.
	order := []string{stageValidity, stagePredicates, stageProvenance, stageConflicts, stageDedup, stageDiversity, stageBudget, stageResponseFit}
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

// ShortID renders a row id for a note, so a trace line stays readable.
//
// It is EXPORTED, and it is the one implementation, because there were four
// spellings of "the first eight characters of an id" and this one had drifted
// from all of them (#810). A helper whose correctness is "a line is one line" and
// "eight is eight characters" is exactly the kind that must not be copyable.
//
// Two rules, and the order between them is the whole argument.
//
// EIGHT CHARACTERS, not bytes, and the measurement is `memref.Short`'s rather
// than a slice written here. An id is not necessarily hex — `ghost import` writes
// an artifact's ids verbatim, `RestoreSnapshot` reinstates what it recorded,
// `internal/bench` seeds `bench:<project>:<key>` — so `id[:8]` on a CJK id
// returned the first two runes and two bytes of the third, which is not text:
// a note a reader cannot read, and a ref the prefix query can never match. The
// hazard is recorded in three other places already (`memref.Short`'s own comment,
// `cmd/ghost/lifecycle.go`, and the resolution helpers), which is what a copied
// rule looks like once it is copied.
//
// It is worth being exact about how much of that is still load-bearing HERE,
// because the honest answer is "less than it looks" and the reader deserves it.
// Every rune `isTokenRune` writes bare is ASCII, so an id that reaches the
// measurement below is pure ASCII and byte count equals rune count — a byte cut
// at that line is unobservable today, and a mutation that restored one survives
// the suite. The call is kept anyway, for the two reasons that are not the test:
// it is the shared rule rather than a fourth spelling of it, and `isTokenRune`
// admitting a non-ASCII name — a plausible change for a user whose ids are not
// hex — is the one edit that would make the difference observable, and this keeps
// the note correct when it lands. What IS observable, and what the tests pin, is
// the property the byte cut broke: a note naming a multi-byte id is readable
// text, and a note is never invalid UTF-8.
//
// THEN through Token, and only a well-formed id is abbreviated. A note is
// rendered BARE — `assemblerNotes` writes it between "(Note: " and ")" with no
// «...» around it — so an id holding a newline forges a line and a `«` opens a
// data block of its own. An id Token had to quote is therefore returned WHOLE
// rather than truncated first. Truncating first is safe, because a cut of a
// newline-bearing id holds no newline once quoted; it is useless, because
// eight runes of an escape renders as `"AAAA\n- ["` — half an escape and nothing
// a reader can act on. So this ordering is a LEGIBILITY property and the test
// says so, which is the wording `internal/mcpserver`'s copy of this rule uses.
//
// A non-ASCII id is one Token must quote — `isTokenRune` writes bare only the
// ASCII set a stored name plausibly uses — so a CJK id arrives here in the
// ASCII-only quoted form rather than abbreviated. That is the same answer the
// mcpserver listings give for the same id, which is the point of sharing the
// function, and it is deliberately NOT what `cmd/ghost`'s and `memref`'s report
// forms do: those print to a terminal for a human to paste, where an id must
// stay copyable, and no note is pasted.
func ShortID(id string) string {
	// Empty stays empty rather than becoming the quoted empty string Token
	// renders it as: a note column showing `""` for a row with no id is noise,
	// and an empty id cannot forge a line.
	if id == "" {
		return ""
	}
	if rendered := Token(id); rendered != id {
		return rendered
	}
	return memref.Short(id)
}
