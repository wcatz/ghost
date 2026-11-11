package ai

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
)

var errUnknownSource = errors.New("unknown source: no CLI backend available for this host")

type SourceProvider struct {
	backend cliBackend
	name    string
}

// NewSourceProviderForSource returns a provider for the given source token.
func NewSourceProviderForSource(source string, cfgBinaries ...string) *SourceProvider {
	return newSourceProvider(source, "", cfgBinaries...)
}

// NewSourceProviderForSourceWithModel returns a provider for the given source token with an opencode model pin.
func NewSourceProviderForSourceWithModel(source, model string, cfgBinaries ...string) *SourceProvider {
	return newSourceProvider(source, model, cfgBinaries...)
}

var knownSources = []string{"claude-code", "opencode", "codex", "goose"}

// KnownSources returns the source tokens NewSourceProviderForSource resolves.
func KnownSources() []string { return slices.Clone(knownSources) }

// IsKnownSource reports whether source is a known source token.
func IsKnownSource(source string) bool { return slices.Contains(knownSources, source) }

func newSourceProvider(source, model string, cfgBinaries ...string) *SourceProvider {
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
		return resolveCLI(claudeBin, "claude", "cli", model)
	case "opencode":
		return resolveCLI(opencodeBin, "opencode", "opencode", model)
	case "codex":
		return resolveCLI(codexBin, "codex", "codex", model)
	case "goose":
		return resolveCLI(gooseBin, "goose", "goose", model)
	default:
		return &SourceProvider{backend: nil, name: "none"}
	}
}

func resolveCLI(configured, defaultName, providerName, model string) *SourceProvider {
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
		return &SourceProvider{backend: NewOpenCodeClientWithBinaryAndModel(bin, model), name: "opencode"}
	case "codex":
		return &SourceProvider{backend: NewCodexClientWithBinary(bin), name: "codex"}
	case "goose":
		return &SourceProvider{backend: NewGooseClientWithBinary(bin), name: "goose"}
	}
	return &SourceProvider{backend: nil, name: "none"}
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

var clientSourceNames = []struct{ name, source string }{
	{"opencode", "opencode"},
	{"claude", "claude-code"},
	{"codex", "codex"},
	{"goose", "goose"},
}

// SourceForClientName maps an MCP client name to a source token.
func SourceForClientName(clientName string) string {
	lower := strings.ToLower(clientName)
	for _, c := range clientSourceNames {
		if strings.Contains(lower, c.name) {
			return c.source
		}
	}
	return ""
}
