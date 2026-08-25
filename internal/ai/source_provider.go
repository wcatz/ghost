package ai

import (
	"context"
	"errors"
	"os/exec"
)

var errUnknownSource = errors.New("unknown source: no CLI backend available for this host")

type SourceProvider struct {
	backend cliBackend
	name    string
}

// NewSourceProviderForSource returns a provider matching the host source.
// Empty/unknown source falls back to best-available-on-PATH.
func NewSourceProviderForSource(source string, cfgBinaries ...string) *SourceProvider {
	claudeBin, opencodeBin := "", ""
	if len(cfgBinaries) > 0 {
		claudeBin = cfgBinaries[0]
	}
	if len(cfgBinaries) > 1 {
		opencodeBin = cfgBinaries[1]
	}
	switch source {
	case "claude-code":
		return resolveCLI(claudeBin, "claude", "cli")
	case "opencode":
		return resolveCLI(opencodeBin, "opencode", "opencode")
	case "codex":
		// Codex doesn't have a Reflect/Classify-compatible CLI yet.
		// Fall back to best available (same as unknown source).
		return fallbackCLI(claudeBin, opencodeBin)
	case "goose":
		// Goose doesn't have a Reflect/Classify-compatible CLI yet.
		return fallbackCLI(claudeBin, opencodeBin)
	default:
		return fallbackCLI(claudeBin, opencodeBin)
	}
}

// resolveCLI resolves a specific CLI backend for the given source. Unlike
// fallbackCLI, it does NOT cascade to other binaries — if the source-specific
// binary isn't found, the provider is unavailable. This is intentional: when
// the user's session used claude, we should use claude, not silently switch
// to opencode.
func resolveCLI(configured, defaultName, providerName string) *SourceProvider {
	bin := configured
	if bin == "" {
		bin = defaultName
	}
	if _, err := exec.LookPath(bin); err != nil {
		return &SourceProvider{backend: nil, name: "none"}
	}
	switch providerName {
	case "cli":
		return &SourceProvider{backend: NewCLIClientWithBinary(bin), name: "cli"}
	case "opencode":
		return &SourceProvider{backend: NewOpenCodeClientWithBinary(bin), name: "opencode"}
	}
	return &SourceProvider{backend: nil, name: "none"}
}

func fallbackCLI(claudeBin, opencodeBin string) *SourceProvider {
	p := NewCLIProviderWithBinaries(claudeBin, opencodeBin)
	return &SourceProvider{backend: p.backend, name: p.name}
}

func (p *SourceProvider) Name() string    { return p.name }
func (p *SourceProvider) Available() bool { return p.backend != nil }

func (p *SourceProvider) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	if p.backend == nil {
		return "", TokenUsage{}, errUnknownSource
	}
	return p.backend.Reflect(ctx, prompt)
}

func (p *SourceProvider) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	if p.backend == nil {
		return "", errUnknownSource
	}
	return p.backend.Classify(ctx, systemPrompt, userContent)
}
