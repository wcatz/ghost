package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func fakeHarnessPolicyBinary(t *testing.T, name, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script policy fake requires a POSIX shell")
	}
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -e\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	switch name {
	case "codex", "claude":
		registerProbeIdentity(t, path)
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
		"GOOSE_MODE":            "auto",
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
	if len(config.Permission) != 1 || config.Permission["*"] != "ask" {
		t.Errorf("permission rules = %v, want only {\"*\": \"ask\"}", config.Permission)
	}
	if len(config.Tools) != 0 {
		t.Errorf("tool map = %v, want none (a disabled shell trips the free-tier check)", config.Tools)
	}
	if len(config.MCP) != 0 || len(config.Plugin) != 0 {
		t.Errorf("MCP/plugins survived isolation: mcp=%v plugin=%v", config.MCP, config.Plugin)
	}
}

func TestOpenCodePolicyConfigsAreGoldens(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{
			name: "V2 ask policy",
			got:  openCodeAskConfig,
			want: `{"$schema":"https://opencode.ai/config.json","permission":{"*":"ask"},"mcp":{},"plugin":[]}`,
		},
		{
			name: "V1 deny policy",
			got:  openCodeDenyConfig,
			want: `{"$schema":"https://opencode.ai/config.json","permission":{"*":"deny","mcp_*":"deny"},"tools":{"bash":false,"edit":false,"write":false,"read":false,"grep":false,"glob":false,"list":false,"patch":false,"webfetch":false,"task":false,"todowrite":false,"todoread":false},"mcp":{},"plugin":[]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got, want any
			if err := json.Unmarshal([]byte(tc.got), &got); err != nil {
				t.Fatalf("the policy blob is not valid JSON, so opencode would reject it: %v", err)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatalf("the golden is not valid JSON: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				gotJSON, _ := json.MarshalIndent(got, "", "  ")
				t.Errorf("the isolated config policy drifted.\n got: %s\nwant: %s", gotJSON, tc.want)
			}
		})
	}
}

func TestHarnessInvocationArgsAreGoldens(t *testing.T) {
	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "claude",
			got: func() []string {
				args, err := claudeInvocationArgs(claudeCapabilities{
					safeMode: true, restricted: true, strictMCP: true,
					tools: true, disallowedTools: true,
					disableSlash: true, settingSources: true,
				})
				if err != nil {
					t.Fatalf("claudeInvocationArgs: %v", err)
				}
				return args
			}(),
			want: []string{
				"-p",
				"--safe-mode",
				"--restricted",
				"--strict-mcp-config",
				"--tools", "",
				"--disallowedTools", "mcp__*",
				"--disable-slash-commands",
				"--setting-sources", "project,local",
			},
		},
		{
			name: "codex",
			got:  codexInvocationArgs(nil),
			want: []string{
				"exec",
				"--sandbox", "read-only",
				"--ignore-user-config",
				"--ignore-rules",
				"--skip-git-repo-check",
				"--ephemeral",
				"-c", "features.shell_tool=false",
				"-c", "features.unified_exec=false",
				"-c", "features.view_image=false",
				"-c", "features.apps=false",
				"-c", "features.plugins=false",
				"-c", "features.tool_suggest=false",
				"-c", "features.skill_mcp_dependency_install=false",
				"-c", "features.remote_plugin=false",
				"-c", "features.hooks=false",
				"-c", "features.multi_agent=false",
				"-c", "agents.enabled=false",
				"-c", `web_search="disabled"`,
				"-",
			},
		},
		{
			name: "goose",
			got:  gooseInvocationArgs(),
			want: []string{"run", "-q", "--no-profile", "--no-session", "-i", "-"},
		},
		{
			name: "opencode v1",
			got:  openCodeInvocationArgs(1, "opencode/big-pickle"),
			want: []string{"run", "--format", "json", "--pure", "--title", "[ghost]", "-m", "opencode/big-pickle"},
		},
		{
			name: "opencode v2",
			got:  openCodeInvocationArgs(2, "opencode/big-pickle"),
			want: []string{"run", "--format", "json", "--standalone", "--title", "[ghost]", "-m", "opencode/big-pickle"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Join(tc.got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("argv =\n%q\nwant\n%q", tc.got, tc.want)
			}
		})
	}
}

func codexFeaturesFake(t *testing.T, rows string) string {
	t.Helper()
	t.Setenv("CODEX_PROBE_LOG", filepath.Join(t.TempDir(), "probes"))
	t.Setenv("CODEX_EXEC_LOG", filepath.Join(t.TempDir(), "exec"))
	t.Setenv("FAKE_FEATURE_ROWS", rows)
	t.Setenv("GHOST_PASSTHROUGH_ENV", "CODEX_PROBE_LOG,CODEX_EXEC_LOG,FAKE_FEATURE_ROWS")
	return fakeHarnessPolicyBinary(t, "codex", `
if [ "$1" = "features" ]; then
  printf 'probed\n' >> "$CODEX_PROBE_LOG"
  [ -n "$FAKE_FEATURE_ROWS" ] || { echo "no features subcommand" >&2; exit 2; }
  printf '%s' "$FAKE_FEATURE_ROWS"
  exit 0
fi
# The real turn: record every argument so a test can read the policy the child
# was actually given. Each -c value lands on its own line because a
# features.shell_tool=false pair must survive intact, and a separator marks each
# invocation so a test that made several calls can read the one it cares about.
printf -- '--- invocation\n' >> "$CODEX_EXEC_LOG"
for arg in "$@"; do
  case "$arg" in
    -c) ;;
    *) printf '%s\n' "$arg" >> "$CODEX_EXEC_LOG" ;;
  esac
done
printf '%s' 'KEEP'
`)
}

func resetCodexFeatureProbe(t *testing.T) {
	t.Helper()
	codexFeatureCache.Clear()
	codexUnverifiedWarned = sync.Once{}
	codexWeakerWarned = sync.Once{}
	t.Cleanup(func() {
		codexFeatureCache.Clear()
		codexUnverifiedWarned = sync.Once{}
		codexWeakerWarned = sync.Once{}
	})
}

func captureCodexWarnings(t *testing.T) *lockedBuffer {
	t.Helper()
	return captureProcessLogs(t)
}

func TestCodexProbePassesEveryKeyWhenAllAreDeclared(t *testing.T) {
	resetCodexFeatureProbe(t)
	logs := captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)
	bin := codexFeaturesFake(t, "shell_tool stable true\nunified_exec stable true\nview_image stable true\napps stable true\nplugins stable true\ntool_suggest stable true\nskill_mcp_dependency_install stable true\nremote_plugin stable true\nhooks stable true\nmulti_agent stable true\n")

	if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if logs.Len() != 0 {
		t.Errorf("a codex declaring every key logged %q, want silence", logs.String())
	}
	if got := codexPolicyArgvKeys(t); !equalStrings(got, codexNoToolFeatureKeys) {
		t.Errorf("child got feature keys %v, want all of %v", got, codexNoToolFeatureKeys)
	}
}

func TestCodexProbeOmitsUndeclaredKeysAndWarnsOnce(t *testing.T) {
	resetCodexFeatureProbe(t)
	logs := captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)
	bin := codexFeaturesFake(t, "shell_tool stable true\nunified_exec stable true\nhooks stable true\ntool_suggest stable true\nmulti_agent stable true\n")
	for range 3 {
		if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
			t.Fatalf("Reflect: %v", err)
		}
	}
	if got := strings.Count(logs.String(), "WEAKER than on a current codex"); got != 1 {
		t.Errorf("weaker-policy warning appeared %d times over 3 calls, want exactly 1:\n%s", got, logs.String())
	}
	for _, key := range []string{"view_image", "apps", "plugins", "skill_mcp_dependency_install", "remote_plugin"} {
		if !strings.Contains(logs.String(), key) {
			t.Errorf("warning does not name the missing key %q:\n%s", key, logs.String())
		}
	}
	want := []string{"shell_tool", "unified_exec", "tool_suggest", "hooks", "multi_agent"}
	if got := codexPolicyArgvKeys(t); !equalStrings(got, want) {
		t.Errorf("child got feature keys %v, want only the five this codex declares %v", got, want)
	}
	if n := probeCallLog(t); n != 1 {
		t.Errorf("probe ran %d times over 3 calls on one binary, want 1", n)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestCodexProbeFailureFallsBackToEveryKeyAndWarnsOnce(t *testing.T) {
	resetCodexFeatureProbe(t)
	logs := captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)
	bin := codexFeaturesFake(t, "")

	for range 3 {
		if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
			t.Fatalf("Reflect: %v", err)
		}
	}
	if got := strings.Count(logs.String(), "feature probe did not answer"); got != 1 {
		t.Errorf("unverified-policy warning appeared %d times over 3 calls, want exactly 1:\n%s", got, logs.String())
	}
	if strings.Contains(logs.String(), "WEAKER than on a current codex") {
		t.Errorf("an unanswered probe was reported as a weaker POLICY, which claims the codex lacks features it was never asked about:\n%s", logs.String())
	}
	if got := codexPolicyArgvKeys(t); !equalStrings(got, codexNoToolFeatureKeys) {
		t.Errorf("child got feature keys %v, want the whole policy %v after a failed probe", got, codexNoToolFeatureKeys)
	}
}

func TestCodexProbeIsConfinedLikeEveryOtherChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script policy fake requires a POSIX shell")
	}
	resetCodexFeatureProbe(t)
	captureCodexWarnings(t)
	report := filepath.Join(t.TempDir(), "probe-report")
	setHarnessPolicyParentEnv(t)
	t.Setenv("GHOST_PASSTHROUGH_ENV", "CODEX_PROBE_REPORT")
	t.Setenv("CODEX_PROBE_REPORT", report)
	bin := fakeHarnessPolicyBinary(t, "codex", `
if [ "$1" = "features" ]; then
  { printf 'CWD=%s\n' "$PWD"; printf '%s\n' "$@"; env; } > "$CODEX_PROBE_REPORT"
  printf 'shell_tool stable true\n'
  exit 0
fi
printf '%s' 'KEEP'
`)

	if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	seen, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("the probe did not report its environment: %v", err)
	}
	for _, leaked := range []string{
		"GITHUB_TOKEN=github-secret",
		"AWS_SECRET_ACCESS_KEY=aws-secret",
		"GHOST_API_KEY=ghost-secret",
		"GHOST_DATABASE_URL=postgres://secret",
	} {
		if strings.Contains(string(seen), leaked) {
			t.Errorf("the codex probe inherited %s; a diagnostic is still a child and must go through harnessEnv", leaked)
		}
	}
	cwd := ""
	for _, line := range strings.Split(string(seen), "\n") {
		if after, ok := strings.CutPrefix(line, "CWD="); ok {
			cwd = after
		}
	}
	if cwd == "" {
		t.Fatalf("the probe reported no working directory:\n%s", seen)
	}
	if resolvedComparable(t, cwd) == resolvedComparable(t, repoRootForProbeTest(t)) {
		t.Errorf("the codex probe ran in the repository root %s, so it could discover Ghost's own rules and config", cwd)
	}
}

func repoRootForProbeTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func TestCodexProbeRunsOncePerBinaryIdentity(t *testing.T) {
	resetCodexFeatureProbe(t)
	captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)
	bin := codexFeaturesFake(t, "shell_tool stable true\nunified_exec stable true\nview_image stable true\napps stable true\nplugins stable true\ntool_suggest stable true\nskill_mcp_dependency_install stable true\nremote_plugin stable true\nhooks stable true\nmulti_agent stable true\n")

	for range 5 {
		if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
			t.Fatalf("Reflect: %v", err)
		}
	}
	if n := probeCallLog(t); n != 1 {
		t.Errorf("probe ran %d times over 5 calls on one binary identity, want 1", n)
	}
}

func TestCodexParseTakesEveryNonBlankLine(t *testing.T) {
	got := parseCodexFeaturesList("NAME  STAGE  ENABLED\nshell_tool  stable  true\n\n2 features\n")
	want := map[string]bool{"NAME": true, "shell_tool": true, "2": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parse = %v, want every non-blank line's first field %v", got, want)
	}
	if got := parseCodexFeaturesList("   \n\t\n\n"); len(got) != 0 {
		t.Errorf("parse of blank output = %v, want empty", got)
	}
	if parseCodexFeaturesList("") == nil {
		t.Error("parse of empty output returned nil, and a nil map is how an unanswered probe is represented")
	}
}

func TestCodexUnreadableProbeOutputIsNotAnAnswer(t *testing.T) {
	resetCodexFeatureProbe(t)
	logs := captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)

	for _, tc := range []struct {
		name string
		rows string
	}{
		{name: "a single token", rows: "LEN:0\n"},
		{name: "a header and a footer", rows: "NAME  STAGE  ENABLED\n2 features\n"},
		{name: "rows with an unknown stage", rows: "shell_tool SORTED true\nLEN:0\n"},
		{name: "a row with a non-boolean third column", rows: "shell_tool stable maybe\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetCodexFeatureProbe(t)
			logs.Reset()
			bin := codexFeaturesFake(t, tc.rows)
			if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
				t.Fatalf("Reflect: %v", err)
			}
			if !strings.Contains(logs.String(), "probe did not answer") {
				t.Errorf("output %q was not treated as an unreadable probe; log was %q", tc.rows, logs.String())
			}
			if strings.Contains(logs.String(), "WEAKER than on a current codex") {
				t.Errorf("output %q was read as a codex missing features, which strips the policy: %s", tc.rows, logs.String())
			}
		})
	}
}

func codexPolicyArgvKeys(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("CODEX_EXEC_LOG"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read exec log: %v", err)
	}
	text := string(data)
	if idx := strings.LastIndex(text, "--- invocation\n"); idx >= 0 {
		text = text[idx+len("--- invocation\n"):]
	}
	var keys []string
	for _, arg := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		key, ok := strings.CutPrefix(arg, "features.")
		if !ok {
			continue
		}
		key, _, _ = strings.Cut(key, "=")
		keys = append(keys, key)
	}
	return keys
}

func probeCallLog(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("CODEX_PROBE_LOG"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read probe log: %v", err)
	}
	return strings.Count(string(data), "probed")
}

func TestCodexFeatureKeysAreDeclaredNames(t *testing.T) {
	declared := parseCodexFeaturesList(codexFeaturesListTranscript)
	for _, arg := range codexInvocationArgs(nil) {
		key, ok := strings.CutPrefix(arg, "features.")
		if !ok {
			continue // "--sandbox", "agents.enabled", the top-level web_search
		}
		if _, known := declared[strings.TrimSuffix(key, "=false")]; !known {
			t.Errorf("codexInvocationArgs passes features.%s, which codex's own registry does not declare", key)
		}
	}
	for _, required := range []string{
		"shell_tool", "unified_exec", "view_image", "apps", "plugins",
		"tool_suggest", "skill_mcp_dependency_install", "remote_plugin",
		"hooks", "multi_agent",
	} {
		if _, known := declared[required]; !known {
			t.Errorf("required key %q is not in the recorded codex features list; update the transcript and the policy together", required)
		}
	}
}

const codexFeaturesListTranscript = `
apps               stable   true
hooks              stable   true
multi_agent        stable   true
plugins            stable   true
remote_plugin      stable   true
shell_tool         stable   true
skill_mcp_dependency_install stable   true
sleep_tool         stable   true
tool_suggest       stable   true
unified_exec       stable   true
view_image         stable   true
web_search_request deprecated false
`

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
# The EMPTY value is the restriction, and "$*" cannot show it: a missing value
# and an empty one join to the same string. So the child walks its own argv and
# checks the element AFTER --tools, which a tool name would make non-empty.
tools_value=missing
prev=
for arg in "$@"; do
  [ "$prev" = "--tools" ] && tools_value="$arg"
  prev="$arg"
done
[ "$tools_value" = "" ] || { echo "--tools value is '$tools_value', want empty (all tools disabled)" >&2; exit 1; }
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
# Both exec tools, not one: codex registers either the unified PTY-backed tool
# or the one-shot exec, so disabling only features.shell_tool leaves the other.
case "$args" in *"features.shell_tool=false"*) ;; *) echo "shell tool not disabled" >&2; exit 1;; esac
case "$args" in *"features.unified_exec=false"*) ;; *) echo "unified exec not disabled" >&2; exit 1;; esac
# A local file read the read-only sandbox does not cover.
case "$args" in *"features.view_image=false"*) ;; *) echo "view_image not disabled" >&2; exit 1;; esac
# Each can contribute a tool: a local plugin's, an offered install, a connector,
# and an MCP server installed on demand (which runs a command).
case "$args" in *"features.plugins=false"*) ;; *) echo "plugins not disabled" >&2; exit 1;; esac
case "$args" in *"features.tool_suggest=false"*) ;; *) echo "tool_suggest not disabled" >&2; exit 1;; esac
case "$args" in *"features.apps=false"*) ;; *) echo "apps not disabled" >&2; exit 1;; esac
case "$args" in *"features.skill_mcp_dependency_install=false"*) ;; *) echo "on-demand MCP install not disabled" >&2; exit 1;; esac
case "$args" in *"features.remote_plugin=false"*) ;; *) echo "remote plugin not disabled" >&2; exit 1;; esac
case "$args" in *"features.hooks=false"*) ;; *) echo "hooks not disabled" >&2; exit 1;; esac
case "$args" in *"features.multi_agent=false"*) ;; *) echo "multi_agent not disabled" >&2; exit 1;; esac
case "$args" in *"agents.enabled=false"*) ;; *) echo "agents not disabled" >&2; exit 1;; esac
case "$args" in *" --ignore-user-config"*) ;; *) echo "missing --ignore-user-config" >&2; exit 1;; esac
case "$args" in *" --ignore-rules"*) ;; *) echo "missing --ignore-rules" >&2; exit 1;; esac
case "$args" in *" --skip-git-repo-check"*) ;; *) echo "missing --skip-git-repo-check" >&2; exit 1;; esac
case "$args" in *" --ephemeral"*) ;; *) echo "missing --ephemeral" >&2; exit 1;; esac
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
# GOOSE_PATH_ROOT is the variable goose consults FIRST, so the child must not
# keep the real one: the decoy /decoy/goose is a real plugin root, and passing
# it through is what let the child find Ghost's own package.
case "$GOOSE_PATH_ROOT" in "/decoy/goose") echo "GOOSE_PATH_ROOT passed through unconfined" >&2; exit 1;; esac
[ -z "$OPENCODE_API_KEY" ] || { echo "OpenCode key leaked to Goose" >&2; exit 1; }
args="$*"
case "$args" in *" --no-profile"*) ;; *) echo "missing --no-profile" >&2; exit 1;; esac
case "$args" in *" --no-session"*) ;; *) echo "missing --no-session" >&2; exit 1;; esac
# goose's own answer to "run a turn with no tools" is GOOSE_MODE=chat, where it
# does not enable extensions at all. The parent is set to the permissive
# "auto" (the goose default), so a child that merely inherited it would run
# every configured extension's tools unattended.
[ "$GOOSE_MODE" = "chat" ] || { echo "GOOSE_MODE=$GOOSE_MODE, want chat (extensions off)" >&2; exit 1; }
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

func TestGooseConfigRootToleratesASymlinkedAncestor(t *testing.T) {
	real := t.TempDir()
	realConfig := filepath.Join(real, "config")
	if err := os.MkdirAll(realConfig, 0o700); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	link := filepath.Join(home, ".config")
	if err := os.Symlink(realConfig, link); err != nil {
		t.Skipf("this host cannot create a symlink: %v", err)
	}
	if err := linkGooseConfigDirs(t.TempDir(), []string{"HOME=" + home}, home); err != nil {
		t.Fatalf("a symlinked ~/.config was refused: %v", err)
	}
}

func TestGooseIsolationCarriesASymlinkedConfigRoot(t *testing.T) {
	home := t.TempDir()
	real := t.TempDir()
	if err := os.WriteFile(filepath.Join(real, "config.yaml"), []byte("mode: smart\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(home, ".config", "goose")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this host cannot create a symlink: %v", err)
	}

	isolated := t.TempDir()
	if err := linkGooseConfigDirs(isolated, []string{"HOME=" + home}, home); err != nil {
		t.Fatalf("a symlinked ~/.config/goose was refused: %v", err)
	}
	carried := filepath.Join(isolated, ".config", "goose")
	got, err := os.ReadFile(filepath.Join(carried, "config.yaml"))
	if err != nil {
		t.Fatalf("the symlinked config root was not carried into the isolated home: %v", err)
	}
	if string(got) != "mode: smart\n" {
		t.Errorf("carried config.yaml = %q, want the user's own file", got)
	}
	wantReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", real, err)
	}
	if resolved, err := filepath.EvalSymlinks(carried); err != nil || resolved != wantReal {
		t.Errorf("the carried path resolves to %q (err %v), want the user's own %q", resolved, err, wantReal)
	}
}

func TestGooseIsolationRefusesABrokenSymlinkedConfigRoot(t *testing.T) {
	cases := map[string]struct {
		stage      func(t *testing.T, dir string) string
		wantTarget string
	}{
		"a link to a regular file": {
			stage: func(t *testing.T, dir string) string {
				target := filepath.Join(dir, "elsewhere.yaml")
				if err := os.WriteFile(target, []byte("mode: smart\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				link := filepath.Join(dir, "goose")
				if err := os.Symlink(target, link); err != nil {
					t.Skipf("this host cannot create a symlink: %v", err)
				}
				return link
			},
			wantTarget: "elsewhere.yaml",
		},
		"a link whose target moved": {
			stage: func(t *testing.T, dir string) string {
				link := filepath.Join(dir, "goose")
				if err := os.Symlink(filepath.Join(dir, "gone"), link); err != nil {
					t.Skipf("this host cannot create a symlink: %v", err)
				}
				return link
			},
			wantTarget: "gone",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			link := tc.stage(t, t.TempDir())
			if err := os.MkdirAll(filepath.Join(home, ".config"), 0o700); err != nil {
				t.Fatal(err)
			}
			leaf := filepath.Join(home, ".config", "goose")
			if err := os.Symlink(mustReadLink(t, link), leaf); err != nil {
				t.Skipf("this host cannot create a symlink: %v", err)
			}

			isolated := t.TempDir()
			err := linkGooseConfigDirs(isolated, []string{"HOME=" + home}, home)
			if err == nil {
				t.Fatalf("a config root at %s that is %s was carried and reported as a success", leaf, name)
			}
			if !strings.Contains(err.Error(), leaf) {
				t.Errorf("error %q does not name the path (%s)", err, leaf)
			}
			if tc.wantTarget != "" && !strings.Contains(err.Error(), tc.wantTarget) {
				t.Errorf("error %q does not name what the link points at (%s), so it names a path that is not the broken one", err, tc.wantTarget)
			}
			if !errors.Is(err, errGooseConfigBrokenLink) {
				t.Errorf("error %q does not wrap errGooseConfigBrokenLink, so a caller cannot tell this from any other isolation failure", err)
			}
			if errors.Is(err, fs.ErrNotExist) {
				t.Errorf("error %q reads as 'not there' when the link is present and something else is missing", err)
			}
			if _, err := os.Lstat(filepath.Join(isolated, ".config", "goose")); !os.IsNotExist(err) {
				t.Errorf("the broken link was carried into the isolated home anyway, Lstat err = %v", err)
			}
		})
	}
}

func mustReadLink(t *testing.T, link string) string {
	t.Helper()
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink %q: %v", link, err)
	}
	return target
}

func TestGooseIsolationRefusesAConfigRootThatIsAFile(t *testing.T) {
	home := t.TempDir()
	asFile := filepath.Join(home, ".config", "goose")
	if err := os.MkdirAll(filepath.Dir(asFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	isolated := t.TempDir()
	err := linkGooseConfigDirs(isolated, []string{"HOME=" + home}, home)
	if err == nil {
		t.Fatal("a config root that is a plain file was carried and reported as a success")
	}
	if !strings.Contains(err.Error(), asFile) {
		t.Errorf("error %q does not name the path that is not a directory (%s)", err, asFile)
	}
	if _, err := os.Lstat(filepath.Join(isolated, ".config", "goose")); !os.IsNotExist(err) {
		t.Errorf("the file was carried into the isolated home anyway, Lstat err = %v", err)
	}
}

func TestGooseChildCannotDiscoverGhostsOwnPlugin(t *testing.T) {
	realHome := t.TempDir()
	pluginDir := filepath.Join(realHome, ".agents", "plugins", "ghost")
	if err := os.MkdirAll(pluginDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "mcp.json"), []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	setHarnessPolicyParentEnv(t)
	t.Setenv("HOME", realHome)
	t.Setenv("USERPROFILE", realHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(realHome, ".config"))

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
	if err := os.MkdirAll(filepath.Join(source, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(t.TempDir(), "goose-config.yaml")
	if err := os.WriteFile(managed, []byte("provider: managed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := os.Symlink(managed, filepath.Join(source, "managed.yaml")) == nil
	if !linked {
		t.Log("this host cannot create a symlink; asserting the two plain files only")
	}

	target := filepath.Join(t.TempDir(), "goose")
	denied := func(string, string) error { return errors.New("symlink privilege not held") }
	if err := carryGooseConfigDirWith(source, target, denied); err != nil {
		t.Fatalf("carryGooseConfigDirWith: %v", err)
	}

	want := map[string]string{
		"config.yaml":  "provider: openai\n",
		"secrets.yaml": "token: x\n",
	}
	if linked {
		want["managed.yaml"] = "provider: managed\n" // reached through a symlink
	}
	for name, contents := range want {
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil {
			t.Fatalf("copied config missing %s: %v", name, err)
		}
		if string(got) != contents {
			t.Errorf("copied %s = %q, want %q", name, got, contents)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "sessions")); !os.IsNotExist(err) {
		t.Errorf("copy carried a subdirectory: %v", err)
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

// TestCodexProbeIsRekeyedWhenTheBinaryChanges is the other half of the cache's
// contract, and the half that was unprotected: an independent review reduced the
// cache key to the path alone and the whole suite stayed green, while four
// separate places in the code and the docs state that a long-lived process
// re-probes a codex upgraded IN PLACE.
//
// "In place" is the operative phrase. A user upgrading codex with npm replaces
// the binary at the same path, and that is precisely the case the size and
// mtime components of the key exist to catch. Keying on the path alone would make
// the first probe's answer permanent for the life of an MCP server that outlives
// several upgrades — a positive verdict that no longer matches the binary, which
// is the one cached answer that is actively wrong rather than merely stale.
//
// The fake is a real file, so os.Stat sees a real size and a real mtime, and the
// test rewrites it with different content so both change. It is not asserting
// anything about npm: it is asserting that a changed file is a different key,
// which is the property the docs claim.
func TestCodexProbeIsRekeyedWhenTheBinaryChanges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script policy fake requires a POSIX shell")
	}
	resetCodexFeatureProbe(t)
	captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "probes")
	rows := filepath.Join(dir, "rows")
	execLog := filepath.Join(dir, "exec")
	t.Setenv("CODEX_PROBE_LOG", log)
	t.Setenv("CODEX_EXEC_LOG", execLog)
	t.Setenv("FAKE_FEATURE_ROWS_FILE", rows)
	t.Setenv("GHOST_PASSTHROUGH_ENV", "CODEX_PROBE_LOG,CODEX_EXEC_LOG,FAKE_FEATURE_ROWS_FILE")
	// The same body in both revisions, so the only thing that differs is the
	// byte count and mtime. Padding in a comment changes one without changing the
	// other.
	const body = `
if [ "$1" = "features" ]; then
  printf 'probed\n' >> "$CODEX_PROBE_LOG"
  cat "$FAKE_FEATURE_ROWS_FILE"
  exit 0
fi
printf -- '--- invocation\n' >> "$CODEX_EXEC_LOG"
for arg in "$@"; do
  case "$arg" in
    -c) ;;
    *) printf '%s\n' "$arg" >> "$CODEX_EXEC_LOG" ;;
  esac
done
printf '%s' 'KEEP'
`
	// One stable path, rewritten in place. The shell reads the rows from a file
	// rather than an env var so the change is visible to the child without
	// depending on the parent environment changing too.
	writeRows := func(body string) {
		t.Helper()
		if err := os.WriteFile(rows, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "codex")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(bin, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	all := "shell_tool stable true\nunified_exec stable true\nview_image stable true\napps stable true\nplugins stable true\ntool_suggest stable true\nskill_mcp_dependency_install stable true\nremote_plugin stable true\nhooks stable true\nmulti_agent stable true\n"
	writeRows(all)
	write("#!/bin/sh\n" + "# revision one\n" + "#" + strings.Repeat("x", 64) + "\n" + body)
	firstInfo, err := os.Stat(bin)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if n := probeCallLog(t); n != 1 {
		t.Fatalf("probe ran %d times on the first revision, want 1", n)
	}
	if got := codexPolicyArgvKeys(t); !equalStrings(got, codexNoToolFeatureKeys) {
		t.Fatalf("first revision: child got %v, want the whole policy", got)
	}

	// Revision 2: the SAME bytes, so the size is unchanged, and a different
	// mtime. This is the only revision that can be caught by the mtime alone —
	// an earlier version of this test used one revision differing in both, and
	// dropping either component of the key still passed, because the other one
	// noticed. A tool reinstall that rewrites identical bytes is the real case
	// this is here for, so it has to be the one the assertion rests on.
	future := firstInfo.ModTime().Add(2 * time.Hour)
	if err := os.Chtimes(bin, future, future); err != nil {
		t.Fatal(err)
	}
	if second, err := os.Stat(bin); err != nil {
		t.Fatal(err)
	} else if second.Size() != firstInfo.Size() {
		t.Fatalf("fixture drifted: revision 2 is %d bytes, revision 1 was %d", second.Size(), firstInfo.Size())
	}
	writeRows("shell_tool stable true\nunified_exec stable true\n")
	if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect after the rewrite: %v", err)
	}
	if n := probeCallLog(t); n != 2 {
		t.Errorf("probe ran %d times after the binary was rewritten identically, want 2: a tool reinstall changes nothing but the mtime, and keying on the path alone would serve the old verdict forever", n)
	}

	// Revision 3: DIFFERENT bytes, and the ORIGINAL mtime restored. Only the size
	// distinguishes this from revision 1, so the size has to be in the key too.
	write("#!/bin/sh\n" + "# revision three\n" + "#" + strings.Repeat("z", 700) + "\n" + body)
	if err := os.Chtimes(bin, firstInfo.ModTime(), firstInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect after the size change: %v", err)
	}
	if n := probeCallLog(t); n != 3 {
		t.Errorf("probe ran %d times after the binary grew with its mtime restored, want 3: a replaced binary with a preserved timestamp is the case a size-only or mtime-only key misses", n)
	}
	// The current revision declares two keys, and the child must get exactly those
	// two — the same effect the filter has on a real upgrade, asserted here at the
	// end so the count above is never the only thing being checked.
	if got := codexPolicyArgvKeys(t); !equalStrings(got, []string{"shell_tool", "unified_exec"}) {
		t.Errorf("child got %v after the upgrade, want only the two keys the new codex declares", got)
	}
}
