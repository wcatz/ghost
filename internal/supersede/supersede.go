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
// Classifier makes a 3-way SUPERSEDES/CAUSES/NEITHER call for each pair, since
// "replaces a stale claim" and "is caused by / follows from" are distinct
// relations that a binary confirm/reject can't tell apart. SUPERSEDES writes
// a newer->older 'supersedes' link (source 'llm'); CAUSES writes an
// older->newer 'causes' link (cause precedes effect); NEITHER writes nothing.
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

// Classifier decides the relationship between a newer and an older memory:
// a same-fact replacement (SUPERSEDES), a decision citing supporting evidence
// that stays valid (CAUSES), or neither. It also reports whether the answer
// came from a fallback provider (see ai.FallbackProvider) — callers use that
// to withhold writes on a degraded-quality answer. The LLM implementation
// lives in the CLI layer; tests inject a deterministic mock.
type Classifier interface {
	Classify(ctx context.Context, newer, older string) (relation Relation, fromFallback bool, err error)
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
// same verdict for no reason. Fresh candidates are always classified
// regardless, since SelectCandidates already bounds their cost.
//
// If any classification in the batch came from a fallback provider, apply is
// skipped entirely for the whole batch (mirroring internal/resolve) — a
// degraded-quality verdict should never silently write or invalidate a link;
// the dry-run-style preview is still returned so callers can report what
// would have happened.
//
// CreateLink and InvalidateLink are both idempotent no-ops when there's
// nothing to change, so re-running Run converges and self-heals after
// reflection's cascade-delete of links. A classifier error on one pair is
// fatal (the caller decides whether a partial pass is acceptable); a
// link-write error is fatal so a half-written pair is never silently left
// behind.
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
	var classified []Classified
	anyFallback := false
	for _, c := range all {
		verdict, fromFallback, err := cls.Classify(ctx, c.NewerContent, c.OlderContent)
		if err != nil {
			return res, nil, fmt.Errorf("classify %s→%s: %w", c.NewerID, c.OlderID, err)
		}
		if fromFallback {
			anyFallback = true
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

	if apply && anyFallback {
		if logger != nil {
			logger.Warn("supersede: candidates classified via fallback provider, apply skipped — rerun once primary is available",
				"confirmed", res.Confirmed, "causes", res.CausesCreated)
		}
		return res, classified, nil
	}

	if apply {
		for _, c := range classified {
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
	}
	return res, classified, nil
}
