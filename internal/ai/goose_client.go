package ai

import (
	"bytes"
	"context"
	"fmt"
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
	cmd, release, err := c.subprocessEnv(ctx, args)
	if err != nil {
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

// subprocessEnv builds the goose child command and confines it, returning the
// command and a release that is always safe to call. It routes through
// harnessCommand, so the environment allowlist, the working directory and the
// temp variables follow the same policy as the other three clients, and then
// applies the plugin isolation below.
//
// When the scratch root is unusable harnessCommand leaves the working directory
// empty, and the isolation needs a directory to put the child's home in. The
// fallback is a private MkdirTemp tree, mirroring
// OpenCodeClient.subprocessEnv: a broken data dir must not quietly restore
// Ghost's own plugin inside every harness call, so the child either runs
// isolated or the call fails.
func (c *GooseClient) subprocessEnv(ctx context.Context, args []string) (*exec.Cmd, func(), error) {
	cmd, release, ok := harnessCommand(ctx, c.binary, args, os.Environ(), harnessGoose)
	if ok {
		if err := configureGooseIsolation(cmd); err != nil {
			release()
			return nil, nil, err
		}
		return cmd, release, nil
	}

	dir, err := os.MkdirTemp("", "ghost-goose-")
	if err != nil {
		release()
		return nil, nil, err
	}
	cmd.Dir = dir
	cmd.Env = scratchEnv(cmd.Env, dir)
	if err := configureGooseIsolation(cmd); err != nil {
		_ = os.RemoveAll(dir)
		release()
		return nil, nil, err
	}
	return cmd, func() { _ = os.RemoveAll(dir) }, nil
}

// configureGooseIsolation gives the goose child a home that cannot contain a
// discoverable plugin package, and keeps it pointed at the configuration it
// authenticates from.
//
// goose discovers user-scope Agent Plugins under $HOME/.agents/plugins/, and
// that path is home-relative BY SPECIFICATION rather than XDG-relative, so
// overriding XDG_CONFIG_HOME does not move it. `ghost mcp init --client goose`
// installs a package there whose mcp.json registers the Ghost stdio MCP server
// and whose hooks/hooks.json run `ghost hook <event> --source goose` through a
// shell. The child needs a home to read its configuration and authenticate, so
// on its own it also found that package and started Ghost's own server from
// inside a reflect/resolve/supersede call — the recursion this closes. The
// prompt those calls carry is memory text, which routinely contains
// third-party content, so a second Ghost process on the other end of it is not
// a formality. --no-profile does not cover this: it governs the configured
// profile, not plugin discovery, and goose documents no variable that disables
// discovery.
//
// The isolated home is a real directory holding ONE symlink, to the config
// directory, at the path goose resolves it from. goose's config root differs by
// platform and by build — $XDG_CONFIG_HOME/goose, $HOME/.config/goose,
// ~/Library/Application Support on macOS, %APPDATA% on Windows — and this code
// cannot know which one a given goose and user pair uses. Inventing a value
// instead, synthesizing $HOME/.config and exporting it as XDG_CONFIG_HOME, is
// only correct on the platform whose convention it guesses; on the others it
// redirects a working authenticated call to a config root that user's goose was
// never configured from. Linking the directory where the parent already had it
// leaves the child's own resolution untouched — wherever it looked before, it
// finds the same bytes — and moves nothing else, because .agents/ is the only
// other thing under HOME that goose reads for this purpose.
//
// The link goes at $HOME/.config, not at $HOME itself: goose's fallback spells
// the path as $HOME/.config/goose, so a home that IS the config directory would
// make that resolve to <config>/.config/goose and miss.
//
// The Windows home variables are cleared rather than repointed, because
// HOMEDRIVE is a drive letter and cannot be joined onto a scratch path.
// USERPROFILE is the variable that identifies the home there, and it is set,
// so clearing the pair removes the alternate route back to the real one.
func configureGooseIsolation(cmd *exec.Cmd) error {
	if cmd.Dir == "" {
		return fmt.Errorf("goose scratch directory is empty")
	}
	env := cmd.Env

	// Resolved before HOME is replaced, so the link is created where the child
	// will look for it.
	configDir := gooseConfigDir(env)
	if configDir == "" {
		return fmt.Errorf("goose child has no home or config root; refusing to run with plugin discovery unconfined")
	}

	home := filepath.Join(cmd.Dir, "goose-home")
	// Created unconditionally, including the XDG branch below, which adds no
	// link: HOME is handed to the child either way, and a name that does not
	// resolve is a home the child cannot use.
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("goose isolated home %s: %w", home, err)
	}
	if err := linkGooseConfig(home, env, configDir); err != nil {
		return err
	}

	env = setHarnessEnvValue(env, "HOME", home)
	env = setHarnessEnvValue(env, "USERPROFILE", home)
	env = setHarnessEnvValue(env, "HOMEDRIVE", "")
	env = setHarnessEnvValue(env, "HOMEPATH", "")
	cmd.Env = env
	return nil
}

// linkGooseConfig makes the isolated home resolve to configDir by the same rule
// the parent used: through XDG_CONFIG_HOME when it is set, and through the
// home-relative .config/goose when it is not. It never invents a variable —
// the whole point is that the child's own, possibly platform-specific,
// resolution still lands in the right place.
func linkGooseConfig(home string, env []string, configDir string) error {
	if configHome := harnessEnvValue(env, "XDG_CONFIG_HOME"); configHome != "" {
		// Already an absolute path the child reads directly; HOME plays no part.
		return nil
	}
	configHome := filepath.Join(home, ".config")
	if err := os.MkdirAll(configHome, 0o700); err != nil {
		return fmt.Errorf("goose isolated config home %s: %w", configHome, err)
	}
	link := filepath.Join(configHome, "goose")
	if err := os.Symlink(configDir, link); err != nil && !os.IsExist(err) {
		return fmt.Errorf("goose isolated config %s: %w", link, err)
	}
	return nil
}

// gooseConfigDir returns the directory holding the user's goose configuration
// — the one thing the isolated home must still reach — or "" when the parent
// named neither a config root nor a home. XDG_CONFIG_HOME wins when present,
// because that is the variable goose consults first; otherwise the
// home-relative .config is used, which is the documented Linux and macOS
// location. The Windows root (%APPDATA%) is deliberately not restated here:
// the child inherits APPDATA through the allowlist and resolves it itself.
func gooseConfigDir(env []string) string {
	if configHome := harnessEnvValue(env, "XDG_CONFIG_HOME"); configHome != "" {
		return filepath.Join(configHome, "goose")
	}
	for _, homeKey := range []string{"HOME", "USERPROFILE"} {
		if home := harnessEnvValue(env, homeKey); home != "" {
			return filepath.Join(home, ".config", "goose")
		}
	}
	return ""
}
