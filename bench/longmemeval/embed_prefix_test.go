package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/wcatz/ghost/internal/embedding"
)

// recordingOllama is a fake Ollama /api/embed that records every input it is
// handed and answers with a deterministic vector, so a test can assert on the
// exact text the harness embedded rather than on the vectors that come back.
func recordingOllama(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var inputs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ollamaEmbedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		inputs = append(inputs, req.Input...)
		mu.Unlock()
		resp := ollamaEmbedResponse{Embeddings: make([][]float32, len(req.Input))}
		for i := range req.Input {
			resp.Embeddings[i] = []float32{1, 0.5, 0.25, 0.125}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), inputs...)
	}
}

// TestBenchEmbedderAppliesProductionTaskPrefixes: haystack turns go out as
// documents and the question as a query, exactly as production's
// embedding.Client does. LongMemEval retrieval is scored on cosine rank, so an
// unprefixed question searched against unprefixed documents measures a
// comparison production never runs — and the cache that feeds it was keyed on
// text production never embeds.
func TestBenchEmbedderAppliesProductionTaskPrefixes(t *testing.T) {
	srv, inputs := recordingOllama(t)
	emb, err := newCachedEmbedder(srv.URL, "")
	if err != nil {
		t.Fatalf("newCachedEmbedder: %v", err)
	}

	ctx := context.Background()
	const qText = "What city does Alice live in?"
	q := question{
		QuestionID: "q1",
		Question:   qText,
		SessionIDs: []string{"s1"},
		Sessions: [][]turn{{
			{Role: "user", Content: "Alice moved to Lisbon last year."},
		}},
	}
	if _, err := rankSessionsForQuestion(ctx, q, "hybrid", emb); err != nil {
		t.Fatalf("rankSessionsForQuestion: %v", err)
	}

	sent := inputs()
	const content = "Alice moved to Lisbon last year."
	if !slices.Contains(sent, embedding.NomicTaskDocument+content) {
		t.Errorf("haystack turn was not embedded with the production document prefix; sent %q", sent)
	}
	if !slices.Contains(sent, embedding.NomicTaskQuery+qText) {
		t.Errorf("question was not embedded with the production query prefix; sent %q", sent)
	}
	if slices.Contains(sent, content) || slices.Contains(sent, qText) {
		t.Errorf("harness embedded unprefixed text — a vector space production never searches; sent %q", sent)
	}
	// The question must never ride the document path: it used to be seeded
	// into the batch alongside the haystack, which would embed it in the
	// document space and cache that vector under the query's own text.
	if slices.Contains(sent, embedding.NomicTaskDocument+qText) {
		t.Errorf("question was embedded as a document; sent %q", sent)
	}
}
