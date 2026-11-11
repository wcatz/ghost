package ai

import (
	"context"
)

// Provider classifies user content against a system prompt.
type Provider interface {
	Classify(ctx context.Context, systemPrompt, userContent string) (string, error)
}
