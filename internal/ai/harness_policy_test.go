package ai

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

func TestConfigureOpenCodeIsolationWritesDenyConfig(t *testing.T) {
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
	if err := configureOpenCodeIsolation(cmd); err != nil {
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
	if config.Permission["*"] != "deny" || config.Permission["mcp_*"] != "deny" {
		t.Errorf("permission rules = %v, want wildcard and MCP deny", config.Permission)
	}
	if config.Tools["bash"] || config.Tools["edit"] || config.Tools["write"] {
		t.Errorf("dangerous tools are enabled: %v", config.Tools)
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
	if err := configureOpenCodeIsolation(cmd); err != nil {
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
grep -q '"permission"\|"\\*"[[:space:]]*:[[:space:]]*"deny"' "$OPENCODE_CONFIG" || { echo "OpenCode deny config missing" >&2; exit 1; }
args="$*"
case "$args" in *" --standalone"*) ;; *) echo "missing --standalone" >&2; exit 1;; esac
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
