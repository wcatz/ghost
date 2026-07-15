// Package supersede creates directed 'supersedes' links between memories where
// a newer memory replaces an older one about the same subject. It is the
// creation half of staleness-aware ranking; the consumption half
// (SearchParams.SupersedeDemote) already ships. See docs/benchmarks.md Phase 3.
//
// Design: cosine similarity proposes same-subject candidate pairs (cheap,
// local), created_at gives direction (newer supersedes older — SQLite's
// 'YYYY-MM-DD HH:MM:SS' timestamps compare lexicographically), and an LLM
// Classifier confirms each pair is a genuine replacement rather than two
// parallel valid facts. Confirmed pairs become 'supersedes' links (source
// 'llm'); classifying every pair in a same-subject group yields the star links
// the consumer's penalty ordering needs. The pass is re-runnable and self-heals
// after reflection's cascade-delete of links, like the cosine linking worker
// rebuilds 'related' edges.
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

// Classifier decides whether Newer genuinely supersedes Older (a replacement or
// update of the same fact) versus the two being independently valid. The LLM
// implementation lives in the CLI layer; tests inject a deterministic mock.
type Classifier interface {
	Supersedes(ctx context.Context, newer, older string) (bool, error)
}

// vectorStore is the subset of *memory.Store the pass needs; narrowed for
// testability.
type vectorStore interface {
	GetAll(ctx context.Context, projectID string, limit int) ([]memory.Memory, error)
	GetEmbedding(ctx context.Context, memoryID string) ([]float32, error)
	SearchVector(ctx context.Context, projectID string, queryVec []float32, limit int) ([]memory.ScoredMemory, error)
	CreateLink(ctx context.Context, sourceID, targetID, relation string, strength float32, source string) error
}

// SelectCandidates returns the deduped ordered candidate pairs for a project:
// memories whose cosine similarity is at least threshold, oriented newer→older
// by created_at. A pair is emitted once regardless of which endpoint surfaced
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

// orient returns (newer, older) by created_at. SQLite 'YYYY-MM-DD HH:MM:SS'
// strings order chronologically under lexicographic comparison; ties break by
// ID so the pair is deterministic.
func orient(a, b memory.Memory) (newer, older memory.Memory) {
	if a.CreatedAt > b.CreatedAt || (a.CreatedAt == b.CreatedAt && a.ID > b.ID) {
		return a, b
	}
	return b, a
}

// Result summarizes a pass.
type Result struct {
	Candidates int
	Confirmed  int
	Created    int // links written (0 in dry-run)
}

// Run selects candidates, classifies each, and — when apply is true — writes a
// 'supersedes' link (source 'llm') for every confirmed pair. CreateLink is
// idempotent, so re-running converges; reflection's cascade-delete plus a
// re-run is the self-heal path. A classifier error on one pair is fatal (the
// caller decides whether a partial pass is acceptable); a link-write error is
// fatal so a half-written star is never silently left behind.
func Run(ctx context.Context, store vectorStore, cls Classifier, projectID string, threshold float32, apply bool, logger *slog.Logger) (Result, []Candidate, error) {
	cands, err := SelectCandidates(ctx, store, projectID, threshold)
	if err != nil {
		return Result{}, nil, err
	}
	res := Result{Candidates: len(cands)}
	var confirmed []Candidate
	for _, c := range cands {
		ok, err := cls.Supersedes(ctx, c.NewerContent, c.OlderContent)
		if err != nil {
			return res, nil, fmt.Errorf("classify %s→%s: %w", c.NewerID, c.OlderID, err)
		}
		if !ok {
			continue
		}
		res.Confirmed++
		confirmed = append(confirmed, c)
		if apply {
			if err := store.CreateLink(ctx, c.NewerID, c.OlderID, "supersedes", c.Similarity, "llm"); err != nil {
				return res, nil, fmt.Errorf("create supersedes link %s→%s: %w", c.NewerID, c.OlderID, err)
			}
			res.Created++
			if logger != nil {
				logger.Debug("supersede link created", "newer", c.NewerID, "older", c.OlderID)
			}
		}
	}
	return res, confirmed, nil
}
