// internal/ai/provider.go
package ai

import (
	"context"
	"errors"
)

// ErrCreditExhausted marks an Anthropic API response that failed specifically
// because the account is out of credit, as distinct from an invalid key, a
// rate limit, or a network failure. Wrapped so callers use errors.Is instead
// of matching error text.
var ErrCreditExhausted = errors.New("anthropic credit balance too low")

// ClassifyResult is the outcome of a one-word classification call.
// FromFallback is true when the primary provider failed with a credit
// exhaustion error and a secondary provider answered instead — callers use
// this to enforce a dry-run guardrail on unattended writes (see
// FallbackProvider).
type ClassifyResult struct {
	Text         string
	FromFallback bool
}

// Provider answers a one-word classification question given a system prompt
// (the task instructions) and user content (the data to classify).
type Provider interface {
	Classify(ctx context.Context, systemPrompt, userContent string) (string, error)
}

// reflectClient is the one method anthropicClient needs — satisfied by
// *Client. Narrowed so tests never need a real client.
type reflectClient interface {
	Reflect(ctx context.Context, prompt string) (string, TokenUsage, error)
}

// anthropicClient adapts a *Client to the Provider interface.
// Client.Reflect takes a single prompt string and always sends its own fixed
// system block (for reflection, not classification) — so systemPrompt and
// userContent are joined into one prompt here, reproducing the single
// fmt.Sprintf-built prompt the classifiers used before this seam existed.
type anthropicClient struct {
	client reflectClient
}

// NewAnthropicProvider wraps client (typically *Client) as a Provider.
func NewAnthropicProvider(client reflectClient) Provider {
	return &anthropicClient{client: client}
}

func (a *anthropicClient) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	text, _, err := a.client.Reflect(ctx, systemPrompt+"\n\n"+userContent)
	return text, err
}

// isCreditExhausted reports whether err (or anything it wraps) is
// ErrCreditExhausted.
func isCreditExhausted(err error) bool {
	return errors.Is(err, ErrCreditExhausted)
}
