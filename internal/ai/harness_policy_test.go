package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func fakeHarnessPolicyBinary(t *testing.T, name, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script policy fake requires a POSIX shell")
	}
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -e\n"+script), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
	return path
}

func setHarnessPolicyParentEnv(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"AWS_SECRET_ACCESS_KEY": "aws-secret",
		"GITHUB_TOKEN":          "github-secret",
		"GHOST_API_KEY":         "ghost-secret",
		"GHOST_DATABASE_URL":    "postgres://secret",
		"CLAUDE_CONFIG_DIR":     "/decoy/claude",
		"CODEX_HOME":            "/decoy/codex",
		"GOOSE_PATH_ROOT":       "/decoy/goose",
		"OPENCODE_API_KEY":      "opencode-secret",
		"HOME":                  "/decoy/home",
		"USERPROFILE":           "/decoy/home",
		"XDG_CONFIG_HOME":       "/decoy/config",
		"GHOST_PASSTHROUGH_ENV": "",
	} {
		t.Setenv(key, value)
	}
}

func TestConfigureOpenCodeIsolationWritesAskConfig(t *testing.T) {
	dir := t.TempDir()
	cmd := &exec.Cmd{
		Dir: dir,
		Env: []string{
			"HOME=/user/home",
			"USERPROFILE=/user/home",
			"XDG_CONFIG_HOME=/user/config",
			"OPENCODE_API_KEY=opencode-key",
		},
	}
	if err := configureOpenCodeIsolation(cmd, openCodeAskConfig); err != nil {
		t.Fatalf("configureOpenCodeIsolation: %v", err)
	}
	if got := envValue(cmd.Env, "HOME"); got != filepath.Join(dir, "home") {
		t.Errorf("HOME = %q, want isolated home", got)
	}
	if got := envValue(cmd.Env, "USERPROFILE"); got != filepath.Join(dir, "home") {
		t.Errorf("USERPROFILE = %q, want isolated home", got)
	}
	if got := envValue(cmd.Env, "OPENCODE_API_KEY"); got != "opencode-key" {
		t.Errorf("OPENCODE_API_KEY = %q, want preserved auth", got)
	}
	configPath := envValue(cmd.Env, "OPENCODE_CONFIG")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read isolated config: %v", err)
	}
	var config struct {
		Permission map[string]string `json:"permission"`
		Tools      map[string]bool   `json:"tools"`
		MCP        map[string]any    `json:"mcp"`
		Plugin     []string          `json:"plugin"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("isolated config is not JSON: %v", err)
	}
	// Every tool is "ask": a non-interactive `opencode run` declines each call.
	// A deny rule or a disabled tool strips tools from the request, and
	// OpenCode's free tier answers such a request with 403 provider.auth.
	if len(config.Permission) != 1 || config.Permission["*"] != "ask" {
		t.Errorf("permission rules = %v, want only {\"*\": \"ask\"}", config.Permission)
	}
	if len(config.Tools) != 0 {
		t.Errorf("tool map = %v, want none (a disabled tool trips the free-tier check)", config.Tools)
	}
	if len(config.MCP) != 0 || len(config.Plugin) != 0 {
		t.Errorf("MCP/plugins survived isolation: mcp=%v plugin=%v", config.MCP, config.Plugin)
	}
}

func TestClaudeInvocationArgsRequiresNoToolsCapabilities(t *testing.T) {
	_, err := claudeInvocationArgs(claudeCapabilities{restricted: true, strictMCP: true, tools: true, disallowedTools: true})
	if err == nil || !strings.Contains(err.Error(), "upgrade claude") {
		t.Fatalf("missing capability error = %v, want explicit upgrade guidance", err)
	}

	args, err := claudeInvocationArgs(claudeCapabilities{
		safeMode: true, restricted: true, strictMCP: true, tools: true, disallowedTools: true,
	})
	if err != nil {
		t.Fatalf("supported capabilities rejected: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"--safe-mode", "--restricted", "--strict-mcp-config", "--tools", "--disallowedTools"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %s", joined, want)
		}
	}
}

func TestConfigureOpenCodeIsolationCopiesAuthFile(t *testing.T) {
	sourceRoot := t.TempDir()
	sourceAuthDir := filepath.Join(sourceRoot, "opencode")
	if err := os.MkdirAll(sourceAuthDir, 0o700); err != nil {
		t.Fatal(err)
	}
	auth := []byte(`{"opencode":{"type":"api","key":"test-only"}}`)
	if err := os.WriteFile(filepath.Join(sourceAuthDir, "auth.json"), auth, 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cmd := &exec.Cmd{Dir: dir, Env: []string{"XDG_DATA_HOME=" + sourceRoot, "HOME=" + filepath.Join(sourceRoot, "home")}}
	if err := configureOpenCodeIsolation(cmd, openCodeAskConfig); err != nil {
		t.Fatalf("configureOpenCodeIsolation: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "opencode-data", "opencode", "auth.json"))
	if err != nil {
		t.Fatalf("isolated auth file missing: %v", err)
	}
	if string(got) != string(auth) {
		t.Fatalf("isolated auth file = %q, want seeded auth", got)
	}
}

func TestHarnessEnvSelectsBackendAuthRoots(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"CLAUDE_CONFIG_DIR=/claude",
		"CODEX_HOME=/codex",
		"GOOSE_PATH_ROOT=/goose",
		"OPENCODE_API_KEY=opencode-key",
		"OPENAI_API_KEY=openai-key",
	}
	cases := []struct {
		name string
		kind harnessKind
		keep string
	}{
		{name: "claude", kind: harnessClaude, keep: "CLAUDE_CONFIG_DIR"},
		{name: "codex", kind: harnessCodex, keep: "CODEX_HOME"},
		{name: "goose", kind: harnessGoose, keep: "GOOSE_PATH_ROOT"},
		{name: "opencode", kind: harnessOpencode, keep: "OPENCODE_API_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := namesOf(harnessEnv(base, tc.kind))
			if got[tc.keep] == "" {
				t.Errorf("%s was not preserved for %s", tc.keep, tc.name)
			}
			for _, name := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GOOSE_PATH_ROOT", "OPENCODE_API_KEY"} {
				if name != tc.keep {
					if _, ok := got[name]; ok {
						t.Errorf("%s leaked into %s child", name, tc.name)
					}
				}
			}
			if _, ok := got["OPENAI_API_KEY"]; ok {
				t.Error("OPENAI_API_KEY passed despite the final LLM-key strip")
			}
		})
	}
}

func TestHarnessEnvDropsGhostCredentialNames(t *testing.T) {
	got := namesOf(harnessEnv([]string{
		"PATH=/usr/bin",
		"GHOST_OPENCODE_MODEL=opencode/big-pickle",
		"GHOST_API_KEY=secret",
		"GHOST_DATABASE_URL=postgres://secret",
		"GHOST_SERVICE_TOKEN=secret",
	}, harnessClaude))

	for _, name := range []string{"GHOST_API_KEY", "GHOST_DATABASE_URL", "GHOST_SERVICE_TOKEN"} {
		if value, ok := got[name]; ok {
			t.Errorf("credential-looking %s passed to child: %q", name, value)
		}
	}
	if got["GHOST_OPENCODE_MODEL"] != "opencode/big-pickle" {
		t.Fatalf("known Ghost model pin was dropped: %v", got)
	}
}

func TestHarnessEnvKeepsNativeWindowsHomeVariables(t *testing.T) {
	got := namesOf(harnessEnv([]string{
		"Path=C:\\Windows\\system32",
		"UserProfile=C:\\Users\\u",
		"AppData=C:\\Users\\u\\AppData\\Roaming",
		"LocalAppData=C:\\Users\\u\\AppData\\Local",
		"ComSpec=C:\\Windows\\system32\\cmd.exe",
		"PATHEXT=.COM;.EXE;.BAT",
	}, harnessClaude))

	for key, want := range map[string]string{
		"UserProfile":  "C:\\Users\\u",
		"AppData":      "C:\\Users\\u\\AppData\\Roaming",
		"LocalAppData": "C:\\Users\\u\\AppData\\Local",
		"ComSpec":      "C:\\Windows\\system32\\cmd.exe",
		"PATHEXT":      ".COM;.EXE;.BAT",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

func TestCLIClientUsesNoToolPolicy(t *testing.T) {
	setHarnessPolicyParentEnv(t)
	bin := fakeHarnessPolicyBinary(t, "claude", `
if [ "$1" = "--help" ]; then
  printf '%s\n' '--safe-mode' '--restricted' '--strict-mcp-config' '--disable-slash-commands' '--tools' '--disallowedTools' '--setting-sources'
  exit 0
fi
for name in AWS_SECRET_ACCESS_KEY GITHUB_TOKEN GHOST_API_KEY GHOST_DATABASE_URL; do
  eval "value=\${$name-}"
  [ -z "$value" ] || { echo "leaked $name" >&2; exit 1; }
done
[ "$CLAUDE_CONFIG_DIR" = "/decoy/claude" ] || { echo "missing Claude config root" >&2; exit 1; }
[ -z "$CODEX_HOME" ] || { echo "Codex root leaked to Claude" >&2; exit 1; }
[ -z "$OPENCODE_API_KEY" ] || { echo "OpenCode key leaked to Claude" >&2; exit 1; }
args="$*"
case "$args" in *" --restricted"*) ;; *) echo "missing --restricted" >&2; exit 1;; esac
case "$args" in *" --safe-mode"*) ;; *) echo "missing --safe-mode" >&2; exit 1;; esac
case "$args" in *" --strict-mcp-config"*) ;; *) echo "missing --strict-mcp-config" >&2; exit 1;; esac
case "$args" in *" --disable-slash-commands"*) ;; *) echo "missing --disable-slash-commands" >&2; exit 1;; esac
case "$args" in *" --tools "*) ;; *) echo "missing --tools" >&2; exit 1;; esac
case "$args" in *" --disallowedTools mcp__*"*) ;; *) echo "missing MCP deny rule" >&2; exit 1;; esac
printf '%s' '{"memories":[]}'
`)

	text, _, err := (&CLIClient{binary: bin}).Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if text != `{"memories":[]}` {
		t.Fatalf("stdout = %q", text)
	}
}

func TestCodexClientUsesNoToolPolicy(t *testing.T) {
	setHarnessPolicyParentEnv(t)
	bin := fakeHarnessPolicyBinary(t, "codex", `
for name in AWS_SECRET_ACCESS_KEY GITHUB_TOKEN GHOST_API_KEY GHOST_DATABASE_URL; do
  eval "value=\${$name-}"
  [ -z "$value" ] || { echo "leaked $name" >&2; exit 1; }
done
[ "$CODEX_HOME" = "/decoy/codex" ] || { echo "missing Codex home" >&2; exit 1; }
[ -z "$OPENCODE_API_KEY" ] || { echo "OpenCode key leaked to Codex" >&2; exit 1; }
[ -z "$CLAUDE_CONFIG_DIR" ] || { echo "Claude config leaked to Codex" >&2; exit 1; }
args="$*"
case "$args" in *" --ignore-user-config"*) ;; *) echo "missing --ignore-user-config" >&2; exit 1;; esac
case "$args" in *" --ignore-rules"*) ;; *) echo "missing --ignore-rules" >&2; exit 1;; esac
case "$args" in *" --skip-git-repo-check"*) ;; *) echo "missing --skip-git-repo-check" >&2; exit 1;; esac
case "$args" in *" --ephemeral"*) ;; *) echo "missing --ephemeral" >&2; exit 1;; esac
case "$args" in *"features.shell_tool=false"*) ;; *) echo "shell feature not disabled" >&2; exit 1;; esac
case "$args" in *"features.unified_exec=false"*) ;; *) echo "unified exec not disabled" >&2; exit 1;; esac
case "$args" in *"web_search=\"disabled\""*) ;; *) echo "web search not disabled" >&2; exit 1;; esac
printf '%s' 'KEEP'
`)

	text, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if text != "KEEP" {
		t.Fatalf("stdout = %q", text)
	}
}

func TestGooseClientUsesNoToolPolicy(t *testing.T) {
	setHarnessPolicyParentEnv(t)
	bin := fakeHarnessPolicyBinary(t, "goose", `
for name in AWS_SECRET_ACCESS_KEY GITHUB_TOKEN GHOST_API_KEY GHOST_DATABASE_URL; do
  eval "value=\${$name-}"
  [ -z "$value" ] || { echo "leaked $name" >&2; exit 1; }
done
[ "$GOOSE_PATH_ROOT" = "/decoy/goose" ] || { echo "missing Goose root" >&2; exit 1; }
[ -z "$OPENCODE_API_KEY" ] || { echo "OpenCode key leaked to Goose" >&2; exit 1; }
args="$*"
case "$args" in *" --no-profile"*) ;; *) echo "missing --no-profile" >&2; exit 1;; esac
case "$args" in *" --no-session"*) ;; *) echo "missing --no-session" >&2; exit 1;; esac
printf '%s' 'KEEP'
`)

	text, _, err := (&GooseClient{binary: bin}).Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if text != "KEEP" {
		t.Fatalf("stdout = %q", text)
	}
}

// TestGooseChildCannotDiscoverGhostsOwnPlugin: goose discovers user-scope
// Agent Plugins under $HOME/.agents/plugins/, and that is home-relative BY
// SPECIFICATION rather than XDG-relative. `ghost mcp init --client goose`
// installs a package there whose mcp.json registers the Ghost stdio MCP server
// and whose hooks/hooks.json run `ghost hook <event> --source goose` as shell
// commands. The goose child needs the real HOME to read ~/.config/goose and
// authenticate, so without an explicit boundary it also finds that package and
// starts Ghost's own server from inside a reflect/resolve/supersede call.
// --no-profile does not close this: it governs the configured profile, not
// plugin discovery.
func TestGooseChildCannotDiscoverGhostsOwnPlugin(t *testing.T) {
	realHome := t.TempDir()
	// The package `ghost mcp init --client goose` would have installed.
	pluginDir := filepath.Join(realHome, ".agents", "plugins", "ghost")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "mcp.json"), []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setHarnessPolicyParentEnv(t)
	// After the shared decoys, so these are the values the child inherits.
	t.Setenv("HOME", realHome)
	t.Setenv("USERPROFILE", realHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(realHome, ".config"))

	// The other half of the contract: the config the child authenticates from
	// must still be where goose looks, or the isolation just breaks the call.
	gooseConfig := filepath.Join(realHome, ".config", "goose")
	if err := os.MkdirAll(gooseConfig, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gooseConfig, "config.yaml"), []byte("provider: openai\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := fakeHarnessPolicyBinary(t, "goose", `
[ -e "$HOME/.agents/plugins/ghost/mcp.json" ] && { echo "goose child discovered Ghost's own MCP plugin" >&2; exit 1; }
# HOME itself must be confined, not merely happen to miss the plugin path: an
# unset or wrong HOME would also pass the test above.
case "$HOME" in "$GHOST_SCRATCH_DIR"/*) ;; *) echo "goose home not isolated: $HOME" >&2; exit 1;; esac
[ -r "$XDG_CONFIG_HOME/goose/config.yaml" ] || { echo "goose child lost the config it authenticates from" >&2; exit 1; }
printf '%s' 'KEEP'
`)

	text, _, err := (&GooseClient{binary: bin}).Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if text != "KEEP" {
		t.Fatalf("stdout = %q", text)
	}
}

// TestConfigureGooseIsolationFailsClosed: the two states in which the security
// boundary cannot be established must not report success. A child that runs
// with the real HOME finds Ghost's own MCP plugin again, so "isolation
// skipped" is not a safe degradation — it is the vulnerability. Both cases
// return an error, which fails the call rather than the boundary.
func TestConfigureGooseIsolationFailsClosed(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, ".config", "goose")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "config.yaml"), []byte("provider: openai\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("no scratch directory", func(t *testing.T) {
		cmd := &exec.Cmd{Env: []string{"HOME=" + home}}
		if err := configureGooseIsolation(cmd); err == nil {
			t.Fatal("isolation reported success with no directory to confine the child to")
		}
	})

	t.Run("no home and no config root", func(t *testing.T) {
		cmd := &exec.Cmd{Dir: t.TempDir(), Env: []string{"PATH=/usr/bin"}}
		if err := configureGooseIsolation(cmd); err == nil {
			t.Fatal("isolation reported success with nothing to confine")
		}
	})

	// The positive case, so the table cannot pass by always erroring. No
	// XDG_CONFIG_HOME here, so this exercises the home-relative fallback that
	// goose uses when the variable is unset — the branch where the config links
	// have to be created, and where pointing HOME straight at the config
	// directory would silently miss.
	t.Run("resolves and confines", func(t *testing.T) {
		dir := t.TempDir()
		cmd := &exec.Cmd{Dir: dir, Env: []string{"HOME=" + home}}
		if err := configureGooseIsolation(cmd); err != nil {
			t.Fatalf("configureGooseIsolation: %v", err)
		}
		got := envValue(cmd.Env, "HOME")
		if got != filepath.Join(dir, "goose-home") {
			t.Fatalf("HOME = %q, want the isolated home", got)
		}
		// Resolved the way goose resolves it, not the way the code built it.
		data, err := os.ReadFile(filepath.Join(got, ".config", "goose", "config.yaml"))
		if err != nil {
			t.Fatalf("isolated home does not reach the real config: %v", err)
		}
		if string(data) != "provider: openai\n" {
			t.Fatalf("config through the isolated home = %q", data)
		}
		// And the plugin root really is gone, rather than merely unpopulated
		// by accident of this fixture.
		if _, err := os.Stat(filepath.Join(got, ".agents")); !os.IsNotExist(err) {
			t.Errorf("isolated home exposes an .agents directory: %v", err)
		}
		// Only the roots that exist are reproduced: this fixture configures
		// .config, so the macOS root must not be conjured into the home.
		if _, err := os.Stat(filepath.Join(got, "Library")); !os.IsNotExist(err) {
			t.Errorf("isolated home carries a macOS config root that was never there: %v", err)
		}
	})

	// macOS does not set XDG_CONFIG_HOME, and goose documents both
	// ~/.config/goose and ~/Library/Application Support/goose there. Carrying
	// only the first moves the config out from under a child that resolves the
	// second, which would break a call that worked before this branch.
	t.Run("carries the macOS config root", func(t *testing.T) {
		macHome := t.TempDir()
		macConfig := filepath.Join(macHome, "Library", "Application Support", "goose")
		if err := os.MkdirAll(macConfig, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(macConfig, "config.yaml"), []byte("model: gpt\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		cmd := &exec.Cmd{Dir: t.TempDir(), Env: []string{"HOME=" + macHome}}
		if err := configureGooseIsolation(cmd); err != nil {
			t.Fatalf("configureGooseIsolation: %v", err)
		}
		data, err := os.ReadFile(filepath.Join(
			envValue(cmd.Env, "HOME"), "Library", "Application Support", "goose", "config.yaml"))
		if err != nil {
			t.Fatalf("isolated home does not carry the macOS config root: %v", err)
		}
		if string(data) != "model: gpt\n" {
			t.Fatalf("macOS config through the isolated home = %q", data)
		}
	})

	// With XDG_CONFIG_HOME set the child reads an absolute path and HOME plays
	// no part, so no link may be invented — exporting a synthesized value here
	// is exactly the macOS/Windows misdirection this design avoids.
	t.Run("explicit config root is left alone", func(t *testing.T) {
		dir := t.TempDir()
		env := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config")}
		cmd := &exec.Cmd{Dir: dir, Env: env}
		if err := configureGooseIsolation(cmd); err != nil {
			t.Fatalf("configureGooseIsolation: %v", err)
		}
		if got := envValue(cmd.Env, "XDG_CONFIG_HOME"); got != filepath.Join(home, ".config") {
			t.Fatalf("XDG_CONFIG_HOME = %q, want the parent's value", got)
		}
		got := envValue(cmd.Env, "HOME")
		if got != filepath.Join(dir, "goose-home") {
			t.Fatalf("HOME = %q, want the isolated home", got)
		}
		// HOME must EXIST, not merely be named: a child with a missing home
		// fails to start on some platforms, and this branch creates no .config
		// link, so nothing else would have created the directory.
		if info, err := os.Stat(got); err != nil {
			t.Fatalf("isolated home does not exist: %v", err)
		} else if !info.IsDir() {
			t.Fatalf("isolated home is not a directory: %s", got)
		}
	})
}

// TestCarryGooseConfigDirFallsBackToCopy: a Windows host without Developer Mode
// cannot create a symlink, and that is the NORMAL path there because
// XDG_CONFIG_HOME is rarely set. A hard dependency on a privileged filesystem
// operation would fail every goose call on a supported platform, so a refused
// symlink has to degrade to a copy — and the copy has to carry the same files.
func TestCarryGooseConfigDirFallsBackToCopy(t *testing.T) {
	source := filepath.Join(t.TempDir(), "goose")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "config.yaml"), []byte("provider: openai\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "secrets.yaml"), []byte("token: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A subdirectory is not copied: it is not a file, and following it would
	// let a config directory pull a tree into the child.
	if err := os.MkdirAll(filepath.Join(source, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A dotfiles-managed config is the common case and reports a symlink
	// dirent type, so reading the dirent type alone would drop it and leave a
	// directory that reports success and holds nothing goose can use.
	managed := filepath.Join(t.TempDir(), "goose-config.yaml")
	if err := os.WriteFile(managed, []byte("provider: managed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(managed, filepath.Join(source, "managed.yaml")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	target := filepath.Join(t.TempDir(), "goose")
	denied := func(string, string) error { return errors.New("symlink privilege not held") }
	if err := carryGooseConfigDirWith(source, target, denied); err != nil {
		t.Fatalf("carryGooseConfigDirWith: %v", err)
	}

	for name, want := range map[string]string{
		"config.yaml":  "provider: openai\n",
		"secrets.yaml": "token: x\n",
		"managed.yaml": "provider: managed\n", // reached through a symlink
	} {
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil {
			t.Fatalf("copied config missing %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("copied %s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "sessions")); !os.IsNotExist(err) {
		t.Errorf("copy carried a subdirectory: %v", err)
	}
}

// TestGooseConfigRootProbeClassifiesFailures: only "not there" may be passed
// over when a candidate config root is probed, because that is how a platform's
// unused location is recognised. A root that exists but is not usable must
// fail instead: skipping it would hand the child a home with no configuration
// and no indication that Ghost dropped it, and the child would then
// authenticate against goose's defaults while the user saw a provider error
// pointing nowhere.
//
// The classification is injected rather than reproduced from the filesystem,
// because the interesting case is not buildable everywhere: Windows reports "a
// file where a directory belongs" as ERROR_PATH_NOT_FOUND, which Go maps to
// fs.ErrNotExist, so on that host this case legitimately classifies as absent.
// The platform's own answer is honoured either way; what is pinned is that a
// non-ENOENT failure is never skipped.
func TestGooseConfigRootProbeClassifiesFailures(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, ".config", "goose")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "config.yaml"), []byte("provider: openai\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	isolated := t.TempDir()

	t.Run("absent is skipped", func(t *testing.T) {
		probe := func(name string) (os.FileInfo, error) {
			if strings.Contains(name, "Library") {
				return nil, fs.ErrNotExist
			}
			return os.Lstat(name)
		}
		if err := linkGooseConfigDirsWith(isolated, []string{"HOME=" + home}, home, probe); err != nil {
			t.Fatalf("a missing root must be skipped, not refused: %v", err)
		}
		if _, err := os.Stat(filepath.Join(isolated, ".config", "goose", "config.yaml")); err != nil {
			t.Fatalf("the present root was not carried: %v", err)
		}
	})

	t.Run("unusable is refused", func(t *testing.T) {
		probe := func(name string) (os.FileInfo, error) {
			if strings.Contains(name, "Library") {
				return nil, &fs.PathError{Op: "lstat", Path: name, Err: syscall.ENOTDIR}
			}
			return os.Lstat(name)
		}
		if err := linkGooseConfigDirsWith(isolated, []string{"HOME=" + home}, home, probe); err == nil {
			t.Fatal("a root that exists but is unusable was skipped as if absent")
		}
	})
}

func TestOpenCodeClientUsesNoToolPolicy(t *testing.T) {
	setHarnessPolicyParentEnv(t)
	bin := fakeHarnessPolicyBinary(t, "opencode", `
for name in AWS_SECRET_ACCESS_KEY GITHUB_TOKEN GHOST_API_KEY GHOST_DATABASE_URL; do
  eval "value=\${$name-}"
  [ -z "$value" ] || { echo "leaked $name" >&2; exit 1; }
done
[ "$OPENCODE_API_KEY" = "opencode-secret" ] || { echo "missing OpenCode key" >&2; exit 1; }
[ -z "$CODEX_HOME" ] || { echo "Codex root leaked to OpenCode" >&2; exit 1; }
[ -z "$CLAUDE_CONFIG_DIR" ] || { echo "Claude config leaked to OpenCode" >&2; exit 1; }
if [ "$1" = "--version" ]; then
  printf '%s\n' 'opencode v2.0.15'
  exit 0
fi
case "$HOME" in "$GHOST_SCRATCH_DIR"/*) ;; *) echo "OpenCode home not isolated: $HOME" >&2; exit 1;; esac
case "$XDG_CONFIG_HOME" in "$GHOST_SCRATCH_DIR"/*) ;; *) echo "OpenCode XDG config not isolated: $XDG_CONFIG_HOME" >&2; exit 1;; esac
[ -n "$OPENCODE_CONFIG" ] || { echo "OpenCode config path missing" >&2; exit 1; }
grep -q '"\*"[[:space:]]*:[[:space:]]*"ask"' "$OPENCODE_CONFIG" || { echo "OpenCode ask config missing" >&2; exit 1; }
args="$*"
case "$args" in *" --standalone"*) ;; *) echo "missing --standalone" >&2; exit 1;; esac
# The ask policy is only safe while nothing auto-approves the asks.
for flag in --auto --dangerously-skip-permissions --yolo; do
  case " $args " in *" $flag "*) echo "auto-approve flag $flag passed" >&2; exit 1;; esac
done
printf '%s\n' '{"type":"text","part":{"type":"text","text":"KEEP"}}'
`)

	text, _, err := (&OpenCodeClient{binary: bin}).Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if text != "KEEP" {
		t.Fatalf("stdout = %q", text)
	}
}

// TestOpenCodeV1ClientKeepsDenyPolicy: ask-and-decline is verified only for
// opencode V2's non-interactive run, so a V1 binary keeps the deny-all policy.
func TestOpenCodeV1ClientKeepsDenyPolicy(t *testing.T) {
	setHarnessPolicyParentEnv(t)
	bin := fakeHarnessPolicyBinary(t, "opencode", `
if [ "$1" = "--version" ]; then
  printf '%s\n' 'opencode v1.14.0'
  exit 0
fi
grep -q '"\*"[[:space:]]*:[[:space:]]*"deny"' "$OPENCODE_CONFIG" || { echo "V1 deny config missing" >&2; exit 1; }
if grep -q '"ask"' "$OPENCODE_CONFIG"; then echo "V1 got the ask policy" >&2; exit 1; fi
case "$OPENCODE_CONFIG_CONTENT" in *'"deny"'*) ;; *) echo "V1 config content is not deny" >&2; exit 1;; esac
case " $* " in *" --pure "*) ;; *) echo "missing --pure" >&2; exit 1;; esac
printf '%s\n' '{"type":"text","part":{"type":"text","text":"KEEP"}}'
`)

	if _, _, err := (&OpenCodeClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
}

func TestHarnessCommandPortableChildKeepsWindowsHome(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	base := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(t.TempDir(), "home"),
		"USERPROFILE=" + filepath.Join(t.TempDir(), "profile"),
		"APPDATA=" + filepath.Join(t.TempDir(), "appdata"),
		"LOCALAPPDATA=" + filepath.Join(t.TempDir(), "localappdata"),
		"PATHEXT=.COM;.EXE;.BAT",
		"ComSpec=C:\\Windows\\system32\\cmd.exe",
		"SystemRoot=C:\\Windows",
		"Windir=C:\\Windows",
	}
	cmd, release, ok := harnessCommand(
		context.Background(),
		binary,
		[]string{"-test.run=TestHarnessPortableChild", "--", "ghost-harness-child"},
		base,
		harnessClaude,
	)
	if !ok {
		t.Fatal("scratch confinement unexpectedly unavailable")
	}
	defer release()
	if _, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("portable child: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cmd.Dir, "child-ran")); err != nil {
		t.Fatalf("portable child did not run in the confined directory: %v", err)
	}
}

func TestHarnessPortableChild(t *testing.T) {
	const marker = "ghost-harness-child"
	found := false
	for _, arg := range os.Args {
		if arg == marker {
			found = true
			break
		}
	}
	if !found {
		return
	}
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "PATHEXT", "ComSpec"} {
		if os.Getenv(key) == "" {
			t.Fatalf("portable child lost %s", key)
		}
	}
	if err := os.WriteFile("child-ran", []byte("ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCodeVersionProbeUsesHarnessEnvironment(t *testing.T) {
	setHarnessPolicyParentEnv(t)
	bin := fakeHarnessPolicyBinary(t, "opencode", `
[ -z "$AWS_SECRET_ACCESS_KEY" ] || { echo "version probe leaked AWS secret" >&2; exit 1; }
[ -z "$GHOST_API_KEY" ] || { echo "version probe leaked Ghost secret" >&2; exit 1; }
[ "$OPENCODE_API_KEY" = "opencode-secret" ] || { echo "version probe lost OpenCode auth" >&2; exit 1; }
case "$PWD" in "$GHOST_SCRATCH_DIR"/*) ;; *) echo "version probe ran outside scratch" >&2; exit 1;; esac
printf '%s\n' 'opencode v2.0.15'
`)

	if got := (&OpenCodeClient{binary: bin}).majorVersion(context.Background()); got != 2 {
		t.Fatalf("majorVersion = %d, want 2", got)
	}
}
