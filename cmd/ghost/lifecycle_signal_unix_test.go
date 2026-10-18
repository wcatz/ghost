//go:build !windows

package main

// The coordinator's interrupt handling, driven where it is wired: runLifecycle
// in a built ghost binary, against a fake harness. The helpers it is made of
// (interruptOutcome, applyInterrupt, awaitPhaseSweep, phaseWatchdog) have unit
// tests of their own, but none of those can see whether runLifecycle calls
// them, so a loop that dropped the stop between phases, the wait for the sweep
// or the record of the phase in flight passed every one of them. These tests
// read the outcome the run leaves behind, the failure marker, and the processes
// it left alive or not.
//
// The binary is built with `go build` into the test's own directory and never
// re-exec'd from the test binary (see CLAUDE.md), its environment is built from
// an allowlist rather than filtered from os.Environ(), and HOME, every XDG_*
// directory and TMPDIR point inside the test's directory: the coordinator's
// audit sweep deletes stale files under os.TempDir().

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/memory"
)

// signalTestProject is the project both tests run the chain for.
const signalTestProject = "sigproj"

// signalCancelMargin is how long the between-phases test keeps the coordinator
// parked on its config after the signal has left the pending set; see the
// comment where it is used.
const signalCancelMargin = 250 * time.Millisecond

// lifecycleSignalBuffer is a bytes.Buffer whose writes are safe alongside
// reads: os/exec copies the coordinator's output into it from its own
// goroutine while the test reads it to decide when to act.
type lifecycleSignalBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lifecycleSignalBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lifecycleSignalBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// signalHarnessScript is the fake claude the resolve phase calls. The two
// capability checks the claude adapter makes are answered at once; any other
// call is the classification the phase is blocked on, and it:
//
//   - starts a helper in the harness's OWN process group (production gives
//     every harness a group of its own, internal/ai) that ignores SIGTERM, so
//     only the harness escalation's group kill can end it;
//   - records its own pid, its parent's (the phase process, the leader of the
//     phase's group) and the helper's, then writes the ready file last, so a
//     test that sees the ready file can read all three;
//   - exits on SIGTERM, and otherwise waits, bounded so that nothing it starts
//     can outlive a broken run by more than a couple of minutes.
//
// The waits are loops of short sleeps because a shell runs a trap only once
// its current foreground child has finished.
const signalHarnessScript = `#!/bin/sh
dir=@DIR@
case "$1" in
  --version)
    printf '%s\n' '2.0.15'
    exit 0
    ;;
  --help)
    printf '%s\n' '--safe-mode' '--restricted' '--strict-mcp-config' \
      '--disable-slash-commands' '--tools' '--disallowedtools' '--setting-sources'
    exit 0
    ;;
esac
printf '%s\n' "$*" >> "$dir/harness.argv"
cat > /dev/null
(trap '' TERM; i=0; while [ "$i" -lt 2400 ]; do sleep 0.05; i=$((i + 1)); done) </dev/null >/dev/null 2>&1 &
printf '%s\n' "$!" > "$dir/helper.pid"
printf '%s\n' "$$" > "$dir/harness.pid"
printf '%s\n' "$PPID" > "$dir/phase.pid"
trap 'exit 0' TERM
: > "$dir/ready.tmp" && mv "$dir/ready.tmp" "$dir/ready"
i=0
while [ "$i" -lt 2400 ]; do sleep 0.05; i=$((i + 1)); done
exit 0
`

// signalSandbox is one run's directory tree and the environment built for it.
type signalSandbox struct {
	root, bin, data, config string
	env                     []string
}

// newSignalSandbox builds the ghost binary, lays out the sandbox, installs the
// fake harness and writes a config that enables resolve and supersede, so the
// chain has a later phase that must not start.
func newSignalSandbox(t *testing.T) *signalSandbox {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the ghost binary")
	}
	root := t.TempDir()
	s := &signalSandbox{
		root:   root,
		bin:    filepath.Join(root, "bin"),
		data:   filepath.Join(root, "data"),
		config: filepath.Join(root, "config"),
	}
	home := filepath.Join(root, "home")
	cache := filepath.Join(root, "cache")
	state := filepath.Join(root, "state")
	tmp := filepath.Join(root, "tmp")
	scratchDir := filepath.Join(root, "scratch")
	for _, d := range []string{s.bin, s.data, s.config, home, cache, state, tmp, scratchDir, filepath.Join(s.config, "ghost")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if out, err := exec.Command("go", "build", "-o", filepath.Join(s.bin, "ghost"), ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	script := strings.ReplaceAll(signalHarnessScript, "@DIR@", s.bin)
	if err := os.WriteFile(filepath.Join(s.bin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake harness: %v", err)
	}
	s.env = []string{
		"PATH=" + s.bin + string(os.PathListSeparator) + "/usr/bin" + string(os.PathListSeparator) + "/bin",
		"HOME=" + home,
		"USER=ghosttest",
		"LOGNAME=ghosttest",
		"SHELL=/bin/sh",
		"LANG=C",
		"TERM=dumb",
		"TMPDIR=" + tmp,
		"XDG_CONFIG_HOME=" + s.config,
		"XDG_DATA_HOME=" + s.data,
		"XDG_CACHE_HOME=" + cache,
		"XDG_STATE_HOME=" + state,
		"GHOST_SCRATCH_DIR=" + scratchDir,
		"GHOST_OLLAMA_URL=http://127.0.0.1:1",
		// Only the fake claude exists; the other backends name nothing.
		"GHOST_CLI_OPENCODE_BINARY=" + filepath.Join(root, "no-such-opencode"),
		"GHOST_CLI_CODEX_BINARY=" + filepath.Join(root, "no-such-codex"),
		"GHOST_CLI_GOOSE_BINARY=" + filepath.Join(root, "no-such-goose"),
	}
	if v, ok := os.LookupEnv("GHOST_DEV_FORBID_DATA_DIR"); ok {
		s.env = append(s.env, "GHOST_DEV_FORBID_DATA_DIR="+v)
	}
	return s
}

// signalTestConfig enables the two phases these tests chain, with embedding and
// linking off so no phase reaches for a model endpoint.
const signalTestConfig = `embedding:
  enabled: false
linking:
  enabled: false
obsidian:
  auto_sync: false
reflection:
  auto_reflect: false
  auto_resolve: true
  auto_supersede: true
  consolidation_timeout_minutes: 2
  lifecycle_timeout_minutes: 3
`

func (s *signalSandbox) configPath() string {
	return filepath.Join(s.config, "ghost", "config.yaml")
}

func (s *signalSandbox) dataDir() string { return filepath.Join(s.data, "ghost") }

// seed writes the project and one memory carrying a resolution keyword, so the
// resolve phase has a candidate and really blocks on its harness.
func (s *signalSandbox) seed(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(s.dataDir(), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(s.dataDir(), "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	store := memory.NewStore(db, nil)
	defer store.Close() //nolint:errcheck
	ctx := context.Background()
	if err := store.EnsureProject(ctx, signalTestProject, filepath.Join(s.root, "work"), signalTestProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := store.CreateFromCorpus(ctx, signalTestProject, memory.Memory{
		Category: "fact", Content: "the failed migration was reverted on the staging relay", Source: "mcp", Importance: 0.6,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// coordinator is a started `ghost lifecycle` and the channel its Wait closes.
type coordinator struct {
	cmd    *exec.Cmd
	stderr *lifecycleSignalBuffer
	exited chan struct{}
}

// startCoordinator starts the chain with an explicit --source, so the phase
// does not depend on detecting a harness from the test's own ancestry. The
// cleanup kills a coordinator the test left running.
func (s *signalSandbox) startCoordinator(t *testing.T) *coordinator {
	t.Helper()
	cmd := exec.Command(filepath.Join(s.bin, "ghost"), "lifecycle", "--project", signalTestProject, "--source", "claude-code")
	cmd.Env = s.env
	cmd.Dir = s.root
	c := &coordinator{cmd: cmd, stderr: &lifecycleSignalBuffer{}, exited: make(chan struct{})}
	cmd.Stdout = &lifecycleSignalBuffer{}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start ghost lifecycle: %v", err)
	}
	go func() {
		defer close(c.exited)
		_ = cmd.Wait()
	}()
	t.Cleanup(func() {
		select {
		case <-c.exited:
		default:
			_ = cmd.Process.Kill()
			<-c.exited
		}
	})
	return c
}

// waitExit waits for the coordinator to exit, failing the test when it does not.
func (c *coordinator) waitExit(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-c.exited:
	case <-time.After(within):
		t.Fatalf("ghost lifecycle did not exit within %s of the interrupt\n%s", within, c.stderr.String())
	}
}

// hasExited reports whether the coordinator has exited, without waiting.
func (c *coordinator) hasExited() bool {
	select {
	case <-c.exited:
		return true
	default:
		return false
	}
}

// waitForStderr waits until the coordinator's stderr contains want.
func (c *coordinator) waitForStderr(t *testing.T, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !strings.Contains(c.stderr.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the coordinator never said %q within %s\n%s", want, within, c.stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForPath waits until path exists, failing early when the coordinator has
// already exited, since nothing it started can then write the file.
func waitForPath(t *testing.T, c *coordinator, path, what string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if c != nil && c.hasExited() {
			t.Fatalf("the coordinator exited before %s\n%s", what, c.stderr.String())
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lifecycleMarker is the part of the failure marker these tests read.
type lifecycleMarker struct {
	PhasesFailed []string `json:"phases_failed"`
	Error        string   `json:"error"`
}

func (s *signalSandbox) readMarker(t *testing.T, c *coordinator) lifecycleMarker {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.dataDir(), mcpinit.LifecycleFailureMarkerFile(signalTestProject)))
	if err != nil {
		t.Fatalf("no failure marker after the interrupt, so the run went unrecorded: %v\n%s", err, c.stderr.String())
	}
	var m lifecycleMarker
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal the marker %q: %v", b, err)
	}
	return m
}

func (s *signalSandbox) markerExists() bool {
	_, err := os.Stat(filepath.Join(s.dataDir(), mcpinit.LifecycleFailureMarkerFile(signalTestProject)))
	return err == nil
}

// groupEmpty reports whether the process group pgid has no member left: ESRCH
// from signal 0 to -pgid is the one answer that means empty.
func groupEmpty(pgid int) bool {
	return pgid > 0 && errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
}

// waitGroupGone polls until the group pgid is empty.
func waitGroupGone(pgid int, within time.Duration) error {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if groupEmpty(pgid) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("still inhabited after %s", within)
}

// signalPending reports whether sig is still pending for pid, from the
// SigPnd and ShdPnd masks in /proc/<pid>/status. ok is false where /proc is
// not there to ask.
func signalPending(pid int, sig syscall.Signal) (pending, ok bool) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return false, false
	}
	bit := uint64(1) << (uint(sig) - 1)
	for _, line := range strings.Split(string(raw), "\n") {
		name, val, found := strings.Cut(line, ":")
		if !found || (name != "SigPnd" && name != "ShdPnd") {
			continue
		}
		mask, err := strconv.ParseUint(strings.TrimSpace(val), 16, 64)
		if err == nil && mask&bit != 0 {
			return true, true
		}
	}
	return false, true
}

// TestLifecycleInterruptBetweenPhasesStartsNoPhase holds the coordinator in
// the gap before its first phase and signals it there. The coordinator reads
// its config after it has installed its signal handler, and the config here is
// a FIFO: the test's open for writing returns only once the coordinator has
// opened it for reading, which is the point at which the signal is sent, and
// the config is written only after the signal has left the pending set. The
// loop's check at the top of every phase is what must then stop the chain.
//
// The record must name NO phase — none started — and must say which phase was
// never reached. Without the check the next phase is started under the
// cancelled context, dies with "context canceled", and is recorded as a
// failure of work that never ran.
func TestLifecycleInterruptBetweenPhasesStartsNoPhase(t *testing.T) {
	s := newSignalSandbox(t)
	s.seed(t)
	if err := syscall.Mkfifo(s.configPath(), 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	c := s.startCoordinator(t)

	// Rendezvous on the FIFO. The open runs in its own goroutine so a
	// coordinator that died before opening it fails the test instead of
	// hanging it; the cleanup's non-blocking reader frees a writer still
	// parked in open.
	opened := make(chan *os.File, 1)
	go func() {
		f, err := os.OpenFile(s.configPath(), os.O_WRONLY, 0)
		if err != nil {
			close(opened)
			return
		}
		opened <- f
	}()
	t.Cleanup(func() {
		if r, err := os.OpenFile(s.configPath(), os.O_RDONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = r.Close()
		}
	})
	var fifo *os.File
	select {
	case f, ok := <-opened:
		if !ok {
			t.Fatal("could not open the config FIFO for writing")
		}
		fifo = f
	case <-c.exited:
		t.Fatalf("the coordinator exited before it read its config\n%s", c.stderr.String())
	case <-time.After(60 * time.Second):
		t.Fatalf("the coordinator never opened its config\n%s", c.stderr.String())
	}

	if err := c.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal the coordinator: %v", err)
	}
	// The runtime's handler has taken the signal once it is no longer pending.
	// What this cannot see is the step after it: the signal reaches
	// NotifyContext's goroutine, which cancels the root context, a couple of
	// goroutine hand-offs later, and the coordinator exposes nothing an outside
	// test can read when that lands — no log line, no file, no marker until the
	// run is over. So the config is held back for signalCancelMargin on top.
	//
	// That margin is why this is safe on a slow -race runner. For its whole
	// length the coordinator's main goroutine is parked on the FIFO read, so
	// nothing in the process competes with the goroutine doing the cancel, and
	// a scheduling delay of a quarter of a second for a runnable goroutine in an
	// otherwise idle process is not something a loaded machine produces. The
	// coordinator's remaining pre-loop work after the read (YAML parse, binary
	// lookups, the scratch reap, the audit sweep, the start stamp) adds to it.
	// The built binary is not race-instrumented, whatever flags the test runs
	// under. A run where the cancel still lost would fail loudly, as resolve
	// "interrupted after" instead of "interrupted before", rather than pass.
	deadline := time.Now().Add(10 * time.Second)
	for {
		pending, ok := signalPending(c.cmd.Process.Pid, syscall.SIGINT)
		if !ok || !pending {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the coordinator never took the SIGINT")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(signalCancelMargin)
	if _, err := fifo.WriteString(signalTestConfig); err != nil {
		t.Fatalf("write the config: %v", err)
	}
	if err := fifo.Close(); err != nil {
		t.Fatalf("close the config FIFO: %v", err)
	}

	c.waitExit(t, 60*time.Second)
	stderr := c.stderr.String()

	// No phase started: the harness was never called, and no phase reported.
	if _, err := os.Stat(filepath.Join(s.bin, "harness.argv")); err == nil {
		t.Errorf("a phase reached its harness after the interrupt\n%s", stderr)
	}
	for _, phase := range []string{"resolve", "supersede"} {
		for _, verb := range []string{" failed after ", " completed in ", " interrupted after "} {
			if strings.Contains(stderr, "lifecycle: "+phase+verb) {
				t.Errorf("phase %s ran after the interrupt (%q)\n%s", phase, verb, stderr)
			}
		}
	}
	if !strings.Contains(stderr, "lifecycle: interrupted before resolve; not starting the remaining phases") {
		t.Errorf("the coordinator did not stop before the first phase\n%s", stderr)
	}

	m := s.readMarker(t, c)
	if len(m.PhasesFailed) != 0 {
		t.Errorf("phases_failed = %v, want none: no phase started, so none may be blamed (error %q)", m.PhasesFailed, m.Error)
	}
	if want := mcpinit.InterruptedBySignal + " before resolve"; m.Error != want {
		t.Errorf("marker error = %q, want %q", m.Error, want)
	}
}

// TestLifecycleInterruptDuringAPhaseRecordsItAndSweepsItsGroups signals the
// coordinator while the resolve phase is blocked on its harness, with
// supersede enabled behind it. It requires that:
//
//   - the phase's harness, in a process group of its own, is terminated and
//     its group swept, which only happens when the phase's context derives
//     from the coordinator's signal context;
//   - the coordinator stays up, with no marker written, while a member of the
//     PHASE's group that took the SIGTERM is still winding down: it waits for
//     the sweep (awaitPhaseSweep) before it records the run and releases the
//     claim, and only exits once that group is empty;
//   - resolve is recorded as interrupted, not failed, and supersede never
//     starts.
//
// The member of the phase's group is a shell the test starts into that group
// (same session, so setpgid is allowed). It stands for any process left in the
// phase's group that ignores the SIGTERM for a moment; it exits when the test
// releases it, and it is the test's own child, so the test reaps it.
func TestLifecycleInterruptDuringAPhaseRecordsItAndSweepsItsGroups(t *testing.T) {
	s := newSignalSandbox(t)
	s.seed(t)
	if err := os.WriteFile(s.configPath(), []byte(signalTestConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Registered before anything is started, so it runs after every other
	// cleanup: whatever this run left alive is killed by pid, each one checked
	// for liveness first so a reused pid is not hit.
	t.Cleanup(func() {
		for _, name := range []string{"helper.pid", "harness.pid", "member.pid"} {
			if pid := recordedPid(filepath.Join(s.bin, name)); pid > 0 && processAlive(pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		if pid := recordedPid(filepath.Join(s.bin, "phase.pid")); pid > 0 && !groupEmpty(pid) {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}
	})

	c := s.startCoordinator(t)
	waitForPath(t, c, filepath.Join(s.bin, "ready"), "the resolve phase's harness call", 90*time.Second)
	harnessPid := recordedPid(filepath.Join(s.bin, "harness.pid"))
	helperPid := recordedPid(filepath.Join(s.bin, "helper.pid"))
	phasePid := recordedPid(filepath.Join(s.bin, "phase.pid"))
	if harnessPid <= 0 || helperPid <= 0 || phasePid <= 0 {
		t.Fatalf("the harness did not record its pids (harness %d, helper %d, phase %d)", harnessPid, helperPid, phasePid)
	}
	// The layout production gives a phase: the phase leads a group of its own,
	// and the harness it spawned leads another.
	if pg, err := syscall.Getpgid(phasePid); err != nil || pg != phasePid {
		t.Fatalf("the phase (pid %d) is not the leader of its own group (pgid %d, %v)", phasePid, pg, err)
	}
	if pg, err := syscall.Getpgid(harnessPid); err != nil || pg != harnessPid {
		t.Fatalf("the harness (pid %d) is not the leader of its own group (pgid %d, %v)", harnessPid, pg, err)
	}

	// The member of the phase's group, armed before the signal.
	termed := filepath.Join(s.bin, "member.termed")
	release := filepath.Join(s.bin, "member.release")
	armed := filepath.Join(s.bin, "member.armed")
	member := exec.Command("/bin/sh", "-c", fmt.Sprintf(
		"trap ': > %[1]s; while [ ! -f %[2]s ]; do sleep 0.05; done; exit 0' TERM; "+
			": > %[3]s; i=0; while [ \"$i\" -lt 2400 ]; do sleep 0.05; i=$((i + 1)); done",
		termed, release, armed))
	member.Env = s.env
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: phasePid}
	if err := member.Start(); err != nil {
		t.Fatalf("start a member of the phase's group: %v", err)
	}
	memberExited := make(chan struct{})
	go func() {
		defer close(memberExited)
		_ = member.Wait()
	}()
	if err := os.WriteFile(filepath.Join(s.bin, "member.pid"), []byte(strconv.Itoa(member.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatalf("record the member's pid: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o600)
		select {
		case <-memberExited:
		case <-time.After(10 * time.Second):
			_ = member.Process.Kill()
			<-memberExited
		}
	})
	waitForPath(t, c, armed, "the phase-group member to arm its trap", 30*time.Second)

	// Signal the coordinator alone, the way `kill <pid>` does.
	if err := c.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal the coordinator: %v", err)
	}

	// The phase's group took the SIGTERM the phase's Cancel sends, and the
	// harness's own group was terminated and swept: the harness took its
	// SIGTERM, and the helper that ignores it was killed with its group.
	waitForPath(t, c, termed, "the SIGTERM to the phase's group", 30*time.Second)
	if err := waitGone(harnessPid, 30*time.Second); err != nil {
		t.Fatalf("the harness (pid %d) survived the interrupt: %v\n%s", harnessPid, err, c.stderr.String())
	}
	if err := waitGroupGone(harnessPid, 30*time.Second); err != nil {
		t.Fatalf("the harness's group (pgid %d) was not swept: %v\n%s", harnessPid, err, c.stderr.String())
	}
	if processAlive(helperPid) {
		t.Errorf("the harness's helper (pid %d) outlived its group's sweep", helperPid)
	}

	// The phase itself has been reaped; only the member is left in its group.
	c.waitForStderr(t, "lifecycle: resolve interrupted after", 30*time.Second)
	if groupEmpty(phasePid) {
		t.Fatal("the phase's group emptied before the member was released; this case is not covered")
	}

	// While the member is winding down the coordinator must wait for the sweep:
	// no marker, no exit. This is an observation window, not a sync point; a
	// coordinator that skipped the wait records and exits within milliseconds
	// of the line above, and one that waits cannot leave it at all while the
	// group is inhabited.
	window := time.Now().Add(time.Second)
	for time.Now().Before(window) {
		if c.hasExited() {
			t.Fatalf("the coordinator exited while a member of the phase's group was still running: it did not wait for the sweep\n%s", c.stderr.String())
		}
		if s.markerExists() {
			t.Fatalf("the coordinator recorded the run while a member of the phase's group was still running: it did not wait for the sweep\n%s", c.stderr.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatalf("release the member: %v", err)
	}
	c.waitExit(t, 30*time.Second)
	if !groupEmpty(phasePid) {
		t.Errorf("the phase's group (pgid %d) was still inhabited when the coordinator exited", phasePid)
	}
	select {
	case <-memberExited:
	default:
		t.Error("the coordinator exited before the member of the phase's group was gone")
	}

	stderr := c.stderr.String()
	for _, verb := range []string{" failed after ", " completed in ", " interrupted after "} {
		if strings.Contains(stderr, "lifecycle: supersede"+verb) {
			t.Errorf("supersede ran after resolve was interrupted (%q)\n%s", verb, stderr)
		}
	}
	m := s.readMarker(t, c)
	if len(m.PhasesFailed) != 1 || m.PhasesFailed[0] != "resolve" {
		t.Errorf("phases_failed = %v, want exactly [resolve] (error %q)", m.PhasesFailed, m.Error)
	}
	if want := mcpinit.InterruptedBySignal + " while running resolve"; !strings.HasPrefix(m.Error, want) {
		t.Errorf("marker error = %q, want it to start with %q: the phase was interrupted, not failed", m.Error, want)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir(), "lifecycle-"+signalTestProject+".pid")); err == nil {
		t.Errorf("the per-project claim was not released")
	}
}
