// internal/ai/sampling_provider.go
package ai

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sampler is the one method SamplingProvider needs from *mcp.ServerSession —
// narrowed so tests can inject a fake without a real MCP session.
type sampler interface {
	CreateMessage(ctx context.Context, params *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error)
}

// SamplingProvider asks the connected MCP client's own model to classify, via
// MCP sampling (CreateMessage). It is only constructible where a live session
// exists — this is the live-session path, never used headless. Sampling
// support is inconsistent across MCP clients today (some return "Method not
// found"; see internal/mcpserver's ghost_resolve wiring), so callers should
// pair it with NewAlwaysFallbackProvider and a CLIClient secondary rather
// than assume sampling alone is enough.
type SamplingProvider struct {
	session sampler
}

// NewSamplingProvider wraps an *mcp.ServerSession as a Provider.
func NewSamplingProvider(session sampler) *SamplingProvider {
	return &SamplingProvider{session: session}
}

func (s *SamplingProvider) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	result, err := s.session.CreateMessage(ctx, &mcp.CreateMessageParams{
		SystemPrompt: systemPrompt,
		Messages: []*mcp.SamplingMessage{
			{Role: "user", Content: &mcp.TextContent{Text: userContent}},
		},
		MaxTokens: 16,
	})
	if err != nil {
		return "", fmt.Errorf("mcp sampling: %w", err)
	}
	text, ok := result.Content.(*mcp.TextContent)
	if !ok {
		return "", fmt.Errorf("mcp sampling: unexpected content type %T", result.Content)
	}
	return text.Text, nil
}
