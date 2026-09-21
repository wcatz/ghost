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
// Classifier makes a 3-way SUPERSEDES/CAUSES/NEITHER judgment for each pair —
// batched up to classifyBatchSize pairs per harness call so a large project's
// pass does not pay one process spawn and full rubric per pair — since
// "replaces a stale claim" and "is caused by / follows from" are distinct
// relations that a binary confirm/reject can't tell apart. SUPERSEDES writes
// a newer->older 'supersedes' link (source 'llm'); CAUSES writes an
// older->newer 'causes' link (cause precedes effect); NEITHER writes nothing.
// Fresh NEITHER verdicts are cached by pair and both endpoints' content hashes
// (supersede_checked, schema v8), so an unchanged pair is skipped on later
// passes and a converged project makes zero classify calls; reclassify
// candidates are never cache-skipped.
// Run() also re-classifies existing 'supersedes'/'llm' links whose endpoints
// have changed since the link was written, invalidating the link (or
// flipping it to 'causes') when the verdict no longer matches. The pass is
// re-runnable and self-heals after reflection's cascade-delete of links, like
// the cosine linking worker rebuilds 'related' edges — though reclassification
// of existing links only fires for a pair whose endpoint content actually
// changed after the link was written; pairs whose link predates this 3-way
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
// recent memory that may supersede Older.
type Candidate struct {
	NewerID      string
	NewerContent string
	OlderID      string
	OlderContent string
	Similarity   float32
}

// Relation is a classifier verdict on a NEWER/OLDER candidate pair.
type Relation string

const (
	// RelationSupersedes means newer states an updated/changed/replaced value
	// of the SAME fact as older, making older obsolete.
	RelationSupersedes Relation = "supersedes"
	// RelationCauses means newer (typically a decision or change) was informed
	// by older as supporting evidence, but older remains independently true.
	RelationCauses Relation = "causes"
	// RelationNeither means the pair is not a genuine replacement or citation
	// relationship — e.g. two independently valid parallel facts.
	RelationNeither Relation = "neither"
)

// Classifier decides the relationship for candidate pairs: a same-fact
// replacement (SUPERSEDES), a decision citing supporting evidence that stays
// valid (CAUSES), or neither. It returns one verdict per pair, in the same
// order; Relation("") marks a pair whose verdict could not be parsed. The LLM
// implementation (RelationClassifier) batches pairs across as few harness
// calls as possible; tests inject a deterministic mock.
type Classifier interface {
	ClassifyBatch(ctx context.Context, pairs []Candidate) ([]Relation, error)
}

// contentHash is the NEITHER-cache key component, mirroring resolve's
// ContentHash: the classification question is about the notes' text, so a tag
// or importance edit must not invalidate a cached verdict. The "v1\x00" prefix
// versions the key — a future prompt/rubric change that could flip verdicts
// bumps it to reset every cached verdict in one step.
func contentHash(content string) string {
	sum := sha256.Sum256([]byte("v1\x00" + content))
	return hex.EncodeToString(sum[:])
}

// vectorStore is the subset of *memory.Store the pass needs; narrowed for
// testability.
type vectorStore interface {
	GetAll(ctx context.Context, projectID string, limit int) ([]memory.Memory, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	GetEmbedding(ctx context.Context, memoryID string) ([]float32, error)
	SearchVector(ctx context.Context, projectID string, queryVec []float32, limit int) ([]memory.ScoredMemory, error)
	CreateLink(ctx context.Context, sourceID, targetID, relation string, strength float32, source string) error
	InvalidateLink(ctx context.Context, sourceID, targetID, relation string) error
	LinksByRelationSource(ctx context.Context, projectID, relation, source string) ([]memory.Link, error)
	SupersedeChecked(ctx context.Context, projectID string) (map[[2]string]memory.SupersedeCheck, error)
	MarkSupersedeNeither(ctx context.Context, projectID string, checks map[[2]string]memory.SupersedeCheck) error
}

// SelectCandidates returns the deduped ordered candidate pairs for a project:
// memories whose cosine similarity is at least threshold, oriented newer→older
// by updated_at. A pair is emitted once regardless of which endpoint surfaced
// it. Memories without embeddings are skipped (no similarity signal).
func SelectCandidates(ctx context.Context, store vectorStore, projectID string, threshold float32) ([]Candidate, error) {
	mems, err := store.GetAll(ctx, projectID, 100000)
	if err != nil {
		return nil, fmt.Errorf("load memories: %w", err)
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
			continue // no embedding → no similarity candidates
		}
		neighbors, err := store.SearchVector(ctx, projectID, vec, maxNeighbors+1)
		if err != nil {
			return nil, fmt.Errorf("search vector for %s: %w", m.ID, err)
		}
		for _, n := range neighbors {
			if n.MemoryID == m.ID || n.Score < threshold {
				continue
			}
			other, ok := byID[n.MemoryID]
			if !ok {
				continue // e.g. a _global neighbor not in this project's set
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
				NewerID: newer.ID, NewerContent: newer.Content,
				OlderID: older.ID, OlderContent: older.Content,
				Similarity: n.Score,
			})
		}
	}
	return cands, nil
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
//
// A pair whose existing 'supersedes' link predates neither endpoint's last
// update is skipped (skip-if-unchanged) — reclassifying it would repeat the
// same verdict for no reason. Fresh candidates are classified unless both
// endpoints' content still matches a cached NEITHER verdict (the content-keyed
// NEITHER cache, schema v8), so a converged project's pass makes no calls;
// reclassify candidates are never cache-skipped, keeping link self-healing
// untouched. Cache rows are recorded on apply only — dry-run stays
// side-effect-free — and cascade away with their memories via the FK.
//
// Pairs whose endpoints are replaced by a concurrent reflect pass (the stop
// hook spawns both for the same session) are dropped and counted in
// Result.StaleSkipped rather than failing the pass — the pair no longer
// exists, so there is nothing to link.
//
// CreateLink and InvalidateLink are both idempotent no-ops when there's
// nothing to change, so re-running Run converges and self-heals after
// reflection's cascade-delete of links. A pair whose verdict is unparseable is
// skipped and counted (Result.Unclassified); any other classifier error — a
// dead harness, an outage — is fatal so a transport failure cannot look like a
// successful, empty pass. A link-write error is fatal so a half-written pair is
// never silently left behind.
func Run(ctx context.Context, store vectorStore, cls Classifier, projectID string, threshold float32, apply bool, logger *slog.Logger) (Result, []Classified, error) {
	fresh, err := SelectCandidates(ctx, store, projectID, threshold)
	if err != nil {
		return Result{}, nil, err
	}
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
			NewerID: l.SourceID, NewerContent: newerMem.Content,
			OlderID: l.TargetID, OlderContent: olderMem.Content,
			Similarity: l.Strength,
		})
	}

	res := Result{Candidates: len(all)}

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
	aliveSet := make(map[string]bool, len(aliveMems))
	for _, m := range aliveMems {
		aliveSet[m.ID] = true
	}
	live := all[:0]
	for _, c := range all {
		if aliveSet[c.NewerID] && aliveSet[c.OlderID] {
			live = append(live, c)
			continue
		}
		res.StaleSkipped++
		if logger != nil {
			logger.Info("supersede: dropping stale pair (endpoint replaced by a concurrent pass)",
				"newer", c.NewerID, "older", c.OlderID)
		}
	}
	all = live
	res.Candidates = len(all)
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
			}
			if wasReclassify && verdict != RelationSupersedes {
				res.Reclassified++
			}
		}
	}

	if apply {
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
			switch c.Relation {
			case RelationSupersedes:
				if err := store.CreateLink(ctx, c.NewerID, c.OlderID, string(RelationSupersedes), c.Similarity, "llm"); err != nil {
					return res, nil, fmt.Errorf("create supersedes link %s→%s: %w", c.NewerID, c.OlderID, err)
				}
				res.Created++
				if err := store.InvalidateLink(ctx, c.OlderID, c.NewerID, string(RelationCauses)); err != nil {
					return res, nil, fmt.Errorf("invalidate causes link %s→%s: %w", c.OlderID, c.NewerID, err)
				}
			case RelationCauses:
				if err := store.CreateLink(ctx, c.OlderID, c.NewerID, string(RelationCauses), c.Similarity, "llm"); err != nil {
					return res, nil, fmt.Errorf("create causes link %s→%s: %w", c.OlderID, c.NewerID, err)
				}
				if err := store.InvalidateLink(ctx, c.NewerID, c.OlderID, string(RelationSupersedes)); err != nil {
					return res, nil, fmt.Errorf("invalidate supersedes link %s→%s: %w", c.NewerID, c.OlderID, err)
				}
			case RelationNeither:
				if err := store.InvalidateLink(ctx, c.NewerID, c.OlderID, string(RelationSupersedes)); err != nil {
					return res, nil, fmt.Errorf("invalidate supersedes link %s→%s: %w", c.NewerID, c.OlderID, err)
				}
				if err := store.InvalidateLink(ctx, c.OlderID, c.NewerID, string(RelationCauses)); err != nil {
					return res, nil, fmt.Errorf("invalidate causes link %s→%s: %w", c.OlderID, c.NewerID, err)
				}
			}
			if logger != nil {
				logger.Debug("supersede classified", "newer", c.NewerID, "older", c.OlderID, "verdict", c.Relation)
			}
		}

		// Record the freshly judged NEITHER verdicts so the next pass skips
		// them. Cache skips already have a row, reclassify pairs stay
		// classified (their live link must keep self-healing), and
		// SUPERSEDES/CAUSES achieved their effect via the links written above;
		// only a fresh NEITHER verdict is worth caching. A failed cache write
		// costs a re-classify next pass, like resolve's KEEP cache, so warn
		// rather than fail a pass whose primary effect landed.
		newNeither := make(map[[2]string]memory.SupersedeCheck)
		for _, c := range classified {
			key := [2]string{c.NewerID, c.OlderID}
			if c.Relation != RelationNeither || !freshKeys[key] || reclassifyByKey[key] {
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
