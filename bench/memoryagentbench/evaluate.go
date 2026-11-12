package main

import (
	"context"
	"fmt"

	"github.com/wcatz/ghost/internal/memory"
)

const searchLimit = 10
const hitK = 5

type questionOutcome struct {
	QAPairID      string
	BaselineHit1  bool
	BaselineHit5  bool
	SupersedeHit1 bool
	SupersedeHit5 bool
}

func searchQuestion(ctx context.Context, store *memory.Store, project, question string, embedder *cachedEmbedder, p memory.SearchParams) ([]memory.Memory, error) {
	qv, err := embedder.EmbedQuery(ctx, question)
	if err != nil {
		return nil, fmt.Errorf("embed question: %w", err)
	}
	return store.SearchHybridParams(ctx, project, question, qv, searchLimit, p)
}

func evaluateQuestions(ctx context.Context, store *memory.Store, project string, d Demo, embedder *cachedEmbedder, p memory.SearchParams) ([]questionOutcome, error) {
	baseline := p
	baseline.SupersedeDemote = false

	out := make([]questionOutcome, len(d.Questions))
	for i, q := range d.Questions {
		baseResults, err := searchQuestion(ctx, store, project, q, embedder, baseline)
		if err != nil {
			return nil, fmt.Errorf("question %d baseline: %w", i, err)
		}
		supResults, err := searchQuestion(ctx, store, project, q, embedder, p)
		if err != nil {
			return nil, fmt.Errorf("question %d with-supersede: %w", i, err)
		}
		out[i] = questionOutcome{
			QAPairID:      d.QAPairIDs[i],
			BaselineHit1:  topKHit(baseResults, d.Answers[i], 1),
			BaselineHit5:  topKHit(baseResults, d.Answers[i], hitK),
			SupersedeHit1: topKHit(supResults, d.Answers[i], 1),
			SupersedeHit5: topKHit(supResults, d.Answers[i], hitK),
		}
	}
	return out, nil
}
