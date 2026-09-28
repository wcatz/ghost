package ai

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
// gate as the rest of this file rather than running in CI.
//
// It is SKIPPED, not failed, when no goose configuration exists to carry into the
// child. Without one the turn cannot authenticate, and an authentication failure
// is indistinguishable from the mode breaking the backend — so that guard is in
// the body, not only in this comment. A test that can only fail is worse than no
// test, because it reports a problem that is not there.
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

	// The REAL goose configuration, carried by CALLING PRODUCTION'S OWN CARRY, so
	// the file set and the errors are the ones a real reflect call produces.
	//
	// This is the point of the test and it took three attempts to get right. A
	// fresh XDG_CONFIG_HOME seeded with nothing but `GOOSE_MODE: auto` cannot work:
	// harnessEnv drops GOOSE_PROVIDER__API_KEY, OPENAI_API_KEY and
	// ANTHROPIC_API_KEY, and setting XDG_CONFIG_HOME makes linkGooseConfigDirs
	// return early, so the child gets no config at all. `goose run` then fails on
	// authentication, and that failure is indistinguishable from "chat mode breaks
	// the goose backend" — the one question this test exists to answer. A test
	// that can only fail is worse than no test.
	//
	// Naming two files was the second attempt and it was wrong in the way this
	// repo has been bitten before: production carries the whole directory, so a
	// machine whose run depends on settings.json — or on anything else in there —
	// would still have failed here, and with the same misleading message. A list
	// of names is also a second copy of a policy that already exists, so it drifts.
	//
	// The roots are the ones the CHILD would resolve, in production's order (see
	// liveGooseConfigRoots), and a carry failure is a test failure rather than a
	// skip: carrying the config is the precondition for the turn meaning anything,
	// and a permission error there is a fact about this machine, not a reason to
	// report nothing.
	//
	// Each root gets its OWN target, mirroring the relative path under an isolated
	// home — which is what linkGooseConfigDirsWith does, and the reason the
	// earlier flattening into one directory was wrong. Copying two roots into one
	// target has the second silently overwrite the first's same-named files, so
	// the child could read config.yaml from one real root and secrets.yaml from
	// the other: a pairing that exists nowhere on disk and that no production call
	// would ever build. The same class of misleading answer the test exists to
	// prevent, arrived at from the opposite direction.
	//
	// The REAL home is captured BEFORE it is replaced. Order matters here and it
	// is not cosmetic: liveGooseConfigRoots reads HOME to find the config this
	// test has to carry, so replacing HOME first would make it look under an empty
	// temp dir and the test would skip on every host — which is the same class of
	// dead test as one that can only fail, and harder to notice because a skip
	// reads as a pass. The helpers take the real home as a parameter so they
	// mirror production (gooseHomeDir reads the real one) while the isolated home
	// is only ever a copy TARGET.
	realHome := liveGooseRealHome()
	realXDG := os.Getenv("XDG_CONFIG_HOME")
	roots := liveGooseConfigRoots(t, realHome, realXDG)
	if len(roots) == 0 {
		t.Skipf("no real goose config root under %s; run this where `goose configure` has been done", realHome)
	}
	if liveGooseIsolate(t, t.TempDir(), realHome, realXDG) == 0 {
		t.Skipf("no config.yaml or secrets.yaml under %v; nothing would authenticate", roots)
	}
	// A decoy in the PARENT's environment, so a passing turn is attributable to
	// the child's GOOSE_MODE rather than to an inherited "auto". This is the
	// precedence half: the child's value must beat both the file and whatever the
	// parent had.
	t.Setenv("GOOSE_MODE", "auto")
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

// liveGooseIsolate carries the real goose config into an isolated `home`, sets
// HOME and USERPROFILE to it, and returns how many roots now hold what a turn
// needs to authenticate. Zero means the caller should skip rather than run a turn
// that cannot succeed.
//
// It is a named function rather than inline in the live test because the
// control flow inside it had been wrong four review rounds running, and all four
// were invisible for the same reason: nothing outside a GHOST_LIVE_TESTS=1-gated
// test ever ran it. TestGooseIsolatedConfigIsReachableBelow drives every shape of
// the branch against fixtures, so the next mistake here is a red test rather than
// a finding — and it can be, because the whole point of the helper is that the
// branch no longer needs a live goose to reach.
//
// The two cases are EXCLUSIVE, and writing them as a loop plus a trailing branch
// said otherwise. liveGooseConfigRoots returns immediately when XDG is set, so
// the roots are either the one XDG root or the home-relative ones, never both —
// and the loop-then-branch shape left a machine whose XDG_CONFIG_HOME is outside
// gooseHomeConfigRelPaths (~/.xdg) skipping on a counter BEFORE the branch that
// carries it could run.
func liveGooseIsolate(t *testing.T, home, realHome, realXDG string) int {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	roots := liveGooseConfigRoots(t, realHome, realXDG)
	carried := 0
	if realXDG != "" {
		// XDG_CONFIG_HOME is exported only when the PARENT had one, and it is the
		// copy's PARENT, because the copy IS the config root and goose resolves
		// that root as $XDG_CONFIG_HOME/goose. Exporting the root itself points
		// the child at <xdgCopy>/goose/goose, which is never created, so it
		// authenticates against nothing and the failure reads as "chat mode
		// breaks the goose backend".
		//
		// No home-relative root is carried here, and that is production's
		// behaviour rather than a shortcut: linkGooseConfigDirsWith returns
		// immediately on an inherited XDG_CONFIG_HOME, so the child resolves
		// nothing under HOME and carrying one would only invent a path no goose
		// consults. That is also why roots[0] is the right source — with XDG set
		// it is the only one there is, and it need not be under the home at all.
		xdgBase := filepath.Join(home, "xdg")
		xdgCopy := filepath.Join(xdgBase, "goose")
		t.Setenv("XDG_CONFIG_HOME", xdgBase)
		if err := os.MkdirAll(xdgCopy, 0o700); err != nil {
			t.Fatal(err)
		}
		if liveGooseCarry(t, roots[0], xdgCopy) {
			carried++
		}
		return carried
	}
	// Home-relative: each root keeps its OWN path under the isolated home, the way
	// linkGooseConfigDirsWith does. Copying several into one target has the second
	// silently overwrite the first's same-named files, so the child could read
	// config.yaml from one real root and secrets.yaml from the other: a pairing
	// that exists nowhere on disk and that no production call would build. The
	// same class of misleading answer this test exists to prevent, arrived at from
	// the opposite direction.
	for _, source := range roots {
		rel, ok := liveGooseRelPathFor(source, realHome)
		if !ok {
			continue
		}
		target := filepath.Join(append([]string{home}, rel...)...)
		if err := os.MkdirAll(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if liveGooseCarry(t, source, target) {
			carried++
		}
	}
	return carried
}

// TestGooseIsolatedConfigIsReachableBelow drives liveGooseIsolate for every shape
// of the branch, with fixtures standing in for the machine's real configuration
// and no live goose involved.
//
// It exists because the control flow inside that helper was wrong four review
// rounds running and every one of those mistakes was invisible: the code was
// reachable only from a GHOST_LIVE_TESTS=1-gated test, so nothing in CI ever
// entered the branch. The failures were a flattened target, an inverted
// precedence, HOME read after it had been replaced, a skip that fired before the
// branch carrying the config, and an XDG export one level too deep — each
// producing either a false auth failure indistinguishable from a mode failure, or
// a skip that reads as a pass.
//
// The assertion in every case is the one the CHILD makes, not the one the helper
// made: it looks the config up exactly as goose resolves it, so a target in the
// right directory for the wrong reason still fails.
func TestGooseIsolatedConfigIsReachableBelow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture paths are POSIX-shaped; the branch itself is platform-independent")
	}
	// A realistic config directory: two files, so a carry that copies only one
	// named file — the mistake this replaced — fails rather than passing.
	seed := func(t *testing.T, dir string) string {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			"config.yaml":   "active_provider: anthropic\n",
			"settings.json": "{}\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	for _, tc := range []struct {
		name string
		// realXDG is relative to the real home, so the fixture can express both
		// the default XDG and one outside gooseHomeConfigRelPaths.
		xdg     string
		wantXDG bool
	}{
		{name: "no XDG, home-relative root"},
		{name: "XDG at the default .config", xdg: ".config", wantXDG: true},
		// The case that could only skip: ~/.xdg is not one of
		// gooseHomeConfigRelPaths, so the home-relative branch cannot carry it.
		{name: "XDG outside the home-relative list", xdg: ".xdg", wantXDG: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			realHome := t.TempDir()
			seed(t, filepath.Join(realHome, ".config", "goose"))
			realXDG := ""
			if tc.xdg != "" {
				// realXDG is the BASE, not the config root: the helper (like
				// goose) appends "goose" to it, and passing the root itself is
				// what makes it look one level too deep.
				seed(t, filepath.Join(realHome, tc.xdg, "goose"))
				realXDG = filepath.Join(realHome, tc.xdg)
			}
			home := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", "")
			if realXDG != "" {
				t.Setenv("XDG_CONFIG_HOME", realXDG)
			}

			if carried := liveGooseIsolate(t, home, realHome, realXDG); carried != 1 {
				t.Fatalf("carried %d roots, want 1: the branch did not reach the config", carried)
			}

			// What the CHILD resolves, per linkGooseConfigDirsWith's precedence:
			// an inherited XDG decides everything, otherwise the home-relative
			// roots are what it looks under. Only the roots that EXISTED are
			// asserted — production carries the ones present and leaves the rest
			// alone, so demanding a config at a root the machine never had would
			// be asserting something neither production nor the child does.
			var roots []string
			if got := os.Getenv("XDG_CONFIG_HOME"); got != "" {
				roots = []string{filepath.Join(got, "goose")}
			} else {
				for _, rel := range gooseHomeConfigRelPaths {
					real := filepath.Join(append([]string{realHome}, rel...)...)
					if !liveGooseIsDir(real) {
						continue
					}
					roots = append(roots, filepath.Join(append([]string{home}, rel...)...))
				}
			}
			for _, root := range roots {
				if !liveGooseConfigPresent(root) {
					t.Errorf("the child resolves %s and finds no config.yaml or secrets.yaml there", root)
				}
				// The whole directory, not a named subset.
				for _, name := range []string{"config.yaml", "settings.json"} {
					if _, err := os.Stat(filepath.Join(root, name)); err != nil {
						t.Errorf("%s did not arrive under %s", name, root)
					}
				}
			}
			if tc.wantXDG != (os.Getenv("XDG_CONFIG_HOME") != "") {
				t.Errorf("XDG_CONFIG_HOME = %q, want it set: %v", os.Getenv("XDG_CONFIG_HOME"), tc.wantXDG)
			}
			if os.Getenv("HOME") != home {
				t.Errorf("HOME = %q, want the isolated home %s", os.Getenv("HOME"), home)
			}
		})
	}
}

// TestGooseIsolateCarriesNothingRatherThanGuessing is the zero case, and it is
// the one that decides the skip. A config root that exists but holds neither
// config.yaml nor secrets.yaml cannot authenticate, and a turn that cannot
// authenticate reports an error indistinguishable from the mode breaking the
// backend — so this must return 0, and the live test must skip rather than run.
func TestGooseIsolateCarriesNothingRatherThanGuessing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture paths are POSIX-shaped; the branch itself is platform-independent")
	}
	realHome := t.TempDir()
	empty := filepath.Join(realHome, ".config", "goose")
	if err := os.MkdirAll(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(empty, "sessions.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	if carried := liveGooseIsolate(t, t.TempDir(), realHome, ""); carried != 0 {
		t.Errorf("carried %d roots from a directory with no config.yaml or secrets.yaml, want 0: a turn that cannot authenticate reads as a mode failure", carried)
	}
}

// liveGooseCarry copies one config root into an isolated target with PRODUCTION'S
// OWN carry, and reports whether the target now holds what a turn needs to
// authenticate. One helper for both branches above, so the file set and the
// skipped-file report cannot differ between the XDG and home-relative cases.
//
// It calls copyGooseConfigDir rather than listing config.yaml and secrets.yaml by
// name: production carries the whole directory, and settings.json matters on some
// machines, so a name list is both a second copy of a policy that already exists
// and a set that silently grows short. Copy rather than link for the same reason
// production falls back to copying when a symlink is refused — and because the
// child is pointed at paths inside this test's own temp home, so a symlink to a
// real user file cannot outlive the test.
//
// A carry failure FAILS the test rather than skipping it. Carrying the config is
// the precondition for the turn meaning anything, and a permission error there is
// a fact about this machine, not a reason to report nothing.
func liveGooseCarry(t *testing.T, source, target string) bool {
	t.Helper()
	skipped, err := copyGooseConfigDir(source, target)
	if err != nil {
		t.Fatalf("carry goose config from %s to %s: %v", source, target, err)
	}
	if len(skipped) > 0 {
		t.Logf("carry from %s skipped %v, exactly as production would log it", source, skipped)
	}
	return liveGooseConfigPresent(target)
}

// liveGooseRealHome is the home a real `goose configure` wrote to, or "" when
// there is not one. It is read HERE, before the test replaces HOME, which is the
// whole reason it is a separate call: a helper that read HOME itself would look
// under the empty isolated home the test just made, find nothing, and skip on
// every host. A skip reads as a pass, so a test that can only skip is as dead as
// one that can only fail.
func liveGooseRealHome() string {
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	return os.Getenv("USERPROFILE")
}

// liveGooseConfigRoots returns every config root the CHILD would resolve, in the
// order production resolves them. `home` and `xdg` are passed in rather than read
// from the environment, so a caller that has replaced either cannot make this
// resolve against something other than the real configuration.
//
// The order is the point, and it is not a detail. linkGooseConfigDirsWith returns
// immediately when XDG_CONFIG_HOME is set, so in production an inherited
// XDG_CONFIG_HOME decides where the config comes from and no home-relative root is
// consulted at all. A helper that probed the home first would hand back a stale
// ~/.config/goose on a machine whose XDG_CONFIG_HOME points elsewhere, the test
// would copy that and repoint XDG_CONFIG_HOME at the copy, and the child would
// authenticate against the wrong config — failing for a reason that has nothing
// to do with chat mode. So XDG_CONFIG_HOME goes first, matching the early return
// exactly.
//
// Every home-relative root that exists is returned, not the first hit: production
// carries all of them, so a macOS host with both ~/.config/goose and
// ~/Library/Application Support/goose populated gets both, and using one would
// again be a difference from production.
//
// One home, not both: gooseHomeDir takes the FIRST non-empty of HOME and
// USERPROFILE, so consulting the second as well would carry a root production
// would never look at.
func liveGooseConfigRoots(t *testing.T, home, xdg string) []string {
	t.Helper()
	// An inherited XDG_CONFIG_HOME is absolute and the child reads it directly, so
	// it is the only root that matters when it is set.
	if xdg != "" {
		path := filepath.Join(xdg, "goose")
		if liveGooseIsDir(path) {
			return []string{path}
		}
		return nil
	}
	if home == "" {
		return nil
	}
	// gooseHomeConfigRelPaths, not a list spelled out here: it is the same list
	// configureGooseIsolation uses to decide what to carry, so a test spelling its
	// own would test a platform this build does not support and would pass on one
	// machine and skip on another.
	var roots []string
	for _, rel := range gooseHomeConfigRelPaths {
		path := filepath.Join(append([]string{home}, rel...)...)
		if liveGooseIsDir(path) {
			roots = append(roots, path)
		}
	}
	return roots
}

// liveGooseRelPathFor returns the home-relative path a source root sits at under
// the REAL home, which is what makes the isolated copy resolvable at the same
// place the real one was. It is relative to the passed home, not the ambient one,
// for the reason liveGooseRealHome documents.
//
// The SECOND root matters: on macOS both ~/.config/goose and
// ~/Library/Application Support/goose can exist, and a child that must be able to
// resolve either needs the target at the matching path rather than a single
// merged directory.
func liveGooseRelPathFor(source, home string) ([]string, bool) {
	for _, rel := range gooseHomeConfigRelPaths {
		if filepath.Join(append([]string{home}, rel...)...) == source {
			return rel, true
		}
	}
	return nil, false
}

func liveGooseIsDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// liveGooseConfigPresent reports whether a carried root holds what a turn needs
// to authenticate: config.yaml (the provider and model) or secrets.yaml (the
// key). Either alone is enough, so this is an OR rather than a requirement for
// both — a user whose key is in the keyring and whose config names the provider
// has the first, and a file-based one has both.
func liveGooseConfigPresent(dir string) bool {
	for _, name := range []string{"config.yaml", "secrets.yaml"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	return false
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
