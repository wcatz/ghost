package ai

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeOpenCodeBinary writes a shell script standing in for `opencode` that
// emits the given lines to stdout, so run()'s plumbing can be verified without
// a real opencode install.
func fakeOpenCodeBinary(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	// run() opens a scratch dir on every invocation; pin the root to the
	// test's temp dir so the real data dir is never touched.
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	return path
}

func TestParseOpenCodeOutput_ConcatenatesTextEvents(t *testing.T) {
	raw := `{"type":"step_start","timestamp":1,"part":{"type":"step-start"}}
{"type":"text","timestamp":2,"part":{"id":"a","type":"text","text":"{\"memories\":"}}
{"type":"text","timestamp":3,"part":{"id":"b","type":"text","text":"[]}"}}
{"type":"step_finish","timestamp":4,"part":{"id":"c","type":"step-finish","reason":"stop"}}
`
	got, err := parseOpenCodeOutput(raw)
	if err != nil {
		t.Fatalf("parseOpenCodeOutput: %v", err)
	}
	if got != `{"memories":[]}` {
		t.Errorf("got %q, want concatenated text events", got)
	}
}

func TestParseOpenCodeOutput_IgnoresNonText(t *testing.T) {
	raw := `{"type":"step_start","part":{"type":"step-start"}}
{"type":"reasoning","part":{"type":"reasoning","text":"thinking aloud"}}
{"type":"text","part":{"id":"a","type":"text","text":"OK"}}
`
	got, err := parseOpenCodeOutput(raw)
	if err != nil {
		t.Fatalf("parseOpenCodeOutput: %v", err)
	}
	if got != "OK" {
		t.Errorf("got %q, want only text events", got)
	}
}

func TestParseOpenCodeOutput_MalformedLineErrors(t *testing.T) {
	raw := "not json at all\n"
	if _, err := parseOpenCodeOutput(raw); err == nil {
		t.Fatal("expected error for malformed JSON line")
	}
}

func TestParseOpenCodeOutput_BlankLinesSkipped(t *testing.T) {
	raw := `{"type":"text","part":{"id":"a","type":"text","text":"OK"}}

{"type":"text","part":{"id":"b","type":"text","text":" fine"}}
`
	got, err := parseOpenCodeOutput(raw)
	if err != nil {
		t.Fatalf("parseOpenCodeOutput: %v", err)
	}
	if got != "OK fine" {
		t.Errorf("got %q, want blank lines skipped", got)
	}
}

func TestOpenCodeClient_Reflect_ReturnsConcatenatedText(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `printf '%s\n' '{"type":"text","part":{"type":"text","text":"HELLO"}}' '{"type":"text","part":{"type":"text","text":" WORLD"}}'`)
	c := &OpenCodeClient{binary: bin}
	text, usage, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if text != "HELLO WORLD" {
		t.Errorf("got %q, want %q", text, "HELLO WORLD")
	}
	if usage != (TokenUsage{}) {
		t.Errorf("expected zero TokenUsage, got %+v", usage)
	}
}

func TestOpenCodeClient_Run_PropagatesStderrOnFailure(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `echo "boom" >&2; exit 1`)
	c := &OpenCodeClient{binary: bin}
	_, _, err := c.Reflect(context.Background(), "prompt")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("expected error to include stderr, got %v", err)
	}
}

func TestOpenCodeClient_Classify_JoinsSystemAndUserIntoPrompt(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `for last; do :; done
case "$last" in *SYSTEM*USER*) printf '%s\n' '{"type":"text","part":{"type":"text","text":"KEEP"}}';; *) echo "prompt not joined" >&2; exit 1;; esac`)
	c := &OpenCodeClient{binary: bin}
	text, err := c.Classify(context.Background(), "SYSTEM", "USER")
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if text != "KEEP" {
		t.Errorf("got %q, want %q", text, "KEEP")
	}
}

func TestOpenCodeClient_ScrubsEnv(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `
if [ -n "$ANTHROPIC_API_KEY" ]; then echo "LEAKED API KEY" >&2; exit 1; fi
case "$XDG_CONFIG_HOME" in
  "$GHOST_SCRATCH_DIR"/*) ;;
  *) echo "XDG_CONFIG_HOME not confined to the scratch root: $XDG_CONFIG_HOME" >&2; exit 1;;
esac
printf '%s\n' '{"type":"text","part":{"type":"text","text":"OK"}}'
`)
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-leak")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/fake-real-config")
	c := &OpenCodeClient{binary: bin}
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if text != "OK" {
		t.Errorf("got %q, want OK", text)
	}
}

// TestOpenCodeClient_ModelFlagFromEnv: when GHOST_OPENCODE_MODEL is set the
// subprocess invocation must carry `-m <model>` so callers can pin the model
// (e.g. deepseek) despite opencode's scrubbed config dir.
func TestOpenCodeClient_ModelFlagFromEnv(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `printf '%s\n' '{"type":"text","part":{"type":"text","text":"'"$*"'"}}'`)
	t.Setenv("GHOST_OPENCODE_MODEL", "deepseek/deepseek-v4")
	c := &OpenCodeClient{binary: bin}
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if !strings.Contains(text, "-m deepseek/deepseek-v4") || !strings.Contains(text, "--pure") {
		t.Fatalf("model flag not passed, got %q", text)
	}
}

// TestOpenCodeClient_DefaultModelFlag: with GHOST_OPENCODE_MODEL unset or
// empty, the scrubbed child still receives Ghost's explicit Big Pickle default.
func TestOpenCodeClient_DefaultModelFlag(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `printf '%s\n' '{"type":"text","part":{"type":"text","text":"'"$*"'"}}'`)
	t.Setenv("GHOST_OPENCODE_MODEL", "")
	c := &OpenCodeClient{binary: bin}
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if !strings.Contains(text, "-m opencode/big-pickle") {
		t.Fatalf("default model flag missing: %q", text)
	}
}

// TestOpenCodeClient_ConstructorModelWinsOverEnv verifies that a
// constructor-level model pin takes precedence over GHOST_OPENCODE_MODEL
// (important for long-lived MCP servers that must not mutate process env).
func TestOpenCodeClient_ConstructorModelWinsOverEnv(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `printf '%s\n' '{"type":"text","part":{"type":"text","text":"'"$*"'"}}'`)
	t.Setenv("GHOST_OPENCODE_MODEL", "deepseek/deepseek-v4")
	c := NewOpenCodeClientWithBinaryAndModel(bin, "opencode/big-pickle")
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if !strings.Contains(text, "-m opencode/big-pickle") {
		t.Fatalf("expected constructor model flag, got %q", text)
	}
	if strings.Contains(text, "deepseek/deepseek-v4") {
		t.Fatalf("env model must not leak when constructor model is set: %q", text)
	}
}

// TestSubprocessEnvConfinesTempDir pins the fix for the opencode JIT-cache
// leak: opencode writes a hidden ~4.7 MiB shared object into its temp dir on
// every invocation, and a single lifecycle spawns hundreds of processes, so
// inheriting the shared temp dir accumulates gigabytes until the filesystem
// fills and every LLM-backed phase silently fails. The child's temp-dir
// variables must therefore point at the per-invocation scratch dir under
// Ghost's owned root, which the returned cleanup removes.
func TestSubprocessEnvConfinesTempDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	decoy := t.TempDir()
	for _, key := range tempDirKeys {
		t.Setenv(key, decoy)
	}
	t.Setenv("XDG_CONFIG_HOME", decoy)

	client := &OpenCodeClient{binary: "opencode"}
	cmd, cleanup, err := client.subprocessEnv(context.Background(), []string{"run"}, openCodeAskConfig)
	if err != nil {
		t.Fatalf("subprocessEnv: %v", err)
	}
	env := cmd.Env
	scratchDir := envValue(env, "XDG_CONFIG_HOME")
	if scratchDir == "" {
		t.Fatal("XDG_CONFIG_HOME not set")
	}
	if scratchDir == decoy {
		t.Fatal("XDG_CONFIG_HOME still points at the inherited value")
	}
	if got := filepath.Dir(cmd.Dir); got != root {
		t.Errorf("scratch dir parent = %q, want the owned root %q", got, root)
	}
	if got := filepath.Dir(scratchDir); got != cmd.Dir {
		t.Errorf("config dir parent = %q, want the invocation dir %q", got, cmd.Dir)
	}

	for _, key := range tempDirKeys {
		if got := envValue(env, key); got != cmd.Dir {
			t.Errorf("%s = %q, want the invocation dir %q", key, got, cmd.Dir)
		}
	}
	// Each variable must appear once: a stale inherited entry would win or lose
	// depending on the reader.
	for _, key := range tempDirKeys {
		count := 0
		for _, kv := range env {
			if k, _, _ := strings.Cut(kv, "="); strings.EqualFold(k, key) {
				count++
			}
		}
		if count != 1 {
			t.Errorf("%s appears %d times, want exactly 1", key, count)
		}
	}
	if _, err := os.Stat(scratchDir); err != nil {
		t.Fatalf("scratch dir missing before cleanup: %v", err)
	}
	cleanup()
	if _, err := os.Stat(scratchDir); !os.IsNotExist(err) {
		t.Errorf("scratch dir %s survived cleanup (err=%v)", scratchDir, err)
	}
	cleanup() // must be safe to call twice
}

// TestHarnessCommandConfinesEveryClient pins that all four harness clients run
// through the one confinement helper: each gets a working directory directly
// under GHOST_SCRATCH_DIR and exactly one copy of every temp-dir variable,
// pinned there — including when the inherited value uses different case, which
// must be overridden rather than shadowed. No harness is spawned; this is a
// pure env- and command-building assertion.
func TestHarnessCommandConfinesEveryClient(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	decoy := t.TempDir()
	env := []string{
		"PATH=/usr/bin",
		"tmpdir=" + decoy,
		"TMP=" + decoy,
		"TEMP=" + decoy,
	}
	for _, harness := range []struct {
		name string
		kind harnessKind
	}{
		{name: "claude", kind: harnessClaude},
		{name: "codex", kind: harnessCodex},
		{name: "goose", kind: harnessGoose},
		{name: "opencode", kind: harnessOpencode},
	} {
		t.Run(harness.name, func(t *testing.T) {
			cmd, release, ok := harnessCommand(context.Background(), "true", nil, env, harness.kind)
			if !ok {
				t.Fatal("scratch confinement unexpectedly unavailable")
			}
			defer release()
			if got := filepath.Dir(cmd.Dir); got != root {
				t.Errorf("cmd.Dir = %q, want a direct child of the root %q", cmd.Dir, root)
			}
			for _, key := range tempDirKeys {
				if got := envValue(cmd.Env, key); got != cmd.Dir {
					t.Errorf("%s = %q, want the scratch dir %q", key, got, cmd.Dir)
				}
				count := 0
				for _, kv := range cmd.Env {
					if k, _, _ := strings.Cut(kv, "="); strings.EqualFold(k, key) {
						count++
					}
				}
				if count != 1 {
					t.Errorf("%s appears %d times, want exactly 1", key, count)
				}
			}
		})
	}
}

// TestHarnessCommandFallsBackWhenScratchUnavailable proves the degradation is
// a fallback, not a failure: with an unusable scratch root the command is
// still built with the allowlisted environment and the caller's working
// directory, and release is a safe no-op.
func TestHarnessCommandFallsBackWhenScratchUnavailable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHOST_SCRATCH_DIR", filepath.Join(blocker, "scratch"))

	env := []string{"PATH=/usr/bin", "TMPDIR=/inherited"}
	cmd, release, ok := harnessCommand(context.Background(), "true", nil, env, harnessClaude)
	if ok {
		t.Fatal("harnessCommand reported confinement with an unusable scratch root")
	}
	release()
	release() // must be safe to call twice
	if cmd.Dir != "" {
		t.Errorf("cmd.Dir = %q, want the inherited working directory", cmd.Dir)
	}
	if got := envValue(cmd.Env, "TMPDIR"); got != "/inherited" {
		t.Errorf("TMPDIR = %q, want the inherited value", got)
	}
}

// TestSubprocessEnvFallsBackToTempDirWhenScratchUnavailable covers the opencode
// fallback specifically: an unusable scratch root must not fail the run, and
// the child still gets a private MkdirTemp working/config tree and temp dir,
// removed by the returned cleanup.
func TestSubprocessEnvFallsBackToTempDirWhenScratchUnavailable(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHOST_SCRATCH_DIR", filepath.Join(blocker, "scratch"))
	tmp := t.TempDir()
	for _, key := range tempDirKeys {
		t.Setenv(key, tmp)
	}

	client := &OpenCodeClient{binary: "opencode"}
	cmd, cleanup, err := client.subprocessEnv(context.Background(), []string{"run"}, openCodeAskConfig)
	if err != nil {
		t.Fatalf("subprocessEnv: %v", err)
	}
	dir := envValue(cmd.Env, "XDG_CONFIG_HOME")
	if dir == "" || !strings.Contains(filepath.Dir(dir), "ghost-opencode-") {
		t.Fatalf("fallback config dir = %q, want a ghost-opencode- isolated tree", dir)
	}
	if filepath.Dir(dir) != cmd.Dir || !strings.Contains(cmd.Dir, "ghost-opencode-") {
		t.Errorf("fallback cmd.Dir = %q, want the private parent of %q", cmd.Dir, dir)
	}
	for _, key := range tempDirKeys {
		if got := envValue(cmd.Env, key); got != cmd.Dir {
			t.Errorf("%s = %q, want the fallback dir %q", key, got, cmd.Dir)
		}
	}
	if _, err := os.Stat(cmd.Dir); err != nil {
		t.Fatalf("fallback dir missing before cleanup: %v", err)
	}
	cleanup()
	if _, err := os.Stat(cmd.Dir); !os.IsNotExist(err) {
		t.Errorf("fallback dir %s survived cleanup (err=%v)", cmd.Dir, err)
	}
	cleanup() // must be safe to call twice
}

// envValue returns the value of key in an environment slice, or "".
func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, found := strings.Cut(kv, "="); found && strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// versionedOpenCodeBinary is a fake opencode that answers `--version` with
// version and echoes every other invocation's argv back as a text event.
func versionedOpenCodeBinary(t *testing.T, version string) string {
	t.Helper()
	return fakeOpenCodeBinary(t, `if [ "$1" = "--version" ]; then echo '`+version+`'; exit 0; fi
printf '%s\n' '{"type":"text","part":{"type":"text","text":"'"$*"'"}}'`)
}

// TestOpenCodeClient_V1Flags: opencode V1 keeps `--pure` (skip plugins) and
// never gets V2's `--standalone`.
func TestOpenCodeClient_V1Flags(t *testing.T) {
	c := &OpenCodeClient{binary: versionedOpenCodeBinary(t, "1.18.32")}
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if !strings.Contains(text, "--pure") || strings.Contains(text, "--standalone") {
		t.Fatalf("V1 args = %q, want --pure and no --standalone", text)
	}
}

// TestOpenCodeClient_V2Flags: opencode V2 rejects `--pure` ("Unrecognized
// flag"), and without `--standalone` its `run` attaches to the user's shared
// background service — which loads the user's real config, including Ghost's
// own plugin and MCP server. V2 must get --standalone and never --pure.
func TestOpenCodeClient_V2Flags(t *testing.T) {
	c := &OpenCodeClient{binary: versionedOpenCodeBinary(t, "opencode v2.0.15")}
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if strings.Contains(text, "--pure") || !strings.Contains(text, "--standalone") {
		t.Fatalf("V2 args = %q, want --standalone and no --pure", text)
	}
	for _, want := range []string{"run", "--format json", "--title [ghost]", "prompt"} {
		if !strings.Contains(text, want) {
			t.Errorf("V2 args = %q, missing %q", text, want)
		}
	}
}

func TestOpencodeMajorVersion(t *testing.T) {
	cases := map[string]int{
		"1.18.32\n":          1,
		"opencode v2.0.15\n": 2,
		"v3.1.0":             3,
		"":                   0,
		`{"type":"text"}`:    0,
	}
	for in, want := range cases {
		if got := OpencodeMajorVersion(in); got != want {
			t.Errorf("OpencodeMajorVersion(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestOpenCodeClient_VersionCacheFollowsBinaryUpgrade: a long-lived Ghost
// process (the MCP server) must notice an opencode upgrade in place. The
// version cache is keyed by the resolved binary's identity, so replacing the
// file (V1 -> V2 here) re-probes instead of reusing stale flags.
func TestOpenCodeClient_VersionCacheFollowsBinaryUpgrade(t *testing.T) {
	bin := versionedOpenCodeBinary(t, "1.18.32")
	c := &OpenCodeClient{binary: bin}
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil || !strings.Contains(text, "--pure") {
		t.Fatalf("before upgrade: args=%q err=%v, want V1 --pure", text, err)
	}

	upgraded := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'opencode v2.0.15'; exit 0; fi
printf '%s\n' '{"type":"text","part":{"type":"text","text":"'"$*"'"}}'`
	if err := os.WriteFile(bin, []byte(upgraded), 0o755); err != nil {
		t.Fatalf("upgrade fake binary: %v", err)
	}
	text, _, err = c.Reflect(context.Background(), "prompt")
	if err != nil || !strings.Contains(text, "--standalone") || strings.Contains(text, "--pure") {
		t.Fatalf("after upgrade: args=%q err=%v, want V2 --standalone", text, err)
	}
}
