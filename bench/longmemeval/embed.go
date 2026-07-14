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
)

const embedModel = "nomic-embed-text:v1.5"
const embedBatchSize = 64

// cachedEmbedder resolves text embeddings through an append-only JSONL cache
// keyed by content hash, batching cache misses to Ollama. Shared sessions
// across questions (and across runs) are embedded exactly once.
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

// EnsureBatch resolves every text into the cache, batching misses to Ollama.
func (e *cachedEmbedder) EnsureBatch(ctx context.Context, texts []string) error {
	var missing []string
	seen := make(map[string]bool)
	for _, t := range texts {
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

// Embed returns the cached vector for text, resolving it remotely on a miss.
func (e *cachedEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if v, ok := e.cache[hashContent(text)]; ok {
		e.hits++
		return v, nil
	}
	if err := e.EnsureBatch(ctx, []string{text}); err != nil {
		return nil, err
	}
	return e.cache[hashContent(text)], nil
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
