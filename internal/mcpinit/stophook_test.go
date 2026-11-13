package mcpinit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	dir := plantPluginSessionDir(t, sweepTempBase(t))
	path := filepath.Join(dir, "messages.jsonl")

	input := contractInputFor(t, "Stop", "opencode", "opencode-messages",
		fmt.Sprintf(`{"session_id":"oc1","transcript_path":%q,"cwd":"/repo","stop_hook_active":false}`, path))
	var out bytes.Buffer
	RunHostEvent("stop", "opencode", strings.NewReader(input), &out, io.Discard)
	if out.Len() != 0 {
		t.Errorf("the stop hook must print nothing, got %q", out.String())
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

// unsetEnvForTest removes key from the environment for the rest of the test and
// restores it afterwards. Unlike t.Setenv(key, "") it models a runtime that does
// not see the variable at all.
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	if old, ok := os.LookupEnv(key); ok {
		t.Setenv(key, old)
		_ = os.Unsetenv(key)
	}
}

// sweepableGhostDir materialises an opencode-materialized transcript under a
// fresh `ghost-*` directory inside base and returns that directory. base stands
// in for the temp directory the test wants the hook to recognise as the host's.
func sweepableGhostDir(t *testing.T, base, name string) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create ghost session dir: %v", err)
	}
	path := filepath.Join(dir, "messages.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join([]string{lineUser, ocLineBashTool()}, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return dir
}

// isolatedHome points HOME/XDG_CONFIG_HOME/XDG_DATA_HOME at fresh temp dirs
// and clears GHOST_* / ANTHROPIC_API_KEY env vars, so config.Load and
// config.DataDir can never see the developer's real
// platform Ghost user config path, real GHOST_REFLECTION_AUTO_RESOLVE-style
// overrides, or ~/.local/share/ghost/ghost.db. Without this, tests that
// exercise spawnLifecycleIfConfigured would only be hermetic by accident of the
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
	setHome(t, dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	dataHome := filepath.Join(dir, "data")
	t.Setenv("XDG_DATA_HOME", dataHome)
	return dataHome
}

// TestSpawnLifecycle_CooldownSkipWithholdsATokenShapedProjectID is #1327: the
// cooldown's skip line is the one durable record of why a run did not happen,
// and a stored project id is not token-guarded — CheckImportedProjectID refuses
// a control character but never asks the guard — so a session in the affected
// project's recorded directory resolves to a token-shaped id and the line
// interpolated it with a bare %s. Every neighbouring lifecycle surface renders
// the same value (the lock and the start stamp through RefusalArg, the marker's
// alert through renderMarkerProject); this one wrote it into a file that
// operators and diagnostic tooling read.
//
// Both halves are asserted from one log: the token-shaped id must not appear,
// and an ordinary id still must. A renderer that withheld everything would pass
// the first half and leave the line unable to name the project it skipped.
func TestSpawnLifecycle_CooldownSkipWithholdsATokenShapedProjectID(t *testing.T) {
	dataHome := isolatedHome(t)
	writeGhostConfigFile(t, "reflection:\n  auto_resolve: true\n")
	dataDir := filepath.Join(dataHome, "ghost")

	// Obviously fake, shaped by the detector's own GitHub rule. The directory
	// and the name are ordinary on purpose: the creation guard asks the guard of
	// a project's name and path but not of its id — which is the hole itself, an
	// imported artifact's id arriving verbatim — so planting one through
	// EnsureProject is the shape the store really ends up in, and the id is what
	// a session resolves to.
	//
	// The ordinary project is planted in the same store so one log shows both
	// halves.
	const tokenID = "ghp_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	now := time.Now()
	for _, p := range []struct{ id, dir, name string }{
		{tokenID, "tokproj", "Token Project"},
		{"p1", "ordinaryproj", "Ordinary Project"},
	} {
		dir := filepath.Join(t.TempDir(), p.dir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p.dir, err)
		}
		canonical, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("EvalSymlinks: %v", err)
		}
		seedProject(t, dataHome, p.id, canonical, p.name)
		// A stamp inside the window, so this call takes the cooldown branch —
		// the only writer of the line, and the only path that writes nothing
		// else (the spawn log and the pid file are both past it).
		writeStamp(t, dataDir, p.id, now, time.Minute)
		spawnLifecycleIfConfigured(canonical, "", io.Discard)
	}

	b, err := os.ReadFile(filepath.Join(dataDir, "lifecycle.log"))
	if err != nil {
		t.Fatalf("read lifecycle.log: %v", err)
	}
	line := string(b)
	if strings.Contains(line, tokenID) {
		t.Errorf("lifecycle.log repeats the token-shaped project id: %s", line)
	}
	if !strings.Contains(line, "project \"p1\"") {
		t.Errorf("an ordinary project id no longer appears in the log: %s", line)
	}
	// The line still has to be actionable: a bare "withheld" that named neither
	// the argument nor the format found would tell an operator nothing.
	if !strings.Contains(line, "<project withheld: it holds a GitHub personal access token>") {
		t.Errorf("the log does not name the withheld project and its format: %s", line)
	}
	if n := strings.Count(strings.TrimRight(line, "\n"), "\n") + 1; n != 2 {
		t.Errorf("lifecycle.log holds %d lines, want 2 (one skip per project):\n%s", n, line)
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
			results[i], _ = claimPidFile(pidPath)
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

// TestSafeProjectIDComponent pins the pid-path hardening: project ids arrive
// from callers (an MCP client can send any project_id, and EnsureProject stores
// it verbatim), so a value that could reshape the path must be rejected before
// a filename is built from it — but only path-hostile characters, because
// rejecting spaces or non-ASCII names would silently disable auto-consolidation
// for a legitimate existing project.
func TestSafeProjectIDComponent(t *testing.T) {
	valid := []string{
		"ghost", "my-proj_1.2", "a", "Project-123",
		"my project",    // spaces are legal in a filename
		"プロジェクト名",       // non-ASCII must keep working
		"proj (v2)",     // punctuation is fine
		"v1..2-release", // ".." inside a name is not traversal
	}
	for _, id := range valid {
		if !safeProjectIDComponent(id) {
			t.Errorf("safeProjectIDComponent(%q) = false, want true", id)
		}
	}

	invalid := []string{
		"",
		".",
		"..",
		"../../etc/cron.d/evil",
		"lifecycle-../evil",
		"a/b",
		`a\b`,
		"a/../b",
		"nul\x00byte",
		"tab\ttab", // control characters are unusable in filenames on Windows
		"bell\x07", // any control rune, not just NUL
		// A name the filesystem cannot hold: the store accepts ids far past
		// NAME_MAX, and lifecycle-<id>.pid.lock built from one fails to open.
		strings.Repeat("p", 300),
		strings.Repeat("p", maxProjectIDComponentBytes+1),
		strings.Repeat("é", maxProjectIDComponentBytes/2+1), // bytes, not runes
	}
	for _, id := range invalid {
		if safeProjectIDComponent(id) {
			t.Errorf("safeProjectIDComponent(%q) = true, want false", id)
		}
	}
}
