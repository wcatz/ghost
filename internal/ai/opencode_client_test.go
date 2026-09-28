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

// TestParseOpenCodeOutput_AcceptsALongReflectReply is the #608 case. The reader
// carried a 4 MiB bufio.Scanner cap, the same one the stop hook's transcript
// reader outgrew in #632, so a reflect reply that reached it failed with
// bufio.Scanner: token too long — a failure that reads exactly like a
// malformed stream, on a reply that was merely long. The whole stream is
// already in memory by the time it is parsed, so the cap was bounding nothing
// that the caller's own buffer had not already paid for.
//
// The ceiling is NOT lowered here: the point is the shipped value, and a test
// that set the var would be testing its own override. The payload sits past the
// old 4 MiB cap and well inside the current one, which is the band a real
// answer lands in.
func TestParseOpenCodeOutput_AcceptsALongReflectReply(t *testing.T) {
	// One text event whose own payload is over the old cap, which is the shape
	// that breaks a line reader: the line is one JSON object, not many.
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

// TestParseOpenCodeOutput_StillBoundsALinePastTheCeiling is the other half, so
// the cap cannot be satisfied by removing it: past the ceiling the read is
// still an error, because an unbounded parse of a stream the harness produced
// is a way to run a process out of memory.
func TestParseOpenCodeOutput_StillBoundsALinePastTheCeiling(t *testing.T) {
	old := maxOpencodeOutputLine
	maxOpencodeOutputLine = 4 << 20
	t.Cleanup(func() { maxOpencodeOutputLine = old })

	raw := `{"type":"text","part":{"id":"a","type":"text","text":"` + strings.Repeat("A", (4<<20)+1) + `"}}` + "\n"
	if _, err := parseOpenCodeOutput(raw); err == nil {
		t.Fatal("a line past the ceiling parsed, so nothing bounds the read")
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

// TestOpenCodeClient_Classify_JoinsSystemAndUserIntoPrompt: opencode run has
// no --system-prompt flag, so the two are joined — and the joined text arrives
// on stdin (issue #560), not as a positional argument.
func TestOpenCodeClient_Classify_JoinsSystemAndUserIntoPrompt(t *testing.T) {
	bin := fakeOpenCodeBinary(t, `piped=$(cat)
case "$piped" in *SYSTEM*USER*) printf '%s\n' '{"type":"text","part":{"type":"text","text":"KEEP"}}';; *) echo "prompt not joined" >&2; exit 1;; esac`)
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

// TestOpenCodeClient_SessionStoreStaysInScratchRoot pins the #588 isolation:
// the lifecycle child must never see the user's XDG_DATA_HOME, because that
// is where `opencode session list` reads the session store — a child pointed
// at it files every titled "[ghost]" run into the user's session list (6,844
// measured on one workstation before the fix). The fake binary refuses to run
// unless both HOME and XDG_DATA_HOME are inside the scratch root, writes the
// session file OpenCode would write under the data dir it sees, and reports
// that path back; the test asserts the reported path is inside the scratch
// root and that the temp home's own store was never created, let alone
// written to.
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
	// fakeOpenCodeBinary pinned its own scratch root first; pin the one this
	// test asserts against, so the parent can compare against the same path
	// the child reports.
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	// A temp home with the default XDG data path under it: nothing real is at
	// risk, and a child that leaked into it would leave the store behind.
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

// TestOpenCodeClient_SessionStoreDiesWithTheInvocation is the other half of
// the #588 isolation, and the reason Ghost does not delete each run's session
// afterwards. TestOpenCodeClient_SessionStoreStaysInScratchRoot proves the store
// is somewhere inside the scratch root; this proves it is GONE once the call
// returns, because the per-invocation directory that holds it is removed by the
// same deferred cleanup that already removes the child's temp files. The store
// therefore cannot outlive the run no matter what OpenCode wrote into it.
//
// That is what makes an `opencode session delete` per invocation redundant: it
// would spend a second OpenCode process (Ghost spawns one per consolidation, per
// resolve candidate and per supersede pair — hundreds per lifecycle) to delete a
// row in a directory that is about to be removed wholesale. If the teardown ever
// regresses, the store starts accumulating under the scratch root instead, and
// only this test notices.
//
// The fake writes the store the way real OpenCode does — the sqlite database and
// a titled session row under the data dir it was given — then reports both that
// store's path and the credential it can read there, so the same run also pins
// that the isolated data dir carries the auth file the child needs to
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
	// fakeOpenCodeBinary pinned its own scratch root first; pin the one this
	// test walks, so the parent can search the root the child actually used.
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)

	// A credential in a temp home, at the default data path opencode itself
	// documents. Nothing real is at risk, and it is the one file copyOpenCodeAuth
	// is allowed to carry across.
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
	// The file carry only runs when there is no key in the environment:
	// copyOpenCodeAuth returns early for a set OPENCODE_API_KEY, because the
	// child then authenticates from the variable and needs no auth.json. Pin it
	// empty so this exercises the copy, and so the test does not change meaning
	// depending on the harness it happens to run inside — a session spawned by
	// OpenCode itself exports OPENCODE_API_KEY, which would silently turn the
	// assertion below into a check of the wrong path.
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

	// The credential reached the isolated data dir, byte for byte: isolation
	// relocated where OpenCode looks for it instead of hiding it.
	if auth != credential {
		t.Errorf("auth the child could read = %q, want the carried credential %q", auth, credential)
	}
	if !strings.HasPrefix(store, root+string(os.PathSeparator)) {
		t.Fatalf("child store %q is not inside the scratch root %q", store, root)
	}
	// The teardown, not an explicit delete: the whole store must be gone.
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Errorf("session store %s survived the call (stat err=%v) — it will accumulate", store, err)
	}
	// Belt and braces: nothing anywhere under the root may still look like a
	// session store, so a store written outside $XDG_DATA_HOME/opencode would
	// be caught too rather than passing on the reported path alone.
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
	// The message is not one of the arguments: it goes to stdin, so a reflect
	// prompt larger than the kernel's argv ceiling can still be delivered.
	if strings.Contains(text, "prompt") {
		t.Errorf("V2 args = %q, must not carry the prompt as an argv element", text)
	}
	for _, want := range []string{"run", "--format json", "--title [ghost]"} {
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

// declinedStream is opencode v2's stream when the model answers and then asks
// for a tool: the non-interactive run auto-rejects the ask and shuts the
// session down, exiting 1 (captured from opencode v2.0.15).
const declinedStream = `printf '%s\n' '{"type":"step_start","part":{"type":"step-start"}}' '{"type":"text","part":{"type":"text","text":"ANSWER: 42"}}' '{"type":"error","error":{"type":"aborted","message":"Session interrupted: shutdown"}}' '{"type":"tool_use","part":{"type":"tool","tool":"read","state":{"status":"error","error":"The user declined this tool call"}}}'; echo 'permission requested: external_directory (/etc/*); auto-rejecting' >&2; exit 1`

func TestOpenCodeClient_Run_SalvagesAnswerBeforeDeclinedTool(t *testing.T) {
	c := &OpenCodeClient{binary: fakeOpenCodeBinary(t, declinedStream)}
	text, _, err := c.Reflect(context.Background(), "prompt")
	if err != nil {
		t.Fatalf("Reflect: %v (an answer written before a declined tool call must be kept)", err)
	}
	if text != "ANSWER: 42" {
		t.Errorf("text = %q, want %q", text, "ANSWER: 42")
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

// TestOpenCodeClient_Run_PromptSaysNoTools: the no-tools instruction reaches
// the model as part of the message, which travels on stdin.
func TestOpenCodeClient_Run_PromptSaysNoTools(t *testing.T) {
	c := &OpenCodeClient{binary: fakeOpenCodeBinary(t, `piped=$(cat)
case "$piped" in *"Do not call any tool"*) printf '%s\n' '{"type":"text","part":{"type":"text","text":"OK"}}';; *) echo "prompt lacks the no-tools instruction" >&2; exit 1;; esac`)}
	if _, _, err := c.Reflect(context.Background(), "the task"); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
}

func TestOpenCodeClient_Run_AbortWithoutDeclinedToolFails(t *testing.T) {
	c := &OpenCodeClient{binary: fakeOpenCodeBinary(t, `printf '%s\n' '{"type":"text","part":{"type":"text","text":"half an answ"}}' '{"type":"error","error":{"type":"aborted","message":"Session interrupted: shutdown"}}'; exit 1`)}
	if _, _, err := c.Reflect(context.Background(), "prompt"); err == nil {
		t.Fatal("an aborted run with no declined tool call (shutdown, timeout) must not be salvaged")
	}
}

func TestOpenCodeClient_Run_DeclinedToolWithoutAbortFails(t *testing.T) {
	c := &OpenCodeClient{binary: fakeOpenCodeBinary(t, `printf '%s\n' '{"type":"text","part":{"type":"text","text":"ANSWER: 42"}}' '{"type":"tool_use","part":{"type":"tool","tool":"read","state":{"status":"error","error":"The user declined this tool call"}}}'; exit 1`)}
	if _, _, err := c.Reflect(context.Background(), "prompt"); err == nil {
		t.Fatal("exit 1 with a declined tool but no session abort is an unknown failure and must not be salvaged")
	}
}
