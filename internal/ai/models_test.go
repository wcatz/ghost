package ai

import (
	"encoding/json"
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

func TestConstants(t *testing.T) {
	if APIURL != "https://api.anthropic.com/v1/messages" {
		t.Errorf("unexpected APIURL: %q", APIURL)
	}
	if APIVersion != "2023-06-01" {
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
