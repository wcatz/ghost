// internal/ai/provider_test.go
package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeReflectClient struct {
	text      string
	err       error
	gotPrompt string
}

func (f *fakeReflectClient) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	f.gotPrompt = prompt
	return f.text, TokenUsage{}, f.err
}

func TestAnthropicClient_Classify_CombinesSystemPromptAndUserContent(t *testing.T) {
	fake := &fakeReflectClient{text: "RESOLVED"}
	p := NewAnthropicProvider(fake)
	out, err := p.Classify(context.Background(), "some system prompt", "some user content")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "RESOLVED" {
		t.Fatalf("got %q, want RESOLVED", out)
	}
	if !strings.Contains(fake.gotPrompt, "some system prompt") {
		t.Errorf("Reflect prompt %q missing systemPrompt", fake.gotPrompt)
	}
	if !strings.Contains(fake.gotPrompt, "some user content") {
		t.Errorf("Reflect prompt %q missing userContent", fake.gotPrompt)
	}
}

func TestAnthropicClient_Classify_PropagatesError(t *testing.T) {
	fake := &fakeReflectClient{err: ErrCreditExhausted}
	p := NewAnthropicProvider(fake)
	_, err := p.Classify(context.Background(), "sys", "content")
	if !errors.Is(err, ErrCreditExhausted) {
		t.Fatalf("got %v, want ErrCreditExhausted", err)
	}
}

// parseAPIErrorFixtureCreditBalance calls the real parseAPIError so this test
// exercises the actual wrapping path end to end, not a hand-rolled substitute.
func parseAPIErrorFixtureCreditBalance() error {
	body := []byte(`{"error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Claude API."}}`)
	return parseAPIError(400, body)
}

func TestIsCreditExhausted(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"credit exhausted", ErrCreditExhausted, true},
		{"wrapped credit exhausted", parseAPIErrorFixtureCreditBalance(), true},
		{"invalid key", errors.New("invalid API key — check ghost config"), false},
		{"nil", nil, false},
		{"network timeout", errors.New("reflect API call: context deadline exceeded"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCreditExhausted(tt.err); got != tt.want {
				t.Errorf("isCreditExhausted(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
