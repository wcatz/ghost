package ai

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
)

type CodexClient struct {
	binary string
}

func NewCodexClient() *CodexClient {
	return NewCodexClientWithBinary("codex")
}

func NewCodexClientWithBinary(binary string) *CodexClient {
	return &CodexClient{binary: binary}
}

func (c *CodexClient) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	text, err := c.run(ctx, prompt)
	return text, TokenUsage{}, err
}

func (c *CodexClient) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	return c.run(ctx, systemPrompt+"\n\n"+userContent)
}

func (c *CodexClient) run(ctx context.Context, prompt string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	// Ignore user config/rules so a project or user MCP/plugin definition cannot
	// enter this untrusted prompt. The feature overrides remove the remaining
	// side-effecting surfaces; read-only remains a second boundary for any
	// model-provided file operation.
	args := []string{
		"exec",
		"--sandbox", "read-only",
		"--ignore-user-config",
		"--ignore-rules",
		"--skip-git-repo-check",
		"--ephemeral",
		"-c", "features.shell_tool=false",
		"-c", "features.unified_exec=false",
		"-c", "features.apps=false",
		"-c", "features.remote_plugin=false",
		"-c", "features.hooks=false",
		"-c", "features.multi_agent=false",
		"-c", "agents.enabled=false",
		"-c", `web_search="disabled"`,
	}
	args = append(args, prompt)
	cmd, release, _ := harnessCommand(ctx, c.binary, args, os.Environ(), harnessCodex)
	defer release()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("codex exec: %w: %s", err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}
