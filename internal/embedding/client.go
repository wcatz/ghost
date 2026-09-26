// Package embedding provides a client for generating text embeddings
// via a local Ollama instance.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client calls Ollama's /api/embed endpoint to generate embeddings.
type Client struct {
	baseURL    string
	model      string
	dimensions int
	// prefixes is the task-prefix pair this model requires (see prefix.go),
	// resolved once at construction so neither embed path re-derives it.
	prefixes TaskPrefixes
	// identity is what this client stamps into memory_embeddings.model, and so
	// what decides whether the vectors it wrote are still comparable with a
	// query vector. See VectorIdentity.
	identity   string
	httpClient *http.Client
}

// NewClient creates an embedding client.
// baseURL is the Ollama API base (e.g. "http://localhost:11434").
func NewClient(baseURL, model string, dimensions int) *Client {
	return &Client{
		baseURL:    baseURL,
		model:      model,
		dimensions: dimensions,
		prefixes:   TaskPrefixesFor(model),
		identity:   VectorIdentity(model, dimensions),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// embedRequest is the Ollama /api/embed request body.
type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

// embedResponse is the Ollama /api/embed response.
type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// EmbedDocument generates the embedding for text that will be STORED and
// searched over later. Models that separate the two roles (nomic-embed-text)
// need the document task prefix, which is applied here; the worker embeds
// every memory through this method, so a memory never enters the index in the
// wrong space.
//
// Returns an error if the service is unavailable.
func (c *Client) EmbedDocument(ctx context.Context, text string) ([]float32, error) {
	return c.embed(ctx, c.prefixes.prefixedDocument(text))
}

// EmbedQuery generates the embedding for a SEARCH QUERY, applying the query
// task prefix where the model requires one. Only vectors from this method may
// be compared against stored vectors, for the same reason EmbedDocument
// carries the document prefix.
func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return c.embed(ctx, c.prefixes.prefixedQuery(text))
}

// embed performs one /api/embed round trip with input already prepared.
func (c *Client) embed(ctx context.Context, input string) ([]float32, error) {
	body, err := json.Marshal(embedRequest{
		Model: c.model,
		Input: input,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal embed request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("ollama %d: %s", resp.StatusCode, string(respBody))
	}

	var result embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if len(result.Embeddings) == 0 || len(result.Embeddings[0]) == 0 {
		return nil, fmt.Errorf("empty embedding response")
	}

	vec := result.Embeddings[0]
	if len(vec) != c.dimensions {
		return nil, fmt.Errorf("expected %d dimensions, got %d", c.dimensions, len(vec))
	}

	return vec, nil
}

// Alive checks if Ollama is reachable by hitting the root endpoint.
func (c *Client) Alive(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/", nil)
	if err != nil {
		return false
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// tagsResponse is the Ollama /api/tags response.
type tagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// HasModel reports whether the configured model is installed in Ollama.
// A model name without a tag matches its ":latest" variant, mirroring
// Ollama's own resolution.
func (c *Client) HasModel(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tags", nil)
	if err != nil {
		return false, fmt.Errorf("create request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("ollama tags: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("ollama tags: status %d", resp.StatusCode)
	}
	var tags tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return false, fmt.Errorf("decode tags: %w", err)
	}

	want := c.model
	if !strings.Contains(want, ":") {
		want += ":latest"
	}
	for _, m := range tags.Models {
		if m.Name == want {
			return true, nil
		}
	}
	return false, nil
}

// Model returns the configured model name.
func (c *Client) Model() string {
	return c.model
}

// Dimensions returns the expected embedding dimensionality.
func (c *Client) Dimensions() int {
	return c.dimensions
}

// Identity returns the vector-space identity of everything this client embeds:
// the model, its dimensions, and whether a task prefix is applied (see
// VectorIdentity). It is the value stored alongside each vector, and the value
// memory.Store compares against to decide a stored vector is still usable —
// so the store must be given the same string (SetEmbeddingIdentity) or it will
// neither trust this client's vectors nor retire the previous ones.
func (c *Client) Identity() string {
	return c.identity
}
