package ai

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"
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

// probeSettleTimeout bounds ONE settleAbandonedProbes call, and it bounds the
// whole call rather than each join in it — the settle runs from a t.Cleanup,
// where an unbounded wait is a package that never finishes and a result set only
// a killed run produces.
//
// It sits well past the 10s cap both probes put on their own child
// (probeCodexFeatures, probeClaudeCapabilities), because a flight that outlives
// that cap is a killed child winding down rather than a probe that will take
// minutes. A timeout here is a bug report and not a slow machine, which is why it
// fails the test with the flight it was waiting on rather than passing quietly.
const probeSettleTimeout = 30 * time.Second

// probeIdentity is one harness binary as BOTH probe groups key it.
//
// The two identity types stay distinct in production because a codex verdict and
// a claude verdict are not interchangeable answers, but a test's fake is one file
// that either probe may be pointed at. So the ledger keeps both readings of it
// and the settle joins both groups, rather than making every test declare which
// probe it meant.
type probeIdentity struct {
	codex  codexBinaryID
	claude claudeBinaryID
}

// probeLedger is what one test knows about the probes it could have abandoned: the
// identities of the fake harness binaries it created, and whether a settle has been
// claimed for them yet.
//
// It needs no lock of its own. Every access is on the test's own goroutine — a
// test body and the cleanups it registered both run there, and a flight goroutine
// never sees a ledger — so the only thing that could interleave is two tests, and
// those do not share one. The MAP is the other way round: it is package-level and
// keyed by *testing.T, and a plain map would be correct only while every test in
// this package is serial, which is a fact about today's tests rather than a
// property of this code.
type probeLedger struct {
	ids      []probeIdentity
	captured bool
}

// probeLedgers maps *testing.T to its probeLedger.
var probeLedgers sync.Map

// ledgerFor returns t's ledger, creating it — and the one cleanup that forgets it
// — if this is the test's first touch of it. Every path that reaches the ledger
// goes through here, because a path that created one without the forget would pin
// a finished *testing.T in a package-level map for the rest of the run: a capture
// test that never builds a fake reaches the ledger through markCaptureInstalled
// alone, and the settle it asks for has nothing to join.
func ledgerFor(t *testing.T) *probeLedger {
	t.Helper()
	led, loaded := probeLedgers.LoadOrStore(t, &probeLedger{})
	if !loaded {
		// First touch by this test. Deleted last of all, so the map cannot pin a
		// finished test — and everything its closures hold — for the rest of the
		// run.
		t.Cleanup(func() { probeLedgers.Delete(t) })
	}
	return led.(*probeLedger)
}

// recordProbeIdentity adds a fake's identity to t's ledger.
//
// The forget is registered on the ledger's first touch, wherever that is, rather
// than inside a settle, because it is the one position that cannot cut a settle
// short. The settle is registered inside claimProbeSettle, which every
// registration path goes through, and that can happen at any point in the test
// body: a cleanup registered at creation runs LAST (t.Cleanup is LIFO), so it
// runs after every settle whatever order they were registered in. A forget hung
// off a settle would instead run at that settle's position, and a test that
// installed its capture afterwards would have its ledger deleted before the
// capture could drain it.
func recordProbeIdentity(t *testing.T, id probeIdentity) {
	t.Helper()
	led := ledgerFor(t)
	led.ids = append(led.ids, id)
}

// markCaptureInstalled records that this test's settle belongs to its capture, and
// is what makes the capture rather than the fake the owner of it.
//
// The capture is the better owner when a test has both, and the reason is ordering
// rather than tidiness: it can put the settle and the restore of the previous
// logger in ONE cleanup, so nothing can register between them. A fake's settle is
// its own cleanup, and a test that creates its fake BEFORE installing the capture
// registers that one first — which LIFO runs LAST, after the capture's cleanup has
// already restored the handler. It is the last settle of such a test and finds
// nothing in flight, so the drain that matters still happened inside the capture's
// cleanup, before the restore.
func markCaptureInstalled(t *testing.T) {
	t.Helper()
	ledgerFor(t).captured = true
}

// claimProbeSettle registers a settle cleanup for t, unless its capture already
// owns one. Its one caller is registerProbeIdentity, and it covers the tests that
// read no log at all — most of the probe tests, which are just as able to walk
// away from a flight as the ones that do.
//
// It is a claim rather than an "is there anything to settle" check: a test whose
// capture claimed first gets nothing here, because a second settle would drain
// nothing and cost a join per group to prove it.
func claimProbeSettle(t *testing.T) {
	t.Helper()
	led := ledgerFor(t)
	if led.captured {
		return
	}
	led.captured = true
	t.Cleanup(func() { settleAbandonedProbes(t) })
}

// registerProbeIdentity records the identity of a fake harness binary and asks for
// the settle to follow the test out. It is the whole of what a test that creates a
// fake needs, so this is what fakeHarnessPolicyBinary calls for the two harnesses
// that HAVE a probe.
//
// It is here rather than left to each test because a test cannot know whether it
// abandoned a probe: the flight belongs to codexFeaturesFor or
// claudeCapabilitiesFor, which walk away from it on purpose. Asking every test to
// remember that is how #855's settle reached seven of the tests that could use it
// and left the rest leaking into the next test's capture.
//
// A test that also installs a capture gets ONE settle rather than two: the capture
// claims it (markCaptureInstalled) and this stands down. Neither helper has to know
// whether the other has run, which is what keeps the settle-before-restore ordering
// a property of captureProcessLogs alone rather than of the order two helpers happen
// to be called in.
//
// The identity is read HERE, from the file as it was just written, and is never
// re-derived from the path at settle time: t.TempDir has removed the file by
// then, so a LookPath there fails, and a settle that cannot name the key it
// means to join would skip exactly the flight it exists to join. That is also why
// there is nothing to skip on this path — a fake that cannot be resolved cannot
// have started a probe either, and saying so here points at the line that wrote
// the file rather than reporting it as a silent absence during cleanup, where a
// t.Fatalf would abort the rest of the chain including the logger restore.
//
// A test that writes its own fake binary — TestCodexProbeIsRekeyedWhenTheBinaryChanges
// does — must call this itself if a probe on it can outlive the test.
func registerProbeIdentity(t *testing.T, path string) {
	t.Helper()
	// Resolved exactly the way codexFeaturesFor and claudeCapabilitiesFor resolve
	// it, so the keys built from this below are the keys the flights are filed
	// under rather than near misses of them.
	resolved, err := exec.LookPath(path)
	if err != nil {
		t.Fatalf("the fake just written is not resolvable as an executable (%v), so no probe could ever have started on it", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		t.Fatalf("Stat the fake just written: %v", err)
	}
	recordProbeIdentity(t, probeIdentity{
		codex:  codexBinaryID{path: resolved, size: info.Size(), modTime: info.ModTime()},
		claude: claudeBinaryID{path: resolved, size: info.Size(), modTime: info.ModTime()},
	})
	claimProbeSettle(t)
}

// probeFlight is one group, the key this test's fake is filed under in it, the
// path to name in a report about that key, and the no-op flight to join when
// nothing is in flight under it.
type probeFlight struct {
	name  string
	group *singleflight.Group
	key   string
	path  string
	empty func() (any, error)
}

// flightsFor is the two flights one fake binary can be in. Both are named because
// a caller cannot tell from a path whether the flight it abandoned was a probe or
// a capability probe, so a settle joins both and a report says which one was still
// running.
//
// The path is carried per flight rather than read off the codex identity by the
// caller, because the two readings happen to be the same file today and a report
// that named the wrong one would be a lie the ledger's shape does not prevent.
func flightsFor(id probeIdentity) []probeFlight {
	return []probeFlight{
		{"codex", &codexProbeGroup, id.codex.probeKey(), id.codex.path, func() (any, error) { return codexFeatureSupport{}, nil }},
		{"claude", &claudeProbeGroup, id.claude.probeKey(), id.claude.path, func() (any, error) { return claudeCapabilities{}, nil }},
	}
}

// settleAbandonedProbes blocks until no probe goroutine for any fake binary this
// test created is still running.
//
// It is a JOIN, not a sleep. Both codexFeaturesFor and claudeCapabilitiesFor
// deliberately walk away from a flight whose caller has stopped waiting — a
// caller that cannot use the verdict must not block on it — so the goroutine
// singleflight started outlives them and keeps logging through the process default
// logger. A locked capture buffer makes that write SAFE (see lockedBuffer); this
// makes it FINISHED, which is what stops a straggler from one test's abandoned
// probe landing in the NEXT test's capture and failing an assertion that nothing
// was logged.
//
// singleflight.Group.DoChan is the join primitive, and it is exact rather than
// hopeful: with a call already in flight for the key, DoChan hands back THAT
// call's result channel and never runs the function beside it, so receiving from
// it means the abandoned probe has finished. With nothing in flight the function
// runs and returns at once, which is the same answer. There is no window to sleep
// through, and no dependence on the probe's 10s cap.
//
// It is also safe against the callers a test still has running: doCall deletes
// the key from the group's map BEFORE it sends on the result channels, so by the
// time this returns no later caller can join the no-op flight and be handed
// `codexFeatureSupport{}` as a verdict. The no-op stores nothing, so it cannot
// poison the cache either.
//
// Two ways to arrive here, and neither is a skip: a test that created no fake has
// no ledger and nothing to settle, and a test whose flight has already landed pays
// one function call per group to learn so. There is no third path that skips a
// flight this cannot name — every flight a test could have started is under a key
// built from a fake that registered here.
//
// The wait is bounded by probeSettleTimeout and REPORTS rather than aborts, and
// t.Errorf is what reports it: it marks the test failed and returns, so the rest
// of the cleanup chain still runs — including the restore of the previous default
// logger, which is the one step that must not be skipped on the way out. Every
// call site but one reaches this from a cleanup, where a Fatal would take those
// steps with it.
func settleAbandonedProbes(t *testing.T) {
	t.Helper()
	led, ok := probeLedgers.Load(t)
	if !ok {
		return // this test created no fake, so it started no flight
	}
	// Copied rather than read in place, so the ledger outlives this call for the
	// second settle a test with both helpers gets, and so nothing appends to the
	// slice while the loop below is handing its keys to DoChan.
	ids := append([]probeIdentity(nil), led.(*probeLedger).ids...)
	deadline := time.Now().Add(probeSettleTimeout)
	for _, id := range ids {
		for _, probe := range flightsFor(id) {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				t.Errorf("settling this test's probes spent its whole %s budget with a %s probe for %s still in flight", probeSettleTimeout, probe.name, probe.path)
				return
			}
			timer := time.NewTimer(remaining)
			select {
			case <-probe.group.DoChan(probe.key, probe.empty):
				timer.Stop()
			case <-timer.C:
				t.Errorf("a %s probe for %s was still in flight %s after this test's cleanups began, so it outlived the test and its warnings land in whatever capture runs next", probe.name, probe.path, probeSettleTimeout)
				return
			}
		}
	}
}

// sleepingCodexFake is a codex fake that records each `features list` probe and
// holds it open for two seconds, which is what makes "a second caller joined the
// flight" a fact rather than a timing hope. It is countingClaudeFake's codex
// counterpart, and it exists in both this file and the other probe tests only
// because the log path and the sleep are what a caller INSIDE a flight needs.
//
// Two seconds is that use's requirement rather than a default. A test that has to
// put a second caller inside the flight wants a window the caller cannot miss; a
// test that only has to END while the flight is open wants a fraction of it,
// because it pays the sleep again on every -count. The short one is
// sleepingCodexFakeHolding.
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
	return sleepingCodexFakeHolding(t, 2*time.Second)
}

// sleepingCodexFakeHolding is sleepingCodexFake with the probe held open for
// exactly as long as the caller asks.
//
// The hold is CONCATENATED rather than formatted, which is what leaves the
// script's own printf verbs alone, and it is written as a plain fractional count
// rather than Go's duration syntax so the shell gets a number every `sleep`
// takes rather than a suffix it may not. It is not a shorter `sleep` in a second
// copy of this script: the two fakes differ in that one number alone, and a copy
// of the body is a second thing to keep in step with the argv the child is
// driven under.
func sleepingCodexFakeHolding(t *testing.T, hold time.Duration) (bin, probeLog string) {
	t.Helper()
	setHarnessPolicyParentEnv(t)
	probeLog = filepath.Join(t.TempDir(), "probes")
	t.Setenv("CODEX_PROBE_LOG", probeLog)
	t.Setenv("GHOST_PASSTHROUGH_ENV", "CODEX_PROBE_LOG")
	bin = fakeHarnessPolicyBinary(t, "codex", `
if [ "$1" = "features" ]; then
  printf 'probe\n' >> "$CODEX_PROBE_LOG"
  sleep `+strconv.FormatFloat(hold.Seconds(), 'f', 3, 64)+` >/dev/null 2>&1
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
	// Settle the probe this call walked away from before reading the log, which
	// is mid-test and so is NOT the cleanup every probe test now gets from its
	// helper. Two reasons, and the second is the one that made this test a data
	// race on main (#853). The abandoned goroutine keeps running after Reflect
	// returns — deliberately, so a caller that cannot use the verdict does not
	// block on it — and harnessCommand inside it logs a scratch-budget warning
	// through the process default logger, which is `logs`. And even with that
	// write made safe, an unsettled probe makes BOTH assertions below read a log
	// a straggler can still append to. Settling HERE is what makes "the cancelled
	// turn reported nothing" a statement about the whole turn rather than about
	// this instant; the end-of-test settle only keeps the straggler out of the
	// NEXT test.
	settleAbandonedProbes(t)
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

// TestCodexProbeIsSettledAtTestEnd is the contract #855's hand-placed settle
// calls used to carry: a probe a test walked away from is FINISHED before that
// test's cleanups end, whether or not the test installed a capture.
//
// #855 made the settle seven tests' job, and the tests it left out leak — a
// straggler's warning lands in the NEXT test's capture, and
// TestCodexProbePassesEveryKeyWhenAllAreDeclared asserts that a correct install
// is silent. So the settle cannot be something a test remembers to do. It is
// registered by the two helpers every such test already calls, and this is the
// test that says so.
//
// What each row is evidence for is stated rather than implied, because the two
// registrations are NOT interchangeable and only one of them is load-bearing for
// this observable:
//
//   - "no capture" isolates fakeHarnessPolicyBinary's registration. Deleting the
//     claimProbeSettle call fails this row and nothing else, which is the proof
//     that the fake reaches the probe tests that read no log at all.
//   - "a capture installed" is coverage of the shape, not a separate isolation of
//     captureProcessLogs's registration. Deleting markCaptureInstalled leaves it
//     PASSING, because the fake's own settle drains the same keys a moment later
//     and t.Cleanup is LIFO, so the flight is still finished before the cleanups
//     end. What the capture's registration adds is WHERE its settle runs: in the
//     same closure as the restore of the previous handler, so it precedes it.
//
// That ordering is asserted by the shape of captureProcessLogs rather than
// observed here, and deliberately so: it cannot be observed, because no probe logs
// anything at the END of its flight — harnessCommand's warnings are all pre-spawn,
// and warnOnWeakerCodexPolicy belongs to a caller that came back, not to an
// abandoned flight. There is no write late enough for the settle-before-restore
// order to move between one test's buffer and the next, so a test for it could
// only assert the ordering back at itself. The evidence for the order is that the
// settle and the restore are ONE closure with the settle first, not that some
// second helper also drains the flight.
//
// Asserted from the PARENT, because a cleanup cannot be observed from inside the
// test that owns it — a subtest's cleanups all run before its t.Run returns, so
// what the parent sees is the state AFTER them. The observable is the probe's own
// store rather than a sleep: the flight below stores its verdict because its
// caller is alive, so an entry proves the probe ran to completion and an empty
// cache proves it was still in flight when the subtest ended.
//
// Gated on windows the way every other shell-script fake in this package is: the
// fake that holds the flight open is a `#!/bin/sh` script, which
// fakeHarnessPolicyBinary refuses to write there, so there is no flight to settle.
// The gate is on the WHOLE test rather than left to the fake's own skip, because a
// skip inside the inner subtest leaves the parent's assertion running against an
// identity nothing was ever filed under.
func TestCodexProbeIsSettledAtTestEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	resetCodexFeatureProbe(t)
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		// The capture is where #856 put the settle, because it is the one thing
		// every log-reading test installs and the one thing that knows when the
		// test is over. See the note above on what this row does and does not
		// isolate.
		{name: "a capture installed", setup: func(t *testing.T) { captureProcessLogs(t) }},
		// And the fake is the other half, because most probe tests read no log at
		// all and are just as able to walk away from a flight. This is the row
		// that fails if either of that helper's registrations goes away.
		{name: "no capture", setup: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var id codexBinaryID
			// Set by the inner subtest, and checked before the parent's own
			// assertion reads id. A subtest that skipped or failed never assigns,
			// and a zero codexBinaryID is a key no flight was ever filed under --
			// so without this the parent would report "still in flight" about a
			// probe that was never started. That is exactly what happened on
			// windows before this check: fakeHarnessPolicyBinary skips there, which
			// skips only the INNER subtest, and the parent went on to condemn a
			// flight nobody had launched.
			started := false
			t.Run("walks away from a probe", func(t *testing.T) {
				if tc.setup != nil {
					tc.setup(t)
				}
				// A tenth of a second rather than sleepingCodexFake's two,
				// because this test pays the hold twice on every -count and needs
				// only a window no scheduling accident crosses: the body below
				// ends microseconds after the spawn it waits for.
				bin, probeLog := sleepingCodexFakeHolding(t, 100*time.Millisecond)
				// Read here, while the fake still exists: the assertion below runs
				// after this subtest's t.TempDir teardown, and a path that no
				// longer exists cannot name the cache entry it is looking for.
				id = codexIdentityFor(t, bin)
				started = true
				// The flight outliving the TEST rather than its caller is the whole
				// situation, and it needs a live context: this goroutine stays
				// blocked in singleflight's select, so the flight lands a positive
				// verdict the assertion below can read. What nobody is watching is
				// the goroutine, not the caller — and no amount of watching it from
				// here is what settles the flight, which is the property under test.
				go func() { _ = codexFeaturesFor(context.Background(), bin) }()
				waitForProbeSpawns(t, probeLog, 1)
				if _, ok := codexCachedSupport(id); ok {
					t.Fatal("the probe answered before the subtest ended, so there is nothing in flight for the settle to join")
				}
			})
			if !started {
				t.Skip("the subtest above never reached its fake, so there is no flight and nothing to assert about it")
			}
			cached, ok := codexCachedSupport(id)
			if !ok {
				t.Fatal("the probe this subtest walked away from was still in flight when its cleanups ended, so its warnings can still land in whatever capture runs next")
			}
			if !cached.probed {
				t.Errorf("the settled probe stored %+v, want the fake's positive verdict: the settle joined something, but not the probe", cached)
			}
		})
	}
}

// TestProbeLedgerIsForgottenByATestThatBuildsNoFake covers the leak a capture-only
// test would otherwise cause, and it exists because that test shape is easy to
// write without noticing.
//
// probeLedgers is package-level and keyed by *testing.T, so a ledger that is never
// deleted pins a finished test — and every closure its cleanups hold — for the rest
// of the binary's run. A capture-only test reaches the ledger through
// markCaptureInstalled alone: it registers nothing in probeLedger.ids, so nothing
// else ever looks the ledger up again, and if the forget-cleanup rides only on the
// fake's registration path then this test's own ledger is the one left behind.
//
// Asserted from the parent, because the forget is a cleanup: the subtest's cleanups
// have all run by the time t.Run returns, so the parent's lookup is the state AFTER
// them. The subtest also checks the ledger was really created, or the assertion
// below would pass on a ledger that never existed — which is the other way this
// could be wrong.
func TestProbeLedgerIsForgottenByATestThatBuildsNoFake(t *testing.T) {
	var sub *testing.T
	t.Run("installs a capture and no fake", func(t *testing.T) {
		sub = t
		captureProcessLogs(t)
		if _, ok := probeLedgers.Load(t); !ok {
			t.Fatal("a capture-only test kept no ledger, so there is nothing here to forget and the assertion in the parent would be vacuous")
		}
	})
	if _, ok := probeLedgers.Load(sub); ok {
		t.Error("a test that installed a capture and built no fake left its ledger behind, so the package-level map pins the finished test for the rest of the run; the forget-cleanup has to ride on ledgerFor, which every path to the ledger goes through")
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
