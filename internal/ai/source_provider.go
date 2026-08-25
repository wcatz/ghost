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
	var claudeBin, opencodeBin, codexBin, gooseBin string
	if len(cfgBinaries) > 0 {
		claudeBin = cfgBinaries[0]
	}
	if len(cfgBinaries) > 1 {
		opencodeBin = cfgBinaries[1]
	}
	if len(cfgBinaries) > 2 {
		codexBin = cfgBinaries[2]
	}
	if len(cfgBinaries) > 3 {
		gooseBin = cfgBinaries[3]
	}
	switch source {
	case "claude-code":
		return resolveCLI(claudeBin, "claude", "cli")
	case "opencode":
		return resolveCLI(opencodeBin, "opencode", "opencode")
	case "codex":
		return resolveCLI(codexBin, "codex", "codex")
	case "goose":
		return resolveCLI(gooseBin, "goose", "goose")
	default:
		return fallbackCLI(claudeBin, opencodeBin, codexBin, gooseBin)
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
	case "codex":
		return &SourceProvider{backend: NewCodexClientWithBinary(bin), name: "codex"}
	case "goose":
		return &SourceProvider{backend: NewGooseClientWithBinary(bin), name: "goose"}
	}
	return &SourceProvider{backend: nil, name: "none"}
}

func fallbackCLI(claudeBin, opencodeBin, codexBin, gooseBin string) *SourceProvider {
	p := NewCLIProviderWithBinaries(claudeBin, opencodeBin, codexBin, gooseBin)
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
