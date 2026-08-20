// bench/memoryagentbench/evaluate.go
package main

import (
	"context"
	"fmt"

	"github.com/wcatz/ghost/internal/memory"
)

// searchLimit is how many results SearchHybridParams returns per question —
// deep enough to score accuracy@5 (topKHit caps at len(results) regardless).
const searchLimit = 10

// questionOutcome is one question's hit/miss under both ablation conditions.
type questionOutcome struct {
	QAPairID      string
	BaselineHit1  bool
	BaselineHit5  bool
	SupersedeHit1 bool
	SupersedeHit5 bool
}

// searchQuestion embeds question and runs Ghost's production hybrid search
// under params p.
func searchQuestion(ctx context.Context, store *memory.Store, project, question string, embedder *cachedEmbedder, p memory.SearchParams) ([]memory.Memory, error) {
	qv, err := embedder.Embed(ctx, question)
	if err != nil {
		return nil, fmt.Errorf("embed question: %w", err)
	}
	return store.SearchHybridParams(ctx, project, question, qv, searchLimit, p)
}

// evaluateQuestions scores every question in d twice: once under baseline
// (SupersedeDemote off) and once under withSupersede (SupersedeDemote on,
// the production default) — both against the SAME already-seeded store, so
// this must run after the real supersede pass has applied its links (see
// runDemo in main.go). SupersedeDemote is a hard no-op when off regardless of
// whether links exist (internal/memory/vector.go), so running both
// conditions after the same supersede pass is a valid ablation, not just a
// before/after snapshot.
func evaluateQuestions(ctx context.Context, store *memory.Store, project string, d Demo, embedder *cachedEmbedder, baseline, withSupersede memory.SearchParams) ([]questionOutcome, error) {
	out := make([]questionOutcome, len(d.Questions))
	for i, q := range d.Questions {
		baseResults, err := searchQuestion(ctx, store, project, q, embedder, baseline)
		if err != nil {
			return nil, fmt.Errorf("question %d baseline: %w", i, err)
		}
		supResults, err := searchQuestion(ctx, store, project, q, embedder, withSupersede)
		if err != nil {
			return nil, fmt.Errorf("question %d with-supersede: %w", i, err)
		}
		out[i] = questionOutcome{
			QAPairID:      d.QAPairIDs[i],
			BaselineHit1:  topKHit(baseResults, d.Answers[i], 1),
			BaselineHit5:  topKHit(baseResults, d.Answers[i], searchLimit),
			SupersedeHit1: topKHit(supResults, d.Answers[i], 1),
			SupersedeHit5: topKHit(supResults, d.Answers[i], searchLimit),
		}
	}
	return out, nil
}
