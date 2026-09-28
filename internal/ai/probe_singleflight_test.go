package ai

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// coldProbeBurst is the size of the concurrent cold-cache burst both probe tests
// use. 50 is not a measured figure: it is a number no single-flight
// implementation can accidentally satisfy by running the probe twice, and a
// number small enough to keep the package's CI budget boring.
const coldProbeBurst = 50

// allCodexFeatureRows is `codex features list` output naming every key the
// no-tools policy asks for, in the three-field row shape isCodexFeatureTable
// requires. One const rather than a repeat in five fakes: a fake that drifts to
// a two-field row would stop being a feature table, and every test using it
// would quietly be testing the unverified path instead of the positive one.
const allCodexFeatureRows = "shell_tool stable true\nunified_exec stable true\nview_image stable true\napps stable true\nplugins stable true\ntool_suggest stable true\nskill_mcp_dependency_install stable true\nremote_plugin stable true\nhooks stable true\nmulti_agent stable true\n"

// claudeHelpFlags is a `claude --help` body declaring every capability
// claudeInvocationArgs requires, so a probe that reads it refuses nothing. It
// exists for the same reason as allCodexFeatureRows: a fake whose help text
// drifts would make a test pass by REFUSING rather than by answering.
const claudeHelpFlags = "--safe-mode\n--restricted\n--strict-mcp-config\n--disable-slash-commands\n--tools\n--disallowedTools\n--setting-sources\n"

// runColdProbeBurst releases n goroutines together and runs fn(i) on each, so
// every caller reaches a COLD cache at once rather than arriving in a stagger.
// The start channel is what makes this a burst: without it the loop would hand
// goroutine 0 a head start long enough for the probe to finish and the cache to
// fill, and the test would pass on a probe that is not single-flighted at all.
func runColdProbeBurst(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			<-start
			fn(i)
		}()
	}
	close(start)
	wg.Wait()
}

// countProbeSpawns counts how many times a fake harness was actually spawned for
// its probe. The count is a FILE rather than an in-process counter because the
// fake is a shell script in a child process, where a Go variable would be
// invisible — the same reason codexFeaturesFake logs instead of counting.
//
// A missing log is zero spawns, not a failure to read: "the probe never ran" is
// one of the answers this test has to be able to report, and t.Fatalf here would
// hide it behind a read error.
func countProbeSpawns(t *testing.T, logPath string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read probe log: %v", err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == "probe" {
			count++
		}
	}
	return count
}

// waitForProbeSpawns blocks until the fake's log records at least want spawns,
// so a test can put a second caller INSIDE a flight instead of guessing how long
// the probe takes to start. It is the difference between a window and a wish:
// these tests are about what happens while a probe is in flight, and a bare sleep
// against a 2s probe is a race against machine speed dressed up as a window.
func waitForProbeSpawns(t *testing.T, logPath string, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if countProbeSpawns(t, logPath) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe log %s never recorded %d spawns", logPath, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// codexIdentityFor is the cache key codexFeaturesFor computes for a binary, so
// a test can look in the cache the way the production code does — through
// codexCachedSupport, expiry and all — instead of reading the sync.Map and
// re-implementing the freshness rule it is testing.
func codexIdentityFor(t *testing.T, binary string) codexBinaryID {
	t.Helper()
	path, err := exec.LookPath(binary)
	if err != nil {
		t.Fatalf("LookPath(%s): %v", binary, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	return codexBinaryID{path: path, size: info.Size(), modTime: info.ModTime()}
}

// sleepingCodexFake is a codex fake that records each `features list` probe and
// holds it open for two seconds, which is what makes "a second caller joined the
// flight" a fact rather than a timing hope. It is countingClaudeFake's codex
// counterpart, and it exists in both this file and the other probe tests only
// because the log path and the sleep are what a caller INSIDE a flight needs.
//
// The sleep's fds are redirected, and that is load-bearing rather than tidy: the
// sleep is a GRANDCHILD of the process Go spawned, it inherits the stdout pipe,
// and `Cmd.Output` waits for that pipe to close — so without the redirect a
// cancelled probe's `Wait` blocks until the orphaned sleep exits anyway, and
// every test here that needs the flight to LAND would be waiting out a sleep
// instead of observing a cancellation. Redirected, the shell dying closes the
// pipe and the wait returns at once, which is what killing a real single-process
// binary does.
func sleepingCodexFake(t *testing.T) (bin, probeLog string) {
	t.Helper()
	setHarnessPolicyParentEnv(t)
	probeLog = filepath.Join(t.TempDir(), "probes")
	t.Setenv("CODEX_PROBE_LOG", probeLog)
	t.Setenv("GHOST_PASSTHROUGH_ENV", "CODEX_PROBE_LOG")
	bin = fakeHarnessPolicyBinary(t, "codex", `
if [ "$1" = "features" ]; then
  printf 'probe\n' >> "$CODEX_PROBE_LOG"
  sleep 2 >/dev/null 2>&1
  printf '`+allCodexFeatureRows+`'
  exit 0
fi
printf '%s' 'KEEP'
`)
	return bin, probeLog
}

// TestCodexProbeIsSingleFlightedOnAColdCache: N concurrent callers arriving on a
// cold cache run ONE `codex features list`, and every caller gets that one
// probe's answer.
//
// Before the single-flight, the cache was a plain check-then-probe, so each
// caller that arrived before the first one stored its answer ran its own probe:
// measured 50 spawns for 50 callers. Today's callers classify one batch at a time
// in each lifecycle process, so that cost was unreachable — and the long-lived
// `ghost mcp` server, which outlives every batch, is exactly where a concurrent
// dispatch would land.
//
// Two things are asserted, and the second is not implied by the first: the
// spawn COUNT (the work) and the shared `at` stamp (the sharing). A probe
// duplicated once per caller would fail the count, and a deduplication that
// handed each caller its own copy of the answer would pass the count while the
// callers disagreed about when the binary was probed.
func TestCodexProbeIsSingleFlightedOnAColdCache(t *testing.T) {
	resetCodexFeatureProbe(t)
	// The fake holds its probe open for two seconds, and that is not decoration.
	// Without a probe that outlives the burst's arrival, the first caller to be
	// scheduled can finish and fill the cache before the last one looks, and an
	// implementation with no single-flight at all would pass this test on a fast
	// machine. Holding the probe open makes the duplicate spawns certain rather
	// than likely, so the test fails for the reason it names.
	bin, probeLog := sleepingCodexFake(t)

	results := make([]codexFeatureSupport, coldProbeBurst)
	runColdProbeBurst(coldProbeBurst, func(i int) {
		results[i] = codexFeaturesFor(context.Background(), bin)
	})

	if got := countProbeSpawns(t, probeLog); got != 1 {
		t.Errorf("cold burst of %d callers spawned %d `codex features list` probes, want exactly 1", coldProbeBurst, got)
	}
	for i, got := range results {
		if !got.probed {
			t.Errorf("caller %d got an unprobed verdict, want the shared probe's answer", i)
			continue
		}
		for _, key := range codexNoToolFeatureKeys {
			if !got.declared[key] {
				t.Errorf("caller %d: key %q missing from the shared verdict", i, key)
			}
		}
		// `at` is stamped by the goroutine that ran the probe, so an identical
		// stamp across all callers is the observable fact that they shared ONE
		// probe rather than N identical ones.
		if !got.at.Equal(results[0].at) {
			t.Errorf("caller %d was probed at %s, caller 0 at %s: the answers are not one probe's",
				i, got.at.Format("15:04:05.000000000"), results[0].at.Format("15:04:05.000000000"))
		}
	}
}

// TestClaudeCapabilityProbeIsSingleFlightedOnAColdCache: the same property for
// the older probe, which is a separate cache and a separate spawn.
//
// It is a second test rather than a table row because the two probes share
// nothing but the property: they key different caches, one of them EXPIRES a
// negative and retries it, and only one of them can fail. A shared helper that
// asserted one of those differences away would leave the other untested.
func TestClaudeCapabilityProbeIsSingleFlightedOnAColdCache(t *testing.T) {
	resetClaudeCapabilityProbe(t)
	bin, probeLog := countingClaudeFake(t)

	caps := make([]claudeCapabilities, coldProbeBurst)
	errs := make([]error, coldProbeBurst)
	runColdProbeBurst(coldProbeBurst, func(i int) {
		caps[i], errs[i] = claudeCapabilitiesFor(context.Background(), bin)
	})

	if got := countProbeSpawns(t, probeLog); got != 1 {
		t.Errorf("cold burst of %d callers spawned %d `claude --help` probes, want exactly 1", coldProbeBurst, got)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
			continue
		}
		if caps[i] != caps[0] {
			t.Errorf("caller %d got %+v, caller 0 got %+v: the answers are not one probe's", i, caps[i], caps[0])
		}
	}
	// The shared answer has to be the one that REFUSES nothing, or a burst that
	// deduplicated onto an empty capability set would pass both assertions above
	// while failing every real caller at claudeInvocationArgs.
	if !caps[0].safeMode || !caps[0].restricted || !caps[0].strictMCP || !caps[0].tools || !caps[0].disallowedTools {
		t.Errorf("shared verdict %+v lacks a required no-tools capability", caps[0])
	}
}

// countingClaudeFake is codexFeaturesFake's claude counterpart: a fake that
// records each `--help` probe to a log file and holds the probe open for a
// beat, so a test can put a second caller INSIDE a flight rather than racing it.
// The sleep's fds are redirected for the reason given on sleepingCodexFake: the
// sleep holds the stdout pipe open past the shell's death, and a cancelled probe
// would not land until it finished.
func countingClaudeFake(t *testing.T) (bin, probeLog string) {
	t.Helper()
	setHarnessPolicyParentEnv(t)
	probeLog = filepath.Join(t.TempDir(), "probes")
	t.Setenv("CLAUDE_PROBE_LOG", probeLog)
	t.Setenv("GHOST_PASSTHROUGH_ENV", "CLAUDE_PROBE_LOG")
	bin = fakeHarnessPolicyBinary(t, "claude", `
if [ "$1" = "--help" ]; then
  printf 'probe\n' >> "$CLAUDE_PROBE_LOG"
  sleep 2 >/dev/null 2>&1
  printf '%s' "`+claudeHelpFlags+`"
  exit 0
fi
printf '%s' 'KEEP'
`)
	return bin, probeLog
}

// TestClaudeCapabilityProbeFollowerDoesNotInheritALeaderCancellation: a leader
// whose context dies mid-probe must not fail the followers whose contexts are
// fine. Found by review, and it is the one case where the single-flight is
// worse than no single-flight: before it, every caller probed under its own
// context, so one client's disconnect could only fail that client. Sharing the
// leader's ERROR made it fail every concurrent `claude -p` turn in the burst —
// and claudeCapabilitiesFor's error refuses the whole harness call, so in the
// long-lived `ghost mcp` server a disconnect took down unrelated work.
//
// A plain `Do` fails this: the follower receives the leader's wrapped
// context.Canceled. A dead caller's own context cannot rescue it either, which
// is why the fix tests the shared error rather than wrapping the flight.
func TestClaudeCapabilityProbeFollowerDoesNotInheritALeaderCancellation(t *testing.T) {
	resetClaudeCapabilityProbe(t)
	bin, _ := countingClaudeFake(t)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := claudeCapabilitiesFor(leaderCtx, bin)
		leaderDone <- err
	}()
	// Let the leader own the flight, then join it. The fake holds its probe
	// open, so the window is wide rather than lucky.
	time.Sleep(200 * time.Millisecond)
	followerDone := make(chan error, 1)
	var caps claudeCapabilities
	go func() {
		var err error
		caps, err = claudeCapabilitiesFor(context.Background(), bin)
		followerDone <- err
	}()
	time.Sleep(200 * time.Millisecond)
	// Cancel MID-FLIGHT, which is the only ordering that tests anything: the
	// leader's probe has to die while the follower is still waiting on it. A
	// cancel after the follower returns would find a flight that already
	// succeeded, and both callers would be right.
	cancelLeader()

	if err := <-followerDone; err != nil {
		t.Fatalf("follower with a live context inherited the leader's failure: %v", err)
	}
	if !caps.safeMode || !caps.restricted || !caps.strictMCP || !caps.tools || !caps.disallowedTools {
		t.Errorf("follower got %+v, want the capabilities the real binary declares", caps)
	}
	// The leader is the caller that lost its context, so it is the caller that
	// must report it.
	if err := <-leaderDone; err == nil {
		t.Error("the cancelled leader reported success; its own context governed nothing")
	}
}

// TestClaudeCapabilityProbeFollowerWithADeadContextDoesNotInheritSuccess: the
// converse, and the half a fallback alone would leave broken. A caller whose
// OWN context is already dead must report that, not block for the leader's
// probe and then report the leader's success — which is what it did before its
// own context was consulted at all, and which reads to CLIClient.run as a
// working probe from a turn that is already cancelled.
//
// The assertion is the ERROR, not a duration, so it cannot flake on a slow
// machine: the leader's probe succeeds, so the only way this test can fail is
// if the dead caller's context is consulted at all.
func TestClaudeCapabilityProbeFollowerWithADeadContextDoesNotInheritSuccess(t *testing.T) {
	resetClaudeCapabilityProbe(t)
	bin, _ := countingClaudeFake(t)

	leaderDone := make(chan error, 1)
	go func() {
		_, err := claudeCapabilitiesFor(context.Background(), bin)
		leaderDone <- err
	}()
	time.Sleep(200 * time.Millisecond)

	deadCtx, cancelDead := context.WithCancel(context.Background())
	cancelDead()
	_, err := claudeCapabilitiesFor(deadCtx, bin)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("dead-context caller got %v, want an error wrapping context.Canceled", err)
	}
	if err := <-leaderDone; err != nil {
		t.Errorf("the live leader failed: %v", err)
	}
}

// TestCodexProbeFollowerDoesNotInheritALeaderCancellation: the same property on
// the probe that cannot error, where the damage is quieter. A leader whose
// context dies produces a NEGATIVE verdict, and the negative is cached for
// codexFeatureRetry — so a plain `Do` does not merely hand the followers a
// weaker policy once, it poisons the cache and every caller for the next five
// minutes gets it, including callers that never shared the burst. The follower's
// own probe overwrites the negative, which is why the fix re-probes rather than
// returning the zero value.
func TestCodexProbeFollowerDoesNotInheritALeaderCancellation(t *testing.T) {
	resetCodexFeatureProbe(t)
	bin, probeLog := sleepingCodexFake(t)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	go func() { _ = codexFeaturesFor(leaderCtx, bin) }()
	waitForProbeSpawns(t, probeLog, 1)
	followersDone := make(chan struct{})
	var support codexFeatureSupport
	go func() {
		support = codexFeaturesFor(context.Background(), bin)
		close(followersDone)
	}()
	time.Sleep(200 * time.Millisecond)
	// Mid-flight, for the same reason as the claude test: the leader's probe has
	// to be killed while the follower is still waiting on its verdict.
	cancelLeader()
	<-followersDone

	if !support.probed {
		t.Error("follower with a live context inherited the leader's unprobed negative")
	}
	// The negative must be GONE from the cache, not merely bypassed: it is the
	// five-minute retention that turns one cancelled leader into a process-wide
	// weaker policy.
	if cached, ok := codexCachedSupport(codexIdentityFor(t, bin)); !ok || !cached.probed {
		t.Errorf("cache still holds %+v, want the follower's positive verdict", cached)
	}
}

// TestProbeCodexFeaturesStoresNothingAndTheFlightStoresItsVerdict: the store is
// single-sited, and this is the test that says so.
//
// The store moved out of probeCodexFeatures and into the flight, because the
// decision needs BOTH facts at once — whether an answer is worth keeping, and
// whether the caller that produced it was still alive — and a check-then-act
// split across two functions disagrees with itself in the window between the
// two. A second store inside the probe would put back the exact negative the move
// removed, and no other test would notice: every caller reaches its verdict
// through the flight, which stores a verdict of its own.
//
// The first assertion is on the probe alone, so it is synchronous and cannot race
// the flight goroutine — which is the other reason it is here rather than folded
// into the cancelled-leader test, where the write is asynchronous by nature. The
// cancelled leader's end-to-end shape is TestCodexProbeCancelledLeaderLeavesNoCachedNegative,
// and the converse half of the retention rule (a real unanswering codex IS still
// cached) is TestCodexProbeFailureIsCachedButExpires.
func TestProbeCodexFeaturesStoresNothingAndTheFlightStoresItsVerdict(t *testing.T) {
	resetCodexFeatureProbe(t)
	setHarnessPolicyParentEnv(t)
	bin := codexFeaturesFake(t, allCodexFeatureRows)
	id := codexIdentityFor(t, bin)

	if support := probeCodexFeatures(context.Background(), id.path, id); !support.probed {
		t.Fatalf("probe got %+v, want the fake's answer", support)
	}
	if cached, ok := codexCachedSupport(id); ok {
		t.Fatalf("probeCodexFeatures cached %+v itself; the store belongs to the flight, which is the only place that knows whether the caller was alive", cached)
	}

	if support := codexFeaturesFor(context.Background(), bin); !support.probed {
		t.Errorf("codexFeaturesFor got %+v, want the fake's answer", support)
	}
	if cached, ok := codexCachedSupport(id); !ok || !cached.probed {
		t.Errorf("cache holds %+v, want the flight's positive verdict", cached)
	}
}

// TestCodexProbeCancelledLeaderLeavesNoCachedNegative: the end-to-end shape of
// the same rule, on the path where it actually bites. The follower case is
// covered elsewhere, and there the follower's own re-probe OVERWRITES the
// negative, so the cache looks clean whether or not it was ever written. This
// is the case with no follower to do the overwriting: a cancelled leader on a
// cold cache, alone, in the long-lived `ghost mcp` server — and the next caller
// arrives with a perfectly live context, never shared the burst, and is handed
// "this codex declares nothing" for five minutes.
//
// The wait is not slack. The flight goroutine outlives the caller that walked
// away from it, so the write under test can land AFTER the leader returned, and
// a check made at the instant the leader returned would be a check of nothing.
// Polling for it is watching for the bug over a window two orders of magnitude
// longer than the killed child takes to be reaped and its caller to write.
func TestCodexProbeCancelledLeaderLeavesNoCachedNegative(t *testing.T) {
	resetCodexFeatureProbe(t)
	bin, probeLog := sleepingCodexFake(t)
	id := codexIdentityFor(t, bin)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		_ = codexFeaturesFor(leaderCtx, bin)
	}()
	waitForProbeSpawns(t, probeLog, 1)
	cancelLeader()
	<-leaderDone

	settle := time.Now().Add(500 * time.Millisecond)
	for {
		if cached, ok := codexCachedSupport(id); ok {
			t.Fatalf("a cancelled leader left %+v in the cache, which is served to every caller for %s", cached, codexFeatureRetry)
		}
		if time.Now().After(settle) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// And the consequence, which is the part an operator would see: a later
	// caller with a live context still gets a real answer, and it costs the
	// second probe because the first one told us nothing.
	support := codexFeaturesFor(context.Background(), bin)
	if !support.probed {
		t.Errorf("a live caller after a cancelled leader got %+v, want a real verdict", support)
	}
	if got := countProbeSpawns(t, probeLog); got != 2 {
		t.Errorf("probes spawned %d, want 2: the cancelled leader's, and the live caller's", got)
	}
}

// TestCodexProbeCancelledTurnDoesNotBurnTheUnverifiedWarning: a caller that
// stopped waiting has learned NOTHING about this codex, so the placeholder it
// is handed is not a probe result and must not be reported as one. Reporting it
// fires the "unverified" WARN, and that WARN is once per process precisely so a
// later GENUINE unverified verdict is not silenced — so one cancelled turn in a
// long-lived server would suppress the real diagnostic for the rest of the
// process, and the one case a running server most needs to hear about is the one
// it would have swallowed.
//
// Both halves are asserted because either alone passes on a version that only
// skips the warn: a call that skips it and spends the latch anyway, or one that
// spends the latch and then lets the real verdict through, would each be caught
// by the second.
func TestCodexProbeCancelledTurnDoesNotBurnTheUnverifiedWarning(t *testing.T) {
	resetCodexFeatureProbe(t)
	logs := captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)
	// A codex whose `features list` does not answer, which is the unverified
	// case, and whose turn succeeds so the only thing the cancelled call can
	// produce is the probe verdict.
	bin := fakeHarnessPolicyBinary(t, "codex", `
if [ "$1" = "features" ]; then
  printf 'no features subcommand\n' >&2
  exit 2
fi
printf '%s' 'KEEP'
`)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := (&CodexClient{binary: bin}).Reflect(cancelled, "prompt"); err == nil {
		t.Fatal("a cancelled turn reported success")
	}
	if strings.Contains(logs.String(), "unverified") {
		t.Errorf("a caller that stopped waiting reported the unverified verdict: %q", logs.String())
	}

	if _, _, err := (&CodexClient{binary: bin}).Reflect(context.Background(), "prompt"); err != nil {
		t.Fatalf("second Reflect: %v", err)
	}
	if !strings.Contains(logs.String(), "unverified") {
		t.Errorf("a genuine unverified verdict was silenced by the cancelled turn: %q", logs.String())
	}
}

// TestClaudeCapabilityProbeFollowersReDeduplicateAfterALeaderCancellation: the
// burst survives a dead leader, which is the case a direct re-probe loses. Once
// the leader's flight lands as a cancellation, every follower in the burst needs
// an answer of its own, and if each re-probes DIRECTLY the burst is back to one
// child per caller — the #741 bug reappearing on the rare path out of a common
// one, which is worse than not having fixed it because the burst is now slower
// than before.
//
// The count is two, and both numbers are accounted for: one probe for the flight
// that died with its leader, one for the flight the burst shares on the retry.
// A version that probes directly spawns one per follower, and a version that
// gave up on the retry spawns one and answers none of them.
func TestClaudeCapabilityProbeFollowersReDeduplicateAfterALeaderCancellation(t *testing.T) {
	resetClaudeCapabilityProbe(t)
	bin, probeLog := countingClaudeFake(t)
	const followers = 20

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	go func() { _, _ = claudeCapabilitiesFor(leaderCtx, bin) }()
	waitForProbeSpawns(t, probeLog, 1)

	caps := make([]claudeCapabilities, followers)
	errs := make([]error, followers)
	burstDone := make(chan struct{})
	go func() {
		runColdProbeBurst(followers, func(i int) {
			caps[i], errs[i] = claudeCapabilitiesFor(context.Background(), bin)
		})
		close(burstDone)
	}()
	// Let the whole burst arrive on the doomed flight. The fake holds it open
	// for two seconds, so this is a wide window rather than a lucky one.
	time.Sleep(300 * time.Millisecond)
	cancelLeader()
	<-burstDone

	if got := countProbeSpawns(t, probeLog); got != 2 {
		t.Errorf("a burst of %d followers behind a cancelled leader spawned %d probes, want 2: the dead flight's, and the burst's own", followers, got)
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("follower %d: %v", i, err)
			continue
		}
		if !caps[i].safeMode || !caps[i].restricted || !caps[i].strictMCP || !caps[i].tools || !caps[i].disallowedTools {
			t.Errorf("follower %d got %+v, want the capabilities the real binary declares", i, caps[i])
		}
	}
}

// TestCodexProbeFollowersReDeduplicateAfterALeaderCancellation: the same
// property on the probe whose retry has to OVERWRITE rather than merely refill,
// which is why the two paths differ and why neither may be inferred from the
// other. A cancelled codex leader leaves nothing in the cache at all, so the
// retry flight's own cache check misses and the burst really does share one
// probe; the followers' verdicts are positive rather than a placeholder, and the
// negative the dead leader produced reaches nobody.
func TestCodexProbeFollowersReDeduplicateAfterALeaderCancellation(t *testing.T) {
	resetCodexFeatureProbe(t)
	bin, probeLog := sleepingCodexFake(t)
	const followers = 20

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	go func() { _ = codexFeaturesFor(leaderCtx, bin) }()
	waitForProbeSpawns(t, probeLog, 1)

	verdicts := make([]codexFeatureSupport, followers)
	burstDone := make(chan struct{})
	go func() {
		runColdProbeBurst(followers, func(i int) {
			verdicts[i] = codexFeaturesFor(context.Background(), bin)
		})
		close(burstDone)
	}()
	time.Sleep(300 * time.Millisecond)
	cancelLeader()
	<-burstDone

	if got := countProbeSpawns(t, probeLog); got != 2 {
		t.Errorf("a burst of %d followers behind a cancelled leader spawned %d probes, want 2: the dead flight's, and the burst's own", followers, got)
	}
	for i, got := range verdicts {
		if !got.probed {
			t.Errorf("follower %d got %+v, want the shared retry's answer", i, got)
		}
	}
	if cached, ok := codexCachedSupport(codexIdentityFor(t, bin)); !ok || !cached.probed {
		t.Errorf("cache holds %+v, want the burst's positive verdict", cached)
	}
}
