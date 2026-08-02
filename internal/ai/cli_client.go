package ai

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
)

// CLIClient drives Claude via the `claude` CLI (a `claude -p` subprocess)
// instead of the direct Anthropic HTTP API, so it bills to the caller's
// Claude Code subscription rather than API credits. It implements the same
// Reflect/Classify shapes as Client (reflectClient / Provider), so it can
// substitute for the direct API client anywhere ANTHROPIC_API_KEY would
// otherwise be required.
//
// ANTHROPIC_API_KEY is stripped from the subprocess environment: if present,
// it would override subscription/OAuth login and bill the call as
// pay-per-token API usage instead, defeating the point of this client.
// --setting-sources project,local excludes the user settings source so the
// real, unwrapped SessionStart hook in ~/.claude/settings.json never fires —
// without that, a headless ghost process shelling out to `claude -p` could
// trigger `ghost hook session-start`, which opens the same SQLite DB this
// process already holds open.
type CLIClient struct {
	binary string
}

// NewCLIClient creates a CLIClient that invokes the `claude` binary on PATH.
func NewCLIClient() *CLIClient {
	return &CLIClient{binary: "claude"}
}

// Reflect satisfies reflection's reflector interface (see
// internal/reflection/tier_haiku.go). TokenUsage is always zero: subscription
// calls have no per-token API cost to record.
func (c *CLIClient) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	text, err := c.run(ctx, prompt)
	return text, TokenUsage{}, err
}

// Classify satisfies the Provider interface (see internal/ai/provider.go).
func (c *CLIClient) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	return c.run(ctx, systemPrompt+"\n\n"+userContent)
}

func (c *CLIClient) run(ctx context.Context, prompt string) (string, error) {
	cmd := exec.CommandContext(ctx, c.binary, "-p", "--setting-sources", "project,local", prompt)
	cmd.Env = stripAPIKey(os.Environ())
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
		if len(kv) >= 18 && kv[:18] == "ANTHROPIC_API_KEY=" {
			continue
		}
		out = append(out, kv)
	}
	return out
}
