package ai

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
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
	args := []string{"exec", "--sandbox", "read-only"}
	args = append(args, prompt)
	cmd := exec.CommandContext(ctx, c.binary, args...)
	cmd.Env = stripLLMKeys(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("codex exec: %w: %s", err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}
