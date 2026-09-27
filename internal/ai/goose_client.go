package ai

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
	// --no-profile prevents the configured developer/MCP extensions from
	// loading; --no-session keeps the untrusted prompt out of Goose's state.
	args := []string{"run", "-q", "--no-profile", "--no-session"}
	// The prompt goes on stdin, never as an argv element (issue #560): the
	// kernel caps one argument at 32 pages and a reflect prompt is built from
	// up to 2000 memories of 8000 bytes, so a large project produced a prompt
	// that failed the spawn with E2BIG. `-i -` is goose's documented
	// instructions-from-stdin form, and it lands in the same place a text
	// argument does — both become the run's input contents.
	args = append(args, "-i", "-")
	cmd, release, _ := harnessCommand(ctx, c.binary, args, os.Environ(), harnessGoose)
	defer release()
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("goose run: %w: %s", err, harnessFailureOutput(stdout.String(), stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
