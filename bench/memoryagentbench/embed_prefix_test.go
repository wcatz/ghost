package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/wcatz/ghost/internal/embedding"
	"github.com/wcatz/ghost/internal/memory"
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

// TestBenchEmbedderAppliesProductionTaskPrefixes: the facts seedDemo stores
// must go out with the production document prefix and the questions must be
// searched with the production query prefix. Without them the harness compares
// vectors production never compares, and — worse for a cached harness — it
// caches those vectors under text no production embed would ever hash to, so
// the cache silently keeps serving the wrong space.
//
// runDemo itself is not driven here: it runs the supersede classifier, which
// makes billable LLM calls. seedDemo is the half that owns the embeddings.
func TestBenchEmbedderAppliesProductionTaskPrefixes(t *testing.T) {
	srv, inputs := recordingOllama(t)
	emb, err := newCachedEmbedder(srv.URL, "")
	if err != nil {
		t.Fatalf("newCachedEmbedder: %v", err)
	}

	ctx := context.Background()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = store.Close() })

	const project = "mabench-prefix"
	if err := store.EnsureProject(ctx, project, "/bench/"+project, project); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	d := Demo{
		Source:    "unit",
		Context:   "1. Alice moved to Lisbon.\n2. Bob stayed in Oslo.\n",
		Questions: []string{"Where does Alice live now?"},
		Answers:   [][]string{{"Lisbon"}},
		QAPairIDs: []string{"q1"},
	}
	if _, err := seedDemo(ctx, store, db, project, d, emb); err != nil {
		t.Fatalf("seedDemo: %v", err)
	}
	if _, err := searchQuestion(ctx, store, project, d.Questions[0], emb, memory.DefaultSearchParams()); err != nil {
		t.Fatalf("searchQuestion: %v", err)
	}

	sent := inputs()
	facts := splitFacts(d.Context)
	for _, fact := range facts {
		if !slices.Contains(sent, embedding.NomicTaskDocument+fact) {
			t.Errorf("fact %q was not embedded with the production document prefix; sent %q", fact, sent)
		}
		if slices.Contains(sent, fact) {
			t.Errorf("fact was embedded unprefixed — a vector space production never searches; sent %q", sent)
		}
	}
	if !slices.Contains(sent, embedding.NomicTaskQuery+d.Questions[0]) {
		t.Errorf("question was not embedded with the production query prefix; sent %q", sent)
	}
	if slices.Contains(sent, d.Questions[0]) {
		t.Errorf("question was embedded unprefixed; sent %q", sent)
	}
	// The question must never ride the document batch either: it used to be
	// seeded alongside the facts, which would embed it in the document space
	// and cache that vector under the question's own text.
	if slices.Contains(sent, embedding.NomicTaskDocument+d.Questions[0]) {
		t.Errorf("question was embedded as a document; sent %q", sent)
	}
}
