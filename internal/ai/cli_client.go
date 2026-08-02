package ai

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// defaultTimeout bounds a claude -p subprocess call when the caller's context
// carries no earlier deadline — otherwise a stalled subprocess (auth prompt,
// network stall, hung MCP init inside it) blocks the classify/reflect loop
// indefinitely, since resolve/supersede classify candidates one at a time.
const defaultTimeout = 5 * time.Minute

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
	args := append([]string{"-p", "--setting-sources", "project,local"}, extraArgs...)
	args = append(args, prompt)
	cmd := exec.CommandContext(ctx, c.binary, args...)
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
		key, _, found := strings.Cut(kv, "=")
		if found && strings.EqualFold(key, "ANTHROPIC_API_KEY") {
			continue
		}
		out = append(out, kv)
	}
	return out
}
