package mcpinit

import (
	"bytes"
	"encoding/json"
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

// stopInput builds the hook's stdin JSON.
func stopInput(t *testing.T, transcriptPath string, active bool) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"transcript_path":  transcriptPath,
		"stop_hook_active": active,
	})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return string(b)
}

const (
	lineToolBash  = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{}}]}}`
	lineGhostSave = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__ghost__ghost_memory_save","input":{}}]}}`
	lineText      = `{"type":"assistant","message":{"content":[{"type":"text","text":"mentioning mcp__ghost__ghost_memory_save in prose does not count"}]}}`
	lineUser      = `{"type":"user","message":{"content":[{"type":"text","text":"hello"}]}}`
)

func runStopHook(t *testing.T, stdin string) string {
	t.Helper()
	var out strings.Builder
	HandleStopHook(strings.NewReader(stdin), &out)
	return out.String()
}

func TestHandleStopHook(t *testing.T) {
	t.Run("blocks when tools ran but nothing saved", func(t *testing.T) {
		path := writeTranscript(t, lineUser, lineToolBash, lineText)
		out := runStopHook(t, stopInput(t, path, false))
		if !strings.Contains(out, `"decision":"block"`) {
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
		if !strings.Contains(out, `"decision":"block"`) {
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

func TestHandleStopHook_FailsOpenOnEmptyTranscriptPathEvenWithCWD(t *testing.T) {
	// HandleStopHook must stay silent and return promptly when transcript_path
	// is empty, even once cwd is populated and a spawn attempt is made first —
	// proving the new spawnResolveIfConfigured call didn't introduce a hang or
	// panic on the hot path. This does NOT assert spawnResolveIfConfigured was
	// a no-op (see TestSpawnResolveIfConfigured_NoOpWhenDisabled for that); the
	// early return below is guaranteed independently of spawn behavior.
	isolatedHome(t)
	var buf bytes.Buffer
	input := `{"transcript_path":"","stop_hook_active":false,"cwd":"/tmp/does-not-matter"}`
	HandleStopHook(strings.NewReader(input), &buf)
	if buf.Len() != 0 {
		t.Errorf("expected no output for empty transcript_path, got %q", buf.String())
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

	spawnResolveIfConfigured("/tmp/does-not-matter")

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

	spawnSupersedeIfConfigured("/tmp/does-not-matter")

	if _, err := os.Stat(filepath.Join(dataHome, "ghost")); !os.IsNotExist(err) {
		t.Errorf("expected ghost data dir to never be created when auto_supersede is disabled, stat err = %v", err)
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
