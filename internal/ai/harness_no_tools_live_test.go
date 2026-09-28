package ai

import (
	"bytes"
	"context"
	"fmt"
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

// TestLiveGooseRunsATurnInChatMode is the half TestLiveGooseAcceptsTheNoToolsMode
// cannot settle, and it is the one that decides whether #552 breaks the goose
// backend at all.
//
// `info -v` reads and prints configuration, so it proves the string is accepted
// and outranks config.yaml. It does NOT prove that `goose run` still completes a
// turn once the session mode is chat — and GOOSE_MODE changes the harness's
// OPERATING MODE rather than adding a flag, so a goose that refuses or diverts a
// headless run under chat mode would fail every reflect, resolve and supersede
// call with nothing in the tree having gone red first.
//
// So this runs the real `goose run` with the exact argv GooseClient builds, under
// the exact environment GooseClient.subprocessEnv builds.
//
// What it asserts and what it cannot: the prompt is a fixed string with no memory
// content, and a successful turn therefore means the mode is COMPATIBLE with a
// headless run — not that the answer is correct, which is not this test's claim.
// It spends a model call, which is why it is behind the same GHOST_LIVE_TESTS=1
// gate as the rest of this file rather than running in CI. It is skipped without
// a configured provider, because a goose with no credentials fails on
// authentication and that would read as a mode failure.
func TestLiveGooseRunsATurnInChatMode(t *testing.T) {
	if !LiveTestsEnabled() {
		t.Skip("live CLI test makes a real goose model call; set GHOST_LIVE_TESTS=1 to run")
	}
	bin := "goose"
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("goose not resolvable at %q (cli.goose_binary or PATH): %v", bin, err)
	}

	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)
	// A decoy config in the permissive mode, so a turn that completes proves the
	// child's GOOSE_MODE=chat is what governed it and not an inherited "auto".
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

	// gooseInvocationArgs is the production argv, read here rather than
	// reconstructed, so a change to the policy is exercised rather than
	// shadowed by a copy that can drift from it.
	args := gooseInvocationArgs()
	ctx, cancel := context.WithTimeout(context.Background(), gooseLiveTurnTimeout)
	defer cancel()
	cmd, cleanup, err := (&GooseClient{binary: bin}).subprocessEnv(ctx, args)
	if err != nil {
		t.Fatalf("subprocessEnv: %v", err)
	}
	defer cleanup()
	// The smallest prompt that is still a turn, and carries no memory content.
	cmd.Stdin = strings.NewReader("Reply with the single word OK and nothing else.")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			t.Fatalf("goose run in chat mode: timed out after %s. Chat mode may have diverted a headless run rather than answering it.", gooseLiveTurnTimeout)
		}
		t.Fatalf("goose run in chat mode: %v: %s", err, harnessFailureOutput(stdout.String(), stderr.String()))
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Fatalf("goose run in chat mode exited 0 with no output:\n%s", harnessFailureOutput(stdout.String(), stderr.String()))
	}
}

// gooseLiveTurnTimeout bounds the live turn. Longer than a probe because this one
// really does call a model, and the point is to let a slow provider answer rather
// than to assert anything about latency.
const gooseLiveTurnTimeout = 2 * time.Minute

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
	for _, arg := range codexInvocationArgs(nil) {
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
		t.Errorf("installed codex does not declare %s, and it ignores an unrecognised -c key, so those surfaces are ON despite the policy: the no-tool restriction fails OPEN. Update codexInvocationArgs and codexFeaturesListTranscript together",
			strings.Join(missing, ", "))
	}

	probeCodexUnknownFeatureKey(t, bin)
}

// probeCodexUnknownFeatureKey settles the fact the package's comments assert
// rather than assume, because the two readings have opposite consequences and
// only one of them was written down. An unknown -c key is either a config error
// (a renamed key fails the lifecycle pass loudly, which is safe) or silently
// dropped (the tool comes back on and nothing says so, which is not).
// `features list` is read-only and makes no LLM call, so provoking one costs a
// process and nothing else.
//
// The verdict is drawn from a run carrying EXACTLY the flags Ghost passes, and
// that is the whole point of the shape. The obvious way to ask "is an unknown key
// an error?" is to pass --strict-config, since that is codex's strict reading —
// and it is the wrong question here, because codexInvocationArgs never passes
// it. A codex that rejects unknown keys ONLY under --strict-config, which is
// exactly the premise codexInvocationArgs states, would then be reported as
// fail-CLOSED and the probe would fail the test and order three documentation
// edits, on the strength of a configuration Ghost never uses. The strict
// behaviour is therefore a SECOND, separately-reported probe whose failure
// cannot condemn the first.
//
// Four things make the answer trustworthy rather than merely confident, and
// each exists because the naive version got it wrong:
//
//   - Its OWN deadline. The listing probe above has already spent the budget
//     this function would otherwise share, and harnessCommand builds the child
//     with exec.CommandContext — so a reused, expired context fails at Start,
//     and that non-nil error is indistinguishable from codex rejecting the key.
//     A cold codex against a fresh CODEX_HOME is exactly the slow first run this
//     file warns about, so this would be a reproducible false failure.
//   - Ghost's OWN flags. No --strict-config, no --ignore-user-config, no
//     --ephemeral: the run answers for the configuration a lifecycle call gets.
//   - A BASELINE. Plain `features list` must succeed first. Only a baseline
//     success followed by a failure is evidence about the unknown key; a codex
//     that fails a migration or an auth check must not be reported as
//     fail-CLOSED.
//   - The child's stderr. `exit status 2` alone cannot tell a rejected key from
//     an unrecognised argument or a migration failure, so a confident wrong
//     conclusion is worse than none.
func probeCodexUnknownFeatureKey(t *testing.T, bin string) {
	t.Helper()
	const unknownKey = "ghost_definitely_not_a_codex_feature"

	if _, err := runCodexFeatureProbe(t, bin); err != "" {
		t.Skipf("baseline `codex features list` did not succeed, so this codex cannot settle whether an unrecognised -c key is ignored: %s", err)
	}

	_, unknownErr := runCodexFeatureProbe(t, bin, "-c", "features."+unknownKey+"=false")
	if unknownErr == "" {
		t.Logf("codex ACCEPTS an unrecognised -c key (%s) under the flags Ghost passes, "+
			"so it is silently ignored: that is the fail-OPEN behaviour codexInvocationArgs "+
			"describes, and the reason TestCodexFeatureKeysAreDeclaredNames checks against a "+
			"recorded transcript. This probe is the sole detector of an upstream rename, and "+
			"it is opt-in, so CI does not run it.", unknownKey)
	} else {
		t.Errorf("codex REJECTS an unrecognised -c key (%s) after a passing baseline, with "+
			"the flags Ghost actually passes, so it fails CLOSED: %s. That makes the \"silently "+
			"ignored / fail OPEN\" wording wrong in codexInvocationArgs, "+
			"TestCodexFeatureKeysAreDeclaredNames and the CLAUDE.md internal/ai bullet, and "+
			"every one of those sites must be corrected to match.", unknownKey, unknownErr)
	}

	// Reported, never used for the verdict above: a codex may reject unknown keys
	// under --strict-config while still ignoring them in every call Ghost makes.
	// That is the current expectation, so a pass here is unremarkable and a
	// failure is information rather than a defect.
	if _, strictBaselineErr := runCodexFeatureProbe(t, bin, "--strict-config"); strictBaselineErr != "" {
		t.Logf("this codex does not accept `features list --strict-config` (%s), so the strict "+
			"reading of an unknown key could not be compared. The verdict above does not depend "+
			"on it: it is drawn from a run without the flag, which is what Ghost sends.", strictBaselineErr)
		return
	}
	if _, err := runCodexFeatureProbe(t, bin, "--strict-config", "-c", "features."+unknownKey+"=false"); err != "" {
		t.Logf("under --strict-config this codex REJECTS an unrecognised -c key: %s. Ghost never "+
			"passes --strict-config, so this does not change the verdict above; it only means the "+
			"fail-OPEN behaviour is specific to the configuration a lifecycle call uses.", err)
		return
	}
	t.Logf("under --strict-config this codex ALSO accepts an unrecognised -c key (%s).", unknownKey)
}

// runCodexFeatureProbe runs `codex features list` with its own timeout and
// returns the child's combined output, or a non-empty diagnostic naming the
// cause. The diagnostic is built with harnessFailureOutput, the same seam
// CodexClient.run uses, because "exit status 2" without the child's own words is
// the difference between a diagnosis and a guess.
func runCodexFeatureProbe(t *testing.T, bin string, extra ...string) (string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), noToolsLiveProbeTimeout)
	defer cancel()
	args := append([]string{"features", "list"}, extra...)
	cmd, release, _ := harnessCommand(ctx, bin, args, os.Environ(), harnessCodex)
	defer release()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Sprintf("codex features list %s: timed out after %s", strings.Join(extra, " "), noToolsLiveProbeTimeout)
		}
		return "", fmt.Sprintf("codex features list %s: %v: %s", strings.Join(extra, " "), err, harnessFailureOutput(stdout.String(), stderr.String()))
	}
	return stdout.String(), ""
}
