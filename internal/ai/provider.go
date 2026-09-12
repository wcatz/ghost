package ai

// Package ai provides LLM backends for Ghost: four CLI subprocess adapters
// (claude/opencode/codex/goose) plus source-aware routing to the calling
// client's own harness. There is no direct Anthropic HTTP API client — every
// memory-management call (reflect, resolve, supersede) runs through a
// subscription-billed CLI binary or the fully offline SQLite tiers.
//
// Key types: Provider, TokenUsage.

import (
	"context"
)

// Provider answers a one-word classification question given a system prompt
// (the task instructions) and user content (the data to classify).
type Provider interface {
	Classify(ctx context.Context, systemPrompt, userContent string) (string, error)
}