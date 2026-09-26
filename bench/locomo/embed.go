package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/wcatz/ghost/internal/embedding"
)

const embedModel = "nomic-embed-text:v1.5"
const embedBatchSize = 64

// cachedEmbedder resolves text embeddings through an append-only JSONL cache
// keyed by the hash of the role-prefixed input (see documentInput/queryInput),
// batching cache misses to Ollama. Shared sessions across questions (and across
// runs) are embedded exactly once.
type cachedEmbedder struct {
	ollamaURL string
	client    *http.Client
	cache     map[string][]float32
	cacheFile *os.File
	hits      int
	misses    int
}

type cacheLine struct {
	Hash   string    `json:"h"`
	Vector []float32 `json:"v"`
}

func newCachedEmbedder(ollamaURL, cachePath string) (*cachedEmbedder, error) {
	e := &cachedEmbedder{
		ollamaURL: ollamaURL,
		client:    &http.Client{Timeout: 5 * time.Minute},
		cache:     make(map[string][]float32),
	}
	if cachePath == "" {
		return e, nil
	}
	f, err := os.OpenFile(cachePath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open embed cache: %w", err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		var line cacheLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue // torn tail line from an interrupted run; recomputed on miss
		}
		e.cache[line.Hash] = line.Vector
	}
	if err := sc.Err(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("read embed cache: %w", err)
	}
	if _, err := f.Seek(0, 2); err != nil { // append from the end
		_ = f.Close()
		return nil, err
	}
	// A torn tail line (interrupted run) was skipped by the loader above;
	// terminate it so the next append doesn't fuse into an unparseable line.
	if st, statErr := f.Stat(); statErr == nil && st.Size() > 0 {
		last := make([]byte, 1)
		if _, readErr := f.ReadAt(last, st.Size()-1); readErr == nil && last[0] != '\n' {
			if _, writeErr := f.Write([]byte("\n")); writeErr != nil {
				_ = f.Close()
				return nil, fmt.Errorf("terminate torn cache tail: %w", writeErr)
			}
		}
	}
	e.cacheFile = f
	return e, nil
}

func (e *cachedEmbedder) Close() error {
	if e.cacheFile != nil {
		return e.cacheFile.Close()
	}
	return nil
}

func (e *cachedEmbedder) Stats() (hits, misses int) { return e.hits, e.misses }

func hashContent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// documentInput and queryInput are the exact strings sent to Ollama for the
// two roles, and therefore the text the cache is keyed on: the task prefix
// changes every vector the model emits (embedding.VectorIdentity folds it into
// the stored identity), so a key over the raw text would hand back a vector
// embedded without it.
//
// Both go through the helpers Client.EmbedDocument and Client.EmbedQuery build
// their requests from, rather than repeating the prefix here, so the harness
// cannot drift into a vector space production never searches.
func documentInput(text string) string { return embedding.PrefixedDocument(embedModel, text) }
func queryInput(text string) string    { return embedding.PrefixedQuery(embedModel, text) }

// EnsureBatch resolves every document into the cache, batching misses to
// Ollama. texts are stored turns (or about to be stored), so each is embedded
// in the document role. Questions must not be passed here: each is embedded
// per search through EmbedQuery, and a question seeded into this batch would
// be embedded as a document and cached under the query's own text — a vector
// no search ever looks up.
func (e *cachedEmbedder) EnsureBatch(ctx context.Context, texts []string) error {
	inputs := make([]string, len(texts))
	for i, t := range texts {
		inputs[i] = documentInput(t)
	}
	return e.ensure(ctx, inputs)
}

// ensure resolves inputs already in their final, role-prefixed form: the cache
// key and the Ollama payload are the same string, so a hit means "this exact
// text was embedded in this exact role".
func (e *cachedEmbedder) ensure(ctx context.Context, inputs []string) error {
	var missing []string
	seen := make(map[string]bool)
	for _, t := range inputs {
		h := hashContent(t)
		if _, ok := e.cache[h]; ok || seen[h] {
			continue
		}
		seen[h] = true
		missing = append(missing, t)
	}
	for start := 0; start < len(missing); start += embedBatchSize {
		end := min(start+embedBatchSize, len(missing))
		batch := missing[start:end]
		vecs, err := e.embedRemote(ctx, batch)
		if err != nil {
			return err
		}
		for i, t := range batch {
			h := hashContent(t)
			e.cache[h] = vecs[i]
			e.misses++
			if e.cacheFile != nil {
				line, _ := json.Marshal(cacheLine{Hash: h, Vector: vecs[i]})
				if _, err := fmt.Fprintf(e.cacheFile, "%s\n", line); err != nil {
					return fmt.Errorf("append embed cache: %w", err)
				}
			}
		}
	}
	return nil
}

// EmbedDocument returns the cached vector for text that is stored and
// searched over, resolving it remotely on a miss. The counterpart of
// production's Client.EmbedDocument.
func (e *cachedEmbedder) EmbedDocument(ctx context.Context, text string) ([]float32, error) {
	return e.embedInput(ctx, documentInput(text))
}

// EmbedQuery returns the cached vector for a search query, resolving it
// remotely on a miss. Only vectors from here may be compared against stored
// ones — the counterpart of production's Client.EmbedQuery.
func (e *cachedEmbedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return e.embedInput(ctx, queryInput(text))
}

func (e *cachedEmbedder) embedInput(ctx context.Context, input string) ([]float32, error) {
	if v, ok := e.cache[hashContent(input)]; ok {
		e.hits++
		return v, nil
	}
	if err := e.ensure(ctx, []string{input}); err != nil {
		return nil, err
	}
	return e.cache[hashContent(input)], nil
}

type ollamaEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

func (e *cachedEmbedder) embedRemote(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(ollamaEmbedRequest{Model: embedModel, Input: texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.ollamaURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama embed: HTTP %d", resp.StatusCode)
	}
	var er ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("ollama embed decode: %w", err)
	}
	if len(er.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama embed: %d vectors for %d inputs", len(er.Embeddings), len(texts))
	}
	return er.Embeddings, nil
}
