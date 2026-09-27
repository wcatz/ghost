package ai

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
	if err := configureGooseIsolation(cmd); err != nil {
		release()
		return "", err
	}
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

// configureGooseIsolation gives the goose child a home that cannot contain a
// discoverable plugin package, while leaving the config it authenticates from
// where goose looks for it.
//
// goose discovers user-scope Agent Plugins under $HOME/.agents/plugins/, and
// that path is home-relative BY SPECIFICATION rather than XDG-relative, so
// overriding XDG_CONFIG_HOME does not move it. `ghost mcp init --client goose`
// installs a package there whose mcp.json registers the Ghost stdio MCP server
// and whose hooks/hooks.json run `ghost hook <event> --source goose` through a
// shell. The child needs the real HOME to read ~/.config/goose and authenticate,
// so on its own it also finds that package and starts Ghost's own server from
// inside a reflect/resolve/supersede call — the recursion this closes. The
// prompt those calls carry is memory text, which routinely contains third-party
// content, so a second Ghost process on the other end of it is not a
// formality. --no-profile does not cover it: that governs the configured
// profile, not plugin discovery, and goose documents no variable that disables
// discovery.
//
// The child still reaches its configuration, which is what HOME is kept for:
// goose reads config.yaml, secrets.yaml and settings.json from
// $XDG_CONFIG_HOME/goose when it is set and from $HOME/.config/goose otherwise.
// Pinning XDG_CONFIG_HOME to the resolved parent value first means the
// home-relative fallback resolves to the same directory it did before, so
// authentication is unchanged and only the plugin root moves.
//
// This mirrors configureOpenCodeIsolation, which exists for the same reason on
// the opencode side: isolating the config tree is deliberately stronger than
// trusting one variable, because harness versions differ in which root they
// consult.
func configureGooseIsolation(cmd *exec.Cmd) error {
	env := cmd.Env
	configHome := harnessEnvValue(env, "XDG_CONFIG_HOME")
	if configHome == "" {
		for _, homeKey := range []string{"HOME", "USERPROFILE"} {
			if home := harnessEnvValue(env, homeKey); home != "" {
				configHome = filepath.Join(home, ".config")
				break
			}
		}
	}
	if configHome == "" {
		// No home to relocate the plugin root out of, and none to lose: the
		// child can reach nothing under it that this process did not already
		// hand over. Say so rather than failing a call that can still run.
		slog.Warn("goose child has no home directory; plugin discovery is not confined",
			"harness", harnessGoose)
		return nil
	}

	env = setHarnessEnvValue(env, "XDG_CONFIG_HOME", configHome)
	if cmd.Dir != "" {
		// The scratch directory, which harnessCommand already confines the
		// working directory to, dies with the invocation. An empty one means
		// the scratch root is unusable — harnessCommand has already warned, and
		// leaving HOME alone is the safe half of the degradation.
		home := filepath.Join(cmd.Dir, "goose-home")
		if err := os.MkdirAll(home, 0o700); err != nil {
			return fmt.Errorf("goose isolated home %s: %w", home, err)
		}
		env = setHarnessEnvValue(env, "HOME", home)
		env = setHarnessEnvValue(env, "USERPROFILE", home)
	}
	cmd.Env = env
	return nil
}
