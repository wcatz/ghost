package assemble

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/memref"
)

type pipeline struct {
	req   Request
	mode  memory.ProjectMode
	set   *memory.CandidateSet
	trace *Trace

	passive bool

	rows              []memory.Candidate
	blockNotes        []string
	contradictPairs   [][2]string
	separations       []contradictionSeparation
	conflictPartners  map[string][]string
	contradictedBy    map[string][]string
	withheldIDs       map[string]bool
	windowDisclosure  string
	retrievalFailures []string
	qualifiers        []string
	items             []Item
	droppedBy         map[string]int
	noAnswer          *noAnswerVerdict
	exactCosine       map[string]float64
	droppedByBound    map[string]int
	noteBuf           []string
	dropped           map[string]string
	losers            map[string]memory.DroppedLoser
	deferred          map[string]diversityDeferral
	diversityAdmitted map[string]bool
	movedAhead        map[string]string
	noteCut           int
	firstKept         bool
}

type stage struct {
	name string
	run  func(*pipeline)
}

var stages = []stage{
	{name: stageValidity, run: runValidity},
	{name: stagePredicates, run: runPredicates},
	{name: stageProvenance, run: runProvenance},
	{name: stageConflicts, run: runConflicts},
	{name: stageDedup, run: runDedup},
	{name: stageDiversity, run: runDiversity},
	{name: stageLegConsensus, run: runLegConsensus},
	{name: stageCutoff, run: runCutoff},
	{name: stageNoAnswer, run: runNoAnswer},
	{name: stageBudget, run: runBudget},
	{name: stageRender, run: runRender},
}

func runValidity(p *pipeline) {
	in := len(p.rows)
	var dropped []string
	kept := make([]memory.Candidate, 0, len(p.rows))
	for _, c := range p.rows {
		v := readValidity(c, p.req.Now)
		if p.req.AsOf != nil {
			v.state = ""
		}
		sig := p.signal(c)
		sig.ValidityState = v.state
		for _, raw := range v.unparseable {
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
		case p.req.Covered != nil && p.req.Covered(c):
			dropped = append(dropped, c.ID)
			p.dropped[c.ID] = ReasonHostInstructions
			p.droppedBy[stagePredicates]++
			p.droppedBy[dropHostInstructions]++
			p.trace.decide(c.ID, c.ProjectID, stagePredicates, ReasonHostInstructions, c.Score)
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

	dropHostInstructions = "predicate:host_instructions"
)

// ReasonHostInstructions is the drop reason for a row Request.Covered reported:
// the host already loaded the same rule from its own instruction files.
const ReasonHostInstructions = "in_host_instructions"

const provenanceWeight = "1.0"

const maxRenderedConflictPairs = 5

func runProvenance(p *pipeline) {
	evByID := make(map[string]memory.EvidenceCounts, len(p.rows))
	confByID := make(map[string]*float64, len(p.rows))
	for _, c := range p.rows {
		evByID[c.ID] = c.Evidence
		confByID[c.ID] = c.Confidence
	}
	for id, sig := range p.trace.Signals {
		sig.Confidence = confByID[id]
		sig.ConfidenceContribution = 0
		sig.ProvenanceWeight = provenanceWeight
		sig.ProvenanceContribution = 0
		sig.Evidence = evByID[id]
		p.trace.Signals[id] = sig
	}
	p.trace.record(stageProvenance, len(p.rows), len(p.rows), nil,
		"provenance weight is pinned at 1.0: no measured threshold justifies scoring confidence yet")
}

type contradictionSeparation struct {
	withheld string
	kept     []string
}

func runConflicts(p *pipeline) {
	in := len(p.rows)
	var dropped []string
	var notes []string
	var stageNotes []string
	if p.set.EdgesStatus.Status == "err" {
		p.retrievalFailures = append(p.retrievalFailures, formatNote("edges_unavailable: the link lookup failed (%s), so conflict handling had no edges to read", p.set.EdgesStatus.Err))
	}
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
		switch {
		case p.set.EdgesStatus.Status == "unavailable" && p.set.EdgesStatus.Chunks > 1:
			notes = append(notes, formatNote("edges_partial: the link lookup read these candidates in %d queries, so a contradicting pair across a query boundary was not read — this block is not known to be free of one", p.set.EdgesStatus.Chunks))
		case p.set.EdgesStatus.Status == "unavailable":
			notes = append(notes, "edges_unavailable: no link joins two of these candidates")
		}
		seen := make(map[[2]string]bool, len(p.set.Edges))
		adj := make(map[string][]string, len(p.items))
		for _, e := range p.set.Edges {
			if e.Relation != "contradicts" || !admitted[e.From] || !admitted[e.To] {
				continue
			}
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
			if len(against) == 0 || candByID[id].Pinned {
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
		p.withheldIDs = withheld
		p.contradictedBy = make(map[string][]string)
		for _, id := range involved {
			if !keptSet[id] || !candByID[id].Pinned {
				continue
			}
			var newer []string
			for _, nb := range adj[id] {
				if contradictionStamp(candByID[nb]).After(contradictionStamp(candByID[id])) {
					newer = append(newer, nb)
				}
			}
			if len(newer) > 0 {
				sort.Slice(newer, func(i, j int) bool { return rank[newer[i]] < rank[newer[j]] })
				p.contradictedBy[id] = newer
			}
		}
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

func contradictionVerifiedAt(c memory.Candidate) time.Time {
	if c.VerifiedAt == nil {
		return time.Time{}
	}
	t, _ := memory.ParseStamp(*c.VerifiedAt)
	return t
}

func contradictionStamp(c memory.Candidate) time.Time {
	if c.UpdatedAt != "" {
		return parseStamp(c.UpdatedAt)
	}
	return parseStamp(c.CreatedAt)
}

func runDedup(p *pipeline) {
	note := "near-duplicate reordering is applied by the retriever over the window"
	if !p.passive {
		note = "near-duplicate losers are REMOVED by the retriever, so the answer holds one row of every pair the removal did not veto (a row stage 5 separated as a contradiction is already out of the block)"
	} else if p.dropsDemotedLosers() {
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

func (p *pipeline) standInEvidence(id string) []memory.DroppedLoser {
	var out []memory.DroppedLoser
	seen := map[string]bool{id: true}
	frontier := []string{id}
	for len(frontier) > 0 {
		cur := frontier[0]
		frontier = frontier[1:]
		for _, l := range p.losers {
			if seen[l.ID] {
				continue
			}
			for _, sid := range l.StandIns {
				if sid == cur {
					seen[l.ID] = true
					out = append(out, l)
					frontier = append(frontier, l.ID)
					break
				}
			}
		}
	}
	return out
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

func (p *pipeline) dropsDemotedLosers() bool {
	for _, s := range p.req.Budget.Slices {
		if s.DropDemotedLosers {
			return true
		}
	}
	return false
}

type diversityShareWindow struct {
	rows  []int
	slots int
	share int
}

func runDiversity(p *pipeline) {
	in := len(p.rows)
	if !p.passive {
		note := "diversity shares the window on a passive read only: this request carries a " +
			"query, so the ranking decides it and the share would move the row that answered it, " +
			"so no row was deferred"
		p.blockNotes = append(p.blockNotes, note)
		p.trace.record(stageDiversity, in, in, nil, note)
		return
	}
	if len(p.items) != in {
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
		total := p.admitCap()
		if total <= 0 {
			note := "diversity divides a window, and this request states no item cap: " +
				"the answer is bounded by bytes alone, so there is no window to divide"
			p.blockNotes = append(p.blockNotes, note)
			p.trace.record(stageDiversity, in, in, nil, note)
			return
		}
		share := diversityShare(total)
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

	kept := make(map[int]bool, in)
	moved := make(map[int]diversityDeferral, in)
	returned := make(map[int]bool, in)
	for _, w := range windows {
		if len(w.rows) <= w.slots {
			continue
		}
		k, back, def := p.diversityWalk(w)
		byRank := make(map[int]bool, w.slots)
		for _, i := range w.rows[:min(w.slots, len(w.rows))] {
			byRank[i] = true
		}
		if p.diversityAdmitted == nil {
			p.diversityAdmitted = map[string]bool{}
		}
		for _, i := range k {
			kept[i] = true
			if !byRank[i] {
				p.diversityAdmitted[p.rows[i].ID] = true
			}
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

func (p *pipeline) diversityBucket(bucket string) []int {
	var out []int
	for i := range p.rows {
		if p.capBucket(i) == bucket {
			out = append(out, i)
		}
	}
	return out
}

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
		}
	}
	for _, i := range deferred {
		if len(head) >= w.slots {
			break
		}
		backfilled = append(backfilled, i)
		head = append(head, i)
	}
	return head, backfilled, deferred[len(backfilled):]
}

func diversityShare(window int) int {
	if window < 1 {
		return 0
	}
	return (window + 1) / 2
}

func diversitySummaryNote(n int) string {
	return fmt.Sprintf("diversity deferred %d candidates behind their bucket's window: no category may take "+
		"more than half the slots of its own window, so the rows it moved make room for the next-ranked "+
		"rows of other categories", n)
}

type diversityDeferral struct {
	category string
	share    int
	slots    int
}

// maxSingleLegVectorRank and maxSingleLegFTSRank are the rank ceilings that
// admit a row found by only one retrieval leg into a QUERY block. Ranks are
// 0-based, so rank 0 is the best, and both ceilings are 0: a single-leg row is
// admitted only when its OWN leg ranked it first. A row found by both legs is
// always admitted; one the vector leg found alone survives only when
// VectorRank <= maxSingleLegVectorRank, and one the keyword leg found alone
// only when FTSRank <= maxSingleLegFTSRank. Tuned on the synthetic bench
// corpus (#1251), then re-measured for the injection-precision audit (#1304):
// the rank-1 and rank-2 bands the first tuning admitted were 3.3% and 1.1%
// relevant, so admitting them bought tokens rather than answers. Keep them
// named constants.
const (
	maxSingleLegVectorRank = 0
	maxSingleLegFTSRank    = 0
)

func legConsensusAdmits(c memory.Candidate) bool {
	switch {
	case c.KeywordReserved, c.FTSRank >= 0 && c.VectorRank >= 0:
		return true
	case c.FTSRank == -1 && c.VectorRank >= 0 && c.VectorRank <= maxSingleLegVectorRank:
		return true
	case c.VectorRank == -1 && c.FTSRank >= 0 && c.FTSRank <= maxSingleLegFTSRank:
		return true
	}
	return false
}

func runLegConsensus(p *pipeline) {
	in := len(p.rows)
	if p.passive {
		p.trace.record(stageLegConsensus, in, in, nil)
		return
	}
	var ftsLive, vecLive bool
	if p.set != nil && p.set.Legs != nil {
		fs, ok := p.set.Legs["fts"]
		ftsLive = ok && fs.Applicable && fs.Available
		vs, ok := p.set.Legs["vector"]
		vecLive = ok && vs.Applicable && vs.Available
	} else {
		hasFTS, hasVec := false, false
		for _, c := range p.rows {
			if c.FTSRank >= 0 {
				hasFTS = true
			}
			if c.VectorRank >= 0 {
				hasVec = true
			}
		}
		ftsLive, vecLive = hasFTS, hasVec
	}
	if !ftsLive || !vecLive {
		p.trace.record(stageLegConsensus, in, in, nil)
		return
	}
	if in == 0 {
		p.trace.record(stageLegConsensus, in, 0, nil)
		return
	}
	keptRows := make([]memory.Candidate, 0, in)
	keptItems := make([]Item, 0, in)
	var dropped []string
	for i := range p.rows {
		c := p.rows[i]
		bothLegs := c.FTSRank >= 0 && c.VectorRank >= 0
		if c.Unembedded {
			keptRows = append(keptRows, c)
			keptItems = append(keptItems, p.items[i])
			continue
		}
		kept := i == 0 || c.Pinned || bothLegs || legConsensusAdmits(c)
		for _, d := range p.standInEvidence(c.ID) {
			if kept {
				break
			}
			kept = d.Unembedded || d.Pinned || legConsensusAdmits(d.Candidate)
		}
		if kept {
			keptRows = append(keptRows, c)
			keptItems = append(keptItems, p.items[i])
			continue
		}
		dropped = append(dropped, c.ID)
		p.dropped[c.ID] = reasonLegConsensus
		p.droppedBy[stageLegConsensus]++
		p.trace.decide(c.ID, c.ProjectID, stageLegConsensus, reasonLegConsensus, c.Score)
	}
	p.rows, p.items = keptRows, keptItems
	p.trace.record(stageLegConsensus, in, len(keptRows), dropped)
}

func standsInAboveFloor(displaced []memory.DroppedLoser, floor float64) bool {
	for _, d := range displaced {
		if d.Pinned || d.KeywordReserved || d.Base >= floor {
			return true
		}
	}
	return false
}

func runCutoff(p *pipeline) {
	in := len(p.rows)
	share := p.req.RelevanceCutoff
	if p.passive {
		note := "relevance cutoff is a query-mode rule: this request carries no query, so the " +
			"block is a digest and the ranking decides it, and no row was cut"
		p.blockNotes = append(p.blockNotes, note)
		p.trace.record(stageCutoff, in, in, nil, note)
		return
	}
	if share <= 0 || in == 0 {
		p.trace.record(stageCutoff, in, in, nil)
		return
	}
	topBase := p.rows[0].Base
	floor := share * topBase
	keptRows := make([]memory.Candidate, 0, in)
	keptItems := make([]Item, 0, in)
	var dropped []string
	for i := range p.rows {
		c := p.rows[i]
		switch {
		case i == 0 || c.Pinned || c.KeywordReserved || c.Base >= floor,
			standsInAboveFloor(p.standInEvidence(c.ID), floor):
			keptRows = append(keptRows, c)
			keptItems = append(keptItems, p.items[i])
			continue
		}
		dropped = append(dropped, c.ID)
		p.dropped[c.ID] = reasonRelevanceCutoff
		p.droppedBy[stageCutoff]++
		p.trace.decide(c.ID, c.ProjectID, stageCutoff, reasonRelevanceCutoff, c.Score)
	}
	p.rows, p.items = keptRows, keptItems

	note := cutoffSummaryNote(len(dropped), share)
	p.blockNotes = append(p.blockNotes, note)
	p.trace.record(stageCutoff, in, len(keptRows), dropped, note)
}

func cutoffSummaryNote(n int, share float64) string {
	if n == 0 {
		return fmt.Sprintf("relevance cutoff kept every row: no row's fused Base fell below %s of the top match's Base",
			cutoffShareLabel(share))
	}
	return fmt.Sprintf("relevance cutoff stopped the answer where relevance fell off: %d row(s) scored below %s of the top match's Base and were cut",
		n, cutoffShareLabel(share))
}

func cutoffShareLabel(share float64) string {
	return fmt.Sprintf("%.0f%%", share*100)
}

type noAnswerVerdict struct {
	top, bar float64
	withheld int
}

func runNoAnswer(p *pipeline) {
	in := len(p.rows)
	bar := p.req.NoAnswerCosine
	if !p.passive && bar <= 0 && p.req.NoAnswerBarNote != "" {
		p.trace.record(stageNoAnswer, in, in, nil, p.req.NoAnswerBarNote)
		return
	}
	if p.passive || bar <= 0 || in == 0 || !p.vectorLegInPlay() {
		p.trace.record(stageNoAnswer, in, in, nil)
		return
	}
	top, judged := -1.0, false
	for i := range p.rows {
		c := p.rows[i]
		cos, has := p.rowCosine(c)
		if c.Pinned || !has {
			continue
		}
		judged = true
		if cos > top {
			top = cos
		}
	}
	if !judged || top >= bar {
		p.trace.record(stageNoAnswer, in, in, nil)
		return
	}
	keptRows := make([]memory.Candidate, 0, in)
	keptItems := make([]Item, 0, in)
	var dropped []string
	for i := range p.rows {
		c := p.rows[i]
		_, has := p.rowCosine(c)
		if c.Pinned || !has {
			keptRows = append(keptRows, c)
			keptItems = append(keptItems, p.items[i])
			continue
		}
		dropped = append(dropped, c.ID)
		p.dropped[c.ID] = reasonNothingClearedBar
		p.droppedBy[stageNoAnswer]++
		p.trace.decide(c.ID, c.ProjectID, stageNoAnswer, reasonNothingClearedBar, c.Score)
	}
	p.rows, p.items = keptRows, keptItems

	if len(keptRows) == 0 {
		p.noAnswer = &noAnswerVerdict{top: top, bar: bar, withheld: len(dropped)}
	} else {
		p.noAnswer = nil
	}
	note := noAnswerNote(top, bar, len(dropped))
	p.blockNotes = append(p.blockNotes, note)
	p.trace.record(stageNoAnswer, in, len(keptRows), dropped, note)
}

func noAnswerNote(top, bar float64, n int) string {
	return fmt.Sprintf("no-answer bar: the best vector match scored cosine %.3f, below the %.3f bar, so %d row(s) that carry a cosine were withheld",
		top, bar, n)
}

func runBudget(p *pipeline) {
	p.arrangeReplacements()
	in := len(p.rows)
	rows, items, dropped := p.rows, p.items, []string(nil)

	for i := range items {
		if s := p.sliceFor(p.capBucket(i)); s != nil && s.ClampBytes > 0 {
			clamped := clampBytes(items[i].Content, s.ClampBytes)
			items[i].Content = clamped
			items[i].Bytes = len(clamped)
			items[i].Tokens = tokenEstimate(items[i].Bytes)
		}
	}

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

	if p.req.Budget.MaxItems > 0 && len(rows) > p.req.Budget.MaxItems {
		p.droppedByBound[boundTotalItems] += len(rows) - p.req.Budget.MaxItems
		keepRow := make([]bool, len(rows))
		for i := range p.req.Budget.MaxItems {
			keepRow[i] = true
		}
		rows, items, dropped = trim(rows, items, keepRow, dropped, p, "budget")
	}

	p.rows, p.items = rows, items
	notes := []string(nil)
	if p.windowDisclosure != "" {
		notes = append(notes, p.windowDisclosure)
	}
	p.trace.record(stageBudget, in, len(rows), dropped, notes...)
}

func runRender(p *pipeline) {
	p.trace.record(stageRender, len(p.rows), len(p.items), nil)
}

func trim(rows []memory.Candidate, items []Item, keepRow []bool, dropped []string, p *pipeline, reason string) ([]memory.Candidate, []Item, []string) {
	keptRows := make([]memory.Candidate, 0, len(rows))
	keptItems := make([]Item, 0, len(items))
	for i := range rows {
		if !keepRow[i] {
			dropped = append(dropped, rows[i].ID)
			if _, decided := p.dropped[rows[i].ID]; !decided {
				p.dropped[rows[i].ID] = reason
				p.droppedBy[stageBudget]++
				if p.passive && rows[i].Pinned {
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
	for i := range rows {
		if !keepRow[i] {
			continue
		}
		p.withdrawDeferral(rows[i].ID)
	}
	return keptRows, keptItems, dropped
}

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

func (p *pipeline) sliceFor(bucket string) *Slice {
	for i := range p.req.Budget.Slices {
		if p.req.Budget.Slices[i].Bucket == bucket {
			return &p.req.Budget.Slices[i]
		}
	}
	return nil
}

func (p *pipeline) capBucket(i int) string {
	if i < len(p.rows) && p.rows[i].FetchedBy != "" {
		return p.rows[i].FetchedBy
	}
	if i < len(p.items) {
		return p.items[i].Bucket
	}
	return ""
}

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

func (p *pipeline) notes() []string {
	all := make([]string, 0, len(p.noteBuf)+len(p.blockNotes)+len(p.contradictPairs)+1)
	if breakdown := p.breakdownNote(); breakdown != "" {
		all = append(all, breakdown)
	}
	all = append(all, p.retrievalFailures...)
	if len(p.items) > 0 {
		all = append(all, p.blockNotes...)
		admitted := make(map[string]bool, len(p.items))
		for _, it := range p.items {
			admitted[it.ID] = true
		}
		var eligible [][2]string
		for _, pair := range p.contradictPairs {
			kept, withheld := pair[0], pair[1]
			switch {
			case admitted[kept] && admitted[withheld]:
				continue
			case admitted[kept]:
			case admitted[withheld]:
				kept, withheld = withheld, kept
			default:
				continue
			}
			eligible = append(eligible, [2]string{kept, withheld})
		}
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

func (p *pipeline) markConflicts() {
	present := make(map[string]bool, len(p.items))
	for _, it := range p.items {
		present[it.ID] = true
	}
	for i := range p.items {
		p.items[i].ConflictsWith = nil
		p.items[i].ContradictedBy = nil
		for _, w := range p.contradictedBy[p.items[i].ID] {
			if present[w] != p.withheldIDs[w] {
				p.items[i].ContradictedBy = append(p.items[i].ContradictedBy, w)
			}
		}
		partners, ok := p.conflictPartners[p.items[i].ID]
		if !ok {
			continue
		}
		for _, w := range partners {
			if !present[w] {
				p.items[i].ConflictsWith = append(p.items[i].ConflictsWith, w)
			}
		}
	}
}

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
		for _, id := range named[p.items[i].ID] {
			if present[id] {
				p.items[i].SupersededBy = append(p.items[i].SupersededBy, id)
			}
		}
	}
}

func (p *pipeline) arrangeReplacements() {
	if !p.passive {
		return
	}
	named := false
	for _, c := range p.rows {
		if len(c.MaybeReplacedBy) > 0 {
			named = true
			break
		}
	}
	if !named {
		return
	}
	var buckets []string
	members := map[string][]int{}
	for i := range p.rows {
		b := p.capBucket(i)
		if _, ok := members[b]; !ok {
			buckets = append(buckets, b)
		}
		members[b] = append(members[b], i)
	}
	slot := make([]int, len(p.rows)) // the new order, as old indices
	for i := range slot {
		slot[i] = i
	}
	for _, b := range buckets {
		idx := members[b]
		limit := 0
		if s := p.sliceFor(b); s != nil {
			limit = s.MaxItems
		}
		order := p.arrangeBucket(append([]int(nil), idx...), limit)
		for k, pos := range idx {
			slot[pos] = order[k]
		}
	}
	rows := make([]memory.Candidate, len(p.rows))
	items := make([]Item, len(p.items))
	for dst, src := range slot {
		rows[dst], items[dst] = p.rows[src], p.items[src]
	}
	p.rows, p.items = rows, items
}

func (p *pipeline) displayReplacements(res *Result) {
	if p.passive || len(p.items) < 2 || len(p.rows) != len(p.items) {
		return
	}
	named := false
	for _, c := range p.rows {
		if len(c.MaybeReplacedBy) > 0 {
			named = true
			break
		}
	}
	if !named {
		return
	}
	order := make([]int, len(p.rows))
	for i := range order {
		order[i] = i
	}
	p.movedAhead = map[string]string{}
	order = p.arrangeBucket(order, 0)
	if len(p.movedAhead) == 0 {
		return
	}
	rows := make([]memory.Candidate, len(p.rows))
	items := make([]Item, len(p.items))
	for dst, src := range order {
		rows[dst], items[dst] = p.rows[src], p.items[src]
	}
	p.rows, p.items = rows, items
	res.Items = p.items
	res.Response = p.response(*res)
	res.Bytes = len(res.Response)
}

func (p *pipeline) arrangeBucket(order []int, limit int) []int {
	pos := func(id string) int {
		for k, i := range order {
			if p.rows[i].ID == id {
				return k
			}
		}
		return -1
	}
	placed := map[int]bool{}
	reserved := func(i int) bool {
		c := p.rows[i]
		return c.Pinned || c.FloorReserved || p.diversityAdmitted[c.ID] || placed[i]
	}
	move := func(from, to int) {
		i := order[from]
		order = append(order[:from], order[from+1:]...)
		order = append(order[:to], append([]int{i}, order[to:]...)...)
	}
	budget := len(order) * len(order)
	for k := 0; k < len(order) && budget > 0; k++ {
		budget--
		older := order[k]
		if p.rows[older].Pinned || (limit > 0 && k >= limit) {
			continue
		}
		for _, id := range p.rows[older].MaybeReplacedBy {
			at := pos(id)
			if at <= k {
				continue // absent, or already ahead
			}
			newer := order[at]
			if p.rows[newer].Pinned || p.deferred[p.rows[newer].ID].slots > 0 {
				continue
			}
			if limit == 0 || at < limit {
				move(at, k)
			} else {
				victim := -1
				for v := limit - 1; v >= 0; v-- {
					if v != k && !reserved(order[v]) {
						victim = v
						break
					}
				}
				if victim < 0 {
					continue
				}
				move(at, k) // the window now holds limit+1 rows
				if victim >= k {
					victim++
				}
				move(victim, limit) // the displaced row heads the rows behind the cap
			}
			placed[newer] = true
			if p.movedAhead != nil {
				p.movedAhead[p.rows[newer].ID] = p.rows[older].ID
			}
			k--
			break
		}
	}
	return order
}

func (p *pipeline) markMaybeReplaced() {
	present := make(map[string]bool, len(p.items))
	for _, it := range p.items {
		present[it.ID] = true
	}
	named := make(map[string][]string)
	for _, c := range p.set.Rows {
		if len(c.MaybeReplacedBy) > 0 {
			named[c.ID] = c.MaybeReplacedBy
		}
	}
	for i := range p.items {
		p.items[i].MaybeReplacedBy = nil
		for _, id := range named[p.items[i].ID] {
			if present[id] {
				p.items[i].MaybeReplacedBy = append(p.items[i].MaybeReplacedBy, id)
			}
		}
	}
}

func (p *pipeline) breakdownLeads() bool {
	return len(p.rows) == 0 && len(p.dropped) > 0
}

func (p *pipeline) cutNotes(notes []string) []string {
	if p.noteCut <= 0 {
		return notes
	}
	if p.noteCut >= len(notes) {
		return nil
	}
	return notes[:len(notes)-p.noteCut]
}

func (p *pipeline) removalBreakdown() string {
	order := []string{stageValidity, stagePredicates, stageProvenance, stageConflicts, stageDedup, stageDiversity, stageLegConsensus, stageCutoff, stageNoAnswer, stageBudget, stageResponseFit}
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
func ShortID(id string) string {
	if id == "" {
		return ""
	}
	if rendered := Token(id); rendered != id {
		return rendered
	}
	return memref.Short(id)
}
