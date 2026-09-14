package ai

import "testing"

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