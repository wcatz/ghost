// internal/ai/sampling_provider_test.go
package ai

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeSampler struct {
	result    *mcp.CreateMessageResult
	err       error
	gotParams *mcp.CreateMessageParams
}

func (f *fakeSampler) CreateMessage(ctx context.Context, params *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error) {
	f.gotParams = params
	return f.result, f.err
}

func TestSamplingProvider_Classify_Success(t *testing.T) {
	fake := &fakeSampler{
		result: &mcp.CreateMessageResult{
			Content: &mcp.TextContent{Text: "RESOLVED"},
			Role:    "assistant",
		},
	}
	p := NewSamplingProvider(fake)
	out, err := p.Classify(context.Background(), "system prompt text", "note content")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != "RESOLVED" {
		t.Fatalf("got %q, want RESOLVED", out)
	}
	if fake.gotParams.SystemPrompt != "system prompt text" {
		t.Errorf("got system prompt %q, want %q", fake.gotParams.SystemPrompt, "system prompt text")
	}
	if len(fake.gotParams.Messages) != 1 || fake.gotParams.Messages[0].Role != "user" {
		t.Errorf("expected one user message, got %+v", fake.gotParams.Messages)
	}
}

func TestSamplingProvider_Classify_SessionError(t *testing.T) {
	fake := &fakeSampler{err: errors.New("no client-side model available")}
	p := NewSamplingProvider(fake)
	_, err := p.Classify(context.Background(), "sys", "content")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestSamplingProvider_Classify_UnexpectedContentType(t *testing.T) {
	fake := &fakeSampler{
		result: &mcp.CreateMessageResult{Content: &mcp.ImageContent{Data: []byte{0x01}}},
	}
	p := NewSamplingProvider(fake)
	_, err := p.Classify(context.Background(), "sys", "content")
	if err == nil {
		t.Fatal("expected error for non-text content, got nil")
	}
}
