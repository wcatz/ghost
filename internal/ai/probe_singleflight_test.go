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

const coldProbeBurst = 50

const allCodexFeatureRows = "shell_tool stable true\nunified_exec stable true\nview_image stable true\napps stable true\nplugins stable true\ntool_suggest stable true\nskill_mcp_dependency_install stable true\nremote_plugin stable true\nhooks stable true\nmulti_agent stable true\n"

const claudeHelpFlags = "--safe-mode\n--restricted\n--strict-mcp-config\n--disable-slash-commands\n--tools\n--disallowedTools\n--setting-sources\n"

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

const probeSettleTimeout = 30 * time.Second

type probeIdentity struct {
	codex  codexBinaryID
	claude claudeBinaryID
}

type probeLedger struct {
	ids      []probeIdentity
	captured bool
}

var probeLedgers sync.Map

func ledgerFor(t *testing.T) *probeLedger {
	t.Helper()
	led, loaded := probeLedgers.LoadOrStore(t, &probeLedger{})
	if !loaded {
		t.Cleanup(func() { probeLedgers.Delete(t) })
	}
	return led.(*probeLedger)
}

func recordProbeIdentity(t *testing.T, id probeIdentity) {
	t.Helper()
	led := ledgerFor(t)
	led.ids = append(led.ids, id)
}

func markCaptureInstalled(t *testing.T) {
	t.Helper()
	ledgerFor(t).captured = true
}

func claimProbeSettle(t *testing.T) {
	t.Helper()
	led := ledgerFor(t)
	if led.captured {
		return
	}
	led.captured = true
	t.Cleanup(func() { settleAbandonedProbes(t) })
}

// remember that is how #855's settle reached seven of the tests that could use it

func registerProbeIdentity(t *testing.T, path string) {
	t.Helper()
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

type probeFlight struct {
	name  string
	group *singleflight.Group
	key   string
	path  string
	empty func() (any, error)
}

func flightsFor(id probeIdentity) []probeFlight {
	return []probeFlight{
		{"codex", &codexProbeGroup, id.codex.probeKey(), id.codex.path, func() (any, error) { return codexFeatureSupport{}, nil }},
		{"claude", &claudeProbeGroup, id.claude.probeKey(), id.claude.path, func() (any, error) { return claudeCapabilities{}, nil }},
	}
}

func settleAbandonedProbes(t *testing.T) {
	t.Helper()
	led, ok := probeLedgers.Load(t)
	if !ok {
		return
	}
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

func sleepingCodexFake(t *testing.T) (bin, probeLog string) {
	t.Helper()
	return sleepingCodexFakeHolding(t, 2*time.Second)
}

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

func TestCodexProbeIsSingleFlightedOnAColdCache(t *testing.T) {
	resetCodexFeatureProbe(t)
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
		if !got.at.Equal(results[0].at) {
			t.Errorf("caller %d was probed at %s, caller 0 at %s: the answers are not one probe's",
				i, got.at.Format("15:04:05.000000000"), results[0].at.Format("15:04:05.000000000"))
		}
	}
}

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
	if !caps[0].safeMode || !caps[0].restricted || !caps[0].strictMCP || !caps[0].tools || !caps[0].disallowedTools {
		t.Errorf("shared verdict %+v lacks a required no-tools capability", caps[0])
	}
}

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

func TestClaudeCapabilityProbeFollowerDoesNotInheritALeaderCancellation(t *testing.T) {
	resetClaudeCapabilityProbe(t)
	bin, _ := countingClaudeFake(t)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := claudeCapabilitiesFor(leaderCtx, bin)
		leaderDone <- err
	}()

	time.Sleep(200 * time.Millisecond)
	followerDone := make(chan error, 1)
	var caps claudeCapabilities
	go func() {
		var err error
		caps, err = claudeCapabilitiesFor(context.Background(), bin)
		followerDone <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancelLeader()

	if err := <-followerDone; err != nil {
		t.Fatalf("follower with a live context inherited the leader's failure: %v", err)
	}
	if !caps.safeMode || !caps.restricted || !caps.strictMCP || !caps.tools || !caps.disallowedTools {
		t.Errorf("follower got %+v, want the capabilities the real binary declares", caps)
	}

	if err := <-leaderDone; err == nil {
		t.Error("the cancelled leader reported success; its own context governed nothing")
	}
}

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

	support := codexFeaturesFor(context.Background(), bin)
	if !support.probed {
		t.Errorf("a live caller after a cancelled leader got %+v, want a real verdict", support)
	}
	if got := countProbeSpawns(t, probeLog); got != 2 {
		t.Errorf("probes spawned %d, want 2: the cancelled leader's, and the live caller's", got)
	}
}

func TestCodexProbeCancelledTurnDoesNotBurnTheUnverifiedWarning(t *testing.T) {
	resetCodexFeatureProbe(t)
	logs := captureCodexWarnings(t)
	setHarnessPolicyParentEnv(t)
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

	// race on main (#853). The abandoned goroutine keeps running after Reflect

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

// #855 made the settle seven tests' job, and the tests it left out leak — a

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

		{name: "a capture installed", setup: func(t *testing.T) { captureProcessLogs(t) }},
		{name: "no capture", setup: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var id codexBinaryID
			started := false
			t.Run("walks away from a probe", func(t *testing.T) {
				if tc.setup != nil {
					tc.setup(t)
				}
				bin, probeLog := sleepingCodexFakeHolding(t, 100*time.Millisecond)
				id = codexIdentityFor(t, bin)
				started = true
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

// child per caller — the #741 bug reappearing on the rare path out of a common
