package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestScratchEnvConfinesEveryRoot is the isolation contract every ghost process
// in a run inherits: the data dir, the config dir and HOME are all inside the
// run's own scratch tree, so a run cannot reach the developer's store, and no
// inherited XDG or Anthropic credential survives to be used instead.
func TestScratchEnvConfinesEveryRoot(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/leak/data")
	t.Setenv("XDG_CONFIG_HOME", "/leak/config")
	t.Setenv("HOME", "/leak/home")
	t.Setenv("ANTHROPIC_API_KEY", "sk-leak")
	t.Setenv("anthropic_api_key", "sk-leak-lower")
	// The opencode child's own auth path (eval/cycle's mechanism) has to survive:
	// it is the credential the sandboxed LLM stages authenticate with.
	t.Setenv("OPENCODE_API_KEY", "oc-live")

	env := scratchEnv("/scratch", "")
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"XDG_DATA_HOME=/scratch/data",
		"XDG_CONFIG_HOME=/scratch/config",
		"HOME=/scratch/home",
		"OPENCODE_API_KEY=oc-live",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("child env is missing %s", want)
		}
	}
	if strings.Contains(joined, "/leak/") {
		t.Error("an inherited root leaked into the child env")
	}
	for _, kv := range env {
		if key, _, _ := strings.Cut(kv, "="); strings.EqualFold(key, "ANTHROPIC_API_KEY") {
			t.Errorf("ANTHROPIC_API_KEY survived as %q", kv)
		}
	}
}

// TestScratchEnvPinsTheRunModelForEveryHarnessCall: the arc's classification
// (`ghost supersede`, `ghost resolve`) reads GHOST_OPENCODE_MODEL, so a run that
// names a model has to put it in the environment EVERY ghost process inherits —
// otherwise the sessions are graded by one model and the reversal between them is
// decided by another. An inherited pin from the developer's own shell is dropped
// rather than inherited: a run must be reproducible from its own flags.
func TestScratchEnvPinsTheRunModelForEveryHarnessCall(t *testing.T) {
	t.Setenv("GHOST_OPENCODE_MODEL", "somebody-elses/model")

	env := scratchEnv("/scratch", "opencode-go/glm-5.3-flash")
	if !slices.Contains(env, "GHOST_OPENCODE_MODEL=opencode-go/glm-5.3-flash") {
		// The KEY only, never the value list: the child env is the developer's
		// environment plus the run's overrides, so a failing assertion here
		// would print every credential in the shell to the test log.
		t.Error("the run's model is not pinned for the child")
	}

	// No -model means Ghost's own default, so nothing is pinned: an inherited
	// pin would silently decide the arc's verdicts and contradict the report,
	// which attributes the run to the default.
	env = scratchEnv("/scratch", "")
	if slices.ContainsFunc(env, func(kv string) bool {
		return strings.HasPrefix(kv, "GHOST_OPENCODE_MODEL=")
	}) {
		t.Error("an inherited or invented model pin reached the child")
	}
}

// TestSeedOpencodeAuthCopiesTheCredentialIntoTheScratchDataDir pins the file
// half of that mechanism: with HOME confined, the opencode child finds the
// credential where internal/ai looks for it — under the run's own
// XDG_DATA_HOME — and never under the developer's.
func TestSeedOpencodeAuthCopiesTheCredentialIntoTheScratchDataDir(t *testing.T) {
	src := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(src, []byte(`{"opencode":{"type":"api"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	if err := seedOpencodeAuth(scratch, src); err != nil {
		t.Fatalf("seedOpencodeAuth: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(scratch, "data", "opencode", "auth.json"))
	if err != nil {
		t.Fatalf("credential not seeded: %v", err)
	}
	if !strings.Contains(string(b), "opencode") {
		t.Fatalf("seeded credential is not the file that was handed in: %s", b)
	}
}

func TestSeedOpencodeAuthIsANoOpWithoutAPath(t *testing.T) {
	scratch := t.TempDir()
	if err := seedOpencodeAuth(scratch, ""); err != nil {
		t.Fatalf("seedOpencodeAuth(\"\"): %v", err)
	}
	if _, err := os.Stat(filepath.Join(scratch, "data", "opencode", "auth.json")); !os.IsNotExist(err) {
		t.Fatal("an empty path seeded something")
	}
}

func TestSeedOpencodeAuthFailsOnAMissingSource(t *testing.T) {
	scratch := t.TempDir()
	if err := seedOpencodeAuth(scratch, filepath.Join(scratch, "nope.json")); err == nil {
		t.Fatal("a missing credential file was accepted")
	}
}

// TestScratchLayoutLivesInsideTheScratchRoot: every directory a run writes is
// created under the one root it owns, so --keep hands back a tree and the
// default cleanup removes exactly what the run made.
func TestScratchLayoutLivesInsideTheScratchRoot(t *testing.T) {
	root := t.TempDir()
	dirs, err := makeScratchLayout(root)
	if err != nil {
		t.Fatalf("makeScratchLayout: %v", err)
	}
	for _, name := range []string{"data", "config", "home", "work"} {
		want := filepath.Join(root, name)
		if dirs[name] != want {
			t.Errorf("dirs[%q] = %q, want %q", name, dirs[name], want)
		}
		if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
			t.Errorf("%s was not created under the scratch root", want)
		}
	}
}
