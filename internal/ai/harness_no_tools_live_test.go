package ai

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The two live tests in this file check the halves of #552 that NO fake binary
// can demonstrate, and the distinction is the whole point of them.
//
// A shell fake that reads `$GOOSE_MODE` out of its own environment proves that
// Ghost WRITES the string. It cannot prove that the installed goose accepts
// "chat" as a mode name, that goose's documented precedence puts an environment
// variable above config.yaml, or that "chat" actually suppresses extension use.
// Every one of those is a property of the goose binary, and all three fail in
// the same direction — silently: a renamed mode or a build that ignores an
// unrecognised value leaves extensions running on a prompt built from memory
// text, and the whole unit suite stays green. This repo already owns that
// counterexample, in TestLiveOpenCodeDebugPathsHonourScratchRoot: "a fake that
// honours the variable proves only the fake honours it".
//
// The same applies to codex's feature keys, in the other direction. A
// `-c features.<key>=false` override whose key codex does not know is silently
// ignored, so a renamed or removed key leaves a tool enabled — a fail-OPEN
// outcome that no argv golden can catch, because the golden would be pinning
// Ghost's own belief. The recorded transcript in
// TestCodexFeatureKeysAreDeclaredNames is the same claim without a binary; this
// is the claim with one.
//
// Both are gated behind GHOST_LIVE_TESTS=1 like every other test here that
// spawns a real binary, and neither makes an LLM call, so neither spends
// anything: `goose info -v` prints resolved configuration and `codex features
// list` prints the registry. Running them on a machine with the binary installed
// is what turns two documented assumptions into two checked ones.

// sessionCommandTimeout bounds a real-binary probe. Copied from the opencode
// live test, and for the same reason: a first run against a fresh directory can
// do migrations or a version check, and without a bound a stall would burn the
// package's whole go test timeout and panic with every other test's goroutines
// in the dump instead of failing here with a diagnosis.
const noToolsLiveProbeTimeout = 30 * time.Second

// TestLiveGooseAcceptsTheNoToolsMode runs the real `goose info -v` under the
// exact environment GooseClient.subprocessEnv builds (not a hand-rolled one) and
// reads back the mode goose resolved for itself.
//
// `info -v` is the right probe: it dumps goose's merged configuration, and
// Config::get_goose_mode reads the GOOSE_MODE environment variable FIRST, before
// the config file. So the resolved value in that output is the question this
// whole policy rests on — not whether Ghost wrote the string, but whether goose
// reads it back as the mode named here and ranks it above config.yaml.
//
// It makes no LLM call: `info` only reads and prints, and the provider check is
// behind a separate `--check` flag that this does not pass.
//
// A decoy config.yaml carrying `GOOSE_MODE: auto` is placed in the isolated
// tree first, so the assertion is about PRECEDENCE and not merely about the
// variable being visible: if goose preferred the file, the resolved mode would
// come back "auto" and this test would fail. That is the half a hand-set
// environment cannot demonstrate.
func TestLiveGooseAcceptsTheNoToolsMode(t *testing.T) {
	if !LiveTestsEnabled() {
		t.Skip("live CLI test spawns the real goose binary; set GHOST_LIVE_TESTS=1 to run")
	}
	bin := "goose"
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("goose not resolvable at %q (cli.goose_binary or PATH): %v", bin, err)
	}

	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	// A decoy config saying "auto", so precedence is what is under test.
	configRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(configRoot, "goose"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(configRoot, "goose", "config.yaml"),
		[]byte("GOOSE_MODE: auto\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", configRoot)

	c := &GooseClient{binary: bin}
	ctx, cancel := context.WithTimeout(context.Background(), noToolsLiveProbeTimeout)
	defer cancel()
	cmd, cleanup, err := c.subprocessEnv(ctx, []string{"info", "-v"})
	if err != nil {
		t.Fatalf("subprocessEnv: %v", err)
	}
	defer cleanup()

	var stdout strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			t.Fatalf("goose info -v: timed out after %s", noToolsLiveProbeTimeout)
		}
		t.Fatalf("goose info -v: %v", err)
	}

	got := gooseResolvedMode(stdout.String())
	if got == "" {
		t.Fatalf("no GOOSE_MODE in goose info -v output:\n%s", stdout.String())
	}
	if got != gooseNoToolsMode {
		t.Errorf("goose resolved GOOSE_MODE=%q, want %q (the config file says auto, so this is the precedence too)", got, gooseNoToolsMode)
	}
}

// gooseResolvedMode reads the GOOSE_MODE goose reported for itself out of
// `goose info -v`. The output is merged configuration printed as YAML, so the
// line is "GOOSE_MODE: chat". A bare "GOOSE_MODE:" with no value is not a
// resolved mode and reads as absent, because an empty value is what a
// unrecognised key would leave behind.
func gooseResolvedMode(out string) string {
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "GOOSE_MODE" {
			continue
		}
		return strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return ""
}

// TestLiveCodexDeclaresTheNoToolFeatureKeys runs the real `codex features list`
// — the command whose output is recorded in codexFeaturesListTranscript — and
// checks that every key codexInvocationArgs passes is one the installed codex
// declares.
//
// The direction that matters is the FALSE one. codex ignores a `-c` override
// whose key it does not recognise, so a key renamed or removed upstream would
// leave the corresponding tool enabled: the policy fails OPEN, silently, and
// every unit test in this package still passes because the fakes echo back
// whatever Ghost hands them. That is the same fail-open shape this file's goose
// test guards against, reached from the other end.
//
// `features list` makes no LLM call — it prints a table built from the compiled
// feature registry and the loaded config — so running it spends nothing.
//
// A key that IS declared but is no longer load-bearing is not this test's
// business: whether disabling a key still removes its tool is codex's own
// regression surface, and the transcript's stage column is what tells a reader
// when to look.
func TestLiveCodexDeclaresTheNoToolFeatureKeys(t *testing.T) {
	if !LiveTestsEnabled() {
		t.Skip("live CLI test spawns the real codex binary; set GHOST_LIVE_TESTS=1 to run")
	}
	bin := "codex"
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("codex not resolvable at %q (cli.codex_binary or PATH): %v", bin, err)
	}

	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))

	ctx, cancel := context.WithTimeout(context.Background(), noToolsLiveProbeTimeout)
	defer cancel()
	probe, release, _ := harnessCommand(ctx, bin, []string{"features", "list"}, os.Environ(), harnessCodex)
	defer release()
	out, err := probe.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			t.Fatalf("codex features list: timed out after %s", noToolsLiveProbeTimeout)
		}
		t.Fatalf("codex features list: %v", err)
	}

	declared := parseCodexFeaturesList(string(out))
	var missing []string
	for _, arg := range codexInvocationArgs() {
		key, ok := strings.CutPrefix(arg, "features.")
		if !ok {
			continue
		}
		key = strings.TrimSuffix(key, "=false")
		if !declared[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		t.Errorf("installed codex does not declare %s; codex ignores an unrecognised -c key, so the no-tool policy fails OPEN. Update codexInvocationArgs and codexFeaturesListTranscript together",
			strings.Join(missing, ", "))
	}
}
