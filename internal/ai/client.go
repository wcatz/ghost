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

// ChatStream sends a request and streams events through the returned channel.
// The channel is closed when the response is complete or an error occurs.
func (c *Client) ChatStream(
	ctx context.Context,
	messages []Message,
	system []SystemBlock,
	tools []ToolDefinition,
	model string,
	maxTokens int,
) (<-chan StreamEvent, error) {
	reqBody := apiRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    system,
		Stream:    true,
		Messages:  messages,
		Tools:     tools,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", APIURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", APIVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(respBody))
	}

	events := make(chan StreamEvent, 64)
	go func() {
		defer close(events)
		defer resp.Body.Close()
		if err := parseStream(resp.Body, events); err != nil {
			events <- StreamEvent{Type: "error", Error: err}
		}
	}()

	return events, nil
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
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", APIVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", TokenUsage{}, fmt.Errorf("reflect API call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
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
