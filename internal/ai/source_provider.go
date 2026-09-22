package ai

import (
	"context"
	"errors"
	"os/exec"
	"strings"
)

var errUnknownSource = errors.New("unknown source: no CLI backend available for this host")

type SourceProvider struct {
	backend cliBackend
	name    string
}

// NewSourceProviderForSource returns a provider for the given source token, or
// an unavailable one ("none") for an empty or unknown source. There is no
// cascade: silently classifying through a different harness than the caller's
// would bill the wrong subscription and betray the user's routing choice.
// Callers resolve the source (--source, or ai.DetectSource) and fail with an
// actionable error when it is empty.
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
		return &SourceProvider{backend: nil, name: "none"}
	}
}

// resolveCLI resolves a specific CLI backend for the given source. It does NOT
// cascade to other binaries — if the source-specific binary isn't found, the
// provider is unavailable. This is intentional: when the user's session used
// claude, we should use claude, not silently switch to opencode.
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

// clientSourceNames maps an MCP client's self-reported name
// (InitializeParams.ClientInfo.Name) to the source token
// NewSourceProviderForSource understands. Matching is case-insensitive
// substring-based so minor naming variance ("Claude Desktop", "claude-code")
// still resolves. Adding a new client is one table row here.
var clientSourceNames = []struct{ name, source string }{
	{"opencode", "opencode"},
	{"claude", "claude-code"},
	{"codex", "codex"},
	{"goose", "goose"},
}

// SourceForClientName maps an MCP client's reported name to a source token,
// or "" for unknown names. Unknown does not cascade to a default harness:
// callers then detect the calling harness (ai.DetectSource) or fail with an
// actionable error.
func SourceForClientName(clientName string) string {
	lower := strings.ToLower(clientName)
	for _, c := range clientSourceNames {
		if strings.Contains(lower, c.name) {
			return c.source
		}
	}
	return ""
}
