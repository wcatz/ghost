package ai

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

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

func (id claudeBinaryID) probeKey() string {
	return id.path + "\x00" + strconv.FormatInt(id.size, 10) + "\x00" + strconv.FormatInt(id.modTime.UnixNano(), 10)
}

var claudeCapabilityCache sync.Map

var claudeProbeGroup singleflight.Group

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

	for {
		flight := claudeProbeGroup.DoChan(id.probeKey(), func() (any, error) {
			if cached, ok := claudeCapabilityCache.Load(id); ok {
				return cached.(claudeCapabilities), nil
			}
			caps, err := probeClaudeCapabilities(ctx, path, id)
			if err != nil && ctx.Err() != nil {

				return claudeCapabilities{}, probeContextError{err}
			}
			return caps, err
		})

		select {
		case <-ctx.Done():

			return claudeCapabilities{}, fmt.Errorf("claude capability probe: %w", ctx.Err())
		case res := <-flight:

			if ctx.Err() != nil {
				return claudeCapabilities{}, fmt.Errorf("claude capability probe: %w", ctx.Err())
			}
			if res.Err == nil {
				return res.Val.(claudeCapabilities), nil
			}
			if isSharedProbeCancellation(res.Err, ctx) {

				continue
			}
			return claudeCapabilities{}, res.Err
		}
	}
}

func probeClaudeCapabilities(ctx context.Context, path string, id claudeBinaryID) (claudeCapabilities, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	probe, release, err := harnessCommand(probeCtx, path, []string{"--help"}, os.Environ(), harnessClaude)
	if err != nil {
		return claudeCapabilities{}, fmt.Errorf("claude capability probe: %w", err)
	}
	defer release()
	out, err := runHarnessCommandOutput(probe)
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

func (c *CLIClient) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	text, err := c.run(ctx, prompt)
	return text, TokenUsage{}, err
}

func (c *CLIClient) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	return c.run(ctx, userContent, "--system-prompt", systemPrompt)
}

func (c *CLIClient) run(ctx context.Context, prompt string, extraArgs ...string) (_ string, err error) {
	ctx, cancel := boundHarnessCall(ctx)
	defer cancel()
	defer func() { err = explainDefaultBound(ctx, err) }()
	caps, err := claudeCapabilitiesFor(ctx, c.binary)
	if err != nil {
		return "", err
	}
	args, err := claudeInvocationArgs(caps)
	if err != nil {
		return "", err
	}
	args = append(args, extraArgs...)

	cmd, release, err := harnessCommand(ctx, c.binary, args, os.Environ(), harnessClaude)
	if err != nil {
		return "", fmt.Errorf("claude -p: %w", err)
	}
	defer release()
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := runHarnessCommand(cmd); err != nil {
		return "", fmt.Errorf("claude -p: %w: %s", err, harnessFailureOutput(stdout.String(), stderr.String()))
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
