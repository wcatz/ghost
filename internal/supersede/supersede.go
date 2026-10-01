// Package supersede creates directed 'supersedes' and 'causes' links between
// memories where a newer memory replaces an older one about the same subject
// (supersedes), or a newer memory is an effect that followed from an older
// one (causes). It is the creation half of staleness-aware ranking; the
// consumption half (SearchParams.SupersedeDemote) already ships. See
// docs/benchmarks.md Phase 3.
//
// Design: cosine similarity proposes same-subject candidate pairs (cheap,
// local), the timestamps give direction (newer/older by updated_at, then by
// created_at — SQLite's 'YYYY-MM-DD HH:MM:SS' timestamps compare
// lexicographically, and a pair that ties on BOTH has no direction this pass may
// invent), and an LLM Classifier makes a 4-way
// SUPERSEDES/CAUSES/NEITHER/REVERSED judgment for each pair — batched up to
// classifyBatchSize pairs per harness call so a large
// project's pass does not pay one process spawn and full rubric per pair — since
// "replaces a stale claim" and "is caused by / follows from" are distinct
// relations that a binary confirm/reject can't tell apart. SUPERSEDES writes
// a newer->older 'supersedes' link (source 'llm'); CAUSES writes an
// older->newer 'causes' link (cause precedes effect); NEITHER writes nothing;
// REVERSED (the older note is the current one) is refused rather than written.
//
// The pass is KEEP-biased, because the edge is not informational: the ranking
// guards demote its target and `ghost resolve`'s supersedes piggyback stamps
// resolved_at on it, so a wrong edge removes a live memory from every later
// session's context. A SUPERSEDES therefore has to name the older note's claim
// that no longer holds in a `replaced:` field, and a verdict without one reads
// NEITHER (#686, 43% measured precision with every wrong edge a pair whose two
// notes were both still true). Each note's created_at goes to the prompt too,
// because updated_at is the wrong ordering exactly when a note was re-saved
// after the fact it reports (#641): a bare three-way answer cannot decline a
// direction, and the pass wrote a backwards link that demoted a fix and promoted
// the stale claim it replaced.
//
// The unit the pass judges is the UNORDERED PAIR, and it is that unit because
// the two things the edge does — demote a target, stamp resolved_at on it —
// both land on the target alone, so two edges in opposite directions demote
// BOTH memories of one pair and neither can undo the other (#778). A live edge
// therefore decides which way round its pair is judged, a pair claimed in both
// directions is refused rather than judged, and a pair whose two rows share both
// timestamps is not proposed at all: a bulk import stamps a whole batch with
// one updated_at, and the only "chronology" left to invent a direction from
// would be a pair of hex ids. Each of those three is a counted, named refusal
// (Result.OppositeLive, .Bidirectional, .Unoriented) because a pass that
// declined work and reported the totals of one that found nothing to do reads
// as "nothing was skipped".
//
// Fresh NEITHER verdicts are cached by pair and both endpoints' content hashes
// (supersede_checked, schema v8): unchanged fresh pairs are skipped on later
// passes. A cache skip is equivalent to a NEITHER verdict, which is what bounds
// it — so a pair the graph is linked in EITHER relation is never skipped, live
// edge or not (#823; until then only a 'supersedes' edge was exempt, and a
// 'causes' edge could be left stale by a skipping pass under a bargain this
// package no longer makes). A REVERSED verdict is never cached, or the pair would
// be skipped for the
// life of its text — so a converged project makes zero classify calls for its
// fresh candidates EXCEPT reversed ones, which are re-asked on every pass until
// the verdict changes. That is a deliberate billable repeat per reversed pair,
// paid so the refusal is never frozen; it is not zero calls overall.
// Run() also re-classifies existing 'llm' links — of EITHER relation (#823) —
// whose endpoints have changed since the link was written, replacing the link
// with the other relation when the verdict differs, and REPORTING a live
// 'supersedes' edge a denying or reversing verdict no longer supports rather
// than removing it (#845): measured over a real store, 6 of 11 such withdrawals
// were wrong, so the deletion became `ghost supersede --reassess` / `--withdraw`,
// each of which re-judges the edge and shows the operator what it is about to
// remove. The 'causes' sweep is NOT withheld — Reassess loads live
// 'supersedes'/'llm' edges only, so withholding that sweep would leave a
// contradiction under a repair that cannot reach it, and nothing demotes on a
// 'causes' edge (see ordinaryPassWithdrawsSupersedes). A WITHHELD withdrawal is
// not cached, so
// the edge is re-reported on every later pass until a person withdraws it. The
// pass is re-runnable and self-heals after reflection's cascade-delete of links, like
// the cosine linking worker rebuilds 'related' edges — though reclassification
// of existing links only fires for a pair whose endpoint content actually
// changed after the link was written; pairs whose link predates this 4-way
// classifier but whose endpoints haven't changed since are only corrected if
// they still surface as a fresh candidate (see "Skip-if-unchanged" in
// docs/superpowers/specs/2026-07-28-supersede-relation-type-fix-design.md).
package supersede

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"

	"github.com/wcatz/ghost/internal/memory"
)

// maxNeighbors caps how many nearest neighbors each memory contributes as
// candidates — the same bound the linking worker uses, keeping LLM calls
// proportional to memory count, not its square.
const maxNeighbors = 8

// Candidate is an ordered pair proposed for classification: Newer is the more
// recent memory that may supersede Older. The CreatedAt values are the notes'
// own timestamps, which the classifier prompt needs because the updated_at
// ordering that picked the pair can be misleading (see the Run doc comment).
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
	// RelationSupersedes means newer states an updated/changed/replaced value
	// of the SAME fact as older, making older obsolete. A model that cannot
	// name the claim it replaced does not reach this verdict (see
	// requireReplaced), and one that retires only SOME of older's claims does
	// not either (see the every-claim rule in classifyRubric).
	RelationSupersedes Relation = "supersedes"
	// RelationCauses means newer (typically a decision or change) was informed
	// by older as supporting evidence, but older remains independently true.
	RelationCauses Relation = "causes"
	// RelationNeither means the pair is not a genuine replacement or citation
	// relationship — e.g. two independently valid parallel facts.
	RelationNeither Relation = "neither"
	// RelationReversed means the same-fact replacement runs the other way: the
	// OLDER note states the current value and the NEWER one restates a claim
	// that is already obsolete. Run refuses it (see the Run doc comment).
	RelationReversed Relation = "reversed"
)

// Classifier decides the relationship for candidate pairs: a same-fact
// replacement (SUPERSEDES), a decision citing supporting evidence that stays
// valid (CAUSES), a replacement that runs against the pair's orientation
// (REVERSED), or neither. It returns one verdict per pair, in the same
// order; Relation("") marks a pair whose verdict could not be parsed. The LLM
// implementation (RelationClassifier) batches pairs across as few harness
// calls as possible; tests inject a deterministic mock.
//
// An error means the question was not answered for every pair asked about, but
// it may still be answered for some of them, and how many is the
// *PartialVerdictsError's Answered field (see answeredPrefix). A caller that
// ignores that error reads "nothing was decided", which is the safe direction
// and what every caller did before it.
type Classifier interface {
	ClassifyBatch(ctx context.Context, pairs []Candidate) ([]Relation, error)
}

// contentHash is the NEITHER-cache key component, mirroring resolve's
// ContentHash: the classification question is about the notes' text, so a tag
// or importance edit must not invalidate a cached verdict. The "v4\x00" prefix
// versions the key — a prompt/rubric change that could flip verdicts bumps it
// to reset every cached verdict in one step, the same reset resolve performed
// when its rubric changed. A cache hit is a permanent skip for the life of that
// text, so a stored verdict that the current rules would answer differently is
// not a stale row, it is a rule that can never be applied again to those two
// notes. v2 was the #641 rubric (REVERSED plus the created_at signal), v3 is
// #686's: a SUPERSEDES now has to name the older note's retired claim, and
// two-true pairs are NEITHER, so every v2 row has to be re-asked. v4 is #779's:
// a SUPERSEDES now has to retire EVERY claim of the older note rather than one
// of them, and a log entry, a recurring defect and a parallel investigation are
// named as the shapes they are. The three rules above v4 all narrowed the set
// of answers that are SUPERSEDES, so every v3 row is a verdict the current
// rubric would not necessarily give — and a row kept is a pair skipped for the
// life of its text, which is the one direction this cache cannot be wrong in.
func contentHash(content string) string {
	sum := sha256.Sum256([]byte("v4\x00" + content))
	return hex.EncodeToString(sum[:])
}

// vectorStore is the subset of *memory.Store the pass needs; narrowed for
// testability.
type vectorStore interface {
	GetAll(ctx context.Context, projectID string, limit int) ([]memory.Memory, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	GetEmbedding(ctx context.Context, memoryID string) ([]float32, error)
	SearchVectorScoped(ctx context.Context, projectID string, queryVec []float32, limit int, scope map[string]string) ([]memory.ScoredMemory, error)
	// CreateLinkJudged writes the edge as judged, with no opinion about the
	// pair's other direction. It is here for the 'causes' edge, and only that
	// one: a 'causes' cycle demotes nothing, so refusing the second direction
	// there would cost a call per pass forever and buy nothing (see the CAUSES
	// branch of the apply block).
	CreateLinkJudged(ctx context.Context, sourceID, targetID, relation string, strength float32, source, judgedAt string) error
	// CreateLinkUnopposed, for the 'supersedes' edge: the one write the pass must
	// never make is the second direction of a pair another writer has already
	// claimed (#806), because both edges demote one of the pair's two memories
	// and neither withdraws the other. The store reads the reverse edge inside
	// the transaction that inserts this one, so the guard holds between two
	// PROCESSES and not only between two pairs in one run.
	CreateLinkUnopposed(ctx context.Context, sourceID, targetID, relation string, strength float32, source, judgedAt string) (bool, error)
	InvalidateLink(ctx context.Context, sourceID, targetID, relation string) (int64, error)
	LinksByRelationSource(ctx context.Context, projectID, relation, source string) ([]memory.Link, error)
	SupersedeChecked(ctx context.Context, projectID string) (map[[2]string]memory.SupersedeCheck, error)
	MarkSupersedeNeither(ctx context.Context, projectID string, checks map[[2]string]memory.SupersedeCheck) error
}

// Selection is what one candidate scan read: the ordered pairs it proposes, and
// how many of the project's memories it could not score at all. Both are needed
// by a caller that reports the pass, because "0 candidate pairs" means two very
// different things over a fully indexed corpus and a partly indexed one, and
// only the scan can tell them apart.
type Selection struct {
	Candidates []Candidate
	// Unscored counts the memories skipped for want of a usable vector: none at
	// all yet, or one written under a different model, width or task prefix
	// (memory.Store.GetEmbedding returns nil for both). The pairs THIS scan
	// proposes are therefore bounded by the part of the project it could score,
	// and a caller that does not report this number reports a total that
	// silently omits part of it.
	//
	// It is about the scan, not the run: a memory counted here can still be an
	// endpoint of a pair Run re-proposes, because the reclassify half re-reads
	// live 'supersedes' edges from their link rows and never looks at a vector.
	// A caller must therefore scope what it lost to "proposed no new candidate",
	// never "was in no pair this run considered" — after a model change retires
	// every vector in a project, both halves describe the same corpus and only
	// one of the two sentences is true.
	Unscored int
	// Unoriented counts the near-neighbour pairs this scan found and REFUSED to
	// propose, because both rows carry the same updated_at and the same
	// created_at and the pass therefore holds no chronology to order them by
	// (see orient). It is counted, unlike the scope and persistent refusals
	// above, for the reason those are not: those rows are dropped by the query
	// before this function sees them, so a count taken here would read as zero
	// for work the pass really declined. This one is decided HERE, from the two
	// timestamp columns this function has just read, and a bulk import produces
	// a whole corpus of it (#778: 33 rows sharing one updated_at) — so a pass
	// that reported "0 candidate pairs" over a project whose pairs are all tied
	// would be reporting a scan that ran, not a scan that found nothing.
	//
	// Counted per unordered PAIR, not per sighting: every endpoint sees every
	// pair, so a per-sighting count would report each refusal twice.
	Unoriented int
	// unorientedPairs is the same refusals as KEYS, and it exists because the
	// count alone cannot be reconciled against a second refusal of the same
	// pair: Run's reconciliation reaches a tied pair by its live edges as well
	// as by this scan, and the pair that carries a 'causes' edge and is ALSO a
	// near neighbour is refused by both halves — once here and once there. It is
	// unexported because nothing outside this package needs it: `Unoriented` is
	// the number a caller reports, and this is the bookkeeping that keeps the
	// number a per-PAIR count rather than a per-half count.
	unorientedPairs map[pairKey]bool
}

// SelectCandidates returns the deduped ordered candidate pairs for a project:
// memories whose cosine similarity is at least threshold, oriented newer→older
// by updated_at. A pair is emitted once regardless of which endpoint surfaced
// it. Memories without embeddings are skipped (no similarity signal) and counted
// in Selection.Unscored, so the caller can say that part of the project was not
// read rather than reporting a smaller corpus as if it were the whole one.
//
// A pair whose endpoints' scopes conflict is never emitted. The classifier
// cannot see the difference: it is handed the two note bodies and nothing else,
// and scope is a column beside the text, so "development pool timeout is 5s"
// and "production pool timeout is 30s" arrive as the same sentence twice and
// the rubric's NEITHER example can only catch the pair that names its
// environments in prose. Refusing the pair here is what keeps that from costing
// a classify call and writing a 'supersedes' edge between two claims about
// different places — the same exemption the linker's 'related' edge and
// Upsert's 'duplicate' fold already apply (memory.ScopesConflict).
//
// A pair with a `persistent` endpoint is never emitted either, and for a
// stronger reason: this is the pass that makes claims ABOUT a memory, and every
// consequence of the edge it writes lands on the target — the ranking demotion
// and resolve's piggyback both. A keep-forever row is a user statement that this
// must not happen to it, and it is asked before the classify call rather than
// after, because a verdict the pass cannot act on is a bill for a decision it
// never needed to make.
//
// The refusal is made twice, on purpose. The scope reaches the store so the
// neighbour budget counts only rows this source may link to — filtering after
// the cut spends the whole budget on rows that can never be linked, and a
// compatible pair ranked just below the cut is then never examined at all. The
// ScopesConflict test is the invariant the returned candidates must satisfy;
// the shipped store already guarantees it, but vectorStore is an interface and
// the guarantee is a property of one implementation, not of the contract.
//
// Neither refusal is counted in Result. Against the shipped store the
// conflicting rows are dropped by the query, before this function sees them, so
// a count taken here would report zero for work the pass really did decline —
// a number that reads as "nothing was skipped", which is worse than no number.
// Run's reclassify filter can see its own pairs and logs each refusal; on the
// fresh path a refusal shows up as a candidate the pass never spends a call on.
// The THIRD refusal — a pair this scan will not orient because both endpoints
// share both timestamps — is decided here and is counted in Selection.Unoriented
// for the opposite reason: unlike the two above, this function holds the columns
// the decision is made from, so the count is knowable, and a corpus of tied
// bulk-import rows would otherwise read as a corpus with no similar pairs in it.
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

	// seen is keyed by the UNORDERED pair, so a pair is proposed once and a
	// refusal is counted once however many times the two endpoints see it.
	seen := make(map[pairKey]bool)
	var cands []Candidate
	for _, m := range mems {
		vec, err := store.GetEmbedding(ctx, m.ID)
		if err != nil || len(vec) == 0 {
			sel.Unscored++ // no usable embedding → no similarity candidates. A nil
			// vector with no error is a memory whose vector belongs to another
			// vector space (see memory.Store.GetEmbedding); comparing it would
			// propose pairs from a cosine between two spaces, and each confirmed
			// one costs a classify call and writes a demoting edge. It is counted
			// because the memory is then in NO pair this pass could find, and a
			// total that does not say so reads as a smaller corpus.
			continue
		}
		// +1 because the memory itself is its own nearest neighbor.
		neighbors, err := store.SearchVectorScoped(ctx, projectID, vec, maxNeighbors+1, m.Scope)
		if err != nil {
			return sel, fmt.Errorf("search vector for %s: %w", m.ID, err)
		}
		for _, n := range neighbors {
			if n.MemoryID == m.ID || n.Score < threshold {
				continue
			}
			other, ok := byID[n.MemoryID]
			if !ok {
				continue // e.g. a _global neighbor not in this project's set
			}
			// The persistent-tier refusal, asked here and not after the
			// classification: a pair with a keep-forever endpoint is not a pair
			// the model is asked about. The edge this pass writes demotes its
			// target in every later ranking and lets resolve's piggyback stamp
			// resolved_at on it, so the exemption has to be here — a classify call
			// spent on a pair whose only possible outcome is the edge we are
			// refusing is money for a decision we have already made.
			//
			// Both endpoints, because the claim runs newer -> older and either one
			// being untouchable means the claim must not be made: a keep-forever
			// SOURCE would be writing an edge about a memory it may not assert
			// anything about either.
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
				// No chronology: both rows carry the same updated_at AND the same
				// created_at, so this pass cannot say which note is the current
				// one and must not guess. Counted once per pair, and not
				// proposed, so no call is spent on it and nothing is cached
				// under a direction nobody read.
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

// pairKey is the identity of a PAIR of memories, which is unordered: {A,B} and
// {B,A} are the same pair, and a pass that treats them as two is how one pair
// came to be classified twice in opposite orientations in a single run (#778).
// It is deliberately not the [2]string the store's supersede_checked rows and
// the NEITHER cache are keyed by, because those record a verdict ABOUT a
// direction and so must stay ordered.
type pairKey [2]string

// newPairKey is the canonical form of the unordered pair, with the ids in
// ascending order. Every lookup that asks "is this the same pair?" goes through
// it, so the reconciliation in Run cannot be half-applied.
func newPairKey(a, b string) pairKey {
	if a > b {
		return pairKey{b, a}
	}
	return pairKey{a, b}
}

// orient returns (newer, older) by updated_at, then by created_at, and reports
// whether a direction is knowable at all.
//
// updated_at is the primary signal — the same freshness Run()'s skip-if-unchanged
// reclassification already uses. created_at misorders (and mislabels the
// direction of) a memory that was edited long after it was first created, which
// is exactly the reversed-decision case this pass exists to catch, so it decides
// only what updated_at cannot: a pair of rows a bulk import stamped together
// carries one updated_at for the whole batch and a real per-row chronology in
// created_at. SQLite 'YYYY-MM-DD HH:MM:SS' strings order chronologically under
// lexicographic comparison.
//
// ok is false when the two agree on BOTH columns, and that is the answer the
// caller must act on rather than a fallback. A bulk import stamps every row it
// writes (#778 measured 33 rows sharing one `2026-09-20 09:26:05` updated_at), and
// for any pair among them the pass holds no chronology at all. The old third
// tiebreak was the ID, which is a hash: it made the direction of a real
// supersession a function of which of two random hex strings sorted higher,
// rather than of anything about the two notes. There is no fourth column to
// read, and inventing a direction is the harm, so the pair is not proposed (see
// Selection.Unoriented).
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

// liveEdge is one live directed edge, read as a CLAIM about which of its two
// memories is the newer one. It is the unit the reconciliation below reasons
// about, and it exists because the two relations are written in OPPOSITE
// directions: a 'supersedes' edge runs newer→older and a 'causes' edge
// older→newer, so a shape carrying (source, target) directly would assert two
// different directions for one pair depending on which relation the read came
// from. Normalising at the read is what lets every rule below be about the PAIR's
// direction rather than about a relation's.
//
// `source`/`target` are kept alongside the normalised pair because the one log
// line about two disagreeing orientations (#804) has to say which id came from
// where, and re-deriving them from the relation would be a second spelling of the
// convention this type exists to hold in one place.
type liveEdge struct {
	relation Relation
	newer    string
	older    string
	source   string
	target   string
	// stamp is the edge's own created_at, which is the judgement freshness
	// skip-if-unchanged reads (see CreateLinkJudged). It is per-edge, so a pair
	// claimed by more than one edge has more than one, and the quiet below is
	// taken on the NEWEST of them: the question that quiet asks is whether
	// anything moved since the graph last described this pair, and the newest
	// claim is the last time it did.
	stamp    string
	strength float32
}

// claimsFor reads a pair's live edges as the directions they assert, in the
// convention each relation is WRITTEN in. A relation the pass did not ask for is
// skipped rather than guessed at, so a store holding a third relation on the pair
// cannot make this read into a claim nobody asserted.
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

// pairState is what a pair's live edges turn out to be, once read through the
// direction each one asserts. It is a NAMED SET of four because the four are four
// different answers, and two of them are the difference between a pass that acts
// and a pass that declines — a bool would make the caller re-derive which one it
// holds, and it got that derivation wrong once already (see pairDirection).
type pairState int

const (
	// pairFresh is the ordinary case: the graph says nothing about this pair, so
	// the scan's own orientation is the pair's.
	pairFresh pairState = iota
	// pairDirected is a pair one live edge describes. The returned claim is that
	// edge and the pair is judged in its direction.
	pairDirected
	// pairSupersedesCycle is a pair claimed in both directions in 'supersedes',
	// which this pass refuses to judge at all.
	pairSupersedesCycle
	// pairCauses is a pair whose only live edges are 'causes', whether they agree
	// with each other or not. The caller judges it by the TIMESTAMPS and
	// reconciles the edges by the verdict — and that is the whole of what a
	// 'causes' edge is allowed to decide, because a direction that buries a
	// memory belongs to the relation that DEMOTES and to nothing else.
	pairCauses
)

// pairVerdict is what the reconciliation learns about a pair from its live edges:
// a direction to judge it in, or a stamp to hold it quiet with.
//
// The two are SEPARATE FIELDS, and that separation is the fix for the blocker
// rather than a style choice. A 'causes' edge supplies the stamp and must not be
// able to supply the direction, because a direction that buries a memory belongs
// to the relation that DEMOTES and to nothing else: read through a causes
// override, a live `causes newer→older` edge asks "does the January note supersede
// the September one", a SUPERSEDES answer writes exactly that, and the edge
// demotes the memory that is actually current (#641, produced by the pass rather
// than found by it). So the 'causes' cases leave `newer` and `older` EMPTY, and
// a caller that reached for them anyway would write an edge with no ids rather
// than the wrong one — which is the failure mode worth having.
type pairVerdict struct {
	newer    string
	older    string
	stamp    string
	relation Relation
	state    pairState
}

// pairDirection decides which way round a pair holding live edges is JUDGED, and
// which relation's edge says so. See pairVerdict for why the two are separate
// fields, and case 3 for what a 'causes' edge is allowed to decide.
//
//  1. TWO 'supersedes' edges disagreeing is a CYCLE, and it is refused rather than
//     judged. Both edges demote one of the pair's two memories, neither withdraws
//     the other, and no orientation of it can be judged into a state worth keeping
//     (#778). The sweeps below cannot rescue it either: they are scoped to the
//     OTHER relation, so a SUPERSEDES verdict in one direction re-affirms that
//     half and leaves its partner exactly as demoting as before.
//
//  2. Otherwise a 'supersedes' edge's direction wins — INCLUDING where a 'causes'
//     cycle sits on the same pair, which is the shape a pre-#823 store holds
//     (that writer shipped unguarded) and which is a #641-shaped miss if the
//     timestamps get to orient it: the live edge this pass can reach, and the one
//     `--reassess` and resolve's piggyback act on, is the supersedes one, and
//     judging the pair the other way asks about an edge that is not there while
//     leaving the edge that IS in place.
//
//  3. Otherwise the pair is judged by the TIMESTAMPS, whatever 'causes' edges it
//     carries, and that is the fix for the blocker this state exists for: a
//     'causes' edge is a claim about which note CAUSED which, and a supersedes
//     verdict writes a DEMOTATION. Read through a causes override, a live
//     `causes newer→older` edge asks "does the January note supersede the
//     September one", the model answers yes, and the pass writes exactly that —
//     burying the memory that is current (#641, produced by the pass rather than
//     found by it). So a causes edge settles nothing about the question. What it
//     DOES do is hold the pair: its stamp is the freshness skip-if-unchanged
//     reads, and `livePair` keeps the pair out of the NEITHER cache, which is the
//     re-bill #823 is about, and its direction is reconciled by the verdict rather
//     than by the prompt.
//
// Case 2 is the deliberate exception, and it is the whole of the asymmetry: that
// edge IS the demotion, so the direction to judge it in is the direction it was
// written in — which is what lets a REVERSED answer reach the wrong edge and
// withdraw it instead of confirming the reverse one (#641).
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
				// Case 1: a 'supersedes' cycle.
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
		// Case 2. A 'causes' edge is deliberately not consulted, agreeing or not.
		return pairVerdict{
			newer: sup.newer, older: sup.older, stamp: sup.stamp,
			relation: RelationSupersedes, state: pairDirected,
		}
	case haveCau:
		// Case 3. The stamp is the NEWEST 'causes' edge the pass read, which is
		// the right freshness for the quiet: it asks when the graph last
		// described this pair, and the newest claim is the last time it did.
		// The direction is left empty on purpose.
		return pairVerdict{
			stamp: cau.stamp, relation: RelationCauses, state: pairCauses,
		}
	}
	return pairVerdict{state: pairFresh}
}

// claimsHold reports whether the pass read a live edge of this relation in this
// direction on this pair. The apply block's extra sweeps are gated on it, and
// they have to be: each is a second write in the run, and one that can only ever
// move a row the pass has already seen is a write worth making only then.
func claimsHold(claims map[pairKey][]liveEdge, newer, older string, relation Relation) bool {
	for _, e := range claims[newPairKey(newer, older)] {
		if e.relation == relation && e.newer == newer && e.older == older {
			return true
		}
	}
	return false
}

// verdictDrops reports what this verdict's apply block will move in the graph
// OTHER than the one edge the pair was judged around, split by relation: the
// second row every verdict owes, and the reason a verdict that re-affirmed the
// live edge's own relation can still be a mutation on the graph.
//
// It is a PREDICTION, made from what the pass already read, and it has two
// callers. Result.Reclassified is counted BEFORE the apply block runs — counting
// only the relation change is how a run that really did remove a graph row
// reported "0 reclassified". And Classified.CausesDroppable is a dry run's
// forecast: CausesDropped counts what --apply actually invalidated, so it was 0
// in every dry run, and a preview that could not say it would delete an edge was
// a preview of half a mutation. The forecast costs no read — both counts come
// from the edges the pass has loaded — where the obvious alternative, a
// prediction query, is a second read whose failure would then have to fail the
// pass, and this pass's contract is that a write error is the only thing that
// aborts one.
//
// Each number counts DIRECTIONS the pass read, not rows that will be gone: a
// concurrent pass can take one first, which is why the apply block reports its own
// observed counts and the two can differ. The gates are the same `claimsHold`
// predicates the block's sweeps are gated on, so the prediction and the writes
// cannot disagree about whether there IS a row to move.
//
// A NEITHER and a REVERSED are counted by the caller instead, because they deny
// the pair outright and `verdict != liveRelation` already covers them; their counts
// here are only what a row's clause names.
//
// The third value is what withholdSupersedesDrop decided: a non-zero `supersedes`
// count the ordinary pass will not act on, because the apply block deletes nothing
// (#845). It is returned rather than applied here so that the ONE prediction, the
// ONE row field, the ONE counter and the ONE guard in the apply block are all
// derived from the same decision — a suppression applied in two places is one a
// later change can half-undo.
func verdictDrops(claims map[pairKey][]liveEdge, live, verdict Relation, newer, older string) (supersedes, causes int, suppressed bool) {
	// A 'causes' edge `older→newer` carries the claim (newer, older) and one
	// running `newer→older` carries (older, newer): 'causes' is written
	// cause→effect, so the note it calls older is its SOURCE. Naming the two
	// gates after the direction they name rather than after the ids keeps the
	// crossing below visible — it is the whole reason the pairs read crossed
	// against each other, and a reader has to see it to trust either line.
	about := claimsHold(claims, newer, older, RelationCauses)
	opposed := claimsHold(claims, older, newer, RelationCauses)
	switch verdict {
	case RelationSupersedes:
		// The pair's 'causes' edges go in BOTH directions: one is the edge a
		// supersession implies, the other is the edge that contradicts it, and a
		// verdict that keeps either has half-settled the pair.
		if about {
			causes++
		}
		if opposed {
			causes++
		}
	case RelationCauses:
		// The pair's 'supersedes' edge goes, and so does the 'causes' edge in the
		// direction this verdict is NOT about to write — which is a 'causes'
		// cycle's other half, and also the edge a 'causes' verdict running against
		// the timestamps corrects. A pair whose 'causes' edge already agrees
		// moves nothing: the upsert re-stamps the edge the row is about.
		if live == RelationSupersedes {
			supersedes = 1
		}
		if opposed {
			causes = 1
		}
	default:
		// NEITHER and a REVERSED: nothing is written, and both relations are
		// swept in the directions they are written.
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

// withdrawsSupersedes is the ordinary pass's answer to "does a denying verdict
// remove a live 'supersedes' edge?", and it is NO (#845).
//
// The pass has always written new links and, since #785, withdrawn old ones on a
// four-way verdict, and the withdrawal was the part that was never safe: measured
// over a real store, 4 of 11 withdrawals were correct, 1 unsure and 6 wrong, and
// the wrong ones shared a shape — a newer note retiring ONE claim of an older one
// whose other claims were still true. The verdict was a fair reading of the two
// bodies under the every-claim rule, and the pass deleted a correct edge. What
// goes with the deletion is not nothing: the ranking stops demoting the target,
// and every `resolved_at` the edge's piggyback stamped stays, so a wrong
// withdrawal is a stale memory promoted and a current memory hidden, both by a
// flag the operator set for WRITES.
//
// So the ordinary pass names the edges it would withdraw and reports them, in
// both modes, and the withdrawal itself is `ghost supersede --reassess` or
// `--withdraw`: the two commands a person asked for, each of which re-judges the
// edge under the current rules and shows the operator what it is about to remove.
// A creation pass that can delete a correct edge is a pass whose errors are
// invisible until a memory stops being reminded of, which is the opposite of the
// KEEP bias the rest of this file is built on.
//
// It is a constant rather than an Options field because it is not a threshold an
// operator tunes: there is no setting under which the ordinary pass stops
// reporting a withdrawal it declines to make, and a flag that restores the delete
// would have to name the harm it restores. WRITES are untouched — a new
// 'supersedes' or 'causes' edge is still written, and so is the 'causes' sweep
// that denies a supersession (see withholdSupersedesDrop for why that one is not
// withheld: Reassess cannot reach a 'causes' edge, and nothing demotes on one).
const ordinaryPassWithdrawsSupersedes = false

// withholdSupersedesDrop turns a verdictDrops PREDICTION into what the pass will
// actually do, and records the difference for the report.
//
// It takes the two counts the prediction returned and returns the counts the
// apply block will realise. The 'supersedes' count is zeroed whenever it is
// non-zero, because that is precisely the case the ordinary pass declines
// (ordinaryPassWithdrawsSupersedes), and the 'causes' count is passed through
// untouched: a 'causes' edge is swept, not withheld, and the reason is that
// Reassess — the repair every report here names, and the one that re-judges an
// edge under the current rules — loads live 'supersedes'/'llm' edges and can
// never see a 'causes' one, so a 'causes' edge left contradicting a supersession
// would be a contradiction under a repair that cannot reach it, which is the
// shape Result.Bidirectional refuses to create when it declines to judge a cycle.
// (`--withdraw` can remove a 'causes' edge, but only for a pair an operator has
// already named by hand; it is a targeted undo, not a re-judge.) It costs no
// demotion either: nothing ranks on a 'causes' edge, so the harm this rule exists
// to stop cannot come from leaving one live.
//
// suppressed is true only for a pair that CARRIED a live 'supersedes' edge, which
// is what makes the report's two counts disjoint: a fresh pair sweeps nothing
// because there is nothing to sweep, and a 'causes'-edge pair's withdrawal is a
// real one the run really made.
func withholdSupersedesDrop(supersedes, causes int) (dropSup, dropCauses int, suppressed bool) {
	if supersedes > 0 && !ordinaryPassWithdrawsSupersedes {
		return 0, causes, true
	}
	return supersedes, causes, false
}

// sweepCausesBothWays removes the pair's live 'causes' edges in BOTH directions
// and returns how many rows it moved, which is what a denying verdict owes: it
// says the pair is no relation at all, and a 'causes' cycle's two edges deny each
// other as squarely as an edge the verdict happens to name does.
//
// The first statement is unconditional because it is the sweep the pass has
// always made — the 'causes' edge in the direction this verdict implies. The
// second is gated on having READ an edge the other way, so the ordinary pair pays
// no second write and a cycle's other half — which is the only thing the second
// statement can move — is still removed.
//
// A DENYING verdict is the one place the both-directions rule cannot be optional.
// An affirming verdict writes an edge in the direction it was asked about, and the
// guard in front of that write closes the cycle; here nothing is written, so a
// sweep that stopped at one direction would leave the pair's other edge live and
// the report would claim the pair is unlinked while the graph still asserts that
// each of its notes caused the other.
func sweepCausesBothWays(ctx context.Context, store vectorStore, claims map[pairKey][]liveEdge, newer, older string) (int, error) {
	dropped, err := store.InvalidateLink(ctx, older, newer, string(RelationCauses))
	if err != nil {
		return 0, fmt.Errorf("invalidate causes link %s→%s: %w", older, newer, err)
	}
	total := int(dropped)
	if !claimsHold(claims, older, newer, RelationCauses) {
		return total, nil
	}
	reverse, err := store.InvalidateLink(ctx, newer, older, string(RelationCauses))
	if err != nil {
		return total, fmt.Errorf("invalidate reverse causes link %s→%s: %w", newer, older, err)
	}
	return total + int(reverse), nil
}

// Classified pairs a Candidate with the verdict Run() reached for it — used
// by callers (the CLI) to report the actual relation written, not just that
// "something" was confirmed.
//
// Reclassified and Withdrawn are what make a row about an EXISTING edge rather
// than about a proposal, and they are two fields because they are two facts a
// report must not merge. Reclassified says the pair carried a live edge, so any
// verdict but a re-affirmation of THAT edge withdrew something the graph already
// held; Withdrawn says this call's invalidation actually moved it, which
// is 0 in a dry run and can be lower than the count under --apply when a
// concurrent pass took the edge first. A row that carried only the verdict was
// indistinguishable from a fresh pair the pass declined (#785), so a caller
// could neither name the edge the pass withdrew nor the memory whose
// resolution that withdrawal orphaned.
type Classified struct {
	Candidate
	Relation Relation
	// JudgedAt is the freshness of the CONTENT this verdict was made against:
	// the later of the two endpoints' updated_at as this pass read them, before
	// it spent the call. It is the stamp the apply block writes onto the edge it
	// creates, and it is why it is read from the pass's own snapshot rather than
	// from the clock — see memory.CreateLinkJudged, which is where the reason is
	// set out. Empty only when an endpoint is missing from that snapshot, which
	// the existence check has already refused by then.
	JudgedAt string
	// Reclassified marks a pair the graph already asserted: a live edge decided
	// which way round it was judged, so the verdict is about an edge in the store
	// and not about a proposal. That edge is 'supersedes' or 'causes' (#823), so
	// the relation it carried is named rather than assumed.
	Reclassified bool
	// ReclassifiedFrom is the relation the live edge carried, and empty for a
	// fresh pair. It is what tells a RE-AFFIRMATION from a CHANGE, and it exists
	// because the relation became load-bearing rather than incidental.
	//
	// While a live edge could only be 'supersedes', "the pair held a live edge"
	// and "the pair held a live 'supersedes' edge" were the same fact and a
	// verdict that was not SUPERSEDES was necessarily a change. A live 'causes'
	// edge makes the second reading fail in both directions: a CAUSES verdict on
	// such a pair re-affirms it, and a SUPERSEDES verdict on it REPLACES it.
	// Counting the first as a change (or the second as a re-affirmation) makes
	// Result.Reclassified mean whichever of the two the pass happened to take,
	// which is the property every counted outcome in this file is shaped to
	// avoid.
	ReclassifiedFrom Relation
	// Withdrawn is true only when --apply invalidated that live edge in THIS
	// run, whichever relation it carried. It is false in a dry run, and false
	// under --apply when a concurrent pass withdrew the edge first — the same
	// distinction Reassess draws, for the same reason: the report must not claim
	// a graph change it did not make.
	Withdrawn bool
	// WithdrawSuppressed marks a row whose verdict WOULD have taken the pair's
	// live 'supersedes' edge away and did not, because the ordinary pass reports
	// a withdrawal instead of making one (#845). It is the third of the three
	// facts a reclassified row carries — the edge was the pair's
	// (`Reclassified`), this call did not move it (`Withdrawn`), and this call
	// declined to move it — and it is its own field because it is the only one of
	// the three the operator can act on by re-running the same command: a
	// concurrent pass took the edge first, and nothing more is owed, while here
	// the edge is still live and still demoting its target until `--reassess` or
	// `--withdraw` removes it.
	//
	// It is set in BOTH modes and on a dry run, because the rule is the pass's
	// and not the flag's: `--apply` is a write flag, and the pass it selects
	// writes links. It says nothing about a 'causes' edge, which the pass really
	// does sweep (see withdrawsSupersedes).
	WithdrawSuppressed bool
	// OpposedLive marks a row whose edge this run did NOT write because the
	// pair's opposite direction was already live when the write was attempted
	// (#806). Without it the row prints as a link the pass created, which is
	// the same false claim Withdrawn's own comment is about, one decision
	// earlier and on the creating side rather than the withdrawing one.
	//
	// It is false in a dry run, where nothing is attempted and so nothing can
	// be opposed: a dry run's promise is about the verdict, and the race is a
	// property of the write. Only an AFFIRMATIVE verdict can carry it, because
	// only those writes are guarded — and a reclassified row cannot, since the
	// live edge decides the direction a reclassified pair is asked about, so its
	// write is in the edge's own direction and nothing opposes it.
	OpposedLive bool
	// TargetProjectID is the project the target lives in, and the follow-up the
	// CLI prints is scoped to IT rather than to the project the pass was run
	// against: a resolve repair's pool is filtered by project, so a repair scoped
	// to the wrong one silently clears nothing. See RepairableTargets.
	TargetProjectID string
	// CausesDropped is how many live 'causes' edges this pair's verdict ALSO
	// removed, and it is the second graph mutation a verdict performs — Reassess
	// reports it on the row for the same reason, and a report that names the
	// supersedes edge and not the causes edge says the run moved one row when it
	// moved two.
	//
	// It counts only what --apply actually invalidated, so it is 0 in a dry run
	// and CausesDroppable is the forecast for that case: a dry run that could not
	// say it would drop an edge was a preview of half a mutation. The forecast
	// costs no read, because it is counted off the edges this pass already loaded
	// — the alternative, a prediction query, is a second read whose failure would
	// then have to fail the pass, and this pass's contract is that a write error
	// is the only thing that aborts one. The two numbers can differ, because a
	// concurrent pass may take a row between the read and the sweep; the observed
	// one is the truth about what this run did.
	//
	// Both are 0 for a FRESH pair's causes sweep, which is therefore not reported
	// on any row: a fresh pair produces no withdrawal row to carry it, and the
	// pass has no per-pair line for a proposal it declined to link. That is the
	// one place this count is still invisible.
	CausesDropped int
	// CausesDroppable is how many of the pair's live 'causes' edges this verdict
	// WOULD remove — the dry run's forecast of CausesDropped, and 0 outside a
	// reclassified row because a fresh pair reports no sweep.
	CausesDroppable int
}

// withdrewLiveEdge reports whether THIS run took the pair's live edge away, from
// the two counts the apply block's invalidations returned.
//
// The suppressed case is false and not `true`, and that is the whole of #845: the
// pair's 'supersedes' edge is still in the graph, so a row that claimed a
// withdrawal over it would be claiming a deletion nobody made — the identical
// false claim `Withdrawn` is documented against, reached one rule earlier. The
// `causes` rows really did go, so the row still names them (CausesDropped, and
// the report's `[+N causes edge dropped]` clause); they are a different edge, and
// a graph that lost two rows did not keep the one this row is about.
func (c *Classified) withdrewLiveEdge(dropped int64, causesDropped int) bool {
	if c.WithdrawSuppressed {
		return false
	}
	return c.Reclassified && (dropped > 0 || causesDropped > 0)
}

// withdrawSupersedesEdge removes the pair's live 'supersedes' edge, unless this
// run is withholding that removal (#845), in which case it removes nothing and
// the row says so.
//
// It is one function because it is ONE rule, reached from all three apply-block
// branches that deny a pair: a CAUSES verdict, a NEITHER and a REVERSED each
// used to spell the invalidation out, so restoring the delete after #845 would
// have meant finding three call sites and asking whether any of them had grown a
// reason of its own. It returns 0 for a withheld row, which is what keeps the
// branches' `Withdrawn` arithmetic honest through withdrewLiveEdge rather than
// through three separate guards.
func withdrawSupersedesEdge(ctx context.Context, store vectorStore, c *Classified) (int64, error) {
	if c.WithdrawSuppressed {
		return 0, nil
	}
	return store.InvalidateLink(ctx, c.NewerID, c.OlderID, string(RelationSupersedes))
}

// Result summarizes a pass.
type Result struct {
	Candidates int
	Confirmed  int // SUPERSEDES verdicts
	Created    int // supersedes links written (0 in dry-run)
	// CausesCreated counts CAUSES VERDICTS, which is not what Created counts:
	// Created is a WRITE count and this is not, because the 'causes' write is
	// guarded and can be refused in the same run that reached the verdict (#823
	// made that reachable, since before it the write was unguarded and always
	// landed). A field named for what a relation "created" and counting verdicts
	// is read as a write count by every caller that is not holding the diff in
	// its head — which is the whole report, and the reason the write count is its
	// own field, CausesWritten.
	CausesCreated int // CAUSES verdicts (not writes — see CausesWritten)
	// CausesWritten counts 'causes' links actually written, and is Created's
	// twin: 0 in a dry run (nothing is attempted), and CausesCreated minus the
	// pairs the guarded writer refused. It is what a report has to count under
	// --apply, because "1 causes" over a refusal is a claim about the graph that
	// the refusal line two lines below it takes back (#834).
	CausesWritten int
	// Reclassified counts the pairs whose live edge's RELATION changed, or whose
	// live edge was invalidated — a verdict that re-affirmed the relation the
	// pair already held is not one of them, and the distinction is what
	// Classified.ReclassifiedFrom is for.
	Reclassified int // existing links whose relation changed or was invalidated
	// WithdrawSuppressed counts the pairs whose live 'supersedes' edge a denying
	// verdict would have removed and this pass reported instead (#845). It is a
	// count of its own rather than a subtraction from Reclassified because the
	// two are different findings for the operator: Reclassified says the graph
	// changed, this says an edge is still in it that the current rules no longer
	// support, and only a command a person asked for takes that edge out. Counted
	// in both modes — the rule is the pass's, not the flag's — and a pair is
	// counted at most once, because a supersession is written in one direction
	// and a pair that carried the edge in the other is the cycle Result.Bidirectional
	// refuses rather than judges.
	WithdrawSuppressed int
	// StaleSkipped is the PRE-CLASSIFY existence drop: a candidate whose endpoint
	// a concurrent pass had already replaced when the pass re-read the graph, so
	// the pair was discarded BEFORE any harness call was spent on it. It happens in
	// a dry run exactly as it does under --apply, and it is not in
	// Result.Candidates, which is recomputed from the surviving set afterwards.
	StaleSkipped int
	// StaleAtWrite is the OTHER population and the one that needed the counter:
	// a pair that was classified, and then found its endpoint replaced again
	// between the classify and the write, so a verdict was reached and paid for and
	// no edge was written. It is 0 in a dry run, which attempts no write, and it is
	// counted apart from StaleSkipped because a report line about it may not claim
	// a classify call was spent — StaleSkipped's guarantee is that none was.
	StaleAtWrite int
	Skipped      int // fresh pairs skipped via the NEITHER cache
	Unclassified int // pairs skipped because the classifier answer was unparseable
	Reversed     int // REVERSED verdicts: refused, never written
	// Vetoed counts pairs the deterministic imperative veto settled as
	// no-supersedes-edge with no harness call (see VetoSupersede). They are
	// counted rather than silently dropped, because a run that declines work it
	// did not do and reports the same totals as one that found nothing to do
	// reads as "nothing was skipped".
	Vetoed int
	// Unscored counts the project's memories the candidate scan could not score
	// at all, for want of a usable vector — none yet, or one written under
	// another model, width or task prefix. The pairs the pass proposed as NEW
	// candidates are therefore bounded by the part of the project the scan could
	// read, and a caller that reports them without this one reports a partial
	// scan in the voice of a complete one. It is the difference between "this
	// project holds no near-duplicate pair" and "this pass could not read all of
	// it", and the vectors it is waiting for belong to the embedding worker.
	//
	// Only the NEW candidates. A counted memory can still be an endpoint of a
	// reclassified pair, because that half of the pass works from link rows and
	// reads no vector — so Reclassified, and the Candidates total that mixes
	// both halves, are NOT bounded by what the scan could score, and neither is
	// any verdict counted over such a pair. A report that claims such a memory
	// is "in no pair this run considered" is wrong the first time a model change
	// retires a project's vectors and an edited edge comes back for re-judging.
	Unscored int
	// ReclassifiedNoWrite counts the reclassify pairs whose --apply effect is
	// purely destructive: a reversal and a NEITHER both only invalidate the
	// links they find, so neither re-links the pair.
	ReclassifiedNoWrite int
	// Unoriented counts the near-neighbour pairs the scan REFUSED to propose
	// because both rows carry the same updated_at and the same created_at, so
	// the pass has no chronology to order them by (see orient and
	// Selection.Unoriented). No classify call, no link, no cache row: a
	// direction invented from a hash is the #641 harm wearing a tiebreak.
	Unoriented int
	// OppositeLive counts the pairs whose fresh proposal was the REVERSE of a
	// live edge — 'supersedes' or 'causes' (#823) — and whose fresh orientation
	// the pass therefore refused (#778). It counts REFUSED PROPOSALS, not
	// classifications: a
	// refused pair still has to survive the endpoint-existence check,
	// skip-if-unchanged (a live edge whose endpoints have not moved since it was
	// last judged is not re-judged, whichever way the scan proposed it), the
	// scope and persistent filters and the imperative veto before any of them
	// reaches the classifier, so a counted pair may be one the pass spent no
	// call on. What the count is exactly is what the pass decided HERE — the
	// graph's direction beat the scan's — which is why the report line says the
	// orientation was refused rather than claiming a judgment, and why the pairs
	// the classifier did see in the link's direction are the ordinary
	// Candidate/Reclassified totals above. A proposal that AGREED with the edge
	// is not counted at all: nothing was refused.
	OppositeLive int
	// Bidirectional counts the pairs the graph already claims in BOTH directions
	// in 'supersedes' — the cycle a pass before #778 could write, left in place for
	// `--reassess` to withdraw. This pass refuses the pair outright: it judges
	// nothing, writes nothing, and withdraws nothing (that is
	// `ghost supersede <project> --reassess`, the repair path), because a cycle
	// demotes both endpoints and no orientation of it can be judged into a
	// state worth keeping.
	//
	// 'supersedes' and only 'supersedes' — and that is not a gap in the count but
	// the consequence of the harm the rule exists for. A 'causes' cycle demotes
	// nothing, and neither of its two edges can be re-read as a claim about which
	// note is current, so the ordinary pass settles such a pair instead of
	// refusing it: it is judged ONCE in the direction the timestamps give (the
	// only direction a pair claimed both ways has) and whichever verdict comes
	// back converges, because an affirming verdict writes that direction and drops
	// the other and a denying one drops both. A refused pair is a pair nothing
	// will ever judge, and `--reassess` cannot reach a 'causes' edge at all, so
	// refusing one would leave a contradiction in the graph with a repair line
	// pointing at a command that cannot see it.
	Bidirectional int
	// ReverseLive counts the edges this run had a verdict for and did NOT write,
	// because the pair's opposite direction was already live by the time the
	// write reached the store — a concurrent `ghost supersede
	// --apply` that read the same unclaimed pair, scanned it the other way round,
	// and wrote first (#806). It is the write-time twin of OppositeLive: that one
	// is the graph's direction beating this pass's SCAN, this one is the graph
	// beating this pass's WRITE, and a pass can lose the second race having won
	// the first.
	//
	// It is counted rather than absorbed into Created for the reason every other
	// refusal here is: a pass that proposed an edge and wrote nothing reads
	// exactly like a pass that proposed nothing, and an operator reading a report
	// has to be able to tell a racing pass from a quiet corpus. The repair is the
	// next ordinary pass, which reads the live edge and judges the pair in ITS
	// direction — and `ghost supersede --reassess` if the two disagree about
	// which of them is current.
	//
	// BOTH relations since #823. The guarded writer refuses the second direction
	// of a pair whatever the relation is, and a pair can only reach the guarded
	// write in the direction the live edge asserts — so the refusal is a one-off
	// race for 'causes' exactly as it is for 'supersedes'. Before #823 the
	// 'causes' write went through the unguarded spell, because with no direction
	// override for that relation the pair was re-proposed in the flipped direction
	// on every pass and a guard would have refused it on every pass, forever: a
	// permanent paid refusal is worse than the contradiction it prevents, which
	// is why the override and the guard had to land together.
	ReverseLive int
	// Consensus is the number of classification passes this run made over the
	// candidate set: 1 for the ordinary pass, N for a consensus-gated one. It is
	// on the report because it is the multiplier on the pass's cost, and a caller
	// reporting classify calls without it cannot say what was paid for.
	Consensus int
	// ConsensusPairsAsked is how many PAIRS the passes were asked about in
	// total, which is Consensus times the pairs that survived every free filter.
	// It is not the provider call count — batching means one call carries up to
	// eight pairs — so it is the number a caller compares against the candidate
	// total to see the multiplier actually applied, and the number a test asserts
	// against rather than inferring from a mock's call log.
	ConsensusPairsAsked int
	// NotAgreed counts the pairs the N passes did not all answer the same way,
	// and Disputed is one record per such pair. Nothing was written for any of
	// them: no link, no withdrawal, no NEITHER cache row, no decided row.
	//
	// It is a counted, named outcome rather than a silent drop for the reason
	// every other refusal here is: a gated run that proposed edges and wrote none
	// reads exactly like a run that found nothing, and an operator deciding
	// whether to keep a consensus gate needs the count of what it suppressed to
	// judge the gate at all.
	NotAgreed int
	// Disputed names the pairs NotAgreed counts, one record each with the tally
	// that failed to reach a quorum — because "not agreed" without the numbers is
	// not actionable, and the ids are what the operator reads or re-runs.
	Disputed []Disputed
}

// Disputed is one pair the consensus gate refused, and it carries the evidence
// for the refusal rather than only its existence.
//
// The two verdicts it must be able to distinguish are the ones that call for
// different next steps, and the remedies have to be named correctly here because
// this is the comment a maintainer reads before changing the split path. A split
// between two READABLE verdicts is a model that cannot decide the pair, and the
// remedies are a re-run (fresh passes may agree) or a different harness — NOT a
// higher N, which is the opposite of a remedy: raising N makes unanimity
// STRICTLY HARDER, since a pair that split 2-1 at N=3 then has to satisfy one
// more pass at N=4. A tally short because some pass's reply could not be parsed
// is a harness or prompt problem, and the remedy is neither of those. Collapsing
// them into one count would send an operator to raise N for what is really an
// unreadable answer, and N is the wrong dial in the other case too.
type Disputed struct {
	NewerID string
	OlderID string
	// Tally is how many passes said each verdict, keyed by Relation. It is a map
	// rather than a slice because the question a reader asks is "how many said
	// supersedes", and a fixed-order list makes them count the entries.
	Tally map[Relation]int
	// Unreadable is how many passes answered with something the parser could not
	// read. Those passes voted for no verdict, which is why they are held apart
	// from Tally rather than counted as an empty verdict: the report says
	// "2 supersedes, 1 reversed" and "2 supersedes, 1 unreadable" are different
	// findings.
	Unreadable int
}

// voteCount is one pair's tally across the N passes. It is a small struct rather
// than a map per pair because it is built once per pass per pair and read once:
// a map allocation per pair per pass is work the N-fold multiplier should not
// add, and the four verdicts plus the unreadable case are a fixed set.
type voteCount struct {
	supersedes int
	causes     int
	neither    int
	reversed   int
	// unreadable counts Relation("") — a reply the parser could not read. It is
	// its own bucket and NOT a vote, because an unparseable answer expresses no
	// opinion about the pair and counting it as agreement with NEITHER would
	// write a decision nobody made.
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
		// Relation("") and anything else the parser invented: no opinion.
		v.unreadable++
	}
}

// quorum is what the N passes decided about one pair: an agreed verdict to act
// on, no agreement at all, or no readable answer from any of them.
//
// The rule is UNANIMITY over all N passes, not a majority, and that is the whole
// design rather than a simplification. #779's measurement is the argument: an
// edge proposed in all three runs scored 0.79 precision, one proposed in two
// scored 0.56, and one proposed in a single run scored 0.33 — so the number that
// predicted correctness was the count of runs that proposed it, and a 2-of-3
// majority writes exactly the middle row. A plurality rule would be cheaper to
// explain and would write the edges the measurement says are 56%.
//
// The unreadable bucket votes for nothing, so any unreadable pass blocks a
// quorum — a pair the model read in one pass and garbled in another is a pair
// whose verdict the model has not settled, and the gate's whole claim is that it
// writes only settled ones. But "all N unreadable" is a THIRD outcome and not a
// disagreement: nothing was decided, nothing is claimed, and the pair must be
// re-asked on the next pass. So it reads as Unclassified (Result.Unclassified),
// which is the bucket the single-pass path has always put it in, and never as
// NotAgreed — a report that called a broken harness "N pairs the model did not
// agree on" would send an operator to raise the quorum for what is really a
// prompt or transport problem.
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

// verdictState is the outcome of the quorum test: one verdict, a split, or
// nothing readable. A named set of three rather than two booleans, because
// "disagreed" and "unreadable" are different findings with different remedies
// and folding them into one is how a harness fault gets reported as model
// instability.
type verdictState int

const (
	// agreed means all N passes gave the same verdict.
	agreed verdictState = iota
	// disputed means the passes split, or some answered and some did not.
	disputed
	// undecided means no pass produced a readable verdict.
	undecided
)

// tally renders the count as the map a Disputed record carries, and it reports
// the unreadable passes under Relation("") as well as in their own field: a
// caller rendering the map alone must not silently drop a pass that voted for
// nothing, and one rendering both must not count it twice. The map is the
// evidence, the field is the diagnosis.
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

// best is the verdict the most passes gave, for the log line. It reports a tie
// by the first verdict in a fixed order, which is a display choice and not a
// decision: nothing is written from it.
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

// judgedAt is the freshness of the content a verdict was made against: the
// later of the two endpoints' updated_at, read from the pass's OWN snapshot.
//
// It is read from the snapshot rather than from the clock because the two are
// minutes apart — the classify call sits between them — and an edit landing in
// that gap (reflect's consolidation rewrite, a save through a live `ghost mcp`)
// has an updated_at newer than the text the classifier saw. Stamping the write
// instead would cover that edit, and the pair would sit quiet against text no
// verdict was ever given for, which is the one thing the stamp must never do.
//
// An endpoint the snapshot does not hold contributes nothing rather than a zero
// stamp: it was already refused by the existence check before this is reached,
// and a "" here falls back to the write clock, which is the safe direction for a
// write that is about to happen anyway.
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
// CAUSES pair are written; a reclassify pair is re-linked by a CAUSES verdict
// and only invalidated by NEITHER or a reversal. Subtracting the no-write
// reclassifications matters because a pass whose whole effect is deleting a
// link should not tell the operator that --apply will write links for it.
//
// It keys off VERDICTS (Confirmed and CausesCreated) and never off the write
// counts, which is not an oversight: this answers "would an --apply of THIS
// result have something to write", and in a dry run there are no writes to read.
// #834 gave the 'causes' relation a write count of its own (CausesWritten)
// precisely so the report could count writes without changing what this
// promises — swapping CausesCreated for CausesWritten here would silence the
// hint on a dry run that found a verdict and could not act on it, which is the
// one pass whose verdict the operator most needs to be told about.
func (r Result) WouldWriteLinks() bool {
	return r.Confirmed > 0 || r.CausesCreated > 0 || r.Reclassified > r.ReclassifiedNoWrite
}

// endpointsExist reports whether every given memory ID is still live. Used
// to detect memories replaced by a concurrent reflect pass between candidate
// selection and link write.
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
// content evolves), classifies every pair once, and — when apply is true —
// writes or invalidates links per verdict:
//
//   - SUPERSEDES: 'supersedes' newer->older; any stale 'causes' older->newer
//     link for the same pair is invalidated.
//   - CAUSES: 'causes' older->newer (the cause precedes its effect); any
//     stale 'supersedes' newer->older link for the same pair is invalidated.
//   - NEITHER: nothing is written; any existing link for the pair (either
//     relation) is invalidated.
//   - REVERSED: refused. Nothing is written in either direction, and any
//     existing 'supersedes' newer->older or 'causes' older->newer link for the
//     pair is invalidated — the same both-relations sweep NEITHER performs, for
//     a link that would assert what this verdict just denied.
//
// A pair whose existing 'supersedes' link predates neither endpoint's last
// update is skipped (skip-if-unchanged) — reclassifying it would repeat the
// same verdict for no reason, and re-asking a pair whose verdicts are not
// stable run to run (#779) is how a correct edge gets withdrawn and re-created
// on alternating passes. The test is against the LINK's own created_at, so it
// holds whichever way the pair was proposed: a live edge the scan agrees with
// and a live edge the scan contradicts are the same test (#787), and being
// proposed twice is not a reason to be judged twice. The stamp moves when a
// verdict re-confirms the edge, and it is stamped with the freshness of the
// content that verdict was made against rather than with the moment the row
// landed (memory.CreateLinkJudged) — the two are a classify call apart, and an
// edit landing between them must not be covered by a stamp written after it.
// So an edge costs one call per endpoint EDIT rather than one per pass forever. Fresh candidates
// are classified unless both endpoints' content still matches a cached NEITHER
// verdict (the content-keyed NEITHER cache, schema v8). A cache skip is treated
// as a NEITHER verdict, and that is the whole of its warrant: it may not stand in
// for one on a pair the graph is linked in EITHER relation, so a pair a live
// edge names is never cache-skipped, whichever source proposed its direction and
// whichever relation the edge is (#823) — the graph-only-staleness bargain this
// pass used to make, and has withdrawn. Reclassify candidates are therefore never
// cache-skipped, so a live edge whose endpoints HAVE moved is still judged on
// every pass and self-healing is untouched. Cache rows are recorded on apply only — dry-run stays
// side-effect-free — and cascade away with their memories via the FK.
//
// Pairs whose endpoints are replaced by a concurrent reflect pass (the stop
// hook spawns both for the same session) are dropped and counted in
// Result.StaleSkipped rather than failing the pass — the pair no longer
// exists, so there is nothing to link.
//
// A pair whose endpoints' scopes conflict is dropped and logged, fresh or
// reclassifying alike, and any existing link for it is left in place: two claims
// about different environments never stand in a supersession relation, so the
// pass must neither spend a classify call on them nor write the edge.
// SupersedePenalties is what keeps an edge written before this rule existed from
// demoting anything. The reclassify half is what makes the rule reach an edge
// that already exists, and what it saves is one call per endpoint edit, not one
// per pass: skip-if-unchanged already holds an untouched pair quiet, and a
// re-confirm moves the stamp that test reads (#784), so the two halves together
// are what make a converged graph cost nothing per pass.
//
// CreateLink and InvalidateLink are both idempotent no-ops when there's
// nothing to change, so re-running Run converges and self-heals after
// reflection's cascade-delete of links. A pair whose verdict is unparseable is
// skipped and counted (Result.Unclassified); any other classifier error — a
// dead harness, an outage — is fatal so a transport failure cannot look like a
// successful, empty pass. A link-write error is fatal so a half-written pair is
// never silently left behind.
//
// A REVERSED verdict is refused rather than flipped, and never cached as
// NEITHER. The pair is oriented by updated_at, which is the wrong ordering
// exactly when a note was re-saved after the fact it reports: a maintenance
// benchmark on a real database wrote a 'supersedes' link from a stale bug list
// onto the fix that had superseded it hours earlier (#641), demoting the fix and
// promoting the stale claim. Flipping would trust the same unreliable
// judgment to pick the other direction, and the wrong-direction link is the
// harm; skipping costs one un-linked pair, which a later pass can still fix.
// Not caching it matters for the same reason — a NEITHER row would skip the
// pair for the life of its text, freezing the staleness bug this pass exists to
// fix. That is why the classifier is also shown each note's created_at: the
// timestamps are the only signal it has to tell a re-saved stale claim from a
// genuine later update.
//
// A PAIR is the unit, not a direction, and that is what #778 changed. The two
// sources disagreed about which way round a pair runs — the scan orients by
// updated_at, a live edge keeps the direction it was written with — and they
// were reconciled on the ordered pair, so a disagreement matched nothing and one
// pass put the same pair to the classifier twice in opposite orientations. Where
// a verdict could not decline a direction, that pass wrote a cycle, and a cycle
// demotes BOTH endpoints in ranking, so the harm is not "one edge is backwards"
// but "two live memories stop being reminded of". Three rules follow, each
// counted and on the report, and each a refusal rather than a guess:
//
//   - One pass classifies a pair at most once, in the direction the GRAPH
//     asserts whenever a live edge names one — and a live edge is 'supersedes'
//     OR 'causes' (#823), read through liveEdge so that the two relations'
//     opposite writing conventions are normalised before the rules apply. The
//     edge's direction is the one a
//     REVERSED verdict has to be shown, because that is the wrong edge --reassess
//     and this pass's own withdrawal both have to reach. A scan proposal that
//     contradicted it is refused AS A DIRECTION and counted
//     (Result.OppositeLive), which is a count of refused orientations and not of
//     classifications: the pair continues in the link's direction and is
//     re-judged only under skip-if-unchanged, like any other untouched live edge.
//     A scan proposal that AGREED with it is not counted and buys nothing either,
//     because the edge already describes the pair and the edge's stamp — not the
//     scan's vectors — is what says whether it still holds (#787).
//   - A pair the graph already claims in BOTH directions in 'supersedes' is
//     refused outright (Result.Bidirectional): no third direction exists, both
//     edges demote one of the pair's two memories, and the repair is the
//     withdrawal pass, not a verdict. A 'causes' cycle is NOT refused: nothing
//     demotes on a 'causes' edge and --reassess cannot see one, so it is judged
//     once in the timestamps' direction and the ordinary apply block converges
//     it. See pairDirection and Result.Bidirectional.
//   - A fresh pair whose rows tie on updated_at AND created_at is not proposed at
//     all (Result.Unoriented), because a bulk import stamps a whole batch with
//     one timestamp and the only remaining "chronology" is a pair of hex ids. A
//     LIVE edge naming such a pair is still revalidated: it already carries a
//     direction, so the tie rules out proposing one and says nothing about
//     judging one.
//
// Run makes ONE classification pass. RunWith with Options.Consensus above 1 makes
// N of them over the same candidate set and writes only what all N proposed, in
// the same direction; see Options.Consensus for the measurement that motivates
// unanimity over a majority, and for why the filters above it are applied once
// rather than per pass.
func Run(ctx context.Context, store vectorStore, cls Classifier, projectID string, threshold float32, apply bool, logger *slog.Logger) (Result, []Classified, error) {
	return RunWith(ctx, store, cls, projectID, Options{Threshold: threshold, Apply: apply}, logger)
}

// MinConsensus is the smallest consensus N that gates anything.
//
// 1 is the ungated pass and is not an offer: a gate that always agrees is a
// flag that reads like a safety control and is not one, and the CLI therefore
// refuses `--consensus 1` rather than running a second pass to produce the same
// answer twice. Every value at or above it costs N times the classify calls of
// the ungated pass, which is the price the operator is choosing.
const MinConsensus = 2

// Options are Run's per-call decisions. They are one struct rather than a
// seventh and eighth positional argument because every one of them is a THRESHOLD
// on how willing the pass is to act, and a caller that reaches for
// RunWith(ctx, store, cls, project, 0.8, true, 3, nil) cannot tell which is
// which. The zero value is the ordinary single-pass run.
//
// Threshold and Apply keep their Run parameter names and meanings exactly, so
// the two entry points are the same decision and a reader who knows one knows
// both.
type Options struct {
	// Threshold is the minimum cosine similarity for a candidate pair, the same
	// value the CLI's --threshold flag carries.
	Threshold float32
	// Apply writes what the verdicts decide. False is a dry run: the pass
	// classifies and reports and touches nothing, which is the ordinary way to
	// read a corpus before agreeing to any edges at all.
	Apply bool
	// Consensus is how many INDEPENDENT classification passes must answer a pair
	// the same way before its edge is written (#779). 0 and 1 mean the ungated
	// pass: one verdict, acted on. 2 or more runs the candidate set through that
	// many passes and writes only what ALL of them proposed, in the same
	// direction — see RunWith for what a disagreement does, which is nothing.
	//
	// It gates the WRITE, not the question, and the two halves compose with the
	// rules that already existed: skip-if-unchanged and the NEITHER cache are
	// applied ONCE, before the first pass, so a pair either of them would skip is
	// not asked N times; and every pass re-asks the pairs that DO get asked, so
	// no pass can answer from a verdict another pass already made. Those two
	// properties are the whole reason the cost is N times the number of pairs
	// that survive the filters and not N times the candidate set.
	Consensus int
}

// RunWith is Run with the per-call decisions in one value: see Options, whose
// fields carry the reasoning. A Consensus below MinConsensus is treated as 1
// (the ungated pass) rather than refused, so a config key an operator set to 0
// degrades to today's behaviour instead of failing a lifecycle phase at 2am —
// the refusal, with its reason, belongs at the flag and the config boundary.
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
	// Carried into Result, which is what the caller reports from: the pass's
	// totals cover only the memories it could score, and the ones it could not
	// are the whole difference between "this project holds no near-duplicate
	// pair" and "this pass could not read all of this project". Nothing in the
	// pass can fix that — the vectors belong to the embedding worker — but a
	// caller that knows the number can say so instead of implying a complete
	// scan over a corpus it never saw.
	// Consensus is set HERE, where `res` is created, and every return AFTER this
	// point carries `res` rather than a bare Result{} — including the two error
	// returns on the link reads below. A gated run that reported Consensus == 0
	// because a query failed would be indistinguishable on the page from an
	// ungated one, and a caller that inspects the Result alongside the error (the
	// bench harness does) would be told the multiplier was 1 on a run that was
	// configured for 3.
	//
	// The scan error ABOVE is the one path this does not cover, and it is not a
	// gap: `res` does not exist yet, and the CLI prints no report at all on an
	// error (it writes the error to stderr and exits 1), so there is no page on
	// which a wrong multiplier could be read. ConsensusPairsAsked is NOT set here
	// either: it is the multiplier times the pairs that survived every free
	// filter, and there are none on any of these paths, so zero is the answer
	// rather than a value carried to be overwritten. A report reading
	// "consensus 3: 0 pair(s) asked, 5 pair(s) vetoed" is the sentence an operator
	// wants precisely on the fully-vetoed path.
	res := Result{Unscored: sel.Unscored, Unoriented: sel.Unoriented, Consensus: passes}

	// BOTH relations, read before the scan is reconciled against anything. A
	// 'causes' edge was absent from this read until #823, and every rule below
	// was written as though the only live edge a pair could carry was a
	// 'supersedes' one — so a pair whose 'causes' edge disagreed with the
	// timestamps was re-proposed in the flipped direction on every pass, paid a
	// classify call for the re-ask, and answered CAUSES again, which wrote the
	// second direction of the pair. Reading the two together is the fix's whole
	// shape: the graph's direction wins whichever relation asserted it, and the
	// write guard (below) only becomes safe once it does.
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

	// The two sources, reconciled on the UNORDERED pair. Before #778 they were
	// keyed by the ORDERED pair, so a live edge pointing the opposite way from
	// the scan's orientation for the same two memories matched nothing and both
	// orientations were put to the classifier in one pass — and a verdict that
	// cannot decline a direction wrote both edges, a cycle whose two
	// SupersedePenalties demote BOTH endpoints. Keyed unordered, the two sources
	// meet: a pair is on the schedule exactly once.
	//
	// `claims` is the graph's half and it is kept in the apply block below, where
	// a sweep is only worth a second write if the pass has already read the row
	// it would move.
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

	// livePair names the pairs a live edge already asserts a direction for, and it
	// is what separates a reclassification from a fresh proposal from here on.
	// The live edge's DIRECTION is the one the pair is judged in, whatever the
	// scan's timestamps say: the edge is the claim the graph already makes, and
	// judging the pair in that direction is what lets a REVERSED verdict reach the
	// wrong edge and withdraw it (#641) instead of confirming the reverse one and
	// leaving the wrong edge in place.
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
			// A 'supersedes' CYCLE, the state #778's own bidirectional
			// write leaves behind and the one shape this pass refuses to
			// judge: both edges demote one of the pair's two memories,
			// neither withdraws the other, and no orientation of it can be
			// judged into a state worth keeping. Refused, counted, and
			// reported with the repair to run — this pass creates links, and
			// withdrawing one is `ghost supersede --reassess`, which is the
			// one repair that can see a 'supersedes' edge.
			res.Bidirectional++
			if logger != nil {
				logger.Info("supersede: refusing a pair the graph claims in both directions (ghost supersede --reassess --apply withdraws one)",
					"newer", edges[0].source, "older", edges[0].target)
			}
			continue
		case pairCauses:
			// A pair whose only live edges are 'causes' — one of them or a cycle
			// of them. The pair is JUDGED by the timestamps, and the edges are
			// RECONCILED by the verdict, which is the whole of what a 'causes'
			// edge may decide. Reading the edge's own direction here instead is
			// the blocker: a live `causes newer→older` edge then asks "does the
			// January note supersede the September one", a SUPERSEDES answer
			// writes exactly that, and the edge DEMOTES the memory that is
			// current (#641, produced by the pass rather than found by it).
			//
			// So the candidate is built from the same `orient` a fresh pair uses,
			// and the pair is still `livePair`: the edge's stamp is what
			// skip-if-unchanged reads (so an untouched pair costs nothing, which
			// was the re-bill) and the pair stays out of the NEITHER cache (which
			// would assert the edge is not there). Neither of those is a
			// direction, and the apply block is what reconciles the edge: a
			// CAUSES verdict writes the timestamp direction and drops whatever
			// the pair held, a denying one drops both.
			//
			// A 'causes' CYCLE is not refused either, for the reason that decided
			// it before: nothing demotes on a 'causes' edge and `--reassess`
			// loads live 'supersedes'/'llm' edges and can never see one, so a
			// refusal would be a contradiction with no repair. It converges in
			// the pass that settles it, because the CAUSES branch sweeps the
			// cycle's other half BEFORE the write.
			a, aOK := memByID[edges[0].newer]
			b, bOK := memByID[edges[0].older]
			if !aOK || !bOK {
				continue // an endpoint no longer exists
			}
			cm, co, oriented := orient(a, b)
			if !oriented {
				// The #778 tie, and a refusal for the same reason it is one in
				// a scan: both rows carry no chronology at all, so there is no
				// direction to ask "which is newer" about. A live 'supersedes'
				// link on a tied pair is STILL re-judged — that edge carries a
				// direction — and a 'causes' edge does not, so this is the one
				// live shape the tie reaches, which is why the report line has
				// to say which.
				//
				// Counted only where the SCAN did not already count this pair.
				// Both halves of a pass reach it — the scan as a near
				// neighbour, this loop as a live edge — and `Unoriented` is
				// documented as a per-pair count on a report line an operator
				// reads as pairs. The scan's refusal is kept and this one
				// suppressed rather than the reverse, because the scan's set is
				// the one built for the purpose: a pair it never reached has no
				// key in it, so it is still counted here.
				if !sel.unorientedPairs[key] {
					res.Unoriented++
					if logger != nil {
						logger.Info("supersede: a pair linked only by 'causes' whose two rows share both timestamps; no direction to judge it in",
							"a", edges[0].newer, "b", edges[0].older)
					}
				}
				continue
			}
			// skip-if-unchanged, on the EDGE's own stamp, and it is the whole of
			// what the edge buys: an untouched pair costs nothing whatever
			// direction it is asked in. This is the re-bill #823 is about, and it
			// is unaffected by the edge not being a direction any more.
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
		// The live edge decides the pair's DIRECTION. A scan proposal that
		// contradicts it is refused as a direction and counted; a proposal that
		// AGREES with it is not refused — and it is no reason to spend anything
		// either, because the edge already describes this pair and the edge's own
		// created_at, not the scan's vectors, is what says whether it still
		// holds. Both orientations therefore face the same skip-if-unchanged test
		// below. The agreeing one used to skip it by appending the scan's
		// candidate outright, which billed and re-rolled every edge the scan
		// could still see (#787) — and a scan that CAN see it is the ordinary
		// case, not an edge case, because writing an edge retires no vector.
		agree := isFresh && cand.NewerID == v.newer && cand.OlderID == v.older
		if isFresh && !agree {
			res.OppositeLive++
			if logger != nil {
				// Five values, five keys (#804). The line reports TWO disagreeing
				// orientations of one pair, and the labels say which source
				// asserted which: the relation and the link's own source and
				// target, and the scan's proposed newer and older. A shared
				// "link"/"scan" key with an unnamed value after it left slog
				// pairing the arguments itself, so a target became a KEY and the
				// line printed as `link=02EA044F… 3092A7BE…=scan …`. The relation
				// is on the line because 'supersedes' runs newer→older and
				// 'causes' older→newer: without it, source and target do not even
				// say which end of the pair the link's own ids are (#823).
				logger.Info("supersede: scan proposed the reverse of a live link; keeping the link's direction",
					"link_relation", string(v.relation),
					"link_source", edges[0].source, "link_target", edges[0].target,
					"scan_newer", cand.NewerID, "scan_older", cand.OlderID)
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
			// The scan's own candidate, and only because it carries the cosine
			// similarity measured just now, which the link row's stored strength
			// cannot: both describe the SAME orientation, which is the whole of
			// what the classifier is shown, so the edge contributes the freshness
			// reference above and nothing here.
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

	// Existence re-check before any LLM spend: the stop hook spawns
	// reflect and supersede for the same session, and reflect's
	// consolidation replaces rows (delete + new IDs) while this pass is
	// running. A pair whose endpoint already vanished would burn a
	// classify call and then die on the FK at write time.
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
					"newer", c.NewerID, "older", c.OlderID)
			}
			continue
		}
		// The scope refusal, at the one point every pair passes through, so it
		// covers the reclassify path as well as the fresh one. SelectCandidates
		// already refuses a conflicting fresh pair, but a 'supersedes' link
		// written before that guard existed is re-proposed on the first pass
		// after either endpoint is edited (skip-if-unchanged holds the rest
		// quiet, and CreateLink never touches memories.updated_at), and the
		// classifier cannot see scope — so that pass would spend a billed call
		// re-judging two claims about different environments and re-affirm an
		// edge that ought not to exist. The edge itself is left alone: scope
		// exempts a pair from ranking, it does not delete graph history, which
		// is the same line memory.DemotionPenalties draws for 'related' edges.
		if memory.ScopesConflict(newerMem.Scope, olderMem.Scope) {
			if logger != nil {
				logger.Info("supersede: dropping pair whose scopes conflict",
					"newer", c.NewerID, "older", c.OlderID)
			}
			continue
		}
		// The persistent refusal, here for the reason the scope one above is: this
		// is the point every pair passes through, so it covers the reclassify path
		// as well as the fresh one. An edge written before a memory was declared
		// keep-forever is re-proposed on the first pass after either endpoint is
		// edited, and re-affirming it is exactly the outcome the exemption exists
		// to prevent -- a billed call for a verdict that can only re-create a claim
		// about a memory nothing automatic may make.
		//
		// The edge is left alone rather than withdrawn: that is a different command
		// (`--reassess`, `ghost_link_withdraw`), and this pass is not where graph
		// history is deleted. What it stops is spending a call to keep it alive.
		if memory.RetentionExempt(newerMem) || memory.RetentionExempt(olderMem) {
			if logger != nil {
				logger.Info("supersede: dropping pair with a persistent endpoint",
					"newer", c.NewerID, "older", c.OlderID)
			}
			continue
		}
		live = append(live, c)
	}
	all = live
	res.Candidates = len(all)

	// The deterministic imperative veto, before the cache and before any call
	// (issue #686). A pair whose OLDER note states a standing rule and whose
	// NEWER note never names that rule as retired is settled here: no classify
	// call, no link, and no NEITHER cache row, because the veto is free to
	// recompute on every pass and a row would only add a stale one to reason
	// about. Measured over a real store, 43% of the proposed edges were pairs
	// whose two notes were both still true, and one of them demoted a "must"
	// rule — which the ranking guards demote and resolve's supersedes piggyback
	// then stamps resolved_at on, for good.
	//
	// It runs after the scope refusal, so the pass's two free "this pair may not
	// be linked" decisions stand together, and before the cache, so a vetoed
	// pair is never answered from a verdict the current rules would not have
	// given. An existing edge such a pair carries is left alone: this is the
	// creation pass, and Reassess is what withdraws an edge the current rules no
	// longer support.
	keep := all[:0]
	for _, c := range all {
		if reason, vetoed := VetoSupersede(c); vetoed {
			res.Vetoed++
			if logger != nil {
				logger.Debug("supersede: vetoed pair whose older note states a rule",
					"newer", c.NewerID, "older", c.OlderID, "reason", reason)
			}
			continue
		}
		keep = append(keep, c)
	}
	all = keep
	if len(all) == 0 {
		return res, nil, nil
	}

	// NEITHER-cache partition, after the existence re-check so only live pairs
	// can be skipped. A fresh pair whose endpoints' text still matches a stored
	// NEITHER verdict is skipped — no harness call, no write; a pair a live edge
	// names is never skipped, since its job is to revalidate that edge as content
	// evolves.
	//
	// "A live edge" is EITHER relation, and that used to be the one live shape
	// this cache held quiet that it should not have (#823). A cached NEITHER is
	// equivalent to a verdict saying the pair is no relation at all, so skipping a
	// pair a live 'causes' edge names asserts that the edge is not there — which
	// is graph-only staleness and was harmless while a 'causes' edge was only
	// swept, and is not harmless now that its DIRECTION decides which way the pair
	// is judged and therefore what the pass may write.
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

	// The consensus loop, and it sits HERE — after the NEITHER cache, after the
	// veto, after skip-if-unchanged and after the existence check — because that
	// ordering is what makes the multiplier worth paying. Every filter above is a
	// question the pass can settle about a pair for free, and every one of them
	// is a question whose answer cannot change between two passes over the same
	// text. So the N passes are spent on `pending` alone, and the cost is
	// N x len(pending) pairs asked rather than N x the candidate set: a pair the
	// NEITHER cache would have skipped is skipped ONCE, and a #787-era live edge
	// whose endpoints have not moved since it was last judged is not re-asked at
	// all, let alone N times.
	//
	// It is also what makes each pass a real re-ask. The cache is READ once, here,
	// and nothing writes a cache row until every pass is in hand and the apply
	// block runs, so pass 2 cannot be answered out of pass 1's NEITHER: the rows
	// MarkSupersedeNeither writes are this run's own output, not an input to it.
	// The store is not re-read between passes either, so pass 2 sees the same
	// text pass 1 saw — which is the point of asking again, and is also why an
	// edit landing mid-run is caught by the pre-write existence check rather
	// than by a fresher read (see the apply block).
	//
	// Each pass is an independent ClassifyBatch over the SAME candidate set in
	// the SAME order, so the only thing that can differ between two passes is the
	// model's own answer to the same question. That is the whole measurement
	// #779 asks for: 0.79 precision for an edge proposed in all three runs,
	// against 0.55 over distinct proposals and 0.33 for one proposed in a single
	// run. The gate writes the first number's subset and refuses the rest.
	//
	// A disputed pair is DROPPED before the classified slice, before the counts
	// below and before the apply block, which is what "never written" has to
	// mean: no link, no withdrawal, no NEITHER cache row and no row in the
	// report's decided list, only its own "not agreed" line carrying the ids and
	// the tally.
	//
	// It is also not a reversal, and must not be read as one. The pass asked N
	// times and got N different answers, which is a statement about the model's
	// stability and not about which endpoint is current, so it neither writes a
	// backwards link nor withdraws a live one: a live edge on such a pair is
	// re-judged on the next pass and can still be withdrawn there, on a quorum.
	var classified []Classified

	// The multiplier APPLIED, which is Consensus times the pairs that survived
	// every free filter — the one number the report and the bill are checked
	// against, and the reason the gate is affordable. Set here rather than at
	// construction because `pending` does not exist yet there; `Consensus` itself
	// was set where `res` is created so that the early returns carry it.
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
				// No pass produced a readable verdict. The single-pass branch's
				// behaviour verbatim, and for the same reason: an odd *phrasing*
				// must not abort the pass (a single unparseable verdict ended a
				// 9-minute run after links for earlier pairs had already been
				// written), so the pair is skipped, counted, logged, and left
				// uncached so the next pass re-asks it. A transport failure is
				// not this and stays fatal inside ClassifyBatch.
				res.Unclassified++
				if logger != nil {
					logger.Warn("supersede: skipping pair with an unclassifiable verdict",
						"newer", c.NewerID, "older", c.OlderID, "passes", passes)
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
						"newer", c.NewerID, "older", c.OlderID,
						"passes", passes, "top", string(top), "top_votes", n)
				}
				continue
			}
			key := newPairKey(c.NewerID, c.OlderID)
			// The relation the pair's live edge carried, or "" for a fresh
			// pair. It is what separates a verdict that RE-AFFIRMED the graph
			// from one that changed it, in the row and in the counter below — and
			// it cannot be inferred from the verdict, because a live edge may be
			// either relation (#823) and both readings are reachable.
			liveRelation := livePair[key]
			wasReclassify := liveRelation != ""
			// Carried on the row rather than left to the caller to reconstruct:
			// `livePair` dies with this call, and a caller reading only the
			// report could not tell a withdrawal from a fresh pair's silence
			// (#785). JudgedAt is #792's: the content freshness the apply block
			// stamps onto the edge it creates, read from this pass's own snapshot.
			// TargetProjectID comes from the same existence check, so the follow-up
			// can be scoped to the project whose repair pool holds that memory.
			// What the verdict's apply block will move beyond the one edge this
			// row is about, counted off the pass's own read and therefore with no
			// read of its own. The counter below needs it before the block runs;
			// the row needs it because a DRY RUN runs no block, so this is the
			// only count a preview of the deletion can have.
			//
			// A FRESH pair forecasts 0 without being told to, and the reason is
			// local: every path that appends a candidate sets `livePair` for that
			// pair, so a judged pair with an empty `livePair` entry is a pair with
			// no live edges — which is what `claims` being empty for it says. A
			// reset keyed on `wasReclassify` would be a second copy of that.
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
				// Counted HERE rather than where the apply block would have
				// invalidated the row, for the reason Reclassified is counted
				// here: a dry run runs no block, and the report has to carry this
				// number in both modes because the rule it reports is the pass's
				// and not --apply's.
				res.WithdrawSuppressed++
				if logger != nil {
					logger.Info("supersede: a denying verdict on a live supersedes edge is reported, not applied (ghost supersede --reassess --apply withdraws it)",
						"newer", c.NewerID, "older", c.OlderID, "verdict", string(verdict))
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
					// Neutral by design: this loop runs before the apply
					// block and cannot know whether the pair survives the
					// existence check, so it claims no write. The drop, if
					// it happens, is logged where it happens.
					logger.Warn("supersede: refusing a reversed verdict (older note is the current one)",
						"newer", c.NewerID, "older", c.OlderID)
				}
			}
			// A verdict that came back as the relation the live edge already
			// held changed nothing and is not counted; any other verdict replaced
			// that edge's relation or dropped it, and both are the mutation this
			// count has always meant. The two tests were the same test before
			// #823, because a live edge could only be 'supersedes' — see
			// Classified.ReclassifiedFrom for why they stopped being one.
			//
			// A re-affirmation is not automatically a non-event: a verdict can
			// keep the relation the pair was judged around and still remove a row
			// of the OTHER one — a 'causes' CYCLE answered CAUSES keeps the
			// direction it was asked about and drops the edge asserting the other,
			// and a SUPERSEDES verdict re-affirming a 'supersedes' edge beside a
			// 'causes' cycle drops both of the cycle's edges. Counting only the
			// relation change is how a run that really did delete a graph row
			// reported "0 reclassified"; verdictSweepsBeyond is the half that
			// catches it, on the same predicate the apply block's sweeps are
			// gated on.
			//
			// A WITHHELD withdrawal is excluded from the whole test, because
			// it is not a change: a NEITHER and a REVERSED on a pair whose
			// only live edge was the withheld 'supersedes' one now move
			// nothing, and counting them prints "1 reclassified" over an
			// --apply pass that wrote no link and deleted none (#845). The
			// relation test still differs for such a pair, so it cannot be
			// dropped to exclude them — the exclusion is its own term. A
			// CAUSES verdict is exempt because the 'causes' write beside the
			// withheld withdrawal really does land, which is the difference
			// between a pair that changed and one that only declined.
			movedSomething := verdict != liveRelation || dropSup > 0 || dropCauses > 0
			if suppressed && verdict != RelationCauses {
				movedSomething = dropCauses > 0
			}
			if wasReclassify && movedSomething {
				res.Reclassified++
				// The other affirmative verdict re-links the pair; NEITHER and a
				// reversal only drop what is there.
				if verdict == RelationNeither || verdict == RelationReversed {
					res.ReclassifiedNoWrite++
				}
			}
		}
	}

	if apply {
		// writable tracks the pairs whose endpoints passed the existence
		// check below. The NEITHER-cache write further down is a single
		// transaction, so a pair that went stale mid-pass must be excluded
		// from it: inserting a row for a deleted endpoint would hit the FK
		// and roll back every other pair's row with it.
		writable := make(map[[2]string]bool, len(classified))
		// Indexed, because the row records what its own write moved: the
		// supersedes invalidation's count is the only evidence of whether THIS
		// run withdrew the edge or a concurrent pass had already taken it, and a
		// report that cannot tell those apart claims a graph change nobody made
		// (#785, the same distinction Reassess draws).
		for i := range classified {
			c := &classified[i]
			// Pre-write existence check: the candidate was alive at
			// selection (and possibly at the batch check above), but a
			// concurrent reflect pass may have replaced it during the
			// classify loop. Writing then would die on the FK and abort
			// the whole pass; skipping is always correct — the pair no
			// longer exists, so there is nothing to link.
			pairAlive, err := endpointsExist(ctx, store, c.NewerID, c.OlderID)
			if err != nil {
				return res, nil, fmt.Errorf("stale check %s→%s: %w", c.NewerID, c.OlderID, err)
			}
			if !pairAlive {
				// StaleAtWrite and NOT StaleSkipped: this pair WAS classified, so
				// the report line about it can honestly say a classify call was
				// spent and no edge was written. The pre-classify site above cannot
				// say that, which is why the two are counted apart.
				res.StaleAtWrite++
				// A WITHHELD withdrawal is UNDONE here, and this is the only site
				// that can undo it (#845). `WithdrawSuppressed` was set in the
				// classify loop, before this check, and the edge it says is still
				// live is NOT: `memory_links.source_id`/`target_id` are
				// `ON DELETE CASCADE`, so the replacement that made this endpoint
				// stale took the edge with it. A row left claiming a live edge
				// here would say the opposite of the truth for a memory that no
				// longer exists, and the summary's withheld line would compound it
				// ("still live and still demoting its target"). Clearing the row
				// puts it back on `already gone`, which is what it was before
				// #845 and is exactly what happened.
				//
				// The counter is decremented rather than left alone for the same
				// reason the row is cleared: it is the count OF these rows, and the
				// report quotes it as what the pass is still holding in the graph.
				// A stale pair is dropped from the NEITHER cache below (`writable`),
				// so it is re-judged next pass regardless, and nothing is lost by
				// not counting it here.
				if c.WithdrawSuppressed {
					c.WithdrawSuppressed = false
					res.WithdrawSuppressed--
				}
				if logger != nil {
					logger.Info("supersede: skipping write, endpoint replaced by a concurrent pass",
						"newer", c.NewerID, "older", c.OlderID)
				}
				continue
			}
			writable[[2]string{c.NewerID, c.OlderID}] = true
			switch c.Relation {
			case RelationSupersedes:
				wrote, err := store.CreateLinkUnopposed(ctx, c.NewerID, c.OlderID, string(RelationSupersedes), c.Similarity, "llm", c.JudgedAt)
				if err != nil {
					return res, nil, fmt.Errorf("create supersedes link %s→%s: %w", c.NewerID, c.OlderID, err)
				}
				if wrote {
					res.Created++
				} else {
					// The pair's OTHER direction was already live when this write
					// reached the store, which is the concurrent pass #806 is about.
					// The edge that exists is the earlier writer's, this run adds
					// nothing to it, and the pair is re-offered on the next pass —
					// where the live edge, rather than this pass's scan, decides the
					// direction the pair is asked about.
					//
					// It does NOT skip the 'causes' sweep below, and that is the half
					// of this branch that is easy to get wrong: the refusal is about
					// one relation, and a live edge in the opposite direction says
					// nothing about whether the pair carries a 'causes' edge. The
					// verdict decided the pair is a supersession, so a 'causes' edge
					// asserting the opposite of that is stale whichever way round the
					// pair is claimed.
					res.ReverseLive++
					c.OpposedLive = true
					if logger != nil {
						logger.Info("supersede: not written, the pair's reverse supersedes edge is already live (a concurrent pass wrote it first)",
							"newer", c.NewerID, "older", c.OlderID)
					}
				}
				// The 'causes' sweep, and it now has a direction to sweep in.
				// A verdict of SUPERSEDES says the pair IS a replacement, which
				// denies a 'causes' claim in EITHER direction: 'causes' asserts
				// that one note is the cause of the other, and a pair whose newer
				// note retires the older one is not that. The existing statement
				// took the one direction the verdict implies; the other is the
				// same denial read from the other end, and leaving it behind is
				// how a 'causes' cycle survives a pass that had every reason to
				// end it. It costs a second write only when the pass has already
				// read an edge that way (claimsHold), which is the only state in
				// which the statement can move a row.
				causesDropped, err := store.InvalidateLink(ctx, c.OlderID, c.NewerID, string(RelationCauses))
				if err != nil {
					return res, nil, fmt.Errorf("invalidate causes link %s→%s: %w", c.OlderID, c.NewerID, err)
				}
				c.CausesDropped = int(causesDropped)
				// The sweep above takes the 'causes' edge that AGREES with the
				// supersession (older→newer), which is the one this pair would
				// otherwise have held. This one takes the edge that CONTRADICTS
				// it — 'causes' source→target, the same two ids the supersedes
				// edge runs, so it claims the reverse chronology — and the two are
				// the same denial read from the other end, so a verdict that
				// keeps leaving one of them behind is a verdict that half-settled
				// the pair. Gated on having read it, because a sweep that can only
				// move a row the pass has already seen is the one worth making.
				if claimsHold(claims, c.OlderID, c.NewerID, RelationCauses) {
					reverseDropped, err := store.InvalidateLink(ctx, c.NewerID, c.OlderID, string(RelationCauses))
					if err != nil {
						return res, nil, fmt.Errorf("invalidate reverse causes link %s→%s: %w", c.NewerID, c.OlderID, err)
					}
					c.CausesDropped += int(reverseDropped)
				}
				// A live 'causes' edge replaced by a 'supersedes' one IS the
				// withdrawal this row reports; a live 'supersedes' edge
				// re-affirmed beside a stale 'causes' edge is not, because the
				// edge the pair is JUDGED around survived. The relation the live
				// edge carried is what tells the two apart, which is the whole
				// reason Classified.ReclassifiedFrom exists.
				c.Withdrawn = c.Reclassified && c.ReclassifiedFrom == RelationCauses && c.CausesDropped > 0
			case RelationCauses:
				// The GUARDED writer, and the reason it could not be before #823
				// is the whole shape of this fix rather than a footnote to it.
				//
				// A guard refuses the second direction of a pair, and while the
				// only live edge the pass read was a 'supersedes' one, a pair
				// whose 'causes' edge ran against the timestamps was re-proposed
				// in the flipped direction on EVERY pass, answered CAUSES again,
				// and would have been refused again, forever: a refusal that never
				// converges costs a classify call per pass for as long as the
				// edge lives, which is worse than the contradiction it prevents.
				// That is why #819 shipped this unguarded and pinned the
				// asymmetry, and it was right to.
				//
				// The direction override above is now built from live edges of
				// BOTH relations, so a pair holding a 'causes' edge is judged in
				// THAT edge's direction and the write here is in the same
				// direction. The refusal is therefore the same one 'supersedes'
				// has: a cross-process race that costs one call, and a state the
				// next pass settles by reading the live edge. A 'causes' cycle
				// demotes nothing, which is why it was ever worth writing — but
				// the contradiction it creates is read as "these two notes
				// caused each other", and the pass can no longer produce one.
				// The SWEEPS first, and the order is the point rather than a
				// style choice.
				//
				// A CAUSES verdict denies a same-fact replacement in either
				// direction, so the 'supersedes' sweep goes both ways for the same
				// reason the SUPERSEDES branch's 'causes' sweep does: the
				// statement below takes the edge that agrees with this
				// supersession (newer→older), and the one inside the guard takes
				// the 'causes' cycle's other half.
				//
				// Sweeping before writing is what lets a 'causes' CYCLE converge
				// in the pass that settles it. Written first, the guarded write
				// finds the cycle's other half live and refuses — correctly, it
				// is the reverse of what it is about to write — and the sweep
				// below then drops that half, so the pair ends up holding the
				// OLD edge with its OLD stamp and the next pass asks about it
				// again. Swept first, the write is unopposed, lands with this
				// verdict's stamp, and the pair is quiet from here on. The race
				// the guard exists for is unaffected either way: another process
				// writing the reverse between the sweep and the write is what
				// makes the write refuse, which is the same one-off race
				// 'supersedes' has.
				dropped, err := withdrawSupersedesEdge(ctx, store, c)
				if err != nil {
					return res, nil, fmt.Errorf("invalidate supersedes link %s→%s: %w", c.NewerID, c.OlderID, err)
				}
				causesDropped := 0
				if claimsHold(claims, c.OlderID, c.NewerID, RelationCauses) {
					// A 'causes' CYCLE this pass judged: the verdict keeps the
					// direction it was asked about and the edge asserting the
					// other one goes. This is the statement that ends the cycle,
					// and it is why a 'causes' cycle is judged rather than refused
					// (see the reconciliation above).
					reverseDropped, err := store.InvalidateLink(ctx, c.NewerID, c.OlderID, string(RelationCauses))
					if err != nil {
						return res, nil, fmt.Errorf("invalidate reverse causes link %s→%s: %w", c.NewerID, c.OlderID, err)
					}
					causesDropped = int(reverseDropped)
				}
				wrote, err := store.CreateLinkUnopposed(ctx, c.OlderID, c.NewerID, string(RelationCauses), c.Similarity, "llm", c.JudgedAt)
				if err != nil {
					return res, nil, fmt.Errorf("create causes link %s→%s: %w", c.OlderID, c.NewerID, err)
				}
				if !wrote {
					// The pair's other 'causes' direction was already live when
					// this write reached the store. The sweeps above dropped the one
					// this pass could SEE, so on a store whose 'causes' edges are all
					// written by this pass — the only way production writes one — the
					// edge that opposed this write is a CONCURRENT writer's: this
					// pass's graph read predates its commit. The edge that exists is
					// the other writer's, the next pass judges the pair in ITS
					// direction, and this run says so rather than reporting a link it
					// did not write.
					//
					// CausesWritten is NOT incremented here, and that is the whole of
					// #834: a verdict and a write are different facts, and a summary
					// that adds one where only the other is true reports an edge this
					// run did not write. The refusal is counted where it happened.
					res.ReverseLive++
					c.OpposedLive = true
					if logger != nil {
						logger.Info("supersede: causes link not written, the pair's reverse causes edge is already live (a concurrent pass wrote it first)",
							"older", c.OlderID, "newer", c.NewerID)
					}
				} else {
					res.CausesWritten++
				}
				c.CausesDropped = causesDropped
				// The withdrawal this row reports is the pair's live edge
				// whichever relation carried it, so the flag is the sum of the two
				// sweeps rather than the 'supersedes' one alone: a live
				// 'causes' cycle judged CAUSES dropped an edge here, and a
				// report that said "would withdraw" over a row this run removed
				// is the same false claim in the other direction. A WITHHELD
				// 'supersedes' sweep is not part of that sum (#845) — it removed
				// nothing, so the row names no withdrawal over it and reports the
				// withholding instead.
				c.Withdrawn = c.withdrewLiveEdge(dropped, causesDropped)
			case RelationNeither:
				dropped, err := withdrawSupersedesEdge(ctx, store, c)
				if err != nil {
					return res, nil, fmt.Errorf("invalidate supersedes link %s→%s: %w", c.NewerID, c.OlderID, err)
				}
				causesDropped, err := sweepCausesBothWays(ctx, store, claims, c.NewerID, c.OlderID)
				if err != nil {
					return res, nil, err
				}
				c.CausesDropped = causesDropped
				// Either relation counts as "this run took the pair's live edge
				// away", and the marker above this row is a claim about the
				// graph: a causes-only pair has no 'supersedes' edge to drop, so
				// deriving it from that count alone printed `already gone` over a
				// run that really did remove a live 'causes' edge. A WITHHELD
				// 'supersedes' sweep took nothing away, so it is not a withdrawal
				// either (#845).
				c.Withdrawn = c.withdrewLiveEdge(dropped, causesDropped)
			case RelationReversed:
				// Nothing is written, in either direction: the classifier
				// says the OLDER note is the current one, so the only link
				// this pair could carry is the backwards one. Links a
				// previous verdict left behind are dropped, exactly as a
				// NEITHER verdict drops both relations — a backwards
				// 'causes' link pointing INTO the obsolete note asserts the
				// opposite of what this verdict just said.
				//
				// 'supersedes' is the ONE relation the ordinary pass leaves
				// alone (#845), and a backwards supersession is the very case
				// that leaves, so the only way it leaves the graph is
				// `ghost supersede --reassess --apply` — which re-judges the
				// edge under the current rules and shows the operator what it
				// is about to remove. That is the trade #845 makes for the
				// 6-of-11 withdrawals that were wrong, and it is named here
				// because this branch is where the backwards edge used to die.
				dropped, err := withdrawSupersedesEdge(ctx, store, c)
				if err != nil {
					return res, nil, fmt.Errorf("invalidate reversed supersedes link %s→%s: %w", c.NewerID, c.OlderID, err)
				}
				causesDropped, err := sweepCausesBothWays(ctx, store, claims, c.NewerID, c.OlderID)
				if err != nil {
					return res, nil, err
				}
				c.Withdrawn = c.withdrewLiveEdge(dropped, causesDropped)
				c.CausesDropped = causesDropped
				// Info, and only when a row really changed: a fresh reversed
				// candidate usually carries no link, so claiming a drop there
				// would put a graph mutation in lifecycle.log that never
				// happened. InvalidateLink's count is what makes the difference
				// between the two cases observable.
				if logger != nil && dropped+int64(causesDropped) > 0 {
					logger.Info("supersede: dropped the links of a reversed pair",
						"newer", c.NewerID, "older", c.OlderID,
						"links", dropped+int64(causesDropped))
				}
			}
			if logger != nil {
				logger.Debug("supersede classified", "newer", c.NewerID, "older", c.OlderID, "verdict", c.Relation)
			}
		}

		// Record the freshly judged NEITHER verdicts so the next pass skips
		// them. Only pairs whose endpoints survived the existence check are
		// eligible (writable): a stale pair's row would hit the FK and roll
		// back the whole cache write. Cache skips already have a row,
		// reclassify pairs stay classified (their live link must keep
		// self-healing), and SUPERSEDES/CAUSES achieved their effect via the
		// links written above; only a fresh NEITHER verdict is worth caching.
		// A failed cache write costs a re-classify next pass, like resolve's
		// KEEP cache, so warn rather than fail a pass whose primary effect
		// landed.
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
