package ai

import "encoding/json"

const (
	APIURL     = "https://api.anthropic.com/v1/messages"
	APIVersion = "2023-06-01"

	ModelSonnet46 = "claude-sonnet-4-5-20250929"
	ModelHaiku45  = "claude-haiku-4-5-20251001"
	ModelOpus46   = "claude-opus-4-6-20250514"
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

// ToolResultMessage creates a tool_result message.
func ToolResultMessage(results []ToolResult) Message {
	blocks := make([]ContentBlock, len(results))
	for i, r := range results {
		blocks[i] = ContentBlock{
			Type:      "tool_result",
			ToolUseID: r.ToolUseID,
			Content:   r.Content,
			IsError:   r.IsError,
		}
	}
	return Message{Role: "user", Content: blocks}
}

// ContentBlock represents a content block in a message.
type ContentBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text,omitempty"`
	ID           string        `json:"id,omitempty"`
	Name         string        `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string        `json:"tool_use_id,omitempty"`
	Content      string        `json:"content,omitempty"`
	IsError      bool          `json:"is_error,omitempty"`
	Source       *ImageSource  `json:"source,omitempty"`       // for type "image"
	CacheControl *cacheControl `json:"cache_control,omitempty"` // for multi-turn caching
}

// ImageSource holds base64-encoded image data for Claude's vision API.
type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/png", "image/jpeg", etc.
	Data      string `json:"data"`       // base64-encoded image bytes
}

// ImageBlock creates a content block with an inline image.
func ImageBlock(mediaType, base64Data string) ContentBlock {
	return ContentBlock{
		Type: "image",
		Source: &ImageSource{
			Type:      "base64",
			MediaType: mediaType,
			Data:      base64Data,
		},
	}
}

// MultimodalMessage creates a user message with text and images.
func MultimodalMessage(text string, images []ContentBlock) Message {
	blocks := make([]ContentBlock, 0, len(images)+1)
	blocks = append(blocks, images...)
	if text != "" {
		blocks = append(blocks, ContentBlock{Type: "text", Text: text})
	}
	return Message{Role: "user", Content: blocks}
}

// ToolResult holds the result of a tool execution.
type ToolResult struct {
	ToolUseID string
	Content   string
	IsError   bool
}

// ToolDefinition defines a tool for the Claude API.
type ToolDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"input_schema"`
}

// TokenUsage holds token counts from a Claude API response.
type TokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// StreamEvent is emitted during streaming for TUI rendering.
type StreamEvent struct {
	Type       string // "text", "thinking", "tool_use_start", "tool_input_delta", "tool_use_end", "tool_diff", "done", "error"
	Text       string // for text deltas and thinking deltas
	ToolUse    *ToolUseEvent
	Usage      *TokenUsage
	StopReason string            // on "done": "end_turn" or "tool_use"
	Error      error
	Metadata   map[string]string // optional extra data (e.g. diff content)
}

// ToolUseEvent holds data for tool-related stream events.
type ToolUseEvent struct {
	ID         string
	Name       string
	InputDelta string          // partial JSON for input_json_delta
	InputFull  json.RawMessage // complete input on tool_use_end
}

// --- internal request/response types ---

// ThinkingConfig controls extended thinking (Claude's internal reasoning).
// When BudgetTokens is 0, adaptive thinking is used (Claude auto-scales effort).
type ThinkingConfig struct {
	Type         string      `json:"type"`                    // "enabled" or "disabled"
	BudgetTokens interface{} `json:"budget_tokens,omitempty"` // int for fixed, or omitted for adaptive
}

type apiRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	System    []SystemBlock    `json:"system,omitempty"`
	Stream    bool             `json:"stream"`
	Messages  []Message        `json:"messages"`
	Tools     []ToolDefinition `json:"tools,omitempty"`
	Thinking  *ThinkingConfig  `json:"thinking,omitempty"`
}

type streamEventRaw struct {
	Type         string          `json:"type"`
	Delta        json.RawMessage `json:"delta,omitempty"`
	ContentBlock *contentBlockRaw `json:"content_block,omitempty"`
	Index        int             `json:"index,omitempty"`
	Usage        *struct {
		OutputTokens int `json:"output_tokens,omitempty"`
	} `json:"usage,omitempty"`
	Message *struct {
		Usage struct {
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message,omitempty"`
}

type contentBlockRaw struct {
	Type string `json:"type"`
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

type deltaRaw struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`    // for thinking_delta
	PartialJSON string `json:"partial_json,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}
