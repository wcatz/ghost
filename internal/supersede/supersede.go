// Package supersede creates directed 'supersedes' and 'causes' links between
// memories where a newer memory replaces an older one about the same subject
// (supersedes), or a newer memory is an effect that followed from an older
// one (causes). It is the creation half of staleness-aware ranking; the
// consumption half (SearchParams.SupersedeDemote) already ships. See
// docs/benchmarks.md Phase 3.
//
// Design: cosine similarity proposes same-subject candidate pairs (cheap,
// local), updated_at gives direction (newer/older — SQLite's
// 'YYYY-MM-DD HH:MM:SS' timestamps compare lexicographically), and an LLM
// Classifier makes a 4-way SUPERSEDES/CAUSES/NEITHER/REVERSED judgment for each
// pair — batched up to classifyBatchSize pairs per harness call so a large
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
// Fresh NEITHER verdicts are cached by pair and both endpoints' content hashes
// (supersede_checked, schema v8): unchanged fresh pairs are skipped on later
// passes — a cache skip is equivalent to a NEITHER verdict, so a stale
// 'causes' link is not invalidated on that pass if the endpoints' text reverted
// to a previously cached version (graph-only staleness; ranking consumes only
// 'supersedes') — while live-link pairs (reclassify) are still validated each
// pass. A REVERSED verdict is never cached, or the pair would be skipped for the
// life of its text — so a converged project makes zero classify calls for its
// fresh candidates EXCEPT reversed ones, which are re-asked on every pass until
// the verdict changes. That is a deliberate billable repeat per reversed pair,
// paid so the refusal is never frozen; it is not zero calls overall.
// Run() also re-classifies existing 'supersedes'/'llm' links whose endpoints
// have changed since the link was written, invalidating the link (flipping it
// to 'causes', or dropping it on a reversed verdict) when the verdict no longer
// matches. The pass is
// re-runnable and self-heals after reflection's cascade-delete of links, like
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
	// of the SAME fact as older, making older obsolete. A model that cannot name
	// the claim it replaced does not reach this verdict (see requireReplaced).
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
type Classifier interface {
	ClassifyBatch(ctx context.Context, pairs []Candidate) ([]Relation, error)
}

// contentHash is the NEITHER-cache key component, mirroring resolve's
// ContentHash: the classification question is about the notes' text, so a tag
// or importance edit must not invalidate a cached verdict. The "v3\x00" prefix
// versions the key — a prompt/rubric change that could flip verdicts bumps it
// to reset every cached verdict in one step, the same reset resolve performed
// when its rubric changed. A cache hit is a permanent skip for the life of that
// text, so a stored verdict that the current rules would answer differently is
// not a stale row, it is a rule that can never be applied again to those two
// notes. v2 was the #641 rubric (REVERSED plus the created_at signal) and v3 is
// #686's: a SUPERSEDES now has to name the older note's retired claim, and
// two-true pairs are NEITHER, so every v2 row has to be re-asked.
func contentHash(content string) string {
	sum := sha256.Sum256([]byte("v3\x00" + content))
	return hex.EncodeToString(sum[:])
}

// vectorStore is the subset of *memory.Store the pass needs; narrowed for
// testability.
type vectorStore interface {
	GetAll(ctx context.Context, projectID string, limit int) ([]memory.Memory, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	GetEmbedding(ctx context.Context, memoryID string) ([]float32, error)
	SearchVectorScoped(ctx context.Context, projectID string, queryVec []float32, limit int, scope map[string]string) ([]memory.ScoredMemory, error)
	CreateLink(ctx context.Context, sourceID, targetID, relation string, strength float32, source string) error
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
	// (memory.Store.GetEmbedding returns nil for both). They are in no pair THIS
	// scan proposes, so a caller that does not report this number reports a
	// total that silently omits part of the project.
	//
	// It is about the scan, not the run: a memory counted here can still be an
	// endpoint of a pair Run re-proposes, because the reclassify half re-reads
	// live 'supersedes' edges from their link rows and never looks at a vector.
	// A caller must therefore say "proposed no new candidate", never "was in no
	// pair this run considered" — after a model change retires every vector in a
	// project, both counts describe the same corpus and only one of the two
	// sentences is true.
	Unscored int
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

	seen := make(map[[2]string]bool)
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
			if memory.ScopesConflict(m.Scope, n.Scope) {
				continue
			}
			newer, older := orient(m, other)
			if newer.ID == older.ID {
				continue // identical created_at and ID collision guard
			}
			key := [2]string{newer.ID, older.ID}
			if seen[key] {
				continue
			}
			seen[key] = true
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

// orient returns (newer, older) by updated_at — the same freshness signal
// Run()'s skip-if-unchanged reclassification already uses. created_at would
// misorder (and mislabel the direction of) a memory that was edited long
// after it was first created, which is exactly the reversed-decision case
// this pass exists to catch. SQLite 'YYYY-MM-DD HH:MM:SS' strings order
// chronologically under lexicographic comparison; ties break by ID so the
// pair is deterministic.
func orient(a, b memory.Memory) (newer, older memory.Memory) {
	if a.UpdatedAt > b.UpdatedAt || (a.UpdatedAt == b.UpdatedAt && a.ID > b.ID) {
		return a, b
	}
	return b, a
}

// Classified pairs a Candidate with the verdict Run() reached for it — used
// by callers (the CLI) to report the actual relation written, not just that
// "something" was confirmed.
type Classified struct {
	Candidate
	Relation Relation
}

// Result summarizes a pass.
type Result struct {
	Candidates    int
	Confirmed     int // SUPERSEDES verdicts
	Created       int // supersedes links written (0 in dry-run)
	CausesCreated int // CAUSES verdicts (causes links written when apply)
	Reclassified  int // existing links whose relation changed or was invalidated
	StaleSkipped  int
	Skipped       int // fresh pairs skipped via the NEITHER cache
	Unclassified  int // pairs skipped because the classifier answer was unparseable
	Reversed      int // REVERSED verdicts: refused, never written
	// Vetoed counts pairs the deterministic imperative veto settled as
	// no-supersedes-edge with no harness call (see VetoSupersede). They are
	// counted rather than silently dropped, because a run that declines work it
	// did not do and reports the same totals as one that found nothing to do
	// reads as "nothing was skipped".
	Vetoed int
	// Unscored counts the project's memories the candidate scan could not score
	// at all, for want of a usable vector — none yet, or one written under
	// another model, width or task prefix. They are in no pair the SCAN proposes,
	// so every other number here is a total over the rest of the project: a
	// caller that reports them without this one reports a partial scan in the
	// voice of a complete one. It is the difference between "this project holds
	// no near-duplicate pair" and "this pass could not read all of it", and the
	// vectors it is waiting for belong to the embedding worker.
	//
	// "The scan", specifically: a counted memory can still be an endpoint of a
	// reclassified pair, because that half of the pass works from link rows and
	// reads no vector. A report that says such a memory is "in no pair this run
	// considered" is wrong the first time a model change retires a project's
	// vectors and an edited edge comes back for re-judging.
	Unscored int
	// ReclassifiedNoWrite counts the reclassify pairs whose --apply effect is
	// purely destructive: a reversal and a NEITHER both only invalidate the
	// links they find, so neither re-links the pair.
	ReclassifiedNoWrite int
}

// WouldWriteLinks reports whether an --apply pass would put a NEW link in the
// graph, which is what the CLI's dry-run hint promises. A confirmed pair and a
// CAUSES pair are written; a reclassify pair is re-linked by a CAUSES verdict
// and only invalidated by NEITHER or a reversal. Subtracting the no-write
// reclassifications matters because a pass whose whole effect is deleting a
// link should not tell the operator that --apply will write links for it.
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
// same verdict for no reason. Fresh candidates are classified unless both
// endpoints' content still matches a cached NEITHER verdict (the content-keyed
// NEITHER cache, schema v8): a cache skip is treated as a NEITHER verdict, so
// if a 'causes' link existed and the endpoints' text later reverted to a
// previously cached version, that link is not invalidated on the skipping pass
// — graph-only staleness, since ranking consumes only 'supersedes'. Reclassify
// candidates are never cache-skipped, so live-link pairs are still validated
// every pass and self-healing is untouched. Cache rows are recorded on apply
// only — dry-run stays side-effect-free — and cascade away with their memories
// via the FK.
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
// per pass: skip-if-unchanged already holds an untouched pair quiet.
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
func Run(ctx context.Context, store vectorStore, cls Classifier, projectID string, threshold float32, apply bool, logger *slog.Logger) (Result, []Classified, error) {
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
	res := Result{Unscored: sel.Unscored}
	freshKeys := make(map[[2]string]bool, len(fresh))
	for _, c := range fresh {
		freshKeys[[2]string{c.NewerID, c.OlderID}] = true
	}

	existingLinks, err := store.LinksByRelationSource(ctx, projectID, string(RelationSupersedes), "llm")
	if err != nil {
		return Result{}, nil, fmt.Errorf("load existing supersedes links: %w", err)
	}

	var lookupIDs []string
	seenID := make(map[string]bool)
	for _, l := range existingLinks {
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
			return Result{}, nil, fmt.Errorf("load reclassify memory content: %w", err)
		}
		for _, m := range mems {
			memByID[m.ID] = m
		}
	}

	reclassifyByKey := make(map[[2]string]bool, len(existingLinks))
	all := append([]Candidate{}, fresh...)
	for _, l := range existingLinks {
		key := [2]string{l.SourceID, l.TargetID}
		reclassifyByKey[key] = true
		if freshKeys[key] {
			continue // already going to be classified via fresh
		}
		newerMem, ok1 := memByID[l.SourceID]
		olderMem, ok2 := memByID[l.TargetID]
		if !ok1 || !ok2 {
			continue // an endpoint no longer exists
		}
		if newerMem.UpdatedAt <= l.CreatedAt && olderMem.UpdatedAt <= l.CreatedAt {
			continue // skip-if-unchanged: neither endpoint changed since this link was written
		}
		all = append(all, Candidate{
			NewerID: l.SourceID, NewerContent: newerMem.Content, NewerCreatedAt: newerMem.CreatedAt,
			OlderID: l.TargetID, OlderContent: olderMem.Content, OlderCreatedAt: olderMem.CreatedAt,
			Similarity: l.Strength,
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
	// NEITHER verdict is skipped — no harness call, no write; reclassify pairs
	// are never skipped, since their job is to revalidate a live link as content
	// evolves.
	checked, err := store.SupersedeChecked(ctx, projectID)
	if err != nil {
		return res, nil, fmt.Errorf("load supersede checks: %w", err)
	}
	var pending []Candidate
	for _, c := range all {
		key := [2]string{c.NewerID, c.OlderID}
		chk, cached := checked[key]
		if freshKeys[key] && !reclassifyByKey[key] && cached &&
			chk.NewerHash == contentHash(c.NewerContent) && chk.OlderHash == contentHash(c.OlderContent) {
			res.Skipped++
			continue
		}
		pending = append(pending, c)
	}

	var classified []Classified
	if len(pending) > 0 {
		relations, err := cls.ClassifyBatch(ctx, pending)
		if err != nil {
			return res, nil, fmt.Errorf("classify %d candidate pair(s): %w", len(pending), err)
		}
		if len(relations) != len(pending) {
			return res, nil, fmt.Errorf("classifier returned %d verdicts for %d pairs", len(relations), len(pending))
		}
		for i, c := range pending {
			verdict := relations[i]
			if verdict == "" {
				// An odd *phrasing* must not abort the pass: a single unparseable
				// verdict ended a 9-minute run after links for earlier pairs had
				// already been written, so the graph never converged whenever the
				// model used wording the parser did not know. Skip that pair and
				// count it (reported by the caller).
				//
				// A transport failure stays fatal inside ClassifyBatch: skipping
				// every pair would write nothing and still report success,
				// blaming the model for a transport failure.
				res.Unclassified++
				if logger != nil {
					logger.Warn("supersede: skipping pair with an unclassifiable verdict",
						"newer", c.NewerID, "older", c.OlderID)
				}
				continue
			}
			classified = append(classified, Classified{Candidate: c, Relation: verdict})

			key := [2]string{c.NewerID, c.OlderID}
			wasReclassify := reclassifyByKey[key]

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
			if wasReclassify && verdict != RelationSupersedes {
				res.Reclassified++
				// A CAUSES verdict re-links the pair; NEITHER and a reversal
				// only drop what is there.
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
		for _, c := range classified {
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
				res.StaleSkipped++
				if logger != nil {
					logger.Info("supersede: skipping write, endpoint replaced by a concurrent pass",
						"newer", c.NewerID, "older", c.OlderID)
				}
				continue
			}
			writable[[2]string{c.NewerID, c.OlderID}] = true
			switch c.Relation {
			case RelationSupersedes:
				if err := store.CreateLink(ctx, c.NewerID, c.OlderID, string(RelationSupersedes), c.Similarity, "llm"); err != nil {
					return res, nil, fmt.Errorf("create supersedes link %s→%s: %w", c.NewerID, c.OlderID, err)
				}
				res.Created++
				if _, err := store.InvalidateLink(ctx, c.OlderID, c.NewerID, string(RelationCauses)); err != nil {
					return res, nil, fmt.Errorf("invalidate causes link %s→%s: %w", c.OlderID, c.NewerID, err)
				}
			case RelationCauses:
				if err := store.CreateLink(ctx, c.OlderID, c.NewerID, string(RelationCauses), c.Similarity, "llm"); err != nil {
					return res, nil, fmt.Errorf("create causes link %s→%s: %w", c.OlderID, c.NewerID, err)
				}
				if _, err := store.InvalidateLink(ctx, c.NewerID, c.OlderID, string(RelationSupersedes)); err != nil {
					return res, nil, fmt.Errorf("invalidate supersedes link %s→%s: %w", c.NewerID, c.OlderID, err)
				}
			case RelationNeither:
				if _, err := store.InvalidateLink(ctx, c.NewerID, c.OlderID, string(RelationSupersedes)); err != nil {
					return res, nil, fmt.Errorf("invalidate supersedes link %s→%s: %w", c.NewerID, c.OlderID, err)
				}
				if _, err := store.InvalidateLink(ctx, c.OlderID, c.NewerID, string(RelationCauses)); err != nil {
					return res, nil, fmt.Errorf("invalidate causes link %s→%s: %w", c.OlderID, c.NewerID, err)
				}
			case RelationReversed:
				// Nothing is written, in either direction: the classifier
				// says the OLDER note is the current one, so the only link
				// this pair could carry is the backwards one. Links a
				// previous verdict left behind are dropped, exactly as a
				// NEITHER verdict drops both relations — a backwards
				// 'supersedes' link is the harm #641 found, and a 'causes'
				// link pointing INTO the obsolete note asserts the opposite
				// of what this verdict just said. It is also the only way
				// either link ever leaves the graph.
				dropped, err := store.InvalidateLink(ctx, c.NewerID, c.OlderID, string(RelationSupersedes))
				if err != nil {
					return res, nil, fmt.Errorf("invalidate reversed supersedes link %s→%s: %w", c.NewerID, c.OlderID, err)
				}
				causesDropped, err := store.InvalidateLink(ctx, c.OlderID, c.NewerID, string(RelationCauses))
				if err != nil {
					return res, nil, fmt.Errorf("invalidate reversed causes link %s→%s: %w", c.OlderID, c.NewerID, err)
				}
				// Info, and only when a row really changed: a fresh reversed
				// candidate usually carries no link, so claiming a drop there
				// would put a graph mutation in lifecycle.log that never
				// happened. InvalidateLink's count is what makes the difference
				// between the two cases observable.
				if logger != nil && dropped+causesDropped > 0 {
					logger.Info("supersede: dropped the links of a reversed pair",
						"newer", c.NewerID, "older", c.OlderID,
						"links", dropped+causesDropped)
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
			if c.Relation != RelationNeither || !freshKeys[key] || reclassifyByKey[key] || !writable[key] {
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
