package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file holds the multi-process half of the concurrency contract: the parts
// of docs/architecture.md's "Concurrency contract" that cannot be observed from
// one process. The in-process guard next door
// (TestConcurrentProcessesMixedReadWrite) opens several handles against one file
// and runs writers against readers, which proves the settings work at the SQLite
// level but not that a *process* gets them: every handle there came from the same
// binary, the same DSN builder and the same code path, so a contract setting that
// only one of the DSN spellings in the tree carried would not be noticed. (This
// fleet opens two of them — memory.OpenDB and memory.readOnlyDSN — and not
// mcpinit's; see assertConnectionContract.)
//
// The processes here are the ones the contract names. A CLI child and a live MCP
// server write through their own handles while reflection/maintenance replaces a
// project's non-manual memories in one transaction and read-only processes search
// the same file — MCP server, CLI, lifecycle subprocess and maintenance run, all
// at once, which is the overlap the contract calls a supported mode.
//
// Nothing here re-execs the test binary. Under `go test` the test binary is the
// suite, so a self-spawn re-runs every test in this package once per spawn and a
// mistake in the barrier logic becomes a fork bomb. The child program is built
// once with `go build` into a temp dir instead; see helperBinary.

// TestMain owns the temp dir the helper program is built into. The build itself
// is lazy (helperBinary), so a -short run, a -run filter that selects no
// multi-process test, and a plain `go vet` pay nothing for it — a cold build
// costs seconds and a warm one is free, and not every invocation of this package
// should pay for either.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ghost-multiproc-build-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "multiproc build dir: %v\n", err)
		os.Exit(1)
	}
	helperBuildDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

var (
	helperBuildDir string
	helperOnce     sync.Once
	helperPath     string
	helperBuildErr error
)

// helperBinary builds the child program once per test binary and returns its
// path. A build failure is a test failure, never a skip: the alternative is a
// suite that silently stops covering the contract on any machine whose Go
// toolchain cannot build it.
func helperBinary(t *testing.T) string {
	t.Helper()
	helperOnce.Do(func() {
		// The child lives in this package's testdata, so it is invisible to
		// `go build ./...` and to the release build while still being vetted by
		// `go vet ./...` like any other package. A test's working directory is
		// its own package directory, so the relative path below resolves
		// through the module rather than to a file on disk.
		name := "multiproc-helper"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		helperPath = filepath.Join(helperBuildDir, name)
		cmd := exec.Command("go", "build", "-o", helperPath, "./testdata/multiproc")
		cmd.Dir = "."
		// The module cache and build cache are the ambient ones: a test may use
		// the toolchain's cache, and must not redirect it somewhere it would be
		// wiped between the two invocations.
		if out, err := cmd.CombinedOutput(); err != nil {
			helperBuildErr = fmt.Errorf("go build ./testdata/multiproc: %v\n%s", err, out)
		}
	})
	if helperBuildErr != nil {
		t.Fatalf("building the multi-process helper: %v", helperBuildErr)
	}
	return helperPath
}

// The fixture. Two projects in one database file: the lifecycle batch replaces
// the batch project's non-manual memories, so the steady-state load needs a
// project of its own — otherwise the batch would delete the load's rows and
// "no write was dropped" could not be counted. Both projects are in the same
// file, which is the point: the write lock SQLite serializes is database-wide,
// so the load and the batch contend for it even though they touch disjoint rows.
const (
	multiprocLoadProject  = "multiproc-load"
	multiprocBatchProject = "multiproc-batch"

	// Content prefixes. The load rows are what the no-dropped-write accounting
	// counts; the seed and replaced rows are what the snapshot and poller
	// invariants count. They must not overlap: a rewrite that accidentally
	// produced a seed prefix would make the batch's own rows look preserved.
	multiprocLoadPrefix     = "mp-load"
	multiprocSeedPrefix     = "mp-seed"
	multiprocReplacedPrefix = "mp-new"

	// multiprocQuery is the FTS query every reader uses. The load content
	// contains its first two terms and the rewritten content contains none of
	// them, so a stale index entry after a rewrite is observable as a result
	// whose content holds no query term at all.
	multiprocQuery = "sqlite wal checkpointing"

	// multiprocBatchRows is the size of the reflection batch. It has to be big
	// enough that a batch applied row by row would leave an intermediate state
	// long enough to be sampled, and small enough to commit well inside the
	// 5s busy timeout while six other processes write the same file.
	multiprocBatchRows = 60

	// multiprocLoadAfter is how many writes each writer completes — or reads, for
	// the read-only roles — before it reports itself in step. The batch waits for
	// every one of them, so it is triggered by writer progress rather than by a
	// clock: at that point the writers are provably mid-stream against each other
	// and against the file the batch is about to lock. It also keeps the corpus
	// small, which matters because the contract's busy timeout is a bound and not
	// a guarantee — a writer queued behind a transaction that has grown with the
	// corpus measures a slower machine, not a broken setting.
	multiprocLoadAfter = 20

	// multiprocBarrierTimeout bounds each of the parent's waits for a child
	// process to reach a state. It is a deadlock backstop, not a performance
	// budget: a healthy run reaches every barrier in well under a second, and a
	// run that cannot is reported by name rather than waited out.
	multiprocBarrierTimeout = 30 * time.Second
)

// procReport mirrors the report the child program prints. The field names must
// match internal/memory/testdata/multiproc/main.go; that file documents why the
// two are not one shared type.
type procReport struct {
	Role     string            `json:"role"`
	Label    string            `json:"label"`
	KV       map[string]string `json:"kv"`
	Observed []string          `json:"observed,omitempty"`
	IDs      []string          `json:"ids,omitempty"`
	Errs     []string          `json:"errs,omitempty"`
	Busy     []string          `json:"busy,omitempty"`
	Aborted  string            `json:"aborted,omitempty"`
}

// count reads one measured counter. A key the role never set reads as zero,
// which is what every caller wants: the assertions that care compare against a
// non-zero expectation, and the rest are counting.
func (r procReport) count(key string) int {
	n, _ := strconv.Atoi(r.KV[key])
	return n
}

func (r procReport) str(key string) string { return r.KV[key] }

// child is one spawned process and everything the parent learns from it.
type child struct {
	name string
	cmd  *exec.Cmd
	out  bytes.Buffer
	errb bytes.Buffer
	// label is the name this process announces itself under in the barrier
	// directory, kept here because the barriers are named after it.
	label string

	mu   sync.Mutex
	done bool
	rep  procReport
	err  error
}

// multiprocPaths are the per-run locations every child is told about. They are
// all temp dirs, so a run cannot reach the developer's real Ghost install even
// if a code path inside a child went looking for one.
type multiprocPaths struct {
	helper  string
	db      string
	barrier string
	home    string
}

// writerLabels names the load's writers, so the maintenance process knows whose
// post-announcement write to wait for. It is a PARAMETER, not a package
// variable, and that is the whole point of the shape: as a variable it was
// assigned after the _txlock probe had already been spawned, so the flag a
// child received depended on whether its spawn happened to be before or after
// that assignment — a value read at one point in the run and applied to another.
// The list is built from the spawn list, and each spawn is now told what to
// pass, so the two cannot drift apart or arrive out of order.
//
// An empty value makes the maintenance process skip that wait, which is correct
// for every role that is not the lifecycle writer, and is what the probe and
// the read-only pair are given.
func writerLabels(labels []string) string { return strings.Join(labels, ",") }

func (c *child) start(ctx context.Context, t *testing.T, p multiprocPaths, role, project, label, writers string, extra ...string) {
	t.Helper()
	args := []string{
		"-role", role,
		"-db", p.db,
		"-barrier", p.barrier,
		"-project", project,
		"-label", label,
		"-load", multiprocLoadPrefix,
		"-seed", multiprocSeedPrefix,
		"-replaced", multiprocReplacedPrefix,
		"-query", multiprocQuery,
		"-batch", fmt.Sprint(multiprocBatchRows),
		"-after", fmt.Sprint(multiprocLoadAfter),
		"-writers", writers,
	}
	args = append(args, extra...)
	c.name = role + "/" + label
	c.label = label
	// CommandContext is the backstop: a child that never reaches its barrier
	// would otherwise sit here until the test binary's own timeout, which
	// reports nothing about which process was stuck.
	c.cmd = exec.CommandContext(ctx, p.helper, args...)
	// The child's HOME family and both XDG roots point into the temp dir, so a
	// code path that reaches for the user's real profile — a config lookup, a
	// data dir — finds nothing there instead of reading or writing the machine's
	// actual Ghost install. The database is passed by path, so nothing in this
	// test needs the real one; this is the guard that keeps it that way.
	c.cmd.Env = append(os.Environ(),
		"HOME="+p.home,
		"USERPROFILE="+p.home,
		"XDG_CONFIG_HOME="+filepath.Join(p.home, "config"),
		"XDG_DATA_HOME="+filepath.Join(p.home, "data"),
		"XDG_STATE_HOME="+filepath.Join(p.home, "state"),
	)
	c.cmd.Stdout = &c.out
	c.cmd.Stderr = &c.errb
	go func() {
		err := c.cmd.Run()
		// The report is parsed even when the process failed: a child that
		// aborts still prints what it measured and why, and that is the only
		// place the reason exists. Dropping it on a non-zero exit would leave
		// "exit status 1" as the entire diagnosis.
		var rep procReport
		decodeErr := json.Unmarshal(c.out.Bytes(), &rep)
		switch {
		case err != nil && decodeErr != nil:
			err = fmt.Errorf("%v (the report was unreadable too: %v: %s)", err, decodeErr, truncate(c.out.String()))
		case err == nil && decodeErr != nil:
			err = fmt.Errorf("decode report: %w (%s)", decodeErr, truncate(c.out.String()))
		}
		c.mu.Lock()
		c.done, c.rep, c.err = true, rep, err
		c.mu.Unlock()
	}()
}

func (c *child) finished() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.done
}

func (c *child) result() (procReport, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rep, c.err
}

func truncate(s string) string {
	const max = 600
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// TestMultiProcessSharedDatabase is the real multi-process concurrency test:
// every setting the contract names is read back from a live connection in a
// process that is not this one, while a lifecycle batch commits underneath a
// pinned read snapshot and six other processes write the same file.
//
// The invariants, none of which is "nothing crashed":
//
//   - every process exits successfully, and no SQLITE_BUSY or
//     SQLITE_BUSY_SNAPSHOT reaches any of them. Nothing in this test retries a
//     database error, so a pass is evidence that the documented 5s busy timeout
//     on its own carried the contention rather than that a backoff hid it.
//   - every write a process reported successful left a row, checked afterwards
//     against a fresh handle by id. A writer that gave up under contention is a
//     lost memory, and a lost memory is invisible until much later.
//   - a read transaction opened before the batch commits still sees the
//     pre-batch state afterwards, and a new snapshot sees the whole batch. This
//     is the invariant a reader observing a half-applied lifecycle batch would
//     break.
//   - a reader taking a fresh snapshot on every sample, all the way across the
//     commit, observes the pre-batch state and the post-batch state and nothing
//     in between. A pinned snapshot cannot show this — it shows the pre-batch
//     state whatever the writer did — so it takes a second reader with a fresh
//     snapshot per sample.
//   - no reader ever sees a row whose content does not contain a term its FTS
//     match claimed, including while other processes rewrite those rows.
//   - WAL, foreign keys, busy_timeout, SetMaxOpenConns(1) and a read-only
//     handle that refuses writes are asserted on every connection the processes
//     open, and _txlock=immediate is asserted behaviourally, by showing that a
//     write from a second connection is refused while a read-then-write
//     transaction is open on the first.
//
// The ordering is carried by barrier files, not by sleeps: each process
// announces the state it has reached and waits for the state it depends on, so
// a slow machine makes the test slower rather than wrong. The poll interval in
// the child's wait loop is a latency knob, not the synchronization.
func TestMultiProcessSharedDatabase(t *testing.T) {
	if testing.Short() {
		// It builds a helper program and starts ten processes, which is more
		// than a unit test should cost; -short is the switch for that. It makes
		// no LLM call of any kind, so unlike the harness suites it does not need
		// GHOST_LIVE_TESTS: the expensive part is process count, and that is
		// opted out here.
		t.Skip("multi-process test: builds a helper binary and spawns processes; skipped under -short")
	}
	// Ten processes: a helper build, a 60-row lifecycle batch under four
	// concurrent writers and two readers, a read transaction pinned across the
	// commit, and a sampler polling it. A healthy run is a couple of seconds;
	// this budget is the backstop for a failing one, where CommandContext kills
	// the children that are still waiting on a barrier nobody will signal.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	dir := t.TempDir()
	paths := multiprocPaths{
		helper:  helperBinary(t),
		db:      filepath.Join(dir, "ghost.db"),
		barrier: filepath.Join(dir, "barrier"),
		home:    filepath.Join(dir, "home"),
	}
	for _, d := range []string{paths.barrier, paths.home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}

	seedMultiprocDB(t, ctx, paths.db)

	var children []*child
	spawn := func(c *child, role, project, label, writers string, extra ...string) *child {
		t.Helper()
		c.start(ctx, t, paths, role, project, label, writers, extra...)
		children = append(children, c)
		return c
	}

	// A barrier that never arrives is a consequence, not a cause: the run keeps
	// going so that every process's own report is collected, because the process
	// that failed is the one that knows why. The first such failure is recorded
	// and the rest are skipped — nothing after it can be reached anyway.
	var barrierFailure error
	reach := func(name string) {
		t.Helper()
		if barrierFailure != nil {
			return
		}
		if err := waitForBarrier(paths.barrier, name); err != nil {
			barrierFailure = err
		}
	}

	// The steady-state load's writers, named once and handed to each spawn
	// below. The list is built BEFORE the probe is spawned, because the probe
	// is one of the writers and used to be told so by a variable assigned after
	// it had already started.
	loadLabels := []string{"mcp0", "mcp1", "cli0", "cli1", "probe"}
	loadWriters := writerLabels(loadLabels)

	// The _txlock=immediate probe is only evidence about its own transaction
	// while nothing else holds the write lock, so it runs in the first process
	// against an otherwise idle database. The rest of the fleet waits for it.
	// It is a writer, but it is not the lifecycle writer, so it is told no
	// writers to wait for — the flag names who the batch waits on, not who the
	// batch is.
	probe := spawn(&child{}, "cli", multiprocLoadProject, "probe", "", "-probe")
	reach("probe-done")

	// The steady-state load: two MCP servers and two CLI children writing, and
	// two read-only processes searching the rows they are rewriting.
	load := []*child{
		spawn(&child{}, "mcp", multiprocLoadProject, "mcp0", loadWriters),
		spawn(&child{}, "mcp", multiprocLoadProject, "mcp1", loadWriters),
		spawn(&child{}, "cli", multiprocLoadProject, "cli0", loadWriters),
		spawn(&child{}, "cli", multiprocLoadProject, "cli1", loadWriters),
		spawn(&child{}, "ro", multiprocLoadProject, "ro0", loadWriters),
		spawn(&child{}, "ro", multiprocLoadProject, "ro1", loadWriters),
	}
	// Every load process has reported itself in step, so the writers are provably
	// live and already contending before the batch looks for them.
	for _, c := range load {
		reach("wrote-" + c.label)
	}
	reach("wrote-" + probe.label)

	var poller, snapshot, maint *child
	if barrierFailure == nil {
		writeBarrier(t, paths.barrier, "load-ready")

		// The two observers, then the batch they exist to observe. Only the
		// batch reads -writers, so it alone is told which load processes it
		// must see commit after it announces itself.
		poller = spawn(&child{}, "poller", multiprocBatchProject, "poller", "")
		snapshot = spawn(&child{}, "snapshot", multiprocBatchProject, "snapshot", "")
		maint = spawn(&child{}, "maint", multiprocBatchProject, "maint", loadWriters)

		// The maintenance process ends the load when it finishes, batch or no
		// batch. This is the backstop for the case where it never gets there —
		// killed, or failed before its own deferred write — so the load cannot
		// outlive the process that releases it and turn a failure into a hung
		// run.
		//
		// Waited for before the test ends, because it is registered as a
		// cleanup that the t.TempDir removal also runs: a goroutine still
		// polling a process that the failing path is killing can write the stop
		// barrier into a directory that is being removed, and the write is then
		// the only thing racing the removal. Joining it here means the stop file
		// is written while the directory is still there — which is the whole
		// point of the backstop.
		reaper := make(chan struct{})
		go func() {
			defer close(reaper)
			for !maint.finished() {
				time.Sleep(5 * time.Millisecond)
			}
			_ = os.WriteFile(filepath.Join(paths.barrier, "stop"), []byte("backstop"), 0o600)
		}()
		// Registered here rather than joined after waitForChildren, because
		// waitForChildren ends in t.Fatalf on a hung child and that would skip
		// the join entirely — which is the path the reaper exists for. A
		// cleanup runs on every exit, and this one was registered after the
		// temp dir's, so it runs BEFORE the directory is removed.
		//
		// Bounded, because that same Fatalf is the case where joining is least
		// safe: the child the reaper is waiting on is the one that hung, so an
		// unbounded wait would replace a 60-second failure with a hang until
		// the package's own go test timeout, and the temp dir would never be
		// removed. The reaper has one job — write the stop barrier so the load
		// cannot outlive the process meant to release it — and it does that the
		// moment the process is gone, so a wait that expires says the process
		// never went away, which is the fact the failure is about.
		t.Cleanup(func() {
			select {
			case <-reaper:
			case <-time.After(30 * time.Second):
				t.Log("the rotation backstop did not finish: the maintenance process never exited")
			}
		})
	} else {
		// No maintenance process is coming to end the load, so end it here.
		writeBarrier(t, paths.barrier, "stop")
	}

	waitForChildren(t, ctx, children)
	// Registered as a cleanup, not written at the end of the test: a Fatalf in any
	// assertion below — including the two reachable only after a barrier failure
	// — would otherwise skip the one thing that says how far each process got.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, c := range children {
			rep, _ := c.result()
			t.Logf("%s measured: %v (observed %v, gave up: %q)", c.name, rep.KV, rep.Observed, rep.Aborted)
		}
	})
	if barrierFailure != nil {
		t.Errorf("%v", barrierFailure)
	}

	reports := map[string]procReport{}
	for _, c := range children {
		rep, err := c.result()
		reports[c.name] = rep
		if err != nil {
			stderr := truncate(c.errb.String())
			if stderr == "" {
				stderr = "(nothing on stderr)"
			}
			t.Errorf("%s: %v\nstderr: %s", c.name, err, stderr)
		}
		for _, e := range rep.Errs {
			t.Errorf("%s reported a failed operation: %s", c.name, e)
		}
		// Contention is the contract's central failure mode, so it is called out
		// on its own rather than left inside the generic failure list above.
		for _, e := range rep.Busy {
			t.Errorf("%s hit SQLite lock contention: %s", c.name, e)
		}
		if rep.Aborted != "" {
			t.Errorf("%s gave up: %s", c.name, rep.Aborted)
		}
		if isBusyText(c.errb.String()) {
			t.Errorf("%s wrote lock contention to stderr:\n%s", c.name, truncate(c.errb.String()))
		}
	}

	// The contract, read back from a live connection in each process rather than
	// from the DSN that produced it. Skipped when a barrier failed, because the
	// processes that would have supplied these measurements never ran.
	if barrierFailure == nil {
		assertConnectionContract(t, reports)

		probeRep, _ := probe.result()
		if got := probeRep.str("tx_lock_held"); got != "true" {
			t.Errorf("a read-then-write transaction on the CLI caller shape did not hold SQLite's write lock "+
				"(tx_lock_held=%q): _txlock=immediate is missing or not honoured, so a concurrent commit "+
				"upgrades this transaction to a SQLITE_BUSY_SNAPSHOT failure", got)
		}
		// What the load does NOT prove about _txlock=immediate, so the assertion
		// above is not read as more than it is. With the parameter removed, the
		// load processes here never hit SQLITE_BUSY_SNAPSHOT: the deferred
		// transactions this fleet runs are write-only, and a write-only
		// transaction that has not read cannot be upgraded out of a shared lock
		// the way a read-then-write one is. The probe above is the ONLY thing
		// here that catches the removal, which is why it runs in its own process
		// against an idle database rather than as one more assertion over the
		// load's reports.
		//
		// The rest of the contract is genuinely load-borne: the settings are
		// read back from a live connection in every process (see
		// assertConnectionContract), and the no-dropped-writes and
		// no-SQLITE_BUSY claims below are about contention the load creates.

		assertLoadWasLive(t, reports)
		assertSnapshotInvariant(t, snapshot)
		assertBatchAtomicity(t, poller)
	}
	assertNoDroppedWrites(t, ctx, paths.db, reports)
	assertDatabaseIntact(t, ctx, paths.db, barrierFailure == nil)

	// What the run actually did, so a pass is a number rather than a shrug. The
	// load is bounded by the batch, not by an iteration count, so these counts
	// vary; what must not vary is that they are not near zero. The dedup column
	// is expected to be non-zero: the fixture's rows really are near-identical,
	// which is what makes the rewrite phase able to catch a stale index entry.
	t.Logf("multi-process run: %d writes (%d issued after the batch was announced), %d rewrites, "+
		"%d reads, %d dedup hits across %d processes; batch of %d rows; poller sampled %d times and saw %v",
		totalInt(reports, "writes"), totalInt(reports, "writes_after_announce"),
		totalInt(reports, "rewrites"), totalInt(reports, "reads"),
		totalInt(reports, "duplicates"), len(children), multiprocBatchRows,
		reports["poller/poller"].count("samples"), reports["poller/poller"].Observed)
}

// totalInt sums one measured counter across every process.
func totalInt(reports map[string]procReport, key string) int {
	sum := 0
	for _, rep := range reports {
		sum += rep.count(key)
	}
	return sum
}

func isBusyText(s string) bool {
	lower := strings.ToLower(s)
	for _, needle := range []string{"sqlite_busy", "sqlite_locked", "database is locked", "(517)"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func writeBarrier(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(time.Now().Format(time.RFC3339Nano)), 0o600); err != nil {
		t.Fatalf("signal barrier %s: %v", name, err)
	}
}

// waitForBarrier blocks until a process has announced the state the run depends
// on. It gives up early if any process has aborted, because in a multi-process
// test a barrier that will never arrive is almost always the consequence of an
// earlier failure rather than the failure itself, and waiting out the timeout
// hides the reason behind a symptom. It reports the failure instead of failing
// the test, so the caller can still collect what the processes themselves said
// — the process that aborted is the one that knows why.
func waitForBarrier(dir, name string) error {
	deadline := time.Now().Add(multiprocBarrierTimeout)
	path := filepath.Join(dir, name)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if reason, ok := abortReason(dir); ok {
			return fmt.Errorf("waiting for barrier %q, but a process failed: %s", name, reason)
		}
		if time.Now().After(deadline) {
			entries, _ := os.ReadDir(dir)
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			sort.Strings(names)
			return fmt.Errorf("barrier %q never appeared in %s; barriers present: %v", name, dir, names)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// abortReason is the failure a process recorded on its way out, so the parent
// can report the first thing that went wrong rather than the barrier that
// stopped because of it. An empty marker is a marker still being written, not
// a report.
func abortReason(dir string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "abort"))
	if err != nil {
		return "", false
	}
	reason := strings.TrimSpace(string(raw))
	if reason == "" {
		return "", false
	}
	return reason, true
}

// waitForChildren waits for every process to exit. It reports nothing itself:
// one reporting site, after the wait, keeps a failure from being printed twice
// and keeps the assertions below in a fixed order.
func waitForChildren(t *testing.T, ctx context.Context, children []*child) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		pending := ""
		for _, c := range children {
			if !c.finished() {
				pending += " " + c.name
			}
		}
		if pending == "" {
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("these processes never finished:%s", pending)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// seedMultiprocDB creates the database, both projects and the batch's pre-state.
// It closes its handle before the children start, so every connection open
// against this file during the run belongs to a process the test spawned — which
// is the only way "the settings hold on every connection the processes open"
// means anything.
func seedMultiprocDB(t *testing.T, ctx context.Context, dbPath string) {
	t.Helper()
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	store := NewStore(db, nil)
	for _, id := range []string{multiprocLoadProject, multiprocBatchProject} {
		if err := store.EnsureProject(ctx, id, filepath.Join(filepath.Dir(dbPath), id), id); err != nil {
			t.Fatalf("EnsureProject %s: %v", id, err)
		}
	}
	// The batch's pre-state: non-manual, unpinned, unresolved rows, which is
	// exactly the set a reflection replace is allowed to delete. source 'mcp' is
	// what an agent-written memory carries; 'manual' and 'builtin' rows are
	// preservation classes the replace must leave alone, so seeding those would
	// test nothing.
	for i := 0; i < multiprocBatchRows; i++ {
		content := fmt.Sprintf("%s %03d pre-batch memory about sqlite wal checkpointing", multiprocSeedPrefix, i)
		if _, _, _, err := store.Upsert(ctx, multiprocBatchProject, "fact", content, "mcp", 0.5, []string{"concurrency"}); err != nil {
			t.Fatalf("seed %s: %v", content, err)
		}
	}
	// Age the seeds, because a reflection replace deliberately preserves
	// anything created at or after its consolidatedSince stamp: that is how a
	// memory saved while the LLM was thinking survives the round trip. The batch
	// child stamps consolidatedSince as "now", and created_at has second
	// precision, so seeds written in the same second as the stamp would be
	// preserved by design and the batch would replace nothing. An hour-old
	// memory is also the honest fixture — these are the memories a previous
	// round left behind.
	if _, err := db.ExecContext(ctx,
		`UPDATE memories SET created_at = datetime('now', '-1 hour')
		 WHERE project_id = ? AND instr(content, ?) = 1`,
		multiprocBatchProject, multiprocSeedPrefix); err != nil {
		t.Fatalf("age the batch seeds: %v", err)
	}
}

// assertConnectionContract checks the settings docs/architecture.md promises, on
// one representative process per shape of caller. The expectations differ
// because the contract table gives busy_timeout(1000) to the read-only paths and
// 5000 to the writers, and a read-only connection deliberately sets no
// journal_mode of its own because writing the header is what a read-only
// connection cannot do — WAL is persisted in the file, so it is asserted on the
// connection's own answer all the same.
//
// What this does NOT cover is the other DSN spellings in the tree. The fleet
// opens two of them: memory.OpenDB for the read-write roles and
// memory.readOnlyDSN for the read-only ones. mcpinit builds its own rwDSN and
// roDSN and cmd/ghost has a third read-only spelling; no child opens those, so a
// regression in one of them — the #288 shape, where a busy_timeout that was too
// short lost a save — would still leave this test green.
func assertConnectionContract(t *testing.T, reports map[string]procReport) {
	t.Helper()
	// One representative process per caller shape: the settings come from the DSN
	// the path builds, so asserting on all ten processes would be ten copies of
	// the same assertion. foreignKeys is empty where the contract table does not
	// claim it — a read-only connection is not asserted for a setting no
	// read-only path in the tree sets.
	type caller struct {
		label        string
		process      string
		journal      string
		foreignKeys  string
		busyTimeout  string
		maxOpenConns string
		readOnly     string
	}
	paths := []caller{
		{"the MCP server's", "mcp/mcp0", "wal", "1", "5000", "1", "false"},
		{"the CLI child's", "cli/probe", "wal", "1", "5000", "1", "false"},
		{"the maintenance writer's", "maint/maint", "wal", "1", "5000", "1", "false"},
		{"the read-only", "ro/ro0", "wal", "", "1000", "1", "true"},
	}
	for _, p := range paths {
		rep, ok := reports[p.process]
		if !ok {
			t.Errorf("%s caller shape: %s reported nothing", p.label, p.process)
			continue
		}
		if got := rep.str("journal_mode"); got != p.journal {
			t.Errorf("%s caller shape (%s): journal_mode = %q, want %q — readers block on a writer "+
				"without WAL, so a hook read can be stalled by a reflection write",
				p.label, p.process, got, p.journal)
		}
		if p.foreignKeys != "" {
			if got := rep.str("foreign_keys"); got != p.foreignKeys {
				t.Errorf("%s caller shape (%s): foreign_keys = %q, want %q — a write may insert a row "+
					"whose project does not exist", p.label, p.process, got, p.foreignKeys)
			}
		}
		if got := rep.str("busy_timeout_ms"); got != p.busyTimeout {
			t.Errorf("%s caller shape (%s): busy_timeout = %q ms, want %q — without it a write arriving "+
				"mid-contention returns SQLITE_BUSY on the first collision and the memory is lost",
				p.label, p.process, got, p.busyTimeout)
		}
		if got := rep.str("max_open_conns"); got != p.maxOpenConns {
			t.Errorf("%s caller shape (%s): MaxOpenConns = %q, want %q — PRAGMA data_version is "+
				"per-connection, so an unpinned pool compares ticks from different connections "+
				"instead of points in the database's history", p.label, p.process, got, p.maxOpenConns)
		}
		if got := rep.str("read_only"); got != p.readOnly {
			t.Errorf("%s caller shape (%s): read_only = %q, want %q", p.label, p.process, got, p.readOnly)
		}
		if p.readOnly == "true" && rep.str("write_rejected") != "true" {
			t.Errorf("%s caller shape (%s): a read-only handle accepted a write (write_rejected=%q)",
				p.label, p.process, rep.str("write_rejected"))
		}
	}

	// The writers must actually have written, or the contract above was asserted
	// on handles that issued no statement. Every reader must have read, for the
	// same reason — and for the readers whose result the run checks, the torn-row
	// check is what the read is for. The MCP readers are not in that second list:
	// ghost_memory_search returns formatted text, so a row rewritten between its
	// FTS match and its hydration is indistinguishable from index rot, and their
	// reads prove only that the tools answer under contention.
	for _, name := range []string{"mcp/mcp0", "mcp/mcp1", "cli/probe", "cli/cli0", "cli/cli1"} {
		if reports[name].count("writes") == 0 {
			t.Errorf("%s reported no writes: its connection settings were read off a handle that never wrote", name)
		}
	}
	for _, name := range []string{"ro/ro0", "ro/ro1", "cli/cli0", "cli/cli1"} {
		if reports[name].count("reads") == 0 {
			t.Errorf("%s reported no reads: the torn-row check in that process never ran", name)
		}
	}
	for _, name := range []string{"mcp/mcp0", "mcp/mcp1"} {
		if reports[name].count("reads") == 0 {
			t.Errorf("%s reported no reads: the MCP tools were never called under contention", name)
		}
	}
	maint := reports["maint/maint"]
	if got := maint.count("batch_rows"); got != multiprocBatchRows {
		t.Errorf("the maintenance writer applied a batch of %d rows, want %d", got, multiprocBatchRows)
	}
	if maint.str("maintenance_rows") != "1" {
		t.Errorf("the maintenance writer recorded %q maintenance runs, want 1", maint.str("maintenance_rows"))
	}
}

// assertLoadWasLive checks the run was not vacuous: every writer must have issued
// a write after the lifecycle process announced its batch.
//
// The structural guarantee is the barrier protocol — the maintenance process waits
// for each writer's announced-<label> file, so the batch cannot commit until every
// writer has come back around its loop and written again — and this is the
// observable half, checking that each file was really a write rather than a stray
// one. It matters because the batch holds SQLite's write lock for its whole
// transaction, so nothing can commit *during* it: a row count taken either side
// would measure the two gaps around the lock, and a writer reading the
// announcement flag around its own write cannot tell a write that straddled the
// instant from one that ran entirely after it.
func assertLoadWasLive(t *testing.T, reports map[string]procReport) {
	t.Helper()
	for _, name := range []string{"mcp/mcp0", "mcp/mcp1", "cli/probe", "cli/cli0", "cli/cli1"} {
		if reports[name].count("writes_after_announce") == 0 {
			t.Errorf("%s reported no write issued after the batch was announced, so the batch never met a "+
				"live writer and the overlap the contract promises was not exercised", name)
		}
	}
}

// assertSnapshotInvariant checks the read transaction that was pinned across the
// batch commit: it must see the whole pre-batch state, unchanged, after the batch
// committed, and a new snapshot must then see the whole post-batch state.
func assertSnapshotInvariant(t *testing.T, snapshot *child) {
	t.Helper()
	rep, err := snapshot.result()
	if err != nil {
		return // already reported
	}
	preSeed, preReplaced := rep.count("pre_seed"), rep.count("pre_replaced")
	inTxSeed, inTxReplaced := rep.count("in_tx_seed"), rep.count("in_tx_replaced")
	postSeed, postReplaced := rep.count("post_seed"), rep.count("post_replaced")

	if preSeed != multiprocBatchRows || preReplaced != 0 {
		t.Errorf("the snapshot reader saw seed=%d replaced=%d before the batch, want seed=%d replaced=0",
			preSeed, preReplaced, multiprocBatchRows)
	}
	if inTxSeed != preSeed || inTxReplaced != preReplaced {
		t.Errorf("a read transaction opened before the batch committed saw seed=%d replaced=%d afterwards, "+
			"want the pre-batch state seed=%d replaced=%d — the snapshot was not held across the commit",
			inTxSeed, inTxReplaced, preSeed, preReplaced)
	}
	if postSeed != 0 || postReplaced != multiprocBatchRows {
		t.Errorf("a new snapshot after the batch saw seed=%d replaced=%d, want seed=0 replaced=%d — "+
			"the batch was partially applied, or a reader is holding a snapshot it should have released",
			postSeed, postReplaced, multiprocBatchRows)
	}
}

// assertBatchAtomicity checks the fresh-snapshot sampler that read across the
// commit. Both states must appear, or the sampler never witnessed the batch and
// proved nothing; and no third state may appear, or a reader could have seen a
// half-applied batch.
func assertBatchAtomicity(t *testing.T, poller *child) {
	t.Helper()
	rep, err := poller.result()
	if err != nil {
		return // already reported
	}
	wantPre := fmt.Sprintf("seed=%d,replaced=0", multiprocBatchRows)
	wantPost := fmt.Sprintf("seed=0,replaced=%d", multiprocBatchRows)
	legal := map[string]bool{wantPre: true, wantPost: true}

	seen := map[string]bool{}
	for _, state := range rep.Observed {
		seen[state] = true
		if !legal[state] {
			t.Errorf("a reader observed %q while the lifecycle batch committed; only %q and %q are "+
				"states a single-transaction batch can produce", state, wantPre, wantPost)
		}
	}
	if !seen[wantPre] {
		t.Errorf("the poller never saw the pre-batch state %q (it saw %v), so it did not sample across "+
			"the start of the batch", wantPre, rep.Observed)
	}
	if !seen[wantPost] {
		t.Errorf("the poller never saw the post-batch state %q (it saw %v), so it did not sample across "+
			"the commit", wantPost, rep.Observed)
	}
	if n := rep.count("samples"); n < 2 {
		t.Errorf("the poller took %d samples, want at least 2: one either side of the commit", n)
	}
}

// assertNoDroppedWrites checks the contract's silent-loss claim from the outside:
// every id a process reported creating is in the database afterwards, read
// through a fresh handle, and no two processes claimed the same row.
//
// A dedup hit is not a dropped write and is not treated as one: Upsert inserts
// the incoming text as its own row and links it to the near-duplicate it folded
// onto, so a successful write that was recognised as a duplicate still leaves
// exactly one new row. The child reports those hits as a count rather than
// failing, because the two paths are both correct and only the row count
// distinguishes a lost write from either.
func assertNoDroppedWrites(t *testing.T, ctx context.Context, dbPath string, reports map[string]procReport) {
	t.Helper()
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB for verification: %v", err)
	}
	defer db.Close() //nolint:errcheck

	seen := map[string]string{}
	var all []string
	for name, rep := range reports {
		for _, id := range rep.IDs {
			if prev, dup := seen[id]; dup {
				t.Errorf("%s and %s both report having written row %s", prev, name, id)
				continue
			}
			seen[id] = name
			all = append(all, id)
		}
	}
	if len(all) == 0 {
		t.Fatalf("no process reported a single successful write; the run proved nothing")
	}

	missing := 0
	// Chunked so a long run cannot approach SQLite's bound-parameter limit.
	const chunk = 400
	for start := 0; start < len(all); start += chunk {
		end := min(start+chunk, len(all))
		batch := all[start:end]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		var found int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM memories WHERE id IN (`+placeholders+`)`, args...).Scan(&found); err != nil {
			t.Fatalf("verify %d rows: %v", len(batch), err)
		}
		missing += len(batch) - found
	}
	if missing > 0 {
		t.Errorf("%d of %d writes that reported success left no row: contention silently dropped them",
			missing, len(all))
	}

	var loadRows int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM memories WHERE project_id = ? AND instr(content, ?) = 1`,
		multiprocLoadProject, multiprocLoadPrefix).Scan(&loadRows); err != nil {
		t.Fatalf("count load rows: %v", err)
	}
	if loadRows != len(all) {
		t.Errorf("the load project holds %d rows but the processes reported %d distinct successful writes",
			loadRows, len(all))
	}
}

// assertDatabaseIntact closes the run: the schema version is unchanged and the
// file passes SQLite's own integrity check.
//
// batchRan says whether the lifecycle process actually ran. When a barrier failed
// the run declined to start it, and its maintenance_runs row is then a claim
// about the contract that no process was in a position to satisfy.
func assertDatabaseIntact(t *testing.T, ctx context.Context, dbPath string, batchRan bool) {
	t.Helper()
	db, err := OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB for verification: %v", err)
	}
	defer db.Close() //nolint:errcheck

	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if want := SchemaVersion(); version != want {
		t.Errorf("user_version = %d after the read-write processes opened the database, want %d — a "+
			"migration raced and left the schema stamped wrong", version, want)
	}
	var check string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&check); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if check != "ok" {
		t.Errorf("integrity_check = %q after ten processes wrote this file concurrently, want \"ok\"", check)
	}
	if !batchRan {
		return
	}
	var maintenance int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM maintenance_runs WHERE kind = ?`, "multiproc-test").Scan(&maintenance); err != nil {
		t.Fatalf("count maintenance runs: %v", err)
	}
	if maintenance != 1 {
		t.Errorf("maintenance_runs holds %d rows for this run, want 1", maintenance)
	}
}
