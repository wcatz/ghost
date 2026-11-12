// Package supersede creates directed 'supersedes' and 'causes' links between
// memories where a newer memory replaces an older one about the same subject
// (supersedes), or a newer memory is an effect that followed from an older
package supersede

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/wcatz/ghost/internal/memory"
)

const maxNeighbors = 8

// Candidate is an ordered pair proposed for classification: Newer is the more
// recent memory that may supersede Older. The CreatedAt values are the notes'
type Candidate struct {
	NewerID        string
	NewerContent   string
	NewerCreatedAt string
	OlderID        string
	OlderContent   string
	OlderCreatedAt string
	Similarity     float32
}

// Relation is a classifier verdict on a NEWER/OLDER candidate pair.
type Relation string

const (
	RelationSupersedes Relation = "supersedes"
	RelationCauses     Relation = "causes"
	RelationNeither    Relation = "neither"
	RelationReversed   Relation = "reversed"
)

// Classifier decides the relationship for candidate pairs: a same-fact
// replacement (SUPERSEDES), a decision citing supporting evidence that stays
type Classifier interface {
	ClassifyBatch(ctx context.Context, pairs []Candidate) ([]Relation, error)
}

func contentHash(content string) string {
	sum := sha256.Sum256([]byte("v4\x00" + content))
	return hex.EncodeToString(sum[:])
}

type vectorStore interface {
	GetAll(ctx context.Context, projectID string, limit int) ([]memory.Memory, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	GetEmbedding(ctx context.Context, memoryID string) ([]float32, error)
	SearchVectorScoped(ctx context.Context, projectID string, queryVec []float32, limit int, scope map[string]string) ([]memory.ScoredMemory, error)
	CreateLinkJudged(ctx context.Context, sourceID, targetID, relation string, strength float32, source, judgedAt string) error
	CreateLinkUnopposed(ctx context.Context, sourceID, targetID, relation string, strength float32, source, judgedAt string) (bool, error)
	InvalidateLink(ctx context.Context, sourceID, targetID, relation string) (int64, error)
	LinksByRelationSource(ctx context.Context, projectID, relation, source string) ([]memory.Link, error)
	SupersedeChecked(ctx context.Context, projectID string) (map[[2]string]memory.SupersedeCheck, error)
	MarkSupersedeNeither(ctx context.Context, projectID string, checks map[[2]string]memory.SupersedeCheck) error
}

// Selection is what one candidate scan read: the ordered pairs it proposes, and
// how many of the project's memories it could not score at all. Both are needed
type Selection struct {
	Candidates      []Candidate
	Unscored        int
	Unoriented      int
	unorientedPairs map[pairKey]bool
}

// SelectCandidates returns the deduped ordered candidate pairs for a project:
// memories whose cosine similarity is at least threshold, oriented newer→older
func SelectCandidates(ctx context.Context, store vectorStore, projectID string, threshold float32) (Selection, error) {
	var sel Selection
	mems, err := store.GetAll(ctx, projectID, 100000)
	if err != nil {
		return sel, fmt.Errorf("load memories: %w", err)
	}
	byID := make(map[string]memory.Memory, len(mems))
	for _, m := range mems {
		byID[m.ID] = m
	}

	seen := make(map[pairKey]bool)
	var cands []Candidate
	for _, m := range mems {
		vec, err := store.GetEmbedding(ctx, m.ID)
		if err != nil || len(vec) == 0 {
			sel.Unscored++ // no usable embedding → no similarity candidates. A nil
			continue
		}
		neighbors, err := store.SearchVectorScoped(ctx, projectID, vec, maxNeighbors+1, m.Scope)
		if err != nil {
			return sel, fmt.Errorf("search vector for %s: %w", memory.RefusalArg("memory_id", m.ID), err)
		}
		for _, n := range neighbors {
			if n.MemoryID == m.ID || n.Score < threshold {
				continue
			}
			other, ok := byID[n.MemoryID]
			if !ok {
				fetched, err := store.GetByIDs(ctx, []string{n.MemoryID})
				if err != nil {
					return sel, fmt.Errorf("load neighbor %s: %w", memory.RefusalArg("memory_id", n.MemoryID), err)
				}
				if len(fetched) == 0 {
					continue
				}
				other = fetched[0]
				if other.ProjectID != projectID {
					continue // e.g. a _global neighbor not in this project's set
				}
				byID[other.ID] = other
			}
			if memory.RetentionExempt(m) || memory.RetentionExempt(other) {
				continue
			}
			if memory.ScopesConflict(m.Scope, n.Scope) {
				continue
			}
			key := newPairKey(m.ID, other.ID)
			if seen[key] {
				continue
			}
			seen[key] = true
			newer, older, ok := orient(m, other)
			if !ok {
				sel.Unoriented++
				if sel.unorientedPairs == nil {
					sel.unorientedPairs = make(map[pairKey]bool)
				}
				sel.unorientedPairs[key] = true
				continue
			}
			if newer.ID == older.ID {
				continue // identical ID (a store that minted one id twice)
			}
			cands = append(cands, Candidate{
				NewerID: newer.ID, NewerContent: newer.Content, NewerCreatedAt: newer.CreatedAt,
				OlderID: older.ID, OlderContent: older.Content, OlderCreatedAt: older.CreatedAt,
				Similarity: n.Score,
			})
		}
	}
	sel.Candidates = cands
	return sel, nil
}

type pairKey [2]string

func newPairKey(a, b string) pairKey {
	if a > b {
		return pairKey{b, a}
	}
	return pairKey{a, b}
}

func orient(a, b memory.Memory) (newer, older memory.Memory, ok bool) {
	if a.UpdatedAt != b.UpdatedAt {
		if a.UpdatedAt > b.UpdatedAt {
			return a, b, true
		}
		return b, a, true
	}
	if a.CreatedAt != b.CreatedAt {
		if a.CreatedAt > b.CreatedAt {
			return a, b, true
		}
		return b, a, true
	}
	return memory.Memory{}, memory.Memory{}, false
}

type liveEdge struct {
	relation Relation
	newer    string
	older    string
	source   string
	target   string
	stamp    string
	strength float32
}

func claimsFor(links []memory.Link) []liveEdge {
	out := make([]liveEdge, 0, len(links))
	for _, l := range links {
		e := liveEdge{source: l.SourceID, target: l.TargetID, stamp: l.CreatedAt, strength: l.Strength}
		switch l.Relation {
		case string(RelationSupersedes):
			e.relation, e.newer, e.older = RelationSupersedes, l.SourceID, l.TargetID
		case string(RelationCauses):
			e.relation, e.newer, e.older = RelationCauses, l.TargetID, l.SourceID
		default:
			continue
		}
		out = append(out, e)
	}
	return out
}

type pairState int

const (
	pairFresh pairState = iota
	pairDirected
	pairSupersedesCycle
	pairCauses
)

type pairVerdict struct {
	newer    string
	older    string
	stamp    string
	relation Relation
	state    pairState
}

func pairDirection(edges []liveEdge) pairVerdict {
	if len(edges) == 0 {
		return pairVerdict{state: pairFresh}
	}
	var sup, cau liveEdge
	haveSup, haveCau := false, false
	for _, e := range edges {
		switch e.relation {
		case RelationSupersedes:
			if !haveSup {
				sup, haveSup = e, true
				continue
			}
			if sup.newer != e.newer || sup.older != e.older {
				return pairVerdict{state: pairSupersedesCycle}
			}
			if e.stamp > sup.stamp {
				sup = e
			}
		case RelationCauses:
			if !haveCau {
				cau, haveCau = e, true
				continue
			}
			if e.stamp > cau.stamp {
				cau = e
			}
		}
	}
	switch {
	case haveSup:
		return pairVerdict{
			newer: sup.newer, older: sup.older, stamp: sup.stamp,
			relation: RelationSupersedes, state: pairDirected,
		}
	case haveCau:
		return pairVerdict{
			stamp: cau.stamp, relation: RelationCauses, state: pairCauses,
		}
	}
	return pairVerdict{state: pairFresh}
}

func claimsHold(claims map[pairKey][]liveEdge, newer, older string, relation Relation) bool {
	for _, e := range claims[newPairKey(newer, older)] {
		if e.relation == relation && e.newer == newer && e.older == older {
			return true
		}
	}
	return false
}

func verdictDrops(claims map[pairKey][]liveEdge, live, verdict Relation, newer, older string) (supersedes, causes int, suppressed bool) {
	about := claimsHold(claims, newer, older, RelationCauses)
	opposed := claimsHold(claims, older, newer, RelationCauses)
	switch verdict {
	case RelationSupersedes:
		if about {
			causes++
		}
		if opposed {
			causes++
		}
	case RelationCauses:
		if live == RelationSupersedes {
			supersedes = 1
		}
		if opposed {
			causes = 1
		}
	default:
		if live == RelationSupersedes {
			supersedes = 1
		}
		if about {
			causes++
		}
		if opposed {
			causes++
		}
	}
	return withholdSupersedesDrop(supersedes, causes)
}

const ordinaryPassWithdrawsSupersedes = false

func withholdSupersedesDrop(supersedes, causes int) (dropSup, dropCauses int, suppressed bool) {
	if supersedes > 0 && !ordinaryPassWithdrawsSupersedes {
		return 0, causes, true
	}
	return supersedes, causes, false
}

func sweepCausesBothWays(ctx context.Context, store vectorStore, claims map[pairKey][]liveEdge, newer, older string) (int, error) {
	dropped, err := store.InvalidateLink(ctx, older, newer, string(RelationCauses))
	if err != nil {
		return 0, fmt.Errorf("invalidate causes link %s→%s: %w", memory.RefusalArg("memory_id", older), memory.RefusalArg("memory_id", newer), err)
	}
	total := int(dropped)
	if !claimsHold(claims, older, newer, RelationCauses) {
		return total, nil
	}
	reverse, err := store.InvalidateLink(ctx, newer, older, string(RelationCauses))
	if err != nil {
		return total, fmt.Errorf("invalidate reverse causes link %s→%s: %w", memory.RefusalArg("memory_id", newer), memory.RefusalArg("memory_id", older), err)
	}
	return total + int(reverse), nil
}

// Classified pairs a Candidate with the verdict Run() reached for it — used
// by callers (the CLI) to report the actual relation written, not just that
type Classified struct {
	Candidate
	Relation           Relation
	JudgedAt           string
	Reclassified       bool
	ReclassifiedFrom   Relation
	Withdrawn          bool
	WithdrawSuppressed bool
	OpposedLive        bool
	TargetProjectID    string
	CausesDropped      int
	CausesDroppable    int
}

func (c *Classified) withdrewLiveEdge(dropped int64, causesDropped int) bool {
	if c.WithdrawSuppressed {
		return false
	}
	return c.Reclassified && (dropped > 0 || causesDropped > 0)
}

func withdrawSupersedesEdge(ctx context.Context, store vectorStore, c *Classified) (int64, error) {
	if c.WithdrawSuppressed {
		return 0, nil
	}
	return store.InvalidateLink(ctx, c.NewerID, c.OlderID, string(RelationSupersedes))
}

// Result summarizes a pass.
type Result struct {
	Candidates          int
	Confirmed           int // SUPERSEDES verdicts
	Created             int // supersedes links written (0 in dry-run)
	CausesCreated       int // CAUSES verdicts (not writes — see CausesWritten)
	CausesWritten       int
	Reclassified        int // existing links whose relation changed or was invalidated
	WithdrawSuppressed  int
	StaleSkipped        int
	StaleAtWrite        int
	Skipped             int // fresh pairs skipped via the NEITHER cache
	Unclassified        int // pairs skipped because the classifier answer was unparseable
	Reversed            int // REVERSED verdicts: refused, never written
	Vetoed              int
	Unscored            int
	ReclassifiedNoWrite int
	Unoriented          int
	OppositeLive        int
	Bidirectional       int
	ReverseLive         int
	Consensus           int
	ConsensusPairsAsked int
	NotAgreed           int
	Disputed            []Disputed
}

// Disputed is one pair the consensus gate refused, and it carries the evidence
// for the refusal rather than only its existence.
type Disputed struct {
	NewerID    string
	OlderID    string
	Tally      map[Relation]int
	Unreadable int
}

type voteCount struct {
	supersedes int
	causes     int
	neither    int
	reversed   int
	unreadable int
}

func (v *voteCount) add(r Relation) {
	switch r {
	case RelationSupersedes:
		v.supersedes++
	case RelationCauses:
		v.causes++
	case RelationNeither:
		v.neither++
	case RelationReversed:
		v.reversed++
	default:
		v.unreadable++
	}
}

func (v *voteCount) quorum(passes int) (Relation, verdictState) {
	if v.unreadable >= passes {
		return "", undecided
	}
	if v.unreadable > 0 {
		return "", disputed
	}
	switch {
	case v.supersedes == passes:
		return RelationSupersedes, agreed
	case v.causes == passes:
		return RelationCauses, agreed
	case v.neither == passes:
		return RelationNeither, agreed
	case v.reversed == passes:
		return RelationReversed, agreed
	}
	return "", disputed
}

type verdictState int

const (
	agreed verdictState = iota
	disputed
	undecided
)

func (v *voteCount) tally() map[Relation]int {
	m := make(map[Relation]int, 4)
	for rel, n := range map[Relation]int{
		RelationSupersedes: v.supersedes,
		RelationCauses:     v.causes,
		RelationNeither:    v.neither,
		RelationReversed:   v.reversed,
	} {
		if n > 0 {
			m[rel] = n
		}
	}
	if v.unreadable > 0 {
		m[Relation("")] = v.unreadable
	}
	return m
}

func (v *voteCount) best() (Relation, int) {
	best, n := Relation(""), 0
	for _, cand := range []struct {
		rel Relation
		n   int
	}{
		{RelationSupersedes, v.supersedes},
		{RelationCauses, v.causes},
		{RelationNeither, v.neither},
		{RelationReversed, v.reversed},
	} {
		if cand.n > n {
			best, n = cand.rel, cand.n
		}
	}
	return best, n
}

func judgedAt(snapshot map[string]memory.Memory, c Candidate) string {
	latest := ""
	for _, id := range [2]string{c.NewerID, c.OlderID} {
		if m, ok := snapshot[id]; ok && m.UpdatedAt > latest {
			latest = m.UpdatedAt
		}
	}
	return latest
}

// WouldWriteLinks reports whether an --apply pass would put a NEW link in the
// graph, which is what the CLI's dry-run hint promises. A confirmed pair and a
func (r Result) WouldWriteLinks() bool {
	return r.Confirmed > 0 || r.CausesCreated > 0 || r.Reclassified > r.ReclassifiedNoWrite
}

func endpointsExist(ctx context.Context, store vectorStore, ids ...string) (bool, error) {
	mems, err := store.GetByIDs(ctx, ids)
	if err != nil {
		return false, err
	}
	got := make(map[string]bool, len(mems))
	for _, m := range mems {
		got[m.ID] = true
	}
	for _, id := range ids {
		if !got[id] {
			return false, nil
		}
	}
	return true, nil
}

// Run selects fresh candidates, unions them with previously-created llm
// 'supersedes' links (so a pair's classification can be revisited as memory
func Run(ctx context.Context, store vectorStore, cls Classifier, projectID string, threshold float32, apply bool, logger *slog.Logger) (Result, []Classified, error) {
	return RunWith(ctx, store, cls, projectID, Options{Threshold: threshold, Apply: apply}, logger)
}

// MinConsensus is the smallest consensus N that gates anything.
const MinConsensus = 2

// Options are Run's per-call decisions. They are one struct rather than a
// seventh and eighth positional argument because every one of them is a THRESHOLD
type Options struct {
	Threshold float32
	Apply     bool
	Consensus int
}

// RunWith is Run with the per-call decisions in one value: see Options, whose
// fields carry the reasoning. A Consensus below MinConsensus is treated as 1
func RunWith(ctx context.Context, store vectorStore, cls Classifier, projectID string, opts Options, logger *slog.Logger) (Result, []Classified, error) {
	passes := opts.Consensus
	if passes < 1 {
		passes = 1
	}
	threshold := opts.Threshold
	apply := opts.Apply
	sel, err := SelectCandidates(ctx, store, projectID, threshold)
	if err != nil {
		return Result{}, nil, err
	}
	fresh := sel.Candidates
	res := Result{Unscored: sel.Unscored, Unoriented: sel.Unoriented, Consensus: passes}

	liveLinks, err := store.LinksByRelationSource(ctx, projectID, string(RelationSupersedes), "llm")
	if err != nil {
		return res, nil, fmt.Errorf("load existing supersedes links: %w", err)
	}
	causeLinks, err := store.LinksByRelationSource(ctx, projectID, string(RelationCauses), "llm")
	if err != nil {
		return res, nil, fmt.Errorf("load existing causes links: %w", err)
	}

	var lookupIDs []string
	seenID := make(map[string]bool)
	for _, l := range append(append([]memory.Link{}, liveLinks...), causeLinks...) {
		for _, id := range []string{l.SourceID, l.TargetID} {
			if !seenID[id] {
				seenID[id] = true
				lookupIDs = append(lookupIDs, id)
			}
		}
	}
	memByID := make(map[string]memory.Memory)
	if len(lookupIDs) > 0 {
		mems, err := store.GetByIDs(ctx, lookupIDs)
		if err != nil {
			return res, nil, fmt.Errorf("load reclassify memory content: %w", err)
		}
		for _, m := range mems {
			memByID[m.ID] = m
		}
	}

	claims := make(map[pairKey][]liveEdge, len(liveLinks)+len(causeLinks))
	claimOrder := make([]pairKey, 0, len(liveLinks)+len(causeLinks))
	for _, links := range [][]memory.Link{liveLinks, causeLinks} {
		for _, l := range links {
			key := newPairKey(l.SourceID, l.TargetID)
			if _, ok := claims[key]; !ok {
				claimOrder = append(claimOrder, key)
			}
			claims[key] = append(claims[key], claimsFor([]memory.Link{l})...)
		}
	}
	freshByPair := make(map[pairKey]Candidate, len(fresh))
	order := make([]pairKey, 0, len(fresh)+len(claimOrder))
	queued := make(map[pairKey]bool, len(fresh)+len(claimOrder))
	for _, c := range fresh {
		key := newPairKey(c.NewerID, c.OlderID)
		freshByPair[key] = c
		if !queued[key] {
			queued[key] = true
			order = append(order, key)
		}
	}
	for _, key := range claimOrder {
		if !queued[key] {
			queued[key] = true
			order = append(order, key)
		}
	}

	livePair := make(map[pairKey]Relation, len(order))
	all := make([]Candidate, 0, len(order))
	for _, key := range order {
		edges := claims[key]
		cand, isFresh := freshByPair[key]
		if len(edges) == 0 {
			all = append(all, cand) // a fresh pair, in the scan's own direction
			continue
		}
		v := pairDirection(edges)
		switch v.state {
		case pairSupersedesCycle:
			res.Bidirectional++
			if logger != nil {
				logger.Info("supersede: refusing a pair the graph claims in both directions (ghost supersede --reassess --consensus 3 --apply withdraws one)",
					"newer", memory.RefusalArg("memory_id", edges[0].source), "older", memory.RefusalArg("memory_id", edges[0].target))
			}
			continue
		case pairCauses:
			a, aOK := memByID[edges[0].newer]
			b, bOK := memByID[edges[0].older]
			if !aOK || !bOK {
				continue // an endpoint no longer exists
			}
			cm, co, oriented := orient(a, b)
			if !oriented {
				if !sel.unorientedPairs[key] {
					res.Unoriented++
					if logger != nil {
						logger.Info("supersede: a pair linked only by 'causes' whose two rows share both timestamps; no direction to judge it in",
							"a", memory.RefusalArg("memory_id", edges[0].newer), "b", memory.RefusalArg("memory_id", edges[0].older))
					}
				}
				continue
			}
			if cm.UpdatedAt <= v.stamp && co.UpdatedAt <= v.stamp {
				continue
			}
			livePair[key] = RelationCauses
			all = append(all, Candidate{
				NewerID: cm.ID, NewerContent: cm.Content, NewerCreatedAt: cm.CreatedAt,
				OlderID: co.ID, OlderContent: co.Content, OlderCreatedAt: co.CreatedAt,
				Similarity: edges[0].strength,
			})
			continue
		case pairFresh, pairDirected:
		}
		livePair[key] = v.relation
		agree := isFresh && cand.NewerID == v.newer && cand.OlderID == v.older
		if isFresh && !agree {
			res.OppositeLive++
			if logger != nil {
				logger.Info("supersede: scan proposed the reverse of a live link; keeping the link's direction",
					"link_relation", string(v.relation),
					"link_source", memory.RefusalArg("memory_id", edges[0].source), "link_target", memory.RefusalArg("memory_id", edges[0].target),
					"scan_newer", memory.RefusalArg("memory_id", cand.NewerID), "scan_older", memory.RefusalArg("memory_id", cand.OlderID))
			}
		}
		newerMem, ok1 := memByID[v.newer]
		olderMem, ok2 := memByID[v.older]
		if !ok1 || !ok2 {
			continue // an endpoint no longer exists
		}
		if newerMem.UpdatedAt <= v.stamp && olderMem.UpdatedAt <= v.stamp {
			continue // skip-if-unchanged: neither endpoint changed since this edge was last judged
		}
		if agree {
			all = append(all, cand)
			continue
		}
		all = append(all, Candidate{
			NewerID: v.newer, NewerContent: newerMem.Content, NewerCreatedAt: newerMem.CreatedAt,
			OlderID: v.older, OlderContent: olderMem.Content, OlderCreatedAt: olderMem.CreatedAt,
			Similarity: edges[0].strength,
		})
	}

	res.Candidates = len(all)

	allIDs := make([]string, 0, len(all)*2)
	for _, c := range all {
		allIDs = append(allIDs, c.NewerID, c.OlderID)
	}
	aliveMems, err := store.GetByIDs(ctx, allIDs)
	if err != nil {
		return res, nil, fmt.Errorf("existence check: %w", err)
	}
	aliveByID := make(map[string]memory.Memory, len(aliveMems))
	for _, m := range aliveMems {
		aliveByID[m.ID] = m
	}
	live := all[:0]
	for _, c := range all {
		newerMem, okNewer := aliveByID[c.NewerID]
		olderMem, okOlder := aliveByID[c.OlderID]
		if !okNewer || !okOlder {
			res.StaleSkipped++
			if logger != nil {
				logger.Info("supersede: dropping stale pair (endpoint replaced by a concurrent pass)",
					"newer", memory.RefusalArg("memory_id", c.NewerID), "older", memory.RefusalArg("memory_id", c.OlderID))
			}
			continue
		}
		if memory.ScopesConflict(newerMem.Scope, olderMem.Scope) {
			if logger != nil {
				logger.Info("supersede: dropping pair whose scopes conflict",
					"newer", memory.RefusalArg("memory_id", c.NewerID), "older", memory.RefusalArg("memory_id", c.OlderID))
			}
			continue
		}
		if memory.RetentionExempt(newerMem) || memory.RetentionExempt(olderMem) {
			if logger != nil {
				logger.Info("supersede: dropping pair with a persistent endpoint",
					"newer", memory.RefusalArg("memory_id", c.NewerID), "older", memory.RefusalArg("memory_id", c.OlderID))
			}
			continue
		}
		live = append(live, c)
	}
	all = live
	res.Candidates = len(all)

	keep := all[:0]
	for _, c := range all {
		if reason, vetoed := VetoSupersede(c); vetoed {
			res.Vetoed++
			if logger != nil {
				logger.Debug("supersede: vetoed pair whose older note states a rule",
					"newer", memory.RefusalArg("memory_id", c.NewerID), "older", memory.RefusalArg("memory_id", c.OlderID), "reason", reason)
			}
			continue
		}
		keep = append(keep, c)
	}
	all = keep
	if len(all) == 0 {
		return res, nil, nil
	}

	checked, err := store.SupersedeChecked(ctx, projectID)
	if err != nil {
		return res, nil, fmt.Errorf("load supersede checks: %w", err)
	}
	var pending []Candidate
	for _, c := range all {
		key := newPairKey(c.NewerID, c.OlderID)
		chk, cached := checked[[2]string{c.NewerID, c.OlderID}]
		if livePair[key] == "" && cached &&
			chk.NewerHash == contentHash(c.NewerContent) && chk.OlderHash == contentHash(c.OlderContent) {
			res.Skipped++
			continue
		}
		pending = append(pending, c)
	}

	var classified []Classified

	res.ConsensusPairsAsked = passes * len(pending)
	if len(pending) > 0 {
		votes := make([]voteCount, len(pending))
		for pass := 0; pass < passes; pass++ {
			relations, err := cls.ClassifyBatch(ctx, pending)
			if err != nil {
				return res, nil, fmt.Errorf("classify %d candidate pair(s) (pass %d of %d): %w", len(pending), pass+1, passes, err)
			}
			if len(relations) != len(pending) {
				return res, nil, fmt.Errorf("classifier returned %d verdicts for %d pairs (pass %d of %d)", len(relations), len(pending), pass+1, passes)
			}
			for i, r := range relations {
				votes[i].add(r)
			}
		}

		for i, c := range pending {
			verdict, state := votes[i].quorum(passes)
			switch state {
			case undecided:
				res.Unclassified++
				if logger != nil {
					logger.Warn("supersede: skipping pair with an unclassifiable verdict",
						"newer", memory.RefusalArg("memory_id", c.NewerID), "older", memory.RefusalArg("memory_id", c.OlderID), "passes", passes)
				}
				continue
			case disputed:
				res.NotAgreed++
				res.Disputed = append(res.Disputed, Disputed{
					NewerID: c.NewerID, OlderID: c.OlderID,
					Tally: votes[i].tally(), Unreadable: votes[i].unreadable,
				})
				if logger != nil {
					top, n := votes[i].best()
					logger.Info("supersede: passes did not agree; no edge written",
						"newer", memory.RefusalArg("memory_id", c.NewerID), "older", memory.RefusalArg("memory_id", c.OlderID),
						"passes", passes, "top", string(top), "top_votes", n)
				}
				continue
			}
			key := newPairKey(c.NewerID, c.OlderID)
			liveRelation := livePair[key]
			wasReclassify := liveRelation != ""
			dropSup, dropCauses, suppressed := verdictDrops(claims, liveRelation, verdict, c.NewerID, c.OlderID)
			classified = append(classified, Classified{
				Candidate:          c,
				Relation:           verdict,
				JudgedAt:           judgedAt(aliveByID, c),
				Reclassified:       wasReclassify,
				ReclassifiedFrom:   liveRelation,
				CausesDroppable:    dropCauses,
				WithdrawSuppressed: suppressed,
				TargetProjectID:    aliveByID[c.OlderID].ProjectID,
			})
			if suppressed {
				res.WithdrawSuppressed++
				if logger != nil {
					logger.Info("supersede: a denying verdict on a live supersedes edge is reported, not applied (ghost supersede --reassess --consensus 3 --apply withdraws it)",
						"newer", memory.RefusalArg("memory_id", c.NewerID), "older", memory.RefusalArg("memory_id", c.OlderID), "verdict", string(verdict))
				}
			}

			switch verdict {
			case RelationSupersedes:
				res.Confirmed++
			case RelationCauses:
				res.CausesCreated++
			case RelationReversed:
				res.Reversed++
				if logger != nil {
					logger.Warn("supersede: refusing a reversed verdict (older note is the current one)",
						"newer", memory.RefusalArg("memory_id", c.NewerID), "older", memory.RefusalArg("memory_id", c.OlderID))
				}
			}
			movedSomething := verdict != liveRelation || dropSup > 0 || dropCauses > 0
			if suppressed && verdict != RelationCauses {
				movedSomething = dropCauses > 0
			}
			if wasReclassify && movedSomething {
				res.Reclassified++
				if verdict == RelationNeither || verdict == RelationReversed {
					res.ReclassifiedNoWrite++
				}
			}
		}
	}

	if apply {
		writable := make(map[[2]string]bool, len(classified))
		for i := range classified {
			c := &classified[i]
			pairAlive, err := endpointsExist(ctx, store, c.NewerID, c.OlderID)
			if err != nil {
				return res, nil, fmt.Errorf("stale check %s→%s: %w", memory.RefusalArg("memory_id", c.NewerID), memory.RefusalArg("memory_id", c.OlderID), err)
			}
			if !pairAlive {
				res.StaleAtWrite++
				if c.WithdrawSuppressed {
					c.WithdrawSuppressed = false
					res.WithdrawSuppressed--
				}
				if logger != nil {
					logger.Info("supersede: skipping write, endpoint replaced by a concurrent pass",
						"newer", memory.RefusalArg("memory_id", c.NewerID), "older", memory.RefusalArg("memory_id", c.OlderID))
				}
				continue
			}
			writable[[2]string{c.NewerID, c.OlderID}] = true
			switch c.Relation {
			case RelationSupersedes:
				wrote, err := store.CreateLinkUnopposed(ctx, c.NewerID, c.OlderID, string(RelationSupersedes), c.Similarity, "llm", c.JudgedAt)
				if err != nil {
					return res, nil, fmt.Errorf("create supersedes link %s→%s: %w", memory.RefusalArg("memory_id", c.NewerID), memory.RefusalArg("memory_id", c.OlderID), err)
				}
				if wrote {
					res.Created++
				} else {
					res.ReverseLive++
					c.OpposedLive = true
					if logger != nil {
						logger.Info("supersede: not written, the pair's reverse supersedes edge is already live (a concurrent pass wrote it first)",
							"newer", memory.RefusalArg("memory_id", c.NewerID), "older", memory.RefusalArg("memory_id", c.OlderID))
					}
				}
				causesDropped, err := store.InvalidateLink(ctx, c.OlderID, c.NewerID, string(RelationCauses))
				if err != nil {
					return res, nil, fmt.Errorf("invalidate causes link %s→%s: %w", memory.RefusalArg("memory_id", c.OlderID), memory.RefusalArg("memory_id", c.NewerID), err)
				}
				c.CausesDropped = int(causesDropped)
				if claimsHold(claims, c.OlderID, c.NewerID, RelationCauses) {
					reverseDropped, err := store.InvalidateLink(ctx, c.NewerID, c.OlderID, string(RelationCauses))
					if err != nil {
						return res, nil, fmt.Errorf("invalidate reverse causes link %s→%s: %w", memory.RefusalArg("memory_id", c.NewerID), memory.RefusalArg("memory_id", c.OlderID), err)
					}
					c.CausesDropped += int(reverseDropped)
				}
				c.Withdrawn = c.Reclassified && c.ReclassifiedFrom == RelationCauses && c.CausesDropped > 0
			case RelationCauses:
				dropped, err := withdrawSupersedesEdge(ctx, store, c)
				if err != nil {
					return res, nil, fmt.Errorf("invalidate supersedes link %s→%s: %w", memory.RefusalArg("memory_id", c.NewerID), memory.RefusalArg("memory_id", c.OlderID), err)
				}
				causesDropped := 0
				if claimsHold(claims, c.OlderID, c.NewerID, RelationCauses) {
					reverseDropped, err := store.InvalidateLink(ctx, c.NewerID, c.OlderID, string(RelationCauses))
					if err != nil {
						return res, nil, fmt.Errorf("invalidate reverse causes link %s→%s: %w", memory.RefusalArg("memory_id", c.NewerID), memory.RefusalArg("memory_id", c.OlderID), err)
					}
					causesDropped = int(reverseDropped)
				}
				wrote, err := store.CreateLinkUnopposed(ctx, c.OlderID, c.NewerID, string(RelationCauses), c.Similarity, "llm", c.JudgedAt)
				if err != nil {
					return res, nil, fmt.Errorf("create causes link %s→%s: %w", memory.RefusalArg("memory_id", c.OlderID), memory.RefusalArg("memory_id", c.NewerID), err)
				}
				if !wrote {
					res.ReverseLive++
					c.OpposedLive = true
					if logger != nil {
						logger.Info("supersede: causes link not written, the pair's reverse causes edge is already live (a concurrent pass wrote it first)",
							"older", memory.RefusalArg("memory_id", c.OlderID), "newer", memory.RefusalArg("memory_id", c.NewerID))
					}
				} else {
					res.CausesWritten++
				}
				c.CausesDropped = causesDropped
				c.Withdrawn = c.withdrewLiveEdge(dropped, causesDropped)
			case RelationNeither:
				dropped, err := withdrawSupersedesEdge(ctx, store, c)
				if err != nil {
					return res, nil, fmt.Errorf("invalidate supersedes link %s→%s: %w", memory.RefusalArg("memory_id", c.NewerID), memory.RefusalArg("memory_id", c.OlderID), err)
				}
				causesDropped, err := sweepCausesBothWays(ctx, store, claims, c.NewerID, c.OlderID)
				if err != nil {
					return res, nil, err
				}
				c.CausesDropped = causesDropped
				c.Withdrawn = c.withdrewLiveEdge(dropped, causesDropped)
			case RelationReversed:
				dropped, err := withdrawSupersedesEdge(ctx, store, c)
				if err != nil {
					return res, nil, fmt.Errorf("invalidate reversed supersedes link %s→%s: %w", memory.RefusalArg("memory_id", c.NewerID), memory.RefusalArg("memory_id", c.OlderID), err)
				}
				causesDropped, err := sweepCausesBothWays(ctx, store, claims, c.NewerID, c.OlderID)
				if err != nil {
					return res, nil, err
				}
				c.Withdrawn = c.withdrewLiveEdge(dropped, causesDropped)
				c.CausesDropped = causesDropped
				if logger != nil && dropped+int64(causesDropped) > 0 {
					logger.Info("supersede: dropped the links of a reversed pair",
						"newer", memory.RefusalArg("memory_id", c.NewerID), "older", memory.RefusalArg("memory_id", c.OlderID),
						"links", dropped+int64(causesDropped))
				}
			}
			if logger != nil {
				logger.Debug("supersede classified", "newer", memory.RefusalArg("memory_id", c.NewerID), "older", memory.RefusalArg("memory_id", c.OlderID), "verdict", c.Relation)
			}
		}

		newNeither := make(map[[2]string]memory.SupersedeCheck)
		for _, c := range classified {
			key := [2]string{c.NewerID, c.OlderID}
			if c.Relation != RelationNeither || livePair[newPairKey(c.NewerID, c.OlderID)] != "" || !writable[key] {
				continue
			}
			newNeither[key] = memory.SupersedeCheck{
				NewerHash: contentHash(c.NewerContent),
				OlderHash: contentHash(c.OlderContent),
			}
		}
		if len(newNeither) > 0 {
			if err := store.MarkSupersedeNeither(ctx, projectID, newNeither); err != nil {
				if logger != nil {
					logger.Warn("supersede NEITHER cache write failed", "error", err)
				}
			}
		}
	}
	return res, classified, nil
}
