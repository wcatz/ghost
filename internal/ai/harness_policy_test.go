package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
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

// TestHarnessInvocationArgsAreGoldens pins the exact argv each harness builds.
// A per-flag grep is what let a whole tool surface go missing: it passes when the
// flags it checks are present and says nothing about a flag that was never added
// at all. The full slice is the assertion, so a dropped or reordered element is
// a diff a reviewer reads rather than a gap a test waves through.
//
// These are the flags as the harnesses' own help and config registries document
// them, so a rename upstream shows up here as a failing golden.
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
			// A nil key set is what an unanswering probe yields, and it means
			// "pass everything" — the behaviour before the probe existed. So the
			// golden is the FULL policy, which is also the worst case.
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
		// opencode's argv splits by major version, and BOTH shapes are pinned:
		// V1 is the only one that takes --pure and the only one that may take
		// the deny policy, so a golden covering just the V2 shape would leave
		// the V1 path unpinned.
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

// TestCodexFeatureKeysAreDeclaredNames holds the codex policy to the one thing
// a CI run can check: every feature key Ghost passes must be a key codex's own
// feature registry declares. The set comes from a recorded `codex features list`
// transcript rather than a hand-written list, so a key that does not exist
// upstream cannot be asserted into existence here — the argv golden would
// happily pin a typo, and codex silently ignores a `-c` override whose key it
// does not know, which is the fail-OPEN direction this policy exists to prevent.
//
// It fixes both directions of drift: a key passed but not declared is a silent
// no-op, and a required key missing from the transcript says the transcript and
// the policy must be updated together.
//
// WHAT IT DOES NOT DO, stated plainly because the distinction is the whole
// point: a frozen transcript cannot fail when UPSTREAM renames a key. It only
// fails when Ghost's own argv drifts from the record, and an upstream rename
// leaves it passing unchanged. This test pins Ghost's BELIEF about which keys
// exist; TestLiveCodexDeclaresTheNoToolFeatureKeys is the sole detector of an
// upstream rename, and it is GHOST_LIVE_TESTS=1-gated, so it is absent from CI.
// Between them: CI keeps the belief honest, and a machine with codex installed
// is the only place the belief is actually tested.
// codexFeaturesFake builds a fake codex that answers `features list` with the
// given rows and records BOTH the probe invocations and the argv of the real
// `exec` invocation.
//
// Recording the exec argv is not optional decoration. Without it the filter
// could be deleted outright and every test in the package would still pass: the
// fakes echo back whatever Ghost hands them, so a probe test that only counts
// warnings and probe calls says nothing about which keys the child actually
// received. That was a real gap, found by an independent review that deleted the
// filter and watched the suite stay green. codexPolicyArgvKeys below is what
// closes it.
func codexFeaturesFake(t *testing.T, rows string) string {
	t.Helper()
	// The log path is a file, not a Go variable, because the fake is a shell
	// script in a child process: a shared in-process counter would be invisible
	// to it, and a channel would need a reader goroutine to outlive the test.
	//
	// Both variables reach the child through GHOST_PASSTHROUGH_ENV, the escape
	// hatch harnessEnv exists for, rather than by being added to the allowlist.
	// That is deliberate twice over: a test-only name in the production allowlist
	// would be a hole in the very policy these tests verify, and the hatch is the
	// only sanctioned way to add a variable for one invocation.
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
# features.shell_tool=false pair must survive intact.
for arg in "$@"; do
  case "$arg" in
    -c) ;;
    *) printf '%s\n' "$arg" >> "$CODEX_EXEC_LOG" ;;
  esac
done
printf '%s' 'KEEP'
`)
}

// resetCodexFeatureProbe puts the process-wide probe cache and the two warning
// latches back to their cold state. All three are deliberately process-wide (one
// probe per binary identity, one warning per verdict per process), which is
// exactly what makes them untestable without this. There are TWO latches because
// the two verdicts must not suppress each other; see the var in codex_client.go.
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

// captureCodexWarnings redirects slog for the duration of a test and returns the
// buffer holding what was logged. The warning is a WARN because it has to
// interrupt a lifecycle phase's output, and a test that cannot see it cannot
// prove it fires once rather than once per call.
func captureCodexWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &logs
}

// TestCodexProbePassesEveryKeyWhenAllAreDeclared: the ordinary case must be
// silent. A warning here would be a false alarm on every correct install, and a
// lifecycle run logs enough that one per phase would be noise.
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
	// The argv the child received, not just the absence of a warning. A codex
	// declaring every key must get every key: a filter that dropped declared keys
	// would leave a tool on and still be silent.
	if got := codexPolicyArgvKeys(t); !equalStrings(got, codexNoToolFeatureKeys) {
		t.Errorf("child got feature keys %v, want all of %v", got, codexNoToolFeatureKeys)
	}
}

// TestCodexProbeOmitsUndeclaredKeysAndWarnsOnce is the reason the probe exists.
// An older codex must get the keys it understands, be told once that its policy
// is weaker, and still be driven — refusing would fail every lifecycle call on a
// working install.
func TestCodexProbeOmitsUndeclaredKeysAndWarnsOnce(t *testing.T) {
	resetCodexFeatureProbe(t)
	logs := captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)
	// A codex predating the plugin/connector group: view_image and the four
	// keys after it are absent, and `tool_suggest` is declared so the assertion
	// covers a key that is missing in the MIDDLE of the list rather than a
	// suffix of it.
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
	// The argv is the assertion that matters. The warning text is a report; this
	// is the effect. Both directions, because the filter could be deleted (all
	// ten, silent policy claim) or inverted (only the missing ones, which
	// disables nothing) and the warning assertions would be satisfied either way.
	want := []string{"shell_tool", "unified_exec", "hooks", "tool_suggest", "multi_agent"}
	if got := codexPolicyArgvKeys(t); !equalStrings(got, want) {
		t.Errorf("child got feature keys %v, want only the five this codex declares %v", got, want)
	}
	if n := probeCallLog(t); n != 1 {
		t.Errorf("probe ran %d times over 3 calls on one binary, want 1", n)
	}
}

// equalStrings compares two string slices in order. Written rather than using
// reflect.DeepEqual so a failure prints the two lists side by side rather than
// dumping a diff of bools, and so an order change is reported: the argv order IS
// the policy, and the golden checks it.
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

// TestCodexProbeFailureFallsBackToEveryKeyAndWarnsOnce: a probe that cannot
// answer is not evidence about the codex, so it must not strip the policy. The
// pre-probe behaviour is the correct fallback, and it is said out loud.
func TestCodexProbeFailureFallsBackToEveryKeyAndWarnsOnce(t *testing.T) {
	resetCodexFeatureProbe(t)
	logs := captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)
	// Empty rows: the fake answers `features list` with a non-zero exit, which
	// is also what a codex without the subcommand does.
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
	// The fallback is the FULL policy, and it has to be asserted on argv: an
	// unanswered probe that quietly passed nothing would strip the policy while
	// emitting the reassuring "unverified" warning, which is the worst of the two
	// failure directions.
	if got := codexPolicyArgvKeys(t); !equalStrings(got, codexNoToolFeatureKeys) {
		t.Errorf("child got feature keys %v, want the whole policy %v after a failed probe", got, codexNoToolFeatureKeys)
	}
}

// TestCodexProbeIsConfinedLikeEveryOtherChild: a diagnostic is still a child.
// The probe runs `codex features list` on the user's machine, so it inherits
// whatever environment and working directory the process happens to have unless
// it goes through the same funnel as everything else — and an independent review
// found that replacing harnessCommand with a raw exec.CommandContext passed the
// whole suite, so nothing was watching.
//
// The consequence of getting that wrong is not subtle: a codex diagnostic process
// carrying AWS_SECRET_ACCESS_KEY, GITHUB_TOKEN, the age/SOPS variables and
// whatever kubeconfig resolves, running with Ghost's repository as its working
// directory, with whatever tool surface codex enables on its own. opencode's
// version probe has had this test since it was added; the codex probe is new here
// and arrived without one.
func TestCodexProbeIsConfinedLikeEveryOtherChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script policy fake requires a POSIX shell")
	}
	resetCodexFeatureProbe(t)
	captureCodexWarnings(t)
	report := filepath.Join(t.TempDir(), "probe-report")
	setHarnessPolicyParentEnv(t)
	// The hatch is set after setHarnessPolicyParentEnv, which blanks it, and it
	// carries the decoy credentials the probe must NOT see as well as the
	// passthrough itself — so a probe that inherited the parent environment would
	// carry them, which is what makes the assertion below meaningful.
	t.Setenv("GHOST_PASSTHROUGH_ENV", "GITHUB_TOKEN,AWS_SECRET_ACCESS_KEY,GHOST_API_KEY,GHOST_DATABASE_URL,CODEX_PROBE_REPORT")
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
	// Its working directory must be inside the invocation's scratch root, not the
	// repository the test was run from. A codex reading a rule or config file out
	// of the cwd is exactly the discovery problem harnessCommand exists to
	// prevent, and it is invisible in the argv.
	cwd := ""
	for _, line := range strings.Split(string(seen), "\n") {
		if after, ok := strings.CutPrefix(line, "CWD="); ok {
			cwd = after
		}
	}
	if cwd == "" {
		t.Fatalf("the probe reported no working directory:\n%s", seen)
	}
	if cwd == repoRootForProbeTest(t) {
		t.Errorf("the codex probe ran in the repository root %s, so it could discover Ghost's own rules and config", cwd)
	}
}

// repoRootForProbeTest returns the directory the test binary was run from, which
// is what a probe that inherited its working directory would report. It is read
// rather than hard-coded because `go test` runs each package in its own
// directory, and the assertion is about "wherever the repository is", not one
// path.
func repoRootForProbeTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// TestCodexProbeRunsOncePerBinaryIdentity is the cost half. A lifecycle run
// spawns hundreds of harness processes on one codex; a probe per call would
// double the process count of the whole run.
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

// TestCodexOneRealRowIsEnoughToTrustTheTable is the other edge of the shape
// test, and it is deliberately its own test. A single well-formed row establishes
// the table, so one real feature plus a stray token is a genuine answer — and the
// "unreadable" table above deliberately does NOT include that case, because
// calling it unreadable would strip a real codex's policy over one noisy line.
// Which warning is right there is a judgement, and it belongs in a test that
// states it rather than in a list of examples that only asserts the other side.
func TestCodexOneRealRowIsEnoughToTrustTheTable(t *testing.T) {
	resetCodexFeatureProbe(t)
	logs := captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)
	bin := codexFeaturesFake(t, "shell_tool stable true\nLEN:0\n")
	if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if !strings.Contains(logs.String(), "WEAKER than on a current codex") {
		t.Errorf("a table with one well-formed row was treated as unreadable, which passes every key and claims nothing:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "view_image") {
		t.Errorf("the warning does not name the keys this codex is missing, so shell_tool would not be disabled either:\n%s", logs.String())
	}
}

// TestCodexUnreadableProbeOutputIsNotAnAnswer: a positive verdict is cached for
// the life of the process, so reading output that is not a feature table as one
// strips the whole policy permanently and silently — a single token is enough,
// because `parseCodexFeaturesList` takes any line's first field as a key.
//
// This is not hypothetical. An unrelated fake codex in prompt_stdin_test.go
// answers every argument with one line, so its probe run yields something that
// parses to exactly one key. The test drives that shape directly rather than
// pointing at the other test, so the invariant does not depend on a fixture
// elsewhere staying the way it is.
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
			// The safe direction: pass everything, and say we learned nothing.
			if !strings.Contains(logs.String(), "probe did not answer") {
				t.Errorf("output %q was not treated as an unreadable probe; log was %q", tc.rows, logs.String())
			}
			if strings.Contains(logs.String(), "WEAKER than on a current codex") {
				t.Errorf("output %q was read as a codex missing features, which strips the policy: %s", tc.rows, logs.String())
			}
		})
	}
}

// TestCodexGenuinelyMissingKeysStayAnAnswer is the other side of the floor, and
// it is the case the reviewer's suggested fix would have broken. A real codex
// older than this policy answers with a full table naming features Ghost has
// never heard of and none of the ten it disables. That is an ANSWER — a genuine,
// reportable weaker policy — not "we could not read the output", which would pass
// every key and claim nothing. So the validity floor tests the table's SHAPE
// rather than its overlap with the policy's own key list.
//
// Mutation-checked: replacing the shape test with an overlap test fails this AND
// the unreadable-output test, which is why the choice is a test rather than a
// line of reasoning in a comment.
func TestCodexGenuinelyMissingKeysStayAnAnswer(t *testing.T) {
	resetCodexFeatureProbe(t)
	logs := captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)
	// A well-formed table full of real features, none of which the policy names:
	// a codex older than this policy. This must be an ANSWER, not an unreadable
	// one, because an unreadable answer passes every key and claims nothing —
	// while the truth is that this codex cannot have those surfaces switched off.
	bin := codexFeaturesFake(t, "daemon_auto_start stable true\nsqlite stable true\nfast_mode stable true\n")
	if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if !strings.Contains(logs.String(), "WEAKER than on a current codex") {
		t.Errorf("a real table naming none of the policy keys was not reported as a weaker policy:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "shell_tool") {
		t.Errorf("the warning does not name the missing keys:\n%s", logs.String())
	}
}

// TestCodexBothVerdictsCanBeReported: two latches, not one. The reachable order
// is a long-lived MCP server whose first probe fails while the binary is
// mid-upgrade, followed by a codexFeatureRetry re-ask that returns a real answer
// naming the missing keys. A single shared sync.Once would consume itself on the
// first message and silently drop the second — which is the one that says which
// surfaces are on.
func TestCodexBothVerdictsCanBeReported(t *testing.T) {
	resetCodexFeatureProbe(t)
	logs := captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)
	unanswering := codexFeaturesFake(t, "")

	if _, _, err := (&CodexClient{binary: unanswering}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if !strings.Contains(logs.String(), "probe did not answer") {
		t.Fatalf("the unverified warning did not fire:\n%s", logs.String())
	}

	// The re-ask after the retry interval returns a real, weaker answer. The fake
	// changes behaviour in place, which is what an in-place upgrade looks like
	// from here.
	t.Setenv("FAKE_FEATURE_ROWS", "shell_tool stable true\ndaemon_auto_start stable true\n")
	ageCodexFeatureCache(t, -codexFeatureRetry-time.Minute)
	if _, _, err := (&CodexClient{binary: unanswering}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect after upgrade: %v", err)
	}
	if !strings.Contains(logs.String(), "WEAKER than on a current codex") {
		t.Errorf("the weaker-policy warning was suppressed by the earlier unverified one; both verdicts must be reportable:\n%s", logs.String())
	}
	// And the reverse order, because the shared latch would fail this one too.
	resetCodexFeatureProbe(t)
	logs.Reset()
	weaker := codexFeaturesFake(t, "shell_tool stable true\ndaemon_auto_start stable true\n")
	if _, _, err := (&CodexClient{binary: weaker}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	t.Setenv("FAKE_FEATURE_ROWS", "")
	// A different binary identity, so it gets its own cache entry rather than
	// the one just stored above.
	other := codexFeaturesFake(t, "")
	if _, _, err := (&CodexClient{binary: other}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if !strings.Contains(logs.String(), "probe did not answer") {
		t.Errorf("the unverified warning was suppressed by an earlier weaker-policy one:\n%s", logs.String())
	}
}

// TestCodexProbeFailureIsCachedButExpires is the cost half of the FAILING case,
// and the two halves pull in opposite directions.
//
// A codex whose `features list` does not answer is the old-codex install this
// design exists to support, so probing it on every call would add a process to
// each of the hundreds of calls a lifecycle makes — the doubling the cache
// exists to prevent, for the whole run rather than for a window. So the negative
// is cached, and the test moves the cached entry's timestamp back instead of
// sleeping through the interval, so the expiry is asserted in milliseconds.
//
// Caching it for the life of the process is the opposite failure: an in-place
// upgrade under a long-lived MCP server would never be noticed. So the negative
// expires, and a call after the interval re-probes. The test moves the cached
// entry's timestamp rather than sleeping through the interval, so the expiry is
// asserted without a five-minute test.
func TestCodexProbeFailureIsCachedButExpires(t *testing.T) {
	resetCodexFeatureProbe(t)
	captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)
	bin := codexFeaturesFake(t, "")

	for range 4 {
		if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
			t.Fatalf("Reflect: %v", err)
		}
	}
	if n := probeCallLog(t); n != 1 {
		t.Errorf("an unanswering probe ran %d times over 4 calls, want 1 — that is the per-call probe the cache exists to avoid", n)
	}

	// Age every cached entry past the retry interval, which is what the expiry
	// check reads. The timestamp is reached through the real Store path rather
	// than by poking the struct, so this test still fails if the field the
	// expiry compares against is ever dropped.
	aged := ageCodexFeatureCache(t, -codexFeatureRetry-time.Minute)
	if aged == 0 {
		t.Fatal("no cache entry to age, so the expiry path was never reached")
	}
	if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect after expiry: %v", err)
	}
	if n := probeCallLog(t); n != 2 {
		t.Errorf("probe ran %d times after the cached negative expired, want 2 (one before, one re-ask): an in-place upgrade must still be noticed", n)
	}
}

// ageCodexFeatureCache shifts every cached probe result's timestamp by delta and
// returns how many entries it touched.
func ageCodexFeatureCache(t *testing.T, delta time.Duration) int {
	t.Helper()
	aged := 0
	codexFeatureCache.Range(func(key, value any) bool {
		support := value.(codexFeatureSupport)
		support.at = support.at.Add(delta)
		codexFeatureCache.Store(key, support)
		aged++
		return true
	})
	return aged
}

// TestCodexProbeSuccessIsCachedIndefinitely: the positive answer has no expiry,
// because a key set that was right an hour ago is still right, and re-probing it
// per call is the cost the cache exists to avoid. The asymmetry with the negative
// case above is deliberate.
func TestCodexProbeSuccessIsCachedIndefinitely(t *testing.T) {
	resetCodexFeatureProbe(t)
	captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)
	bin := codexFeaturesFake(t, "shell_tool stable true\nunified_exec stable true\nview_image stable true\napps stable true\nplugins stable true\ntool_suggest stable true\nskill_mcp_dependency_install stable true\nremote_plugin stable true\nhooks stable true\nmulti_agent stable true\n")

	for range 2 {
		if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
			t.Fatalf("Reflect: %v", err)
		}
	}
	ageCodexFeatureCache(t, -codexFeatureRetry-time.Minute)
	if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("Reflect after ageing: %v", err)
	}
	if n := probeCallLog(t); n != 1 {
		t.Errorf("a successful probe ran %d times, want 1: a positive answer is not subject to the retry interval", n)
	}
}

// codexPolicyArgvKeys returns the `features.<key>=false` keys the fake codex
// actually received on its `exec` invocation, in the order they arrived.
//
// This reads what reached the CHILD rather than what the code computed, which is
// the whole point: the unit under test is the filter's effect on argv, and a
// fake that echoes its input back proves nothing about that on its own.
func codexPolicyArgvKeys(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(os.Getenv("CODEX_EXEC_LOG"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read exec log: %v", err)
	}
	var keys []string
	for _, arg := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		key, ok := strings.CutPrefix(arg, "features.")
		if !ok {
			continue
		}
		key, _, _ = strings.Cut(key, "=")
		keys = append(keys, key)
	}
	return keys
}

// probeCallLog counts how many times the fake codex answered `features list`.
// The fake appends one line per probe to a file named by CODEX_PROBE_LOG, which
// is how a shell fake can be observed without a channel or a shared Go variable.
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
	// The reverse direction, so a key removed from the policy but still real
	// upstream is visible here rather than only in review.
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

// codexFeaturesListTranscript is the recorded output of `codex features list`
// on codex 0.147.x (2026-09-28), trimmed to the keys Ghost's policy depends on
// plus a few neighbours, so a reader can see the stage and default column. The
// third column is codex's effective state, which is the "on by default" fact
// the policy comments rest on.
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

// TestGooseNoToolsModeIsChat: goose has no flag for this (its extension options
// only ADD extensions), so the restriction is the GOOSE_MODE environment
// variable, and its value is load-bearing. "auto" is goose's own default and
// means it approves tool calls; "chat" is the mode it documents as no tool calls
// at all. A typo or a future edit that picks any other value re-opens the
// developer extension's shell on a prompt built from memory text.
func TestGooseNoToolsModeIsChat(t *testing.T) {
	if gooseNoToolsMode != "chat" {
		t.Fatalf("gooseNoToolsMode = %q, want chat", gooseNoToolsMode)
	}
	// And it must not be settable from the parent: the allowlist drops it, so a
	// user with GOOSE_MODE=auto exported cannot weaken the child.
	if _, ok := namesOf(harnessEnv([]string{"PATH=/usr/bin", "GOOSE_MODE=auto"}, harnessGoose))["GOOSE_MODE"]; ok {
		t.Error("GOOSE_MODE passed through the allowlist, so a parent value could override the child's no-tools mode")
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

// TestFirstExistingAncestor: the walk's two bounds are load-bearing. It must
// start at the path itself, or the contract in its own comment is a lie; and it
// must stop at a CLEANED home, because a HOME that is not already clean (a
// trailing separator is set by launchers and systemd units) would never match
// and the walk would climb above the home — where a file can be found and an
// unused platform location blamed for it.
func TestFirstExistingAncestor(t *testing.T) {
	home := t.TempDir()
	deep := filepath.Join(home, "Library", "Application Support", "goose")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("starts at the path itself", func(t *testing.T) {
		got, info, err := firstExistingAncestor(deep, home, os.Stat)
		if err != nil {
			t.Fatalf("firstExistingAncestor: %v", err)
		}
		if got != deep {
			t.Errorf("walk started at %q, want the path itself %q", got, deep)
		}
		if info == nil || !info.IsDir() {
			t.Errorf("info = %v, want the directory at %q", info, deep)
		}
	})

	t.Run("an unclean home still bounds the walk", func(t *testing.T) {
		outer := t.TempDir()
		inner := filepath.Join(outer, "home")
		if err := os.MkdirAll(inner, 0o700); err != nil {
			t.Fatal(err)
		}
		// The home reports absent, so the bound is the only thing that can stop
		// the walk — and an unclean bound never matches, because the walk's
		// values come from filepath.Dir and are always cleaned while
		// HOME=/home/user/ (a trailing separator is set by launchers and
		// systemd units) is not. Escaping the bound lets a location that is
		// merely unused be refused, or blamed on an unrelated file above it.
		missing := filepath.Join(inner, "no-such-dir", "goose")
		visited := make(map[string]bool)
		probe := func(name string) (os.FileInfo, error) {
			visited[name] = true
			if name == inner {
				return nil, fs.ErrNotExist
			}
			return os.Lstat(name)
		}
		if _, _, err := firstExistingAncestor(missing, inner+string(os.PathSeparator), probe); err != nil {
			t.Fatalf("firstExistingAncestor: %v", err)
		}
		for name := range visited {
			clean := filepath.Clean(name)
			if clean != inner && !strings.HasPrefix(clean, inner+string(os.PathSeparator)) {
				t.Errorf("walk probed %q, outside the home %q — the bound did not hold", name, inner)
			}
		}
	})
}

// A symlinked ancestor is a directory, not the fault this walk looks for. A
// home that is entirely a symlink, or a ~/.config pointing into a dotfiles
// repo, is ordinary — and treating the symlink as a non-directory would refuse
// a perfectly good location and fail every goose call on those machines, for a
// user who has simply never run `goose configure`.
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
	// No <link>/goose: the leaf is genuinely absent, which is the state that
	// sent the walk climbing in the first place.
	//
	// Called through the production wrapper, not with probes spelled out here,
	// so the wiring is what is under test: the two probes differ on purpose
	// (Lstat for the leaf, Stat for the walk) and a test that passes them
	// explicitly would keep passing if the wrapper regressed.
	if err := linkGooseConfigDirs(t.TempDir(), []string{"HOME=" + home}, home); err != nil {
		t.Fatalf("a symlinked ~/.config was refused: %v", err)
	}
}

// TestGooseIsolationNamesTheDirectoryItCouldNotProbe: the failure this branch
// reports is the only thing pointing at the cause, so it has to name the
// directory the walk actually failed on. Reporting the leaf's error instead
// says "no such file or directory" for a directory that exists and cannot be
// read — a reader, or errors.Is against fs.ErrNotExist, would conclude the
// location is merely unpopulated, which is the misreading this branch exists to
// prevent.
func TestGooseIsolationNamesTheDirectoryItCouldNotProbe(t *testing.T) {
	home := t.TempDir()
	blocked := filepath.Join(home, "Library", "Application Support")
	if err := os.MkdirAll(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	probe := func(name string) (os.FileInfo, error) {
		if name == blocked {
			return nil, &fs.PathError{Op: "lstat", Path: name, Err: syscall.EACCES}
		}
		return os.Lstat(name)
	}

	err := linkGooseConfigDirsWith(t.TempDir(), []string{"HOME=" + home}, home, probe, probe)
	if err == nil {
		t.Fatal("an unreadable directory above the config root was skipped")
	}
	if !strings.Contains(err.Error(), blocked) {
		t.Errorf("error %q does not name the directory that could not be probed (%s)", err, blocked)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error %q reads as 'not there' when the fault is an unreadable directory", err)
	}
}

// TestGooseIsolationCarriesASymlinkedConfigRoot is the other half of the
// not-a-directory check, and the reason that check has to exclude symlinks.
// The leaf probe is Lstat, so a link to a real directory reports IsDir false —
// and ~/.config/goose symlinked into a dotfiles repository is an ordinary setup,
// the one the Lstat leaf probe exists so the link can be CARRIED rather than
// followed. A check that refuses on IsDir alone breaks every reflect, resolve
// and supersede classification through the goose harness on those machines, and
// calls a working directory broken.
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

	// No XDG_CONFIG_HOME, so the first candidate root is the one under test.
	isolated := t.TempDir()
	if err := linkGooseConfigDirs(isolated, []string{"HOME=" + home}, home); err != nil {
		t.Fatalf("a symlinked ~/.config/goose was refused: %v", err)
	}
	// Carried as itself, so the isolated copy resolves to the user's directory
	// rather than to a copy that can drift from it.
	carried := filepath.Join(isolated, ".config", "goose")
	got, err := os.ReadFile(filepath.Join(carried, "config.yaml"))
	if err != nil {
		t.Fatalf("the symlinked config root was not carried into the isolated home: %v", err)
	}
	if string(got) != "mode: smart\n" {
		t.Errorf("carried config.yaml = %q, want the user's own file", got)
	}
	// Compared against the resolved form of real, not real itself:
	// t.TempDir hands back a SHORT path on Windows (RUNNER~1 for the profile
	// directory) and EvalSymlinks returns the long one, so comparing the two
	// strings would fail on Windows for a carry that is correct.
	wantReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", real, err)
	}
	if resolved, err := filepath.EvalSymlinks(carried); err != nil || resolved != wantReal {
		t.Errorf("the carried path resolves to %q (err %v), want the user's own %q", resolved, err, wantReal)
	}
}

// TestGooseIsolationRefusesABrokenSymlinkedConfigRoot is the other side of the
// symlink branch, and the reason it asks the walk probe what the link points at
// rather than exempting every symlink. A link to a regular file, and a link
// whose target has moved, are both a config root nobody can read — and carrying
// them into the isolated home with a success is the exact failure the
// not-a-directory check exists for, arriving through the check's own exemption.
//
// A link to a real directory is the case that must keep working, and
// TestGooseIsolationCarriesASymlinkedConfigRoot is what pins that; this is the
// other side of the same branch, and neither test passes if the two are merged
// into a single "not a directory" test.
func TestGooseIsolationRefusesABrokenSymlinkedConfigRoot(t *testing.T) {
	cases := map[string]struct {
		stage func(t *testing.T, dir string) string
		// wantTarget is the base name the message must carry, "" when the case
		// has no readable target to name.
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
			// FileInfo.Name() from the stat is the base name of the path the
			// stat was GIVEN, which is the link — so a message built from it
			// names "goose" twice and never the file that is actually wrong.
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
			// ~/.config/goose is the link, one level below the location staged
			// above, so the leaf is what the test is about.
			leaf := filepath.Join(home, ".config", "goose")
			if err := os.Symlink(mustReadLink(t, link), leaf); err != nil {
				t.Skipf("this host cannot create a symlink: %v", err)
			}

			// No XDG_CONFIG_HOME, so the first candidate root is the one under test.
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
			// A sentinel, not a wrapped platform error. For a link whose target
			// moved, os.Stat fails ENOENT while the path in the message is the
			// LINK, which is present — so wrapping that errno would make this
			// read as "merely unpopulated" and errors.Is as a missing path, the
			// misreading the walk branch two dozen lines up is written to
			// prevent and its own test pins against.
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

// mustReadLink returns the target path a staged symlink points at, so the test
// can re-link it where the production path looks.
func mustReadLink(t *testing.T, link string) string {
	t.Helper()
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink %q: %v", link, err)
	}
	return target
}

// TestGooseIsolationRefusesAConfigRootThatIsAFile: the leaf probe succeeded, so
// nothing asked whether what it found was a directory. A plain file at
// ~/.config/goose was then carried into the isolated home as itself and the
// call reported success — so goose got a config "directory" that is a file, and
// a failure that reads as a provider error with nothing pointing at the cause.
// The walk that runs when the leaf is ABSENT already asks this question, which
// is why the miss was only visible on the one path where the probe happened to
// answer.
//
// Isolation still holds either way, which is why this is a clarity fix rather
// than an escape: the call is refused before the child is spawned, and the
// message names the path.
func TestGooseIsolationRefusesAConfigRootThatIsAFile(t *testing.T) {
	home := t.TempDir()
	asFile := filepath.Join(home, ".config", "goose")
	if err := os.MkdirAll(filepath.Dir(asFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(asFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	// No XDG_CONFIG_HOME, so the first candidate root is the one under test.
	isolated := t.TempDir()
	err := linkGooseConfigDirs(isolated, []string{"HOME=" + home}, home)
	if err == nil {
		t.Fatal("a config root that is a plain file was carried and reported as a success")
	}
	if !strings.Contains(err.Error(), asFile) {
		t.Errorf("error %q does not name the path that is not a directory (%s)", err, asFile)
	}
	// Nothing was carried: the refusal has to come before the link, or the
	// child would be handed the file anyway.
	if _, err := os.Lstat(filepath.Join(isolated, ".config", "goose")); !os.IsNotExist(err) {
		t.Errorf("the file was carried into the isolated home anyway, Lstat err = %v", err)
	}
}

// TestGooseChildCannotDiscoverGhostsOwnPluginUnderPathRoot: goose resolves its
// plugin directory from GOOSE_PATH_ROOT FIRST, before any home variable. In
// goose's own Paths::get_dir, path_root() short-circuits the whole match, so
// when that variable is set the child reads
// $GOOSE_PATH_ROOT/.agents/plugins and home_dir() is never consulted — which
// means an isolated HOME confines nothing.
//
// The allowlist passes GOOSE_PATH_ROOT through, so a user who sets it (a CI
// environment, or GOOSE_PATH_ROOT=$HOME to relocate goose's own data) keeps
// pointing the child at the real plugin root, and `ghost mcp init --client
// goose` put Ghost's MCP server there. This drives the real spawn-env builder
// rather than the isolation helper, because the wiring is the claim: the
// variable has to be rewritten to a root inside the scratch home, and the real
// config has to travel with it.
func TestGooseChildCannotDiscoverGhostsOwnPluginUnderPathRoot(t *testing.T) {
	realRoot := t.TempDir()
	// The package `ghost mcp init --client goose` would have installed, and
	// the config the child authenticates from.
	plantPlugin(t, filepath.Join(realRoot, ".agents", "plugins", "ghost"))
	plantGooseConfig(t, filepath.Join(realRoot, "config"))

	bin := fakeHarnessPolicyBinary(t, "goose", `
root="$GOOSE_PATH_ROOT"
[ -e "$root/.agents/plugins/ghost/mcp.json" ] && { echo "goose child discovered Ghost's own plugin under GOOSE_PATH_ROOT=$root" >&2; exit 1; }
[ -r "$root/config/config.yaml" ] || { echo "goose child lost the config it authenticates from" >&2; exit 1; }
case "$root" in "$GHOST_SCRATCH_DIR"/*) ;; *) echo "GOOSE_PATH_ROOT not confined: $root" >&2; exit 1;; esac
printf '%s' 'KEEP'
`)

	t.Setenv("HOME", realRoot)
	t.Setenv("USERPROFILE", realRoot)
	t.Setenv("GOOSE_PATH_ROOT", realRoot)

	text, _, err := (&GooseClient{binary: bin}).Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if text != "KEEP" {
		t.Fatalf("stdout = %q", text)
	}
}

// TestGooseSubprocessEnvFallsBackToTempDirWhenScratchUnavailable covers the goose
// fallback specifically: an unusable scratch root must not fail the run, and the
// child still gets an isolated HOME — and an isolated GOOSE_PATH_ROOT, which is
// the one that governs plugin discovery — inside a private MkdirTemp tree that
// the returned cleanup removes.
//
// It mirrors TestSubprocessEnvFallsBackToTempDirWhenScratchUnavailable for
// opencode. The fallback is the branch that runs when a user's data dir is
// broken, which is exactly when nobody is watching for a boundary quietly
// reopening, so the boundary has to be asserted here rather than assumed.
func TestGooseSubprocessEnvFallsBackToTempDirWhenScratchUnavailable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHOST_SCRATCH_DIR", filepath.Join(blocker, "scratch"))
	tmp := t.TempDir()
	for _, key := range tempDirKeys {
		t.Setenv(key, tmp)
	}

	realHome := t.TempDir()
	plantPlugin(t, filepath.Join(realHome, ".agents", "plugins", "ghost"))
	// Only the path-root layout: GOOSE_PATH_ROOT is set, so that is the config
	// the child reads.
	plantGooseConfig(t, filepath.Join(realHome, "config"))
	t.Setenv("HOME", realHome)
	t.Setenv("USERPROFILE", realHome)
	t.Setenv("GOOSE_PATH_ROOT", realHome)
	// Pinned empty, because an inherited XDG_CONFIG_HOME changes which layout
	// this asserts: the child would read it instead of $ROOT/config. A test
	// that passes on a laptop and fails in CI over an ambient variable is not
	// a test of the fallback.
	t.Setenv("XDG_CONFIG_HOME", "")

	client := &GooseClient{binary: "goose"}
	cmd, cleanup, err := client.subprocessEnv(context.Background(), []string{"run", "-q"})
	if err != nil {
		t.Fatalf("subprocessEnv: %v", err)
	}
	defer cleanup()

	if !strings.Contains(cmd.Dir, "ghost-goose-") {
		t.Fatalf("fallback cmd.Dir = %q, want a private ghost-goose- tree", cmd.Dir)
	}
	for _, key := range []string{"HOME", "GOOSE_PATH_ROOT"} {
		got := envValue(cmd.Env, key)
		if got == "" {
			t.Errorf("%s is empty in the fallback child", key)
			continue
		}
		if got == realHome {
			t.Errorf("%s = %q — the fallback restored the real home", key, got)
		}
		if !strings.HasPrefix(got, cmd.Dir+string(os.PathSeparator)) {
			t.Errorf("%s = %q, want it inside the fallback dir %q", key, got, cmd.Dir)
		}
	}
	for _, key := range tempDirKeys {
		if got := envValue(cmd.Env, key); got != cmd.Dir {
			t.Errorf("%s = %q, want the fallback dir %q", key, got, cmd.Dir)
		}
	}
	// The config the child authenticates from has to have travelled. With
	// GOOSE_PATH_ROOT set, goose reads $ROOT/config and the home-relative roots
	// are deliberately not reproduced — path_root() short-circuits before any
	// home variable — so this asserts the one layout the child will use.
	if _, err := os.Stat(filepath.Join(envValue(cmd.Env, "GOOSE_PATH_ROOT"), "config", "config.yaml")); err != nil {
		t.Errorf("fallback child lost the path-root config: %v", err)
	}
	if _, err := os.Stat(filepath.Join(envValue(cmd.Env, "GOOSE_PATH_ROOT"), "config", "secrets.yaml")); err != nil {
		t.Errorf("fallback child lost the credentials it authenticates with: %v", err)
	}
}

// plantPlugin writes the package `ghost mcp init --client goose` installs.
func plantPlugin(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// plantGooseConfig writes a config directory with the files goose reads, one of
// which carries the credentials the child needs to authenticate.
func plantGooseConfig(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"config.yaml":  "provider: openai\n",
		"secrets.yaml": "token: carried\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A relative GOOSE_PATH_ROOT is not a plugin root at all: goose's
// validated_path_root drops anything that is not absolute, so the child falls
// back to the home variables this code already confines. Rewriting it would be
// changing a variable the child ignores, and would repoint it at a scratch path
// in case some build stops filtering.
func TestGooseIsolationLeavesARelativePathRootAlone(t *testing.T) {
	home := t.TempDir()
	cmd := &exec.Cmd{Dir: t.TempDir(), Env: []string{"HOME=" + home, "GOOSE_PATH_ROOT=relative/goose"}}
	if err := configureGooseIsolation(cmd); err != nil {
		t.Fatalf("configureGooseIsolation: %v", err)
	}
	if got := envValue(cmd.Env, "GOOSE_PATH_ROOT"); got != "relative/goose" {
		t.Errorf("GOOSE_PATH_ROOT = %q, want the value left untouched", got)
	}
	// The home is still confined, so the fallback the relative value triggers
	// cannot reach the real plugin root.
	if got := envValue(cmd.Env, "HOME"); got == home {
		t.Error("HOME was not confined alongside a relative path root")
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
	//
	// Creating the link can itself be refused — it is the same privilege this
	// test exists for — so the expectation is built conditionally instead of
	// skipping. Skipping would abandon the whole copy branch on exactly the
	// host that needs it, which is where a regression would be invisible.
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
// file where a directory belongs" as ERROR_PATH_NOT_FOUND and maps ENOTDIR to
// that same constant, so on that host this case legitimately classifies as
// absent. The platform's own answer is honoured either way; what is pinned is
// that a non-ENOENT failure is never skipped, which EACCES states on every
// platform.
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
		if err := linkGooseConfigDirsWith(isolated, []string{"HOME=" + home}, home, probe, probe); err != nil {
			t.Fatalf("a missing root must be skipped, not refused: %v", err)
		}
		if _, err := os.Stat(filepath.Join(isolated, ".config", "goose", "config.yaml")); err != nil {
			t.Fatalf("the present root was not carried: %v", err)
		}
	})

	t.Run("unusable is refused", func(t *testing.T) {
		// EACCES, not ENOTDIR: on Windows syscall.ENOTDIR is
		// ERROR_PATH_NOT_FOUND, which Go maps to fs.ErrNotExist, so a test
		// using it asserts a different rule on that platform. EACCES is not
		// ErrNotExist on any of them.
		probe := func(name string) (os.FileInfo, error) {
			if strings.Contains(name, "Library") {
				return nil, &fs.PathError{Op: "lstat", Path: name, Err: syscall.EACCES}
			}
			return os.Lstat(name)
		}
		if err := linkGooseConfigDirsWith(isolated, []string{"HOME=" + home}, home, probe, probe); err == nil {
			t.Fatal("a root that exists but is unusable was skipped as if absent")
		}
	})

	// A file where the config root's PARENT belongs — "~/.config is a file" —
	// must fail closed on every platform, so the shape is arranged on the real
	// filesystem rather than mocked. A mocked leaf plus a real parent probe
	// would pass on Linux and fail on Windows, where the same shape is reported
	// as ERROR_PATH_NOT_FOUND and maps to ErrNotExist.
	t.Run("non-directory parent is refused", func(t *testing.T) {
		shapedHome := t.TempDir()
		if err := os.WriteFile(filepath.Join(shapedHome, ".config"), []byte("not a directory\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := linkGooseConfigDirsWith(t.TempDir(), []string{"HOME=" + shapedHome}, shapedHome, os.Lstat, os.Stat); err == nil {
			t.Fatal("a config root behind a non-directory parent was skipped as if absent")
		}
	})

	// The ordinary case the ancestor walk must not break: ~/.config exists,
	// the user has never run `goose configure`, so there is no config dir. A
	// machine in exactly this state — no goose config at all — has to keep
	// working, and its config comes from the environment.
	t.Run("unpopulated leaf behind a real directory is skipped", func(t *testing.T) {
		shapedHome := t.TempDir()
		if err := os.MkdirAll(filepath.Join(shapedHome, ".config"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := linkGooseConfigDirsWith(t.TempDir(), []string{"HOME=" + shapedHome}, shapedHome, os.Lstat, os.Stat); err != nil {
			t.Fatalf("a location the user has not populated must be skipped, not refused: %v", err)
		}
	})

	// The conflation reaches deeper than the leaf, which is why a one-level
	// parent check is not enough. With ~/Library a file, Windows reports
	// ~/Library/Application Support/goose as not-found, and so reports
	// ~/Library/Application Support as not-found too — so walking up one level
	// sees nothing and the fault goes unreported. The walk has to keep going
	// until something actually exists, and then ask whether it is a directory.
	t.Run("a file above the parent is still found", func(t *testing.T) {
		shapedHome := t.TempDir()
		if err := os.WriteFile(filepath.Join(shapedHome, "Library"), []byte("not a directory\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		// Model the Windows errno for the paths THROUGH the blocking file,
		// which is what it collapses to not-found; the file itself still
		// stats, and that is exactly what the walk has to find.
		probe := func(name string) (os.FileInfo, error) {
			if strings.Contains(name, filepath.Join("Library", "Application Support")) {
				return nil, fs.ErrNotExist
			}
			return os.Lstat(name)
		}
		if err := linkGooseConfigDirsWith(t.TempDir(), []string{"HOME=" + shapedHome}, shapedHome, probe, probe); err == nil {
			t.Fatal("a file above the parent was walked past as if the location were simply unused")
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
