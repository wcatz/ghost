// Package embedding generates text embeddings via Ollama.
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

// Client generates embeddings through Ollama's /api/embed endpoint.
type Client struct {
	baseURL    string
	model      string
	dimensions int
	prefixes   TaskPrefixes
	identity   string
	httpClient *http.Client
}

// NewClient creates an embedding client for baseURL.
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

type embedRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

// EmbedDocument returns the embedding for stored text.
func (c *Client) EmbedDocument(ctx context.Context, text string) ([]float32, error) {
	return c.embed(ctx, c.prefixes.prefixedDocument(text))
}

// EmbedQuery returns the embedding for a search query.
func (c *Client) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	return c.embed(ctx, c.prefixes.prefixedQuery(text))
}

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

// Reachability reports the result of a liveness probe.
type Reachability int

const (
	// Reachable means the endpoint answered 200.
	Reachable Reachability = iota
	// Unreachable means the endpoint refused or answered non-200.
	Unreachable
	// Inconclusive means the probe deadline expired.
	Inconclusive
)

// String returns the reachability state name.
func (r Reachability) String() string {
	switch r {
	case Reachable:
		return "reachable"
	case Unreachable:
		return "unreachable"
	default:
		return "inconclusive"
	}
}

var aliveProbeTimeout = 2 * time.Second

// Probe checks whether Ollama is reachable.
func (c *Client) Probe(ctx context.Context) Reachability {
	ctx, cancel := context.WithTimeout(ctx, aliveProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/", nil)
	if err != nil {
		return Unreachable
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Inconclusive
		}
		return Unreachable
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return Reachable
	}
	return Unreachable
}

// Alive reports whether Ollama is reachable.
func (c *Client) Alive(ctx context.Context) bool {
	return c.Probe(ctx) == Reachable
}

type tagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// HasModel reports whether the configured model is installed in Ollama.
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

	want := resolvedModelName(c.model)
	for _, m := range tags.Models {
		if resolvedModelName(m.Name) == want {
			return true, nil
		}
	}
	return false, nil
}

func resolvedModelName(name string) string {
	name = strings.TrimPrefix(name, "registry.ollama.ai/")
	name = strings.TrimPrefix(name, "library/")
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		if i < len(name)-1 {
			return name
		}
		name = name[:i]
	}
	return name + ":latest"
}

// Model returns the configured model name.
func (c *Client) Model() string {
	return c.model
}

// Dimensions returns the embedding dimensionality.
func (c *Client) Dimensions() int {
	return c.dimensions
}

// Identity returns the vector-space identity for this client.
func (c *Client) Identity() string {
	return c.identity
}
