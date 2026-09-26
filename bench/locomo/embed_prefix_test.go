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

// TestBenchEmbedderAppliesProductionTaskPrefixes: the harness embedder must
// send what production sends — "search_document: " on stored turns,
// "search_query: " on the question (embedding.Client.EmbedDocument/EmbedQuery).
// Unprefixed bench embeds live in a different vector space from every other
// Ghost vector, so the bench numbers measured them are not the numbers
// production would see, and its cache was keyed on text nobody in production
// embeds.
func TestBenchEmbedderAppliesProductionTaskPrefixes(t *testing.T) {
	srv, inputs := recordingOllama(t)
	emb, err := newCachedEmbedder(srv.URL, "")
	if err != nil {
		t.Fatalf("newCachedEmbedder: %v", err)
	}

	ctx := context.Background()
	const project = "prefix-probe"
	sessions := []locoSession{{
		ID: "s1", Num: 1, Date: "2024-01-01",
		Turns: []locoTurn{{Speaker: "Alice", DiaID: "d1", Text: "Alice moved to Lisbon"}},
	}}
	store, memMeta, cleanup, err := openStore(ctx, project, sessions, "hybrid", emb)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer cleanup()

	const question = "Where does Alice live?"
	if _, err := rankTurns(ctx, store, project, question, "hybrid", emb, memMeta); err != nil {
		t.Fatalf("rankTurns: %v", err)
	}

	content := memoryText(sessions[0], sessions[0].Turns[0])
	sent := inputs()
	if !slices.Contains(sent, embedding.NomicTaskDocument+content) {
		t.Errorf("stored turn was not embedded with the production document prefix; sent %q", sent)
	}
	if !slices.Contains(sent, embedding.NomicTaskQuery+question) {
		t.Errorf("question was not embedded with the production query prefix; sent %q", sent)
	}
	if slices.Contains(sent, content) || slices.Contains(sent, question) {
		t.Errorf("harness embedded unprefixed text — a vector space production never searches; sent %q", sent)
	}
}
