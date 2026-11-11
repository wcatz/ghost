package ai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
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

func (c *GooseClient) run(ctx context.Context, prompt string) (_ string, err error) {
	ctx, cancel := boundHarnessCall(ctx)
	defer cancel()
	defer func() { err = explainDefaultBound(ctx, err) }()
	cmd, release, err := c.subprocessEnv(ctx, gooseInvocationArgs())
	if err != nil {
		return "", err
	}
	defer release()
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := runHarnessCommand(cmd); err != nil {
		return "", fmt.Errorf("goose run: %w: %s", err, harnessFailureOutput(stdout.String(), stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func gooseInvocationArgs() []string {
	return []string{"run", "-q", "--no-profile", "--no-session", "-i", "-"}
}

func (c *GooseClient) subprocessEnv(ctx context.Context, args []string) (*exec.Cmd, func(), error) {
	cmd, release, err := harnessCommand(ctx, c.binary, args, os.Environ(), harnessGoose)
	if err != nil {
		return nil, nil, err
	}
	if err := configureGooseIsolation(cmd); err != nil {
		release()
		return nil, nil, err
	}
	return cmd, release, nil
}

const gooseNoToolsMode = "chat"

func configureGooseIsolation(cmd *exec.Cmd) error {
	if cmd.Dir == "" {
		return fmt.Errorf("goose scratch directory is empty")
	}
	env := cmd.Env

	homeDir := gooseHomeDir(env)
	if homeDir == "" {
		return fmt.Errorf("goose child has no home (HOME or USERPROFILE); refusing to run with plugin discovery unconfined")
	}

	home := filepath.Join(cmd.Dir, "goose-home")

	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("goose isolated home %s: %w", home, err)
	}
	if err := linkGooseConfigDirs(home, env, homeDir); err != nil {
		return err
	}

	env = setHarnessEnvValue(env, "HOME", home)
	env = setHarnessEnvValue(env, "USERPROFILE", home)
	env = setHarnessEnvValue(env, "HOMEDRIVE", "")
	env = setHarnessEnvValue(env, "HOMEPATH", "")

	env = setHarnessEnvValue(env, "GOOSE_MODE", gooseNoToolsMode)
	confined, err := confineGoosePathRoot(home, env)
	if err != nil {
		return err
	}
	cmd.Env = confined
	return nil
}

func confineGoosePathRoot(home string, env []string) ([]string, error) {
	root := harnessEnvValue(env, "GOOSE_PATH_ROOT")
	if root == "" || !filepath.IsAbs(root) {
		return env, nil
	}
	isolated := filepath.Join(home, "goose-root")
	if err := os.MkdirAll(isolated, 0o700); err != nil {
		return env, fmt.Errorf("goose isolated path root %s: %w", isolated, err)
	}

	source := filepath.Join(root, "config")
	if info, err := os.Stat(source); err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return env, fmt.Errorf("goose isolated path root: cannot read %s: %w", source, err)
		}
		slog.Info("goose path root has no config directory; the child will run without one",
			"config", source)
	} else if !info.IsDir() {
		return env, fmt.Errorf("goose isolated path root: %s is not a directory", source)
	} else if err := carryGooseConfigDir(source, filepath.Join(isolated, "config")); err != nil {
		return env, err
	}
	return setHarnessEnvValue(env, "GOOSE_PATH_ROOT", isolated), nil
}

func gooseHomeDir(env []string) string {
	for _, key := range []string{"HOME", "USERPROFILE"} {
		if value := harnessEnvValue(env, key); value != "" {
			return value
		}
	}
	return ""
}

var gooseHomeConfigRelPaths = [][]string{
	{".config", "goose"},
	{"Library", "Application Support", "goose"},
}

func linkGooseConfigDirs(home string, env []string, homeDir string) error {
	return linkGooseConfigDirsWith(home, env, homeDir, os.Lstat, os.Stat)
}

var errGooseConfigBrokenLink = errors.New("config root is a symlink that does not resolve to a directory")

func firstExistingAncestor(path, stop string, probe func(string) (os.FileInfo, error)) (string, os.FileInfo, error) {

	stop = filepath.Clean(stop)
	current := filepath.Clean(path)
	for {
		info, err := probe(current)
		switch {
		case err == nil:
			return current, info, nil
		case !errors.Is(err, fs.ErrNotExist):
			return current, nil, err
		}
		if current == stop {
			return current, nil, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return current, nil, nil
		}
		current = parent
	}
}

func linkGooseConfigDirsWith(home string, env []string, homeDir string, leafProbe, walkProbe func(string) (os.FileInfo, error)) error {
	if harnessEnvValue(env, "XDG_CONFIG_HOME") != "" {

		return nil
	}
	for _, rel := range gooseHomeConfigRelPaths {
		source := filepath.Join(append([]string{homeDir}, rel...)...)
		target := filepath.Join(append([]string{home}, rel...)...)

		if info, err := leafProbe(source); err != nil {

			ancestor, info, ancestorErr := firstExistingAncestor(source, homeDir, walkProbe)
			switch {
			case ancestorErr != nil:

				return fmt.Errorf("goose isolated config %s: cannot reach %s: %w", source, ancestor, ancestorErr)
			case info != nil && !info.IsDir():
				return fmt.Errorf("goose isolated config %s: %s is not a directory", source, ancestor)
			}
			continue
		} else if info.Mode()&os.ModeSymlink != 0 {

			followed, followErr := walkProbe(source)

			linkTarget := ""
			if t, lerr := os.Readlink(source); lerr == nil {
				linkTarget = t
			}
			switch {
			case followErr != nil:
				if linkTarget != "" {
					return fmt.Errorf("goose isolated config %s: %w (it points at %s): %v", source, errGooseConfigBrokenLink, linkTarget, followErr)
				}
				return fmt.Errorf("goose isolated config %s: %w: %v", source, errGooseConfigBrokenLink, followErr)
			case !followed.IsDir():
				if linkTarget != "" {
					return fmt.Errorf("goose isolated config %s: %w (it points at %s)", source, errGooseConfigBrokenLink, linkTarget)
				}
				return fmt.Errorf("goose isolated config %s: %w", source, errGooseConfigBrokenLink)
			}
		} else if !info.IsDir() {

			return fmt.Errorf("goose isolated config %s: %s is not a directory", source, source)
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("goose isolated config %s: %w", filepath.Dir(target), err)
		}
		if err := carryGooseConfigDir(source, target); err != nil {
			return err
		}
	}
	return nil
}

func carryGooseConfigDir(source, target string) error {
	return carryGooseConfigDirWith(source, target, os.Symlink)
}

func carryGooseConfigDirWith(source, target string, linkDir func(string, string) error) error {
	err := linkDir(source, target)
	if err == nil || os.IsExist(err) {
		return nil
	}
	skipped, copyErr := copyGooseConfigDir(source, target)
	if copyErr != nil {
		return copyErr
	}
	if len(skipped) > 0 {

		slog.Warn("goose config copied into the isolated home is incomplete",
			"config", target, "skipped", strings.Join(skipped, ","))
	}
	return nil
}

func copyGooseConfigDir(source, target string) ([]string, error) {
	entries, err := os.ReadDir(source)
	if err != nil {
		return nil, fmt.Errorf("goose isolated config %s: %w", target, err)
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return nil, fmt.Errorf("goose isolated config %s: %w", target, err)
	}
	var skipped []string
	for _, entry := range entries {
		from := filepath.Join(source, entry.Name())
		to := filepath.Join(target, entry.Name())
		info, err := os.Stat(from)
		if err != nil || !info.Mode().IsRegular() {
			skipped = append(skipped, entry.Name())
			continue
		}
		data, err := os.ReadFile(from)
		if err != nil {
			return skipped, fmt.Errorf("goose isolated config %s: %w", to, err)
		}
		if err := os.WriteFile(to, data, 0o600); err != nil {
			return skipped, fmt.Errorf("goose isolated config %s: %w", to, err)
		}
	}
	return skipped, nil
}
