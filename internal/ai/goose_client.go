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
// The isolated home is a real directory that carries the configuration with
// it. goose's config root differs by platform and by build — $XDG_CONFIG_HOME,
// ~/.config/goose, ~/Library/Application Support/goose on macOS, %APPDATA% on
// Windows — and this code cannot know which one a given goose and user pair
// uses. Inventing a value instead, synthesizing $HOME/.config and exporting it
// as XDG_CONFIG_HOME, is only correct on the platform whose convention it
// guesses; on the others it redirects a working authenticated call to a config
// root that user's goose was never configured from. So the home-relative roots
// are reproduced instead — see gooseHomeConfigRelPaths — and the child resolves
// whichever one it would have resolved before. Nothing else moves, because
// .agents/ is the only other thing under HOME that goose reads for this
// purpose.
//
// The links go at the config paths themselves, not at $HOME: goose spells the
// fallback as $HOME/.config/goose, so a home that IS the config directory would
// make that resolve to <config>/.config/goose and miss.
//
// %APPDATA% is deliberately not reproduced. It is an absolute path the
// allowlist already passes through, so a Windows child keeps reaching its
// config however HOME moves — and the Windows home variables are cleared rather
// than repointed, because HOMEDRIVE is a drive letter that cannot be joined onto
// a scratch path. USERPROFILE is the variable that identifies the home there,
// and it is set, so clearing the pair removes the alternate route back to the
// real one.
func configureGooseIsolation(cmd *exec.Cmd) error {
	if cmd.Dir == "" {
		return fmt.Errorf("goose scratch directory is empty")
	}
	env := cmd.Env

	// Resolved before HOME is replaced, so the links are created where the
	// child will look for them.
	homeDir := gooseHomeDir(env)
	if homeDir == "" {
		return fmt.Errorf("goose child has no home (HOME or USERPROFILE); refusing to run with plugin discovery unconfined")
	}

	home := filepath.Join(cmd.Dir, "goose-home")
	// Created unconditionally, including the XDG branch below, which adds no
	// link: HOME is handed to the child either way, and a name that does not
	// resolve is a home the child cannot use.
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
	cmd.Env = env
	return nil
}

// gooseHomeDir returns the real home the child is being moved away from, or ""
// when the parent named neither a home nor a config root. The latter case is
// refused rather than isolated: with no home there is no plugin root to move
// away from, but there is also no way to prove the child cannot find one.
func gooseHomeDir(env []string) string {
	for _, key := range []string{"HOME", "USERPROFILE"} {
		if value := harnessEnvValue(env, key); value != "" {
			return value
		}
	}
	return ""
}

// gooseHomeConfigRelPaths lists the home-relative locations goose may resolve
// its configuration from, and therefore the ones the isolated home has to
// carry. Both are real: goose documents ~/.config/goose for Linux and macOS
// and ~/Library/Application Support for macOS, and which one a given build and
// user uses is not something this code can determine. Carrying only the first
// silently moves the config out from under a macOS child that never set
// XDG_CONFIG_HOME, which is the default there.
//
// %APPDATA% on Windows is absent deliberately: it is an absolute path the
// allowlist already passes through, so the child keeps reaching its config
// however HOME moves and nothing has to be reproduced.
var gooseHomeConfigRelPaths = [][]string{
	{".config", "goose"},
	{"Library", "Application Support", "goose"},
}

// linkGooseConfigDirs carries every home-relative config root into the
// isolated home, so a child resolves whichever one its own goose uses.
//
// A symlink is not assumed: on Windows it needs SeCreateSymbolicLinkPrivilege
// or Developer Mode, and this path is the NORMAL one there because macOS-style
// XDG_CONFIG_HOME is rarely set on a Windows host. Failing every goose call on
// a privileged filesystem operation would be a regression on a supported
// platform, so where a symlink is refused the directory is copied instead.
// The copy is made private to the child and dies with the scratch dir.
func linkGooseConfigDirs(home string, env []string, homeDir string) error {
	return linkGooseConfigDirsWith(home, env, homeDir, os.Lstat, os.Stat)
}

// linkGooseConfigDirsWith takes the probe as a parameter so a test can state a
// classification instead of arranging one. Arranging the interesting case is
// not possible everywhere: Windows reports "a file where a directory belongs"
// as ERROR_PATH_NOT_FOUND, and syscall maps ENOTDIR to that same constant, so
// on that host a source-level check cannot tell that case from a genuine miss.
//
// firstExistingAncestor is what makes the rule hold anyway. It walks up from
// path itself to stop (inclusive) and returns the first path that stats, with
// its info. A non-nil info whose IsDir is false is the Windows-shaped fault; a
// nil info with a nil error means nothing up to and including the home exists,
// so the leaf is merely unpopulated.
//
// The walk starts at path rather than its parent so the contract is true as
// written; a caller that has already probed path pays for one redundant stat.
//
// A probe failure that is not "not there" is returned rather than treated as
// absence, so an untraversable home or a TCC-denied ~/Library is reported
// instead of silently dropping the configuration.
//
// The probe is expected to RESOLVE symlinks (os.Stat), because the walk's only
// question is "is there a directory here", and a symlink to one is that. A
// symlink is not the fault this walk exists to find, and treating it as one
// would refuse a home that is entirely symlinked.
func firstExistingAncestor(path, stop string, probe func(string) (os.FileInfo, error)) (string, os.FileInfo, error) {
	// filepath.Dir cleans, so the walk's values are always clean; stop comes
	// from the environment and may not be (HOME=/home/user/ is a real thing
	// launchers and systemd units set), and an unmatched bound would let the
	// walk climb above the home and blame an unused location on a file up
	// there.
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
		if parent == current { // reached the filesystem root without a match
			return current, nil, nil
		}
		current = parent
	}
}

// linkGooseConfigDirsWith takes two probes because the leaf and the walk need
// different ones, and that difference is the point rather than an accident. The
// leaf is probed WITHOUT following symlinks, so a symlinked config directory is
// carried as itself instead of being followed somewhere else. The walk resolves
// them, so a symlinked home or ~/.config is seen as the directory it is.
func linkGooseConfigDirsWith(home string, env []string, homeDir string, leafProbe, walkProbe func(string) (os.FileInfo, error)) error {
	if harnessEnvValue(env, "XDG_CONFIG_HOME") != "" {
		// An absolute path the child reads directly; HOME plays no part.
		return nil
	}
	for _, rel := range gooseHomeConfigRelPaths {
		source := filepath.Join(append([]string{homeDir}, rel...)...)
		target := filepath.Join(append([]string{home}, rel...)...)
		// Only "not there" means "not this platform's location". Any other
		// failure — an untraversable home, macOS TCC on ~/Library, a
		// file where a directory belongs — means the config is there and
		// unreadable, and skipping it would hand the child an empty profile
		// with nothing saying Ghost dropped it.
		if _, err := leafProbe(source); err != nil {
			// Whether this is "not this platform's location" or a real fault
			// cannot be read from the source's own errno, because on Windows
			// "a file where a directory belongs" is ERROR_PATH_NOT_FOUND and
			// maps to ErrNotExist exactly like a genuine miss. The same
			// conflation applies to every path THROUGH that file, so walking up
			// one level does not clear it either: with ~/Library a file,
			// ~/Library/Application Support reports not-found too.
			//
			// What does clear it is walking up to the first ancestor that
			// actually exists and asking whether it is a directory. A location
			// the platform uses has a real directory somewhere above it, and a
			// missing leaf under one is ordinary — a user who has never run
			// `goose configure` has no config dir either, and that must not fail
			// the call, because their configuration comes from the environment.
			// A file instead of a directory is the fault, and no errno at any
			// depth can report it differently from an absent path.
			//
			// The walk resolves symlinks, so a symlinked $HOME or a ~/.config
			// pointing into a dotfiles repo is seen as the directory it is.
			// Treating a symlink as the fault instead would fail every goose
			// call on those machines — and name a perfectly good directory as
			// broken. The leaf probe above stays Lstat, so a symlinked config
			// DIRECTORY is still carried rather than followed.
			ancestor, info, ancestorErr := firstExistingAncestor(source, homeDir, walkProbe)
			switch {
			case ancestorErr != nil:
				// Report the walk's failure, not the leaf's: the leaf said
				// "not there" and this is the fault found above it, and a
				// message carrying the leaf's ErrNotExist would tell the
				// reader the location is merely unpopulated — the one
				// misreading this whole branch exists to prevent.
				return fmt.Errorf("goose isolated config %s: cannot reach %s: %w", source, ancestor, ancestorErr)
			case info != nil && !info.IsDir():
				return fmt.Errorf("goose isolated config %s: %s is not a directory", source, ancestor)
			}
			continue
		}
		// After the probe, so the isolated home does not gain a
		// Library/Application Support directory on the platforms that cannot
		// have one there.
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fmt.Errorf("goose isolated config %s: %w", filepath.Dir(target), err)
		}
		if err := carryGooseConfigDir(source, target); err != nil {
			return err
		}
	}
	return nil
}

// carryGooseConfigDir makes target resolve to source, by symlink where the
// platform allows it and by copy where it does not. linkDir is a parameter so
// the fallback is reachable from a test: a Windows host without Developer Mode
// cannot create a symlink, and that host is precisely where the copy has to
// work, so the branch cannot be left to be discovered in production.
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
		// The copy is still a working config directory for everything it did
		// carry, so this is a warning rather than a refusal — but it is named,
		// because a child missing the file it needs fails as a provider error
		// with nothing else pointing here.
		slog.Warn("goose config copied into the isolated home is incomplete",
			"config", target, "skipped", strings.Join(skipped, ","))
	}
	return nil
}

// copyGooseConfigDir is the unprivileged fallback for a symlink the platform
// refuses. It copies the directory's regular files, one level deep and without
// recursing, so a config directory cannot pull a tree into the child.
//
// A symlinked file IS carried: the dirent type of a symlink is not regular, but
// a dotfiles-managed config.yaml or secrets.yaml symlinked into a repo is the
// common way these files are kept, and skipping it would leave the child with a
// config directory that reports success and contains nothing goose can
// authenticate with. The link is resolved with os.Stat so the target's mode
// decides, and a link that dangles or points at a directory is skipped and
// counted rather than followed.
//
// Anything skipped leaves the carried config incomplete, so the names are
// returned for the caller to report. Silently returning a partial directory is
// the one outcome worse than failing: the child would authenticate against
// whatever defaults it has and the user would see a provider error with nothing
// pointing at Ghost.
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
		info, err := os.Stat(from) // resolves a symlink; the target's mode decides
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
