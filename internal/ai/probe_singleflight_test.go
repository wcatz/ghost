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
	setHarnessPolicyParentEnv(t)
	probeLog := filepath.Join(t.TempDir(), "probes")
	t.Setenv("CODEX_PROBE_LOG", probeLog)
	t.Setenv("GHOST_PASSTHROUGH_ENV", "CODEX_PROBE_LOG")

	// The sleep is not decoration. Without a probe that outlives the burst's
	// arrival, the first caller to be scheduled can finish and fill the cache
	// before the last one looks, and an implementation with no single-flight at
	// all would pass this test on a fast machine. Holding the probe open makes
	// the duplicate spawns certain rather than likely, so the test fails for the
	// reason it names.
	bin := fakeHarnessPolicyBinary(t, "codex", `
if [ "$1" = "features" ]; then
  printf 'probe\n' >> "$CODEX_PROBE_LOG"
  sleep 1
  printf 'shell_tool stable true\nunified_exec stable true\nview_image stable true\napps stable true\nplugins stable true\ntool_suggest stable true\nskill_mcp_dependency_install stable true\nremote_plugin stable true\nhooks stable true\nmulti_agent stable true\n'
  exit 0
fi
printf '%s' 'KEEP'
`)

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
	setHarnessPolicyParentEnv(t)
	probeLog := filepath.Join(t.TempDir(), "probes")
	t.Setenv("CLAUDE_PROBE_LOG", probeLog)
	t.Setenv("GHOST_PASSTHROUGH_ENV", "CLAUDE_PROBE_LOG")

	bin := fakeHarnessPolicyBinary(t, "claude", `
if [ "$1" = "--help" ]; then
  printf 'probe\n' >> "$CLAUDE_PROBE_LOG"
  sleep 1
  printf '%s\n' '--safe-mode' '--restricted' '--strict-mcp-config' '--disable-slash-commands' '--tools' '--disallowedTools' '--setting-sources'
  exit 0
fi
printf '%s' 'KEEP'
`)

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
func countingClaudeFake(t *testing.T) (bin, probeLog string) {
	t.Helper()
	setHarnessPolicyParentEnv(t)
	probeLog = filepath.Join(t.TempDir(), "probes")
	t.Setenv("CLAUDE_PROBE_LOG", probeLog)
	t.Setenv("GHOST_PASSTHROUGH_ENV", "CLAUDE_PROBE_LOG")
	bin = fakeHarnessPolicyBinary(t, "claude", `
if [ "$1" = "--help" ]; then
  printf 'probe\n' >> "$CLAUDE_PROBE_LOG"
  sleep 2
  printf '%s\n' '--safe-mode' '--restricted' '--strict-mcp-config' '--disable-slash-commands' '--tools' '--disallowedTools' '--setting-sources'
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
	setHarnessPolicyParentEnv(t)
	probeLog := filepath.Join(t.TempDir(), "probes")
	t.Setenv("CODEX_PROBE_LOG", probeLog)
	t.Setenv("GHOST_PASSTHROUGH_ENV", "CODEX_PROBE_LOG")
	bin := fakeHarnessPolicyBinary(t, "codex", `
if [ "$1" = "features" ]; then
  printf 'probe\n' >> "$CODEX_PROBE_LOG"
  sleep 2
  printf 'shell_tool stable true\nunified_exec stable true\nview_image stable true\napps stable true\nplugins stable true\ntool_suggest stable true\nskill_mcp_dependency_install stable true\nremote_plugin stable true\nhooks stable true\nmulti_agent stable true\n'
  exit 0
fi
printf '%s' 'KEEP'
`)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	go func() { _ = codexFeaturesFor(leaderCtx, bin) }()
	time.Sleep(200 * time.Millisecond)
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
	path, err := exec.LookPath(bin)
	if err != nil {
		t.Fatalf("LookPath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if cached, ok := codexCachedSupport(codexBinaryID{path: path, size: info.Size(), modTime: info.ModTime()}); !ok || !cached.probed {
		t.Errorf("cache still holds %+v, want the follower's positive verdict", cached)
	}
}
