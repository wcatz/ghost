package ai

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fakeOpenCodeBinary(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
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

// TestParseOpenCodeOutput_AcceptsALongReflectReply is the #608 case. The reader
// reader outgrew in #632, so a reflect reply that reached it failed with
func TestParseOpenCodeOutput_AcceptsALongReflectReply(t *testing.T) {
	big := strings.Repeat("A", 5<<20)
	raw := `{"type":"text","part":{"id":"a","type":"text","text":"` + big + `"}}` + "\n"

	got, err := parseOpenCodeOutput(raw)
	if err != nil {
		t.Fatalf("a text event of %d bytes failed to parse: %v", len(big), err)
	}
	if got != big {
		t.Errorf("got %d bytes, want the %d the event carried", len(got), len(big))
	}
}

func TestParseOpenCodeOutput_StillBoundsALinePastTheCeiling(t *testing.T) {
	old := maxOpencodeOutputLine
	maxOpencodeOutputLine = 4 << 20
	t.Cleanup(func() { maxOpencodeOutputLine = old })

	raw := `{"type":"text","part":{"id":"a","type":"text","text":"` + strings.Repeat("A", (4<<20)+1) + `"}}` + "\n"
	if _, err := parseOpenCodeOutput(raw); err == nil {
		t.Fatal("a line past the ceiling parsed, so nothing bounds the read")
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

// on stdin (issue #560), not as a positional argument.
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

// TestOpenCodeClient_SessionStoreStaysInScratchRoot pins the #588 isolation:
func TestOpenCodeClient_SessionStoreStaysInScratchRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	bin := fakeOpenCodeBinary(t, `
case "$HOME" in
  "$GHOST_SCRATCH_DIR"/*) ;;
  *) echo "HOME not inside the scratch root: $HOME" >&2; exit 1;;
esac
case "$XDG_DATA_HOME" in
  "$GHOST_SCRATCH_DIR"/*) ;;
  *) echo "XDG_DATA_HOME not inside the scratch root: $XDG_DATA_HOME" >&2; exit 1;;
esac
mkdir -p "$XDG_DATA_HOME/opencode/storage/session" || exit 1
printf '%s\n' '{}' > "$XDG_DATA_HOME/opencode/storage/session/ses_ghost.json" || exit 1
printf '%s\n' '{"type":"text","part":{"type":"text","text":"'"$XDG_DATA_HOME"'"}}'
`)
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	home := t.TempDir()
	userData := filepath.Join(home, ".local", "share")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", userData)

	c := &OpenCodeClient{binary: bin}
	dataDir, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if !strings.HasPrefix(dataDir, root+string(os.PathSeparator)) {
		t.Errorf("child XDG_DATA_HOME = %q, want it inside the scratch root %q", dataDir, root)
	}
	if strings.HasPrefix(dataDir, home) {
		t.Errorf("child XDG_DATA_HOME = %q is inside the user's home %q", dataDir, home)
	}
	if _, err := os.Stat(userData); !os.IsNotExist(err) {
		t.Errorf("the user's data dir %s must stay untouched (stat err=%v)", userData, err)
	}
}

// the #588 isolation, and the reason Ghost does not delete each run's session
// authenticate (issue #627) without the user's config or MCP tree.
func TestOpenCodeClient_SessionStoreDiesWithTheInvocation(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `
store="$XDG_DATA_HOME/opencode"
mkdir -p "$store" || exit 1
printf 'sqlite-store' > "$store/opencode.db" || exit 1
printf '[ghost]' > "$store/session" || exit 1
if [ -r "$store/auth.json" ]; then auth=$(cat "$store/auth.json"); else auth=MISSING; fi
printf '%s\n' '{"type":"text","part":{"type":"text","text":"store='"$store"' ;; auth='"$auth"' ;; "}}'
`)
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)

	const credential = "ghost-test-credential-sentinel"
	home := t.TempDir()
	authDir := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("seed auth dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(credential), 0o600); err != nil {
		t.Fatalf("seed auth file: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	t.Setenv("OPENCODE_API_KEY", "")

	c := &OpenCodeClient{binary: bin}
	report, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	store, auth, ok := strings.Cut(report, " ;; ")
	if !ok {
		t.Fatalf("fake binary reported %q, want a store=... ;; auth=... report", report)
	}
	store = strings.TrimPrefix(store, "store=")
	auth = strings.TrimSuffix(strings.TrimPrefix(auth, "auth="), " ;; ")

	if auth != credential {
		t.Errorf("auth the child could read = %q, want the carried credential %q", auth, credential)
	}
	if !strings.HasPrefix(store, root+string(os.PathSeparator)) {
		t.Fatalf("child store %q is not inside the scratch root %q", store, root)
	}
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Errorf("session store %s survived the call (stat err=%v) — it will accumulate", store, err)
	}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "opencode.db" {
			t.Errorf("session store %s left under the scratch root", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk scratch root: %v", err)
	}
}

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
			cmd, release, err := harnessCommand(context.Background(), "true", nil, env, harness.kind)
			if err != nil {
				t.Fatalf("harnessCommand: %v", err)
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

func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, found := strings.Cut(kv, "="); found && strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func versionedOpenCodeBinary(t *testing.T, version string) string {
	t.Helper()
	return fakeOpenCodeBinary(t, `if [ "$1" = "--version" ]; then echo '`+version+`'; exit 0; fi
printf '%s\n' '{"type":"text","part":{"type":"text","text":"'"$*"'"}}'`)
}

func TestOpenCodeClient_V2Flags(t *testing.T) {
	c := &OpenCodeClient{binary: versionedOpenCodeBinary(t, "opencode v2.0.15")}
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if strings.Contains(text, "--pure") || !strings.Contains(text, "--standalone") {
		t.Fatalf("V2 args = %q, want --standalone and no --pure", text)
	}
	if strings.Contains(text, "prompt") {
		t.Errorf("V2 args = %q, must not carry the prompt as an argv element", text)
	}
	for _, want := range []string{"run", "--format json", "--title [ghost]"} {
		if !strings.Contains(text, want) {
			t.Errorf("V2 args = %q, missing %q", text, want)
		}
	}
}

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

func TestOpenCodeClient_UnparseableVersionIsNotCached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	probeLog := filepath.Join(t.TempDir(), "probes")
	bin := fakeOpenCodeBinary(t, `
if [ "$1" = "--version" ]; then
  n=0
  [ -f '`+probeLog+`' ] && n=$(wc -l < '`+probeLog+`')
  n=$((n+1))
  echo "$n" >> '`+probeLog+`'
  if [ "$n" -eq 1 ]; then echo 'garbled version output'; else echo 'opencode v2.0.15'; fi
  exit 0
fi
for arg in "$@"; do
  if [ "$arg" = "--pure" ]; then echo 'unknown flag: --pure was removed' >&2; exit 1; fi
done
printf '%s\n' '{"type":"text","part":{"type":"text","text":"OK"}}'
`)
	c := &OpenCodeClient{binary: bin}
	if _, _, err := c.Reflect(context.Background(), "prompt"); err == nil {
		t.Fatal("first call read a garbled version as V1, and this fake rejects --pure")
	}
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("second Reflect: %v", err)
	}
	if text != "OK" {
		t.Errorf("second Reflect = %q, want OK from the V2 invocation", text)
	}
	raw, err := os.ReadFile(probeLog)
	if err != nil {
		t.Fatalf("read probe log: %v", err)
	}
	if probes := strings.Count(string(raw), "\n"); probes != 2 {
		t.Errorf("version probes = %d, want 2: an unparseable answer must be asked again", probes)
	}
}

const declinedStream = `printf '%s\n' '{"type":"step_start","part":{"type":"step-start"}}' '{"type":"text","part":{"type":"text","text":"ANSWER: 42"}}' '{"type":"error","error":{"type":"aborted","message":"Session interrupted: shutdown"}}' '{"type":"tool_use","part":{"type":"tool","tool":"read","state":{"status":"error","error":"The user declined this tool call"}}}'; echo 'permission requested: external_directory (/etc/*); auto-rejecting' >&2; exit 1`

const declinedStreamThenSleep = `if [ "$1" = "--version" ]; then echo 'opencode v2.0.15'; exit 0; fi
cat >/dev/null
printf '%s\n' '{"type":"step_start","part":{"type":"step-start"}}' '{"type":"text","part":{"type":"text","text":"ANSWER: 42"}}' '{"type":"error","error":{"type":"aborted","message":"Session interrupted: shutdown"}}' '{"type":"tool_use","part":{"type":"tool","tool":"read","state":{"status":"error","error":"The user declined this tool call"}}}'
sleep 30`

func TestOpenCodeClient_Run_SalvagesAnswerBeforeDeclinedTool(t *testing.T) {
	c := &OpenCodeClient{binary: fakeOpenCodeBinary(t, declinedStream)}
	if _, _, err := c.Reflect(context.Background(), "prompt"); err == nil {
		t.Fatal("a failed opencode run must not be promoted to success through declined-tool salvage")
	}
}

func TestOpenCodeClient_Run_DeclinedToolWithoutAnswerFails(t *testing.T) {
	c := &OpenCodeClient{binary: fakeOpenCodeBinary(t, `printf '%s\n' '{"type":"error","error":{"type":"aborted","message":"Session interrupted: shutdown"}}' '{"type":"tool_use","part":{"type":"tool","tool":"read","state":{"status":"error","error":"The user declined this tool call"}}}'; exit 1`)}
	if _, _, err := c.Reflect(context.Background(), "prompt"); err == nil {
		t.Fatal("a declined run with no answer text must fail")
	}
}

func TestOpenCodeClient_Run_OtherFailureWithTextStillFails(t *testing.T) {
	c := &OpenCodeClient{binary: fakeOpenCodeBinary(t, `printf '%s\n' '{"type":"text","part":{"type":"text","text":"partial"}}' '{"type":"error","error":{"type":"provider.auth","message":"free tier"}}'; exit 1`)}
	if _, _, err := c.Reflect(context.Background(), "prompt"); err == nil {
		t.Fatal("a failure other than a declined tool call must not be salvaged")
	}
}

func TestOpenCodeClient_Run_DeadlineCutDeclinedShapeFails(t *testing.T) {
	c := &OpenCodeClient{binary: fakeOpenCodeBinary(t, declinedStreamThenSleep)}
	ctx, cancel := context.WithTimeout(context.Background(), injectedDefault)
	defer cancel()
	text, _, err := c.Reflect(ctx, "prompt")
	if err == nil {
		t.Fatalf("a run cut at the caller's deadline returned %q with no error", text)
	}
	if text != "" {
		t.Errorf("text = %q, want no partial answer from a killed run", text)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the failure does not carry the deadline: %v", err)
	}
}

func TestOpenCodeClient_Run_DefaultBoundCutDeclinedShapeFails(t *testing.T) {
	withDefaultTimeout(t, injectedDefault)
	c := &OpenCodeClient{binary: fakeOpenCodeBinary(t, declinedStreamThenSleep)}
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err == nil {
		t.Fatalf("a run cut at the client default returned %q with no error", text)
	}
	if text != "" {
		t.Errorf("text = %q, want no partial answer from a killed run", text)
	}
	if !strings.Contains(err.Error(), "default") {
		t.Errorf("a cut at the client default must name it: %v", err)
	}
}

// TestParseOpenCodeOutput_ErrorEventAfterTextFails pins #1622: a top-level error
func TestParseOpenCodeOutput_ErrorEventAfterTextFails(t *testing.T) {
	raw := `{"type":"text","part":{"type":"text","text":"KEEP"}}` + "\n" +
		`{"type":"error","error":{"type":"aborted"}}` + "\n"
	got, err := parseOpenCodeOutput(raw)
	if err == nil {
		t.Fatal("expected error for an error event after text")
	}
	if got != "" {
		t.Errorf("got %q, want no partial text on failure", got)
	}
	if !strings.Contains(err.Error(), "aborted") {
		t.Errorf("error does not name the error type: %v", err)
	}
}

func TestParseOpenCodeOutput_ErrorEventWithoutTextFails(t *testing.T) {
	raw := `{"type":"error","error":{"type":"provider.auth","message":"free tier"}}` + "\n"
	got, err := parseOpenCodeOutput(raw)
	if err == nil {
		t.Fatal("expected error for a lone error event")
	}
	if got != "" {
		t.Errorf("got %q, want empty on failure", got)
	}
	if !strings.Contains(err.Error(), "provider.auth") {
		t.Errorf("error does not name the error type: %v", err)
	}
	if !strings.Contains(err.Error(), "free tier") {
		t.Errorf("error does not carry the message: %v", err)
	}
}

func TestParseOpenCodeOutput_ErrorEventMessageIsGuarded(t *testing.T) {
	for _, msg := range []string{"AKIAIOSFODNN7EXAMPLE", strings.Repeat("a", 300)} {
		raw := `{"type":"error","error":{"type":"x","message":"` + msg + `"}}` + "\n"
		_, err := parseOpenCodeOutput(raw)
		if err == nil {
			t.Fatal("expected error")
		}
		if strings.Contains(err.Error(), msg) {
			t.Errorf("raw message leaked into error: %v", err)
		}
		if !strings.Contains(err.Error(), "withheld") {
			t.Errorf("guarded message not rendered as withheld: %v", err)
		}
	}
}

func TestParseOpenCodeOutput_TextAndControlEventsParse(t *testing.T) {
	raw := `{"type":"step_start","part":{"type":"step-start"}}` + "\n" +
		"\n" +
		`{"type":"reasoning","part":{"type":"reasoning","text":"thinking aloud"}}` + "\n" +
		`{"type":"text","part":{"type":"text","text":"OK"}}` + "\n" +
		`{"type":"text","part":{"type":"text","text":" fine"}}` + "\n" +
		`{"type":"step_finish","part":{"type":"step-finish","reason":"stop"}}` + "\n" +
		"\n"
	got, err := parseOpenCodeOutput(raw)
	if err != nil {
		t.Fatalf("parseOpenCodeOutput: %v", err)
	}
	if got != "OK fine" {
		t.Errorf("got %q, want %q", got, "OK fine")
	}
}
