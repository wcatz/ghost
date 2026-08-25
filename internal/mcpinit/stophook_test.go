package mcpinit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// writeTranscript writes lines to a temp transcript file and returns its path.
func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

// stopInput builds the hook's stdin JSON: a Claude Code Stop payload wrapped
// with the contract-v1 envelope (the only accepted shape since the contract
// gained no legacy mode).
func stopInput(t *testing.T, transcriptPath string, active bool) string {
	t.Helper()
	return contractInputFor(t, "Stop", "claude-code", "claude-jsonl",
		fmt.Sprintf(`{"session_id":"s1","transcript_path":%q,"cwd":"/repo","stop_hook_active":%t}`, transcriptPath, active))
}

// contractInput wraps a native Claude Code Stop payload with the ghost
// contract envelope, mirroring what an explicit-envelope adapter would send.
func contractInput(t *testing.T, inner string) string {
	t.Helper()
	return contractInputFor(t, "Stop", "claude-code", "claude-jsonl", inner)
}

// nativeStopInput is the exact JSON Claude Code sends on Stop — no contract
// object. Envelope completion in hostevent.Parse must make it dispatch.
func nativeStopInput(transcriptPath string) string {
	return fmt.Sprintf(`{"hook_event_name":"Stop","session_id":"s1","transcript_path":%q,"cwd":"/repo","stop_hook_active":false,"source":"startup"}`, transcriptPath)
}

// contractInputFor is contractInput with every envelope field selectable.
func contractInputFor(t *testing.T, eventName, source, format, inner string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(inner), &m); err != nil {
		t.Fatalf("contractInput inner payload: %v", err)
	}
	m["hook_event_name"] = eventName
	m["contract"] = map[string]any{
		"version":           1,
		"source":            source,
		"transcript_format": format,
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal contract input: %v", err)
	}
	return string(b)
}

const (
	lineToolBash  = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{}}]}}`
	lineGhostSave = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__ghost__ghost_memory_save","input":{}}]}}`
	lineText      = `{"type":"assistant","message":{"content":[{"type":"text","text":"mentioning mcp__ghost__ghost_memory_save in prose does not count"}]}}`
	lineUser      = `{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}`
)

// ocLineBashTool is an opencode-format assistant tool-call line for sweep
// tests (the sweep must run regardless of which decision branch follows).
func ocLineBashTool() string {
	return `{"info":{"role":"assistant"},"parts":[{"type":"tool","tool":"bash","state":{"status":"completed"}}]}`
}

func runStopHook(t *testing.T, stdin string) string {
	t.Helper()
	var out strings.Builder
	RunHostEvent("stop", "claude-code", strings.NewReader(stdin), &out, io.Discard)
	return out.String()
}

func TestRunStop(t *testing.T) {
	t.Run("blocks when tools ran but nothing saved", func(t *testing.T) {
		path := writeTranscript(t, lineUser, lineToolBash, lineText)
		out := runStopHook(t, stopInput(t, path, false))
		if !strings.Contains(out, `"decision":"approve"`) {
			t.Errorf("expected block decision, got %q", out)
		}
		if !strings.Contains(out, "ghost_memory_save") {
			t.Errorf("reason should mention ghost_memory_save, got %q", out)
		}
	})

	t.Run("allows when a ghost save happened", func(t *testing.T) {
		path := writeTranscript(t, lineToolBash, lineGhostSave)
		if out := runStopHook(t, stopInput(t, path, false)); out != "" {
			t.Errorf("expected silence, got %q", out)
		}
	})

	t.Run("allows pure conversation with no tool calls", func(t *testing.T) {
		path := writeTranscript(t, lineUser, lineText)
		if out := runStopHook(t, stopInput(t, path, false)); out != "" {
			t.Errorf("expected silence, got %q", out)
		}
	})

	t.Run("tool name in prose does not count as a save", func(t *testing.T) {
		path := writeTranscript(t, lineToolBash, lineText)
		out := runStopHook(t, stopInput(t, path, false))
		if !strings.Contains(out, `"decision":"approve"`) {
			t.Errorf("prose mention must not suppress the nudge, got %q", out)
		}
	})

	t.Run("stop_hook_active short-circuits", func(t *testing.T) {
		path := writeTranscript(t, lineToolBash)
		if out := runStopHook(t, stopInput(t, path, true)); out != "" {
			t.Errorf("expected silence when already active, got %q", out)
		}
	})

	t.Run("fail-open on missing transcript", func(t *testing.T) {
		if out := runStopHook(t, stopInput(t, "/nonexistent/transcript.jsonl", false)); out != "" {
			t.Errorf("expected silence, got %q", out)
		}
	})

	t.Run("fail-open on empty transcript path", func(t *testing.T) {
		if out := runStopHook(t, stopInput(t, "", false)); out != "" {
			t.Errorf("expected silence, got %q", out)
		}
	})

	t.Run("fail-open on garbage stdin", func(t *testing.T) {
		if out := runStopHook(t, "{not json"); out != "" {
			t.Errorf("expected silence, got %q", out)
		}
	})

	t.Run("skips unparseable transcript lines", func(t *testing.T) {
		path := writeTranscript(t, "garbage not json", lineToolBash, "{{{{", lineGhostSave)
		if out := runStopHook(t, stopInput(t, path, false)); out != "" {
			t.Errorf("expected silence (save found despite garbage), got %q", out)
		}
	})
}

// TestRunHostEvent_NativeClaudePayloadCompletesEnvelope pins the envelope-
// completion rule at the dispatch level: Claude Code sends no contract
// object, so the argv values must complete it — a native payload blocks
// exactly like an explicit-envelope one.
func TestRunHostEvent_NativeClaudePayloadCompletesEnvelope(t *testing.T) {
	isolatedHome(t)
	path := writeTranscript(t, lineUser, lineToolBash, lineText)
	out := runStopHook(t, nativeStopInput(path))
	if !strings.Contains(out, `"decision":"approve"`) {
		t.Errorf("native payload should complete its envelope and block, got %q", out)
	}
}

// TestRunHostEvent_SessionStartSuppressedOnNonInjectingHost pins the
// capability-scoped output rule: opencode cannot consume injected context,
// so its session-start is a silent success — never stdout it would misread.
func TestRunHostEvent_SessionStartSuppressedOnNonInjectingHost(t *testing.T) {
	isolatedHome(t)
	var out bytes.Buffer
	input := contractInputFor(t, "SessionStart", "opencode", "none",
		`{"session_id":"oc1","cwd":"/repo"}`)
	RunHostEvent("session-start", "opencode", strings.NewReader(input), &out, io.Discard)
	if out.Len() != 0 {
		t.Errorf("expected silent no-op session-start for opencode, got %q", out.String())
	}
}

// TestRunStop_SweepsTransientTempTranscript pins the consumer-side sweep:
// an opencode-materialized transcript under a mkdtemp ghost-* dir in the OS
// temp dir is removed after the hook runs, even though the spawning host may
// have exited before its own cleanup could fire (`opencode run`).
func TestRunStop_SweepsTransientTempTranscript(t *testing.T) {
	isolatedHome(t)
	dir, err := os.MkdirTemp("", "ghost-sweeptest-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	path := filepath.Join(dir, "messages.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join([]string{lineUser, ocLineBashTool()}, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	input := contractInputFor(t, "Stop", "opencode", "opencode-messages",
		fmt.Sprintf(`{"session_id":"oc1","transcript_path":%q,"cwd":"/repo","stop_hook_active":false}`, path))
	var out bytes.Buffer
	RunHostEvent("stop", "opencode", strings.NewReader(input), &out, io.Discard)
	// opencode now receives the nudge on stdout (its plugin re-presents it as a
	// non-blocking reminder); the key invariant here is that the transient
	// transcript dir is still swept after the hook runs.
	if !strings.Contains(out.String(), `"decision":"approve"`) {
		t.Errorf("expected nudge block decision on stdout, got %q", out.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("transient transcript dir was not swept: %v", err)
	}
}

// TestRunStop_NeverSweepsForeignPaths pins the sweep's blast radius: a
// transcript anywhere other than <tmpdir>/ghost-*/ is left untouched.
func TestRunStop_NeverSweepsForeignPaths(t *testing.T) {
	isolatedHome(t)
	path := writeTranscript(t, lineToolBash)
	runStopHook(t, contractInputFor(t, "Stop", "claude-code", "claude-jsonl",
		fmt.Sprintf(`{"session_id":"s1","transcript_path":%q,"cwd":"/repo","stop_hook_active":false}`, path)))
	if _, err := os.Stat(path); err != nil {
		t.Errorf("native transcript must never be swept: %v", err)
	}
}

// TestRunHostEvent_StopHookActiveGatesOnlyStop pins the §2.2 split:
// stop_hook_active suppresses a repeat stop entirely, but session-end fires
// once per real session end, so its lifecycle spawns run regardless of any
// earlier block cycle. Observable via config.DataDir(): with auto_resolve
// enabled, reaching the spawn probe creates the ghost data dir.
func TestRunHostEvent_StopHookActiveGatesOnlyStop(t *testing.T) {
	dataHome := isolatedHome(t)
	writeGhostConfigFile(t, "reflection:\n  auto_resolve: true\n")

	input := contractInputFor(t, "SessionEnd", "claude-code", "none",
		`{"session_id":"s1","transcript_path":"","cwd":"/tmp/does-not-matter","stop_hook_active":true}`)
	var out bytes.Buffer
	RunHostEvent("session-end", "claude-code", strings.NewReader(input), &out, io.Discard)
	if _, err := os.Stat(filepath.Join(dataHome, "ghost")); err != nil {
		t.Errorf("session-end must run lifecycle spawns despite stop_hook_active; data dir not created: %v", err)
	}
}

// TestRunHostEvent_StopStillGuardedWhenActive pins the other half: a stop
// carrying stop_hook_active still short-circuits before any spawn work.
func TestRunHostEvent_StopStillGuardedWhenActive(t *testing.T) {
	dataHome := isolatedHome(t)
	writeGhostConfigFile(t, "reflection:\n  auto_resolve: true\n")

	path := writeTranscript(t, lineToolBash)
	if out := runStopHook(t, stopInput(t, path, true)); out != "" {
		t.Errorf("expected silence on guarded stop, got %q", out)
	}
	if _, err := os.Stat(filepath.Join(dataHome, "ghost")); !os.IsNotExist(err) {
		t.Errorf("guarded stop must short-circuit before spawn probes; data dir exists")
	}
}

func TestRunHostEvent_FailsOpenOnEmptyTranscriptPathEvenWithCWD(t *testing.T) {
	// RunHostEvent must stay silent on stdout and return promptly when
	// transcript_path is empty, even once cwd is populated and a spawn attempt
	// is made first — proving the spawnResolveIfConfigured call didn't
	// introduce a hang or panic on the hot path. This does NOT assert
	// spawnResolveIfConfigured was a no-op (see
	// TestSpawnResolveIfConfigured_NoOpWhenDisabled for that); the early
	// return below is guaranteed independently of spawn behavior.
	isolatedHome(t)
	var buf bytes.Buffer
	input := contractInput(t, `{"transcript_path":"","cwd":"/tmp/does-not-matter","stop_hook_active":false}`)
	RunHostEvent("stop", "claude-code", strings.NewReader(input), &buf, io.Discard)
	if buf.Len() != 0 {
		t.Errorf("expected no output for empty transcript_path, got %q", buf.String())
	}
}

// TestRunHostEvent_MissingSourceFailsOpen pins the no-legacy-mode rule: an
// invocation without --source can't be routed, so it fails open with one
// stderr line, no stdout, and returns normally (the CLI exits 0).
func TestRunHostEvent_MissingSourceFailsOpen(t *testing.T) {
	var out, errBuf bytes.Buffer
	path := writeTranscript(t, lineToolBash)
	RunHostEvent("stop", "", strings.NewReader(fmt.Sprintf(`{"transcript_path":%q}`, path)), &out, &errBuf)
	if out.Len() != 0 {
		t.Errorf("expected no stdout, got %q", out.String())
	}
	if !strings.Contains(errBuf.String(), "missing --source") {
		t.Errorf("expected missing-source guidance on stderr, got %q", errBuf.String())
	}
}

// TestRunHostEvent_SessionEndNeverNudges pins spec §2.2: session-end runs the
// lifecycle spawns only — a tool-using session with zero saves must pass in
// silence even though the identical payload blocks on stop.
func TestRunHostEvent_SessionEndNeverNudges(t *testing.T) {
	isolatedHome(t)
	path := writeTranscript(t, lineUser, lineToolBash, lineText)
	input := contractInputFor(t, "SessionEnd", "claude-code", "claude-jsonl",
		fmt.Sprintf(`{"session_id":"s1","transcript_path":%q,"cwd":"","stop_hook_active":false}`, path))
	var out strings.Builder
	RunHostEvent("session-end", "claude-code", strings.NewReader(input), &out, io.Discard)
	if out.String() != "" {
		t.Errorf("session-end must never emit a block decision, got %q", out.String())
	}
}

// TestRunHostEvent_NudgeEmittedForAllHosts pins the contract: the save-nudge
// block decision is emitted on stdout for every host that reaches the stop
// nudge path — including non-blocking hosts like opencode (which cannot honor
// the block but re-present it as a non-blocking reminder via their plugin). No
// stderr line is ever emitted, so there is no terminal bleed.
func TestRunHostEvent_NudgeEmittedForAllHosts(t *testing.T) {
	isolatedHome(t)
	path := writeTranscript(t, lineUser, lineToolBash, lineText)
	input := contractInputFor(t, "Stop", "opencode", "claude-jsonl",
		fmt.Sprintf(`{"session_id":"s1","transcript_path":%q,"cwd":"","stop_hook_active":false}`, path))

	var out, errBuf bytes.Buffer
	RunHostEvent("stop", "opencode", strings.NewReader(input), &out, &errBuf)
	if !strings.Contains(out.String(), `"decision":"approve"`) {
		t.Errorf("nudge must emit block decision on stdout, got %q", out.String())
	}
	if errBuf.Len() != 0 {
		t.Errorf("nudge must not emit stderr (no terminal bleed), got %q", errBuf.String())
	}
}

// isolatedHome points HOME/XDG_CONFIG_HOME/XDG_DATA_HOME at fresh temp dirs
// and clears GHOST_* / ANTHROPIC_API_KEY env vars, so config.Load and
// config.DataDir can never see the developer's real
// ~/.config/ghost/config.yaml, real GHOST_REFLECTION_AUTO_RESOLVE-style
// overrides, or ~/.local/share/ghost/ghost.db. Without this, tests that
// exercise spawnResolveIfConfigured would only be hermetic by accident of the
// machine and environment they happen to run in.
func isolatedHome(t *testing.T) string {
	t.Helper()
	for _, e := range os.Environ() {
		if key, _, ok := strings.Cut(e, "="); ok && (strings.HasPrefix(key, "GHOST_") || key == "ANTHROPIC_API_KEY") {
			if old, ok := os.LookupEnv(key); ok {
				t.Setenv(key, old)
				_ = os.Unsetenv(key)
			}
		}
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	dataHome := filepath.Join(dir, "data")
	t.Setenv("XDG_DATA_HOME", dataHome)
	return dataHome
}

func TestSpawnResolveIfConfigured_NoOpWhenDisabled(t *testing.T) {
	// With no config file present, reflection.auto_resolve defaults to false
	// (internal/config/config.go's defaults map). spawnResolveIfConfigured
	// must return immediately after that check — before ever calling
	// config.DataDir (which creates ~/.local/share/ghost), let alone touching
	// ghost.db, the pidfile, or resolve.log.
	dataHome := isolatedHome(t)

	spawnResolveIfConfigured("/tmp/does-not-matter", "")

	if _, err := os.Stat(filepath.Join(dataHome, "ghost")); !os.IsNotExist(err) {
		t.Errorf("expected ghost data dir to never be created when auto_resolve is disabled, stat err = %v", err)
	}
}

func TestSpawnSupersedeIfConfigured_NoOpWhenDisabled(t *testing.T) {
	// With no config file present, reflection.auto_supersede defaults to false
	// (internal/config/config.go's defaults map). spawnSupersedeIfConfigured
	// must return immediately after that check — before ever calling
	// config.DataDir (which creates ~/.local/share/ghost), let alone touching
	// ghost.db, the pidfile, or supersede.log.
	dataHome := isolatedHome(t)

	spawnSupersedeIfConfigured("/tmp/does-not-matter", "")

	if _, err := os.Stat(filepath.Join(dataHome, "ghost")); !os.IsNotExist(err) {
		t.Errorf("expected ghost data dir to never be created when auto_supersede is disabled, stat err = %v", err)
	}
}

func TestSpawnReflectIfConfigured_NoOpWhenDisabled(t *testing.T) {
	// With no config file present, reflection.auto_reflect defaults to false
	// (internal/config/config.go's defaults map). spawnReflectIfConfigured must
	// return immediately after that check — before ever calling config.DataDir
	// (which creates ~/.local/share/ghost), let alone touching ghost.db, the
	// pidfile, or reflect.log.
	dataHome := isolatedHome(t)

	spawnReflectIfConfigured("/tmp/does-not-matter", "")

	if _, err := os.Stat(filepath.Join(dataHome, "ghost")); !os.IsNotExist(err) {
		t.Errorf("expected ghost data dir to never be created when auto_reflect is disabled, stat err = %v", err)
	}
}

func TestSpawnReflectIfConfigured_NoOpWithoutLLM(t *testing.T) {
	// auto_reflect enabled, but no LLM is available (no API key, no claude, no
	// opencode on PATH). The no-LLM guard must return before config.DataDir, so
	// no Jaccard-only reflect ever spawns and no data dir is created.
	dataHome := isolatedHome(t)
	cfgDir := os.Getenv("XDG_CONFIG_HOME")
	if err := os.MkdirAll(filepath.Join(cfgDir, "ghost"), 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "ghost", "config.yaml"), []byte("reflection:\n  auto_reflect: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("PATH", t.TempDir()) // no claude, no opencode

	spawnReflectIfConfigured("/tmp/does-not-matter", "")

	if _, err := os.Stat(filepath.Join(dataHome, "ghost")); !os.IsNotExist(err) {
		t.Errorf("expected no ghost data dir when no LLM is available, stat err = %v", err)
	}
}

// TestClaimPidFile_ConcurrentCallersOnlyOneWins races many goroutines against
// an empty pidPath, simulating near-simultaneous stop hooks for the same
// project when no resolve has ever run. Exactly one must win the claim.
func TestClaimPidFile_ConcurrentCallersOnlyOneWins(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "resolve-test.pid")

	wins := raceClaimPidFile(t, pidPath, 50)
	if wins != 1 {
		t.Errorf("expected exactly 1 winner among 50 concurrent claimants, got %d", wins)
	}

	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pidPath: %v", err)
	}
	pidStr, _, _ := strings.Cut(strings.TrimSpace(string(data)), ":")
	if _, err := strconv.Atoi(pidStr); err != nil {
		t.Errorf("pidPath content is never a valid PID (empty/stale window observed): %q", data)
	}
}

// TestClaimPidFile_ConcurrentCallersOnlyOneWinsAgainstStaleClaim seeds
// pidPath with a definitely-dead PID first, then races many goroutines
// against it. This exercises the reclaim path specifically — the one a
// prior fix attempt got wrong, letting multiple callers each independently
// decide the stale claim was theirs to steal and all "win." The exclusive
// lock in claimPidFile must serialize the check-then-write against every
// caller, not just protect the initial claim, so exactly one winner here
// proves the reclaim path is race-free too.
func TestClaimPidFile_ConcurrentCallersOnlyOneWinsAgainstStaleClaim(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "resolve-test.pid")
	// A PID astronomically unlikely to be alive on any real machine.
	if err := os.WriteFile(pidPath, []byte("999999999"), 0o600); err != nil {
		t.Fatalf("seed stale pidfile: %v", err)
	}

	wins := raceClaimPidFile(t, pidPath, 20)
	if wins != 1 {
		t.Errorf("expected exactly 1 winner reclaiming a stale pidfile, got %d", wins)
	}
}

func raceClaimPidFile(t *testing.T, pidPath string, n int) int {
	t.Helper()
	var wg sync.WaitGroup
	results := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = claimPidFile(pidPath)
		}(i)
	}
	wg.Wait()

	wins := 0
	for _, ok := range results {
		if ok {
			wins++
		}
	}
	return wins
}

// TestClaimPidFile_PlaceholderCarriesToken proves the placeholder PID
// claimPidFile writes (the caller's own PID, before it has spawned
// anything) is written in the same "pid:token" format as a real spawn —
// otherwise an abandoned placeholder (caller hits an early-return failure
// path after claiming but before cmd.Start()) has no creation-time guard
// and reproduces the exact PID-reuse bug this design fixes.
func TestClaimPidFile_PlaceholderCarriesToken(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "resolve-test.pid")

	if !claimPidFile(pidPath) {
		t.Fatal("claimPidFile on an empty slot = false, want true")
	}

	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pidPath: %v", err)
	}
	content := strings.TrimSpace(string(data))
	pidStr, token, haveToken := strings.Cut(content, ":")
	if _, err := strconv.Atoi(pidStr); err != nil {
		t.Errorf("placeholder content has no valid leading PID: %q", content)
	}
	if wantToken, ok := processStartTime(os.Getpid()); ok && (!haveToken || token != wantToken) {
		t.Errorf("processStartTime succeeded (token=%q) but placeholder token is %q (haveToken=%v): %q", wantToken, token, haveToken, content)
	}
}
