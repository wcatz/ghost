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
// thinkingBudget > 0 enables extended thinking with the given token budget.
func (c *Client) ChatStream(
	ctx context.Context,
	messages []Message,
	system []SystemBlock,
	tools []ToolDefinition,
	model string,
	maxTokens int,
	thinkingBudget int,
) (<-chan StreamEvent, error) {
	reqBody := apiRequest{
		Model:     model,
		MaxTokens: maxTokens,
		System:    system,
		Stream:    true,
		Messages:  messages,
		Tools:     tools,
	}
	if thinkingBudget > 0 {
		reqBody.Thinking = &ThinkingConfig{
			Type:         "enabled",
			BudgetTokens: thinkingBudget,
		}
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
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		// Retry on rate limit (429) and overloaded (529) with exponential backoff.
		if resp.StatusCode == 429 || resp.StatusCode == 529 {
			retryAfter := 2 * time.Second
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if d, err := time.ParseDuration(ra + "s"); err == nil {
					retryAfter = d
				}
			}
			for attempt := 0; attempt < 3; attempt++ {
				c.logger.Warn("API rate limited, retrying", "status", resp.StatusCode, "attempt", attempt+1, "wait", retryAfter)
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-time.After(retryAfter):
				}
				retryReq, _ := http.NewRequestWithContext(ctx, "POST", APIURL, bytes.NewReader(body))
				retryReq.Header.Set("Content-Type", "application/json")
				retryReq.Header.Set("x-api-key", c.apiKey)
				retryReq.Header.Set("anthropic-version", APIVersion)
				resp, err = c.httpClient.Do(retryReq)
				if err != nil {
					return nil, fmt.Errorf("retry request: %w", err)
				}
				if resp.StatusCode == http.StatusOK {
					break
				}
				_ = resp.Body.Close()
				retryAfter *= 2
			}
			if resp.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("API rate limited after 3 retries (status %d)", resp.StatusCode)
			}
		} else {
			return nil, parseAPIError(resp.StatusCode, respBody)
		}
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

// CountTokens returns the exact token count for a set of messages + system blocks.
// Uses the /v1/messages/count_tokens API endpoint.
func (c *Client) CountTokens(ctx context.Context, messages []Message, system []SystemBlock, tools []ToolDefinition, model string) (int, error) {
	reqBody := struct {
		Model    string           `json:"model"`
		System   []SystemBlock    `json:"system,omitempty"`
		Messages []Message        `json:"messages"`
		Tools    []ToolDefinition `json:"tools,omitempty"`
	}{
		Model:    model,
		System:   system,
		Messages: messages,
		Tools:    tools,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return 0, fmt.Errorf("marshal count request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages/count_tokens", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("create count request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", APIVersion)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("count tokens request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return 0, fmt.Errorf("read count response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("count tokens API status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return 0, fmt.Errorf("unmarshal count response: %w", err)
	}
	return result.InputTokens, nil
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
