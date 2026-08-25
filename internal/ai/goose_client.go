package ai

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type GooseClient struct {
	binary string
}

func NewGooseClient() *GooseClient {
	return NewGooseClientWithBinary("goose")
}

func NewGooseClientWithBinary(binary string) *GooseClient {
	return &GooseClient{binary: binary}
}

func (c *GooseClient) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	text, err := c.run(ctx, prompt)
	return text, TokenUsage{}, err
}

func (c *GooseClient) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	return c.run(ctx, systemPrompt+"\n\n"+userContent)
}

func (c *GooseClient) run(ctx context.Context, prompt string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	args := []string{"run", "-q"}
	args = append(args, prompt)
	cmd := exec.CommandContext(ctx, c.binary, args...)
	cmd.Env = stripLLMKeys(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("goose run: %w: %s", err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}
