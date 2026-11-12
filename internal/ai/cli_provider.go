package ai

import (
	"context"
	"errors"
	"os/exec"
)

var errNoCLIBackend = errors.New("no CLI LLM backend available (install claude, opencode, codex, or goose)")

type cliBackend interface {
	Reflect(ctx context.Context, prompt string) (string, TokenUsage, error)
	Classify(ctx context.Context, systemPrompt, userContent string) (string, error)
}

// CLIProvider selects the best available CLI backend on PATH.
type CLIProvider struct {
	backend cliBackend
	name    string
}

// NewCLIProviderWithBinaries returns a CLIProvider using explicit binaries when available.
func NewCLIProviderWithBinaries(claudeBinary, opencodeBinary, codexBinary, gooseBinary string) *CLIProvider {
	if claudeBinary != "" {
		if _, err := exec.LookPath(claudeBinary); err == nil {
			return &CLIProvider{backend: NewCLIClientWithBinary(claudeBinary), name: "cli"}
		}
	}
	if _, err := exec.LookPath("claude"); err == nil {
		return &CLIProvider{backend: NewCLIClient(), name: "cli"}
	}
	if opencodeBinary != "" {
		if _, err := exec.LookPath(opencodeBinary); err == nil {
			return &CLIProvider{backend: NewOpenCodeClientWithBinary(opencodeBinary), name: "opencode"}
		}
	}
	if _, err := exec.LookPath("opencode"); err == nil {
		return &CLIProvider{backend: NewOpenCodeClient(), name: "opencode"}
	}
	if codexBinary != "" {
		if _, err := exec.LookPath(codexBinary); err == nil {
			return &CLIProvider{backend: NewCodexClientWithBinary(codexBinary), name: "codex"}
		}
	}
	if _, err := exec.LookPath("codex"); err == nil {
		return &CLIProvider{backend: NewCodexClient(), name: "codex"}
	}
	if gooseBinary != "" {
		if _, err := exec.LookPath(gooseBinary); err == nil {
			return &CLIProvider{backend: NewGooseClientWithBinary(gooseBinary), name: "goose"}
		}
	}
	if _, err := exec.LookPath("goose"); err == nil {
		return &CLIProvider{backend: NewGooseClient(), name: "goose"}
	}
	return &CLIProvider{backend: nil, name: "none"}
}

// Name reports which backend was selected.
func (p *CLIProvider) Name() string { return p.name }

// Available reports whether a backend is available.
func (p *CLIProvider) Available() bool { return p.backend != nil }

func (p *CLIProvider) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	if p.backend == nil {
		return "", TokenUsage{}, errNoCLIBackend
	}
	return p.backend.Reflect(ctx, prompt)
}

func (p *CLIProvider) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	if p.backend == nil {
		return "", errNoCLIBackend
	}
	return p.backend.Classify(ctx, systemPrompt, userContent)
}
