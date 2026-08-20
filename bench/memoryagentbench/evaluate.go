// bench/memoryagentbench/evaluate.go
package main

import (
	"context"
	"fmt"

	"github.com/wcatz/ghost/internal/memory"
)

// searchLimit is the retrieval depth passed to SearchHybridParams — how many
// ranked results come back per question.
const searchLimit = 10

// hitK is the top-k cutoff for the "@5" scoring fields below. It must be
// strictly less than searchLimit: SupersedeDemote/recency only ever reorder
// the already-fetched searchLimit-sized result window (internal/memory/vector.go's
// fuseAndRank truncates to the window before either reranking step runs), so
// checking all searchLimit results for a hit is order-independent — baseline
// and with-supersede would always agree. Cutting off at a strictly smaller k
// makes the two conditions capable of actually differing: a reorder can push
// a hit across the hitK boundary even though it stays inside searchLimit.
const hitK = 5

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

// evaluateQuestions scores every question in d twice: once under p with
// SupersedeDemote forced off (baseline) and once under p as given (the
// with-supersede condition — the caller passes memory.DefaultSearchParams(),
// which has SupersedeDemote true) — both against the SAME already-seeded
// store, so this must run after the real supersede pass has applied its
// links (see runDemo in main.go). Deriving baseline from p here, rather than
// accepting two independently-constructed SearchParams, guarantees the two
// conditions differ in exactly the one field these results are labeled
// against — a caller can't accidentally vary anything else and mislabel the
// comparison.
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
