package ai

import (
	"encoding/json"
	"strings"
)

const (
	APIURL     = "https://api.anthropic.com/v1/messages"
	APIVersion = "2023-06-01"

	// Beta features enabled via anthropic-beta header (comma-separated).
	BetaInterleavedThinking = "interleaved-thinking-2025-05-14"

	ModelSonnet46 = "claude-sonnet-4-6"
	ModelHaiku45  = "claude-haiku-4-5-20251001"
	ModelOpus46   = "claude-opus-4-6"

	// Context window sizes by model family.
	ContextOpus46   = 1_000_000
	ContextSonnet46 = 1_000_000
	ContextHaiku45  = 200_000
)

// SystemBlock is a system prompt block for the Claude API.
// Multiple blocks enable prompt caching: static blocks with CacheControl
// are cached across requests (5min TTL), saving ~90% on input token cost.
type SystemBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"`
}

// CacheControlEphemeral is the cache_control value for ephemeral caching.
var CacheControlEphemeral = cacheControl{Type: "ephemeral"}

// CachedBlock creates a system block with ephemeral cache control.
func CachedBlock(text string) SystemBlock {
	return SystemBlock{
		Type:         "text",
		Text:         text,
		CacheControl: &cacheControl{Type: "ephemeral"},
	}
}

// PlainBlock creates a system block without cache control.
func PlainBlock(text string) SystemBlock {
	return SystemBlock{
		Type: "text",
		Text: text,
	}
}

// Message represents a conversation message.
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// TextMessage creates a simple text message.
func TextMessage(role, text string) Message {
	return Message{
		Role: role,
		Content: []ContentBlock{
			{Type: "text", Text: text},
		},
	}
}

// ContentBlock represents a content block in a message.
// For tool_result blocks, Content must be a list per the Claude API spec.
type ContentBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`  // for type "thinking" — API requires this field name
	Signature    string          `json:"signature,omitempty"` // for type "thinking" — multi-turn continuity
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"` // tool_result: array of content blocks
	IsError      bool            `json:"is_error,omitempty"`
	Source       *ImageSource    `json:"source,omitempty"`        // for type "image"
	CacheControl *cacheControl   `json:"cache_control,omitempty"` // for multi-turn caching
}

// ImageSource holds base64-encoded image data for Claude's vision API.
type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/png", "image/jpeg", etc.
	Data      string `json:"data"`       // base64-encoded image bytes
}

// TokenUsage holds token counts from a Claude API response.
type TokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// --- internal request/response types ---

type apiRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    []SystemBlock `json:"system,omitempty"`
	Stream    bool          `json:"stream"`
	Messages  []Message     `json:"messages"`
}

// ContextForModel returns the context window size for a given model ID.
func ContextForModel(model string) int {
	switch {
	case strings.Contains(model, "opus-4-6"):
		return ContextOpus46
	case strings.Contains(model, "sonnet-4-5"), strings.Contains(model, "sonnet-4-6"):
		return ContextSonnet46
	case strings.Contains(model, "haiku"):
		return ContextHaiku45
	default:
		return 200_000
	}
}
