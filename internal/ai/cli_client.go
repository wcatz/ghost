package ai

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// defaultTimeout bounds a claude -p subprocess call when the caller's context
// carries no earlier deadline — otherwise a stalled subprocess (auth prompt,
// network stall, hung MCP init inside it) blocks the classify/reflect loop
// indefinitely, since resolve/supersede classify candidates one at a time.
const defaultTimeout = 5 * time.Minute

type claudeCapabilities struct {
	safeMode        bool
	restricted      bool
	strictMCP       bool
	disableSlash    bool
	tools           bool
	disallowedTools bool
	settingSources  bool
}

type claudeBinaryID struct {
	path    string
	size    int64
	modTime time.Time
}

var claudeCapabilityCache sync.Map // claudeBinaryID -> claudeCapabilities

func parseClaudeCapabilities(help string) claudeCapabilities {
	help = strings.ToLower(help)
	return claudeCapabilities{
		safeMode:        strings.Contains(help, "--safe-mode"),
		restricted:      strings.Contains(help, "--restricted"),
		strictMCP:       strings.Contains(help, "--strict-mcp-config"),
		disableSlash:    strings.Contains(help, "--disable-slash-commands"),
		tools:           strings.Contains(help, "--tools"),
		disallowedTools: strings.Contains(help, "--disallowedtools"),
		settingSources:  strings.Contains(help, "--setting-sources"),
	}
}

func claudeCapabilitiesFor(ctx context.Context, binary string) (claudeCapabilities, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return claudeCapabilities{}, fmt.Errorf("claude capability probe: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return claudeCapabilities{}, fmt.Errorf("claude capability probe: %w", err)
	}
	id := claudeBinaryID{path: path, size: info.Size(), modTime: info.ModTime()}
	if cached, ok := claudeCapabilityCache.Load(id); ok {
		return cached.(claudeCapabilities), nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	probe, release, _ := harnessCommand(probeCtx, path, []string{"--help"}, os.Environ(), harnessClaude)
	defer release()
	out, err := probe.Output()
	if err != nil {
		return claudeCapabilities{}, fmt.Errorf("claude capability probe: %w", err)
	}
	caps := parseClaudeCapabilities(string(out))
	claudeCapabilityCache.Store(id, caps)
	return caps, nil
}

func claudeInvocationArgs(caps claudeCapabilities) ([]string, error) {
	if !caps.safeMode || !caps.restricted || !caps.strictMCP || !caps.tools || !caps.disallowedTools {
		return nil, fmt.Errorf("installed claude lacks required no-tools flags; upgrade claude")
	}
	args := []string{
		"-p",
		"--safe-mode",
		"--restricted",
		"--strict-mcp-config",
		"--tools", "",
		"--disallowedTools", "mcp__*",
	}
	if caps.disableSlash {
		args = append(args, "--disable-slash-commands")
	}
	if caps.settingSources {
		args = append(args, "--setting-sources", "project,local")
	}
	return args, nil
}

// CLIClient drives Claude via the `claude` CLI (a `claude -p` subprocess).
// Authentication and billing belong to the caller's Claude CLI configuration.
// It implements the same Reflect/Classify shapes as the other CLI adapters
// (the cliBackend interface), so it serves reflect/resolve/supersede without a
// Ghost-managed API key.
//
// ANTHROPIC_API_KEY is stripped from the subprocess environment: if present,
// it would override subscription/OAuth login and bill the call as
// pay-per-token API usage instead, defeating the point of this client.
// The invocation also runs in restricted/safe mode with an empty built-in
// tool set and strict MCP configuration. This keeps an untrusted memory
// prompt from reaching shell, plugin, or MCP operations while preserving the
// caller's Claude authentication in CLAUDE_CONFIG_DIR.
type CLIClient struct {
	binary string
}

// NewCLIClient creates a CLIClient that invokes the `claude` binary on PATH.
func NewCLIClient() *CLIClient {
	return NewCLIClientWithBinary("claude")
}

// NewCLIClientWithBinary creates a CLIClient that invokes the given binary path
// (absolute, or a name resolved from PATH).
func NewCLIClientWithBinary(binary string) *CLIClient {
	return &CLIClient{binary: binary}
}

// Reflect satisfies reflection's reflector interface (see
// internal/reflection/tier_llm.go). TokenUsage is currently always zero: the
// CLI adapters do not parse provider usage metadata, and the harness owns its
// own billing.
func (c *CLIClient) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	text, err := c.run(ctx, prompt)
	return text, TokenUsage{}, err
}

// Classify satisfies the Provider interface (see internal/ai/provider.go).
// systemPrompt is passed via the CLI's --system-prompt flag rather than being
// concatenated into the prompt text, so it can't be confused with user content.
func (c *CLIClient) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	return c.run(ctx, userContent, "--system-prompt", systemPrompt)
}

func (c *CLIClient) run(ctx context.Context, prompt string, extraArgs ...string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	caps, err := claudeCapabilitiesFor(ctx, c.binary)
	if err != nil {
		return "", err
	}
	args, err := claudeInvocationArgs(caps)
	if err != nil {
		return "", err
	}
	args = append(args, extraArgs...)
	args = append(args, prompt)
	cmd, release, _ := harnessCommand(ctx, c.binary, args, os.Environ(), harnessClaude)
	defer release()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude -p: %w: %s", err, stderr.String())
	}
	return stdout.String(), nil
}

func stripAPIKey(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, found := strings.Cut(kv, "=")
		if found && strings.EqualFold(key, "ANTHROPIC_API_KEY") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// stripLLMKeys strips API keys for all LLM providers from the environment,
// preventing the subprocess from billing to the wrong account.
func stripLLMKeys(env []string) []string {
	out := stripAPIKey(env)
	out = stripEnvKey(out, "OPENAI_API_KEY")
	out = stripEnvKey(out, "GOOSE_PROVIDER__API_KEY")
	return out
}

func stripEnvKey(env []string, key string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, found := strings.Cut(kv, "=")
		if found && strings.EqualFold(k, key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
