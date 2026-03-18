package ai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCachedBlock(t *testing.T) {
	block := CachedBlock("test content")

	if block.Type != "text" {
		t.Errorf("expected type 'text', got %q", block.Type)
	}
	if block.Text != "test content" {
		t.Errorf("expected text 'test content', got %q", block.Text)
	}
	if block.CacheControl == nil {
		t.Fatal("expected cache control to be set")
	}
	if block.CacheControl.Type != "ephemeral" {
		t.Errorf("expected cache type 'ephemeral', got %q", block.CacheControl.Type)
	}
}

func TestPlainBlock(t *testing.T) {
	block := PlainBlock("plain text")

	if block.Type != "text" {
		t.Errorf("expected type 'text', got %q", block.Type)
	}
	if block.Text != "plain text" {
		t.Errorf("expected text 'plain text', got %q", block.Text)
	}
	if block.CacheControl != nil {
		t.Error("expected no cache control for plain block")
	}
}

func TestTextMessage(t *testing.T) {
	msg := TextMessage("user", "hello world")

	if msg.Role != "user" {
		t.Errorf("expected role 'user', got %q", msg.Role)
	}
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(msg.Content))
	}
	if msg.Content[0].Type != "text" {
		t.Errorf("expected type 'text', got %q", msg.Content[0].Type)
	}
	if msg.Content[0].Text != "hello world" {
		t.Errorf("expected text 'hello world', got %q", msg.Content[0].Text)
	}
}

func TestToolResultMessage(t *testing.T) {
	results := []ToolResult{
		{ToolUseID: "tool1", Content: "output1", IsError: false},
		{ToolUseID: "tool2", Content: "error message", IsError: true},
	}

	msg := ToolResultMessage(results)

	if msg.Role != "user" {
		t.Errorf("expected role 'user', got %q", msg.Role)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(msg.Content))
	}

	// Check first result
	if msg.Content[0].Type != "tool_result" {
		t.Errorf("expected type 'tool_result', got %q", msg.Content[0].Type)
	}
	if msg.Content[0].ToolUseID != "tool1" {
		t.Errorf("expected tool_use_id 'tool1', got %q", msg.Content[0].ToolUseID)
	}
	if !strings.Contains(string(msg.Content[0].Content), "output1") {
		t.Errorf("expected content to contain 'output1', got %s", string(msg.Content[0].Content))
	}
	if msg.Content[0].IsError {
		t.Error("expected IsError false")
	}

	// Check second result
	if msg.Content[1].IsError == false {
		t.Error("expected IsError true")
	}
}

func TestImageBlock(t *testing.T) {
	block := ImageBlock("image/png", "base64data")

	if block.Type != "image" {
		t.Errorf("expected type 'image', got %q", block.Type)
	}
	if block.Source == nil {
		t.Fatal("expected source to be set")
	}
	if block.Source.Type != "base64" {
		t.Errorf("expected source type 'base64', got %q", block.Source.Type)
	}
	if block.Source.MediaType != "image/png" {
		t.Errorf("expected media type 'image/png', got %q", block.Source.MediaType)
	}
	if block.Source.Data != "base64data" {
		t.Errorf("expected data 'base64data', got %q", block.Source.Data)
	}
}

func TestMultimodalMessage(t *testing.T) {
	images := []ContentBlock{
		ImageBlock("image/jpeg", "jpg_data"),
		ImageBlock("image/png", "png_data"),
	}

	msg := MultimodalMessage("describe these images", images)

	if msg.Role != "user" {
		t.Errorf("expected role 'user', got %q", msg.Role)
	}
	if len(msg.Content) != 3 {
		t.Fatalf("expected 3 content blocks (2 images + 1 text), got %d", len(msg.Content))
	}

	// Images should come first
	if msg.Content[0].Type != "image" {
		t.Errorf("expected first block to be image, got %q", msg.Content[0].Type)
	}
	if msg.Content[1].Type != "image" {
		t.Errorf("expected second block to be image, got %q", msg.Content[1].Type)
	}
	if msg.Content[2].Type != "text" {
		t.Errorf("expected third block to be text, got %q", msg.Content[2].Type)
	}
	if msg.Content[2].Text != "describe these images" {
		t.Errorf("expected text 'describe these images', got %q", msg.Content[2].Text)
	}
}

func TestMultimodalMessage_EmptyText(t *testing.T) {
	images := []ContentBlock{
		ImageBlock("image/png", "data"),
	}

	msg := MultimodalMessage("", images)

	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block (image only), got %d", len(msg.Content))
	}
	if msg.Content[0].Type != "image" {
		t.Errorf("expected image block, got %q", msg.Content[0].Type)
	}
}

func TestSystemBlock_JSONSerialization(t *testing.T) {
	tests := []struct {
		name  string
		block SystemBlock
		want  string
	}{
		{
			name:  "cached block",
			block: CachedBlock("test"),
			want:  `{"type":"text","text":"test","cache_control":{"type":"ephemeral"}}`,
		},
		{
			name:  "plain block",
			block: PlainBlock("test"),
			want:  `{"type":"text","text":"test"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.block)
			if err != nil {
				t.Fatalf("marshal error: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("expected JSON %s, got %s", tt.want, string(got))
			}
		})
	}
}

func TestToolDefinition_JSONSerialization(t *testing.T) {
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "file path",
			},
		},
		"required": []string{"path"},
	}

	tool := ToolDefinition{
		Name:        "file_read",
		Description: "Read a file",
		InputSchema: schema,
	}

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded["name"] != "file_read" {
		t.Errorf("expected name 'file_read', got %v", decoded["name"])
	}
	if decoded["description"] != "Read a file" {
		t.Errorf("expected description 'Read a file', got %v", decoded["description"])
	}
	if decoded["input_schema"] == nil {
		t.Error("expected input_schema to be present")
	}
}

func TestTokenUsage_ZeroValues(t *testing.T) {
	usage := TokenUsage{}

	if usage.InputTokens != 0 {
		t.Errorf("expected InputTokens 0, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 0 {
		t.Errorf("expected OutputTokens 0, got %d", usage.OutputTokens)
	}
	if usage.CacheCreationInputTokens != 0 {
		t.Errorf("expected CacheCreationInputTokens 0, got %d", usage.CacheCreationInputTokens)
	}
	if usage.CacheReadInputTokens != 0 {
		t.Errorf("expected CacheReadInputTokens 0, got %d", usage.CacheReadInputTokens)
	}
}

func TestStreamEvent_Types(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		event     StreamEvent
	}{
		{
			name:      "text event",
			eventType: "text",
			event:     StreamEvent{Type: "text", Text: "hello"},
		},
		{
			name:      "thinking event",
			eventType: "thinking",
			event:     StreamEvent{Type: "thinking", Text: "reasoning..."},
		},
		{
			name:      "tool_use_start",
			eventType: "tool_use_start",
			event: StreamEvent{
				Type: "tool_use_start",
				ToolUse: &ToolUseEvent{
					ID:   "tool_123",
					Name: "file_read",
				},
			},
		},
		{
			name:      "done event",
			eventType: "done",
			event: StreamEvent{
				Type:       "done",
				StopReason: "end_turn",
				Usage:      &TokenUsage{InputTokens: 100, OutputTokens: 50},
			},
		},
		{
			name:      "error event",
			eventType: "error",
			event:     StreamEvent{Type: "error", Error: &json.SyntaxError{}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.event.Type != tt.eventType {
				t.Errorf("expected type %q, got %q", tt.eventType, tt.event.Type)
			}
		})
	}
}

func TestThinkingConfig(t *testing.T) {
	// Fixed budget.
	cfg := ThinkingConfig{
		Type:         "enabled",
		BudgetTokens: 5000,
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	var decoded ThinkingConfig
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if decoded.Type != "enabled" {
		t.Errorf("expected type 'enabled', got %q", decoded.Type)
	}
	// BudgetTokens is interface{} — JSON decodes numbers as float64.
	if budget, ok := decoded.BudgetTokens.(float64); !ok || budget != 5000 {
		t.Errorf("expected budget 5000, got %v", decoded.BudgetTokens)
	}

	// Adaptive thinking — no budget_tokens in JSON.
	adaptive := ThinkingConfig{Type: "enabled"}
	data2, _ := json.Marshal(adaptive)
	s := string(data2)
	if strings.Contains(s, "budget_tokens") {
		t.Errorf("adaptive config should omit budget_tokens, got: %s", s)
	}
}

func TestConstants(t *testing.T) {
	if APIURL != "https://api.anthropic.com/v1/messages" {
		t.Errorf("unexpected APIURL: %q", APIURL)
	}
	if APIVersion != "2025-04-14" {
		t.Errorf("unexpected APIVersion: %q", APIVersion)
	}
	if ModelSonnet46 == "" {
		t.Error("ModelSonnet46 should not be empty")
	}
	if ModelHaiku45 == "" {
		t.Error("ModelHaiku45 should not be empty")
	}
	if ModelOpus46 == "" {
		t.Error("ModelOpus46 should not be empty")
	}
}
