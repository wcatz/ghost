package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Client is a Claude API client with streaming and tool_use support.
type Client struct {
	apiKey     string
	httpClient *http.Client
	logger     *slog.Logger
}

// NewClient creates a new Claude API client.
func NewClient(apiKey string, logger *slog.Logger) *Client {
	return &Client{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 120 * time.Second,
		},
		logger: logger,
	}
}

// Reflect calls Haiku (non-streaming) for memory extraction/reflection.
func (c *Client) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	reqBody := apiRequest{
		Model:     ModelHaiku45,
		MaxTokens: 2000,
		System: []SystemBlock{
			CachedBlock("You analyze a software developer's coding patterns and produce structured memory output. You must return ONLY valid JSON — no markdown fences, no extra text. Be specific and actionable."),
		},
		Stream: false,
		Messages: []Message{
			TextMessage("user", prompt),
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", TokenUsage{}, fmt.Errorf("marshal reflect request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", APIURL, bytes.NewReader(body))
	if err != nil {
		return "", TokenUsage{}, fmt.Errorf("create reflect request: %w", err)
	}
	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", TokenUsage{}, fmt.Errorf("reflect API call: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return "", TokenUsage{}, fmt.Errorf("read reflect response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", TokenUsage{}, fmt.Errorf("reflect API status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", TokenUsage{}, fmt.Errorf("unmarshal reflect response: %w", err)
	}

	var text string
	if len(result.Content) > 0 {
		text = result.Content[0].Text
	}

	usage := TokenUsage{
		InputTokens:  result.Usage.InputTokens,
		OutputTokens: result.Usage.OutputTokens,
	}

	return text, usage, nil
}

// setHeaders applies common headers to an API request.
func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", APIVersion)
	req.Header.Set("anthropic-beta", BetaInterleavedThinking)
}

// parseAPIError extracts a user-friendly message from Claude API error responses.
func parseAPIError(statusCode int, body []byte) error {
	var apiErr struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &apiErr); err == nil && apiErr.Error.Message != "" {
		switch {
		case statusCode == 400 && apiErr.Error.Type == "invalid_request_error" &&
			(len(apiErr.Error.Message) > 20 && apiErr.Error.Message[:20] == "Your credit balance "):
			return fmt.Errorf("credit balance too low — add credits at console.anthropic.com/settings/billing")
		case statusCode == 401:
			return fmt.Errorf("invalid API key — check ghost config")
		case statusCode == 403:
			return fmt.Errorf("permission denied: %s", apiErr.Error.Message)
		default:
			return fmt.Errorf("api error (%d): %s", statusCode, apiErr.Error.Message)
		}
	}
	return fmt.Errorf("API returned %d: %s", statusCode, string(body))
}
