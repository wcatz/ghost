package supersede

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// This file is #806: two CONCURRENT `ghost supersede <project> --apply` passes,
// and whether they can both write their half of a supersedes cycle.
//
// The three rules #778 added all work inside ONE pass: a pair is proposed once,
// a live edge's direction beats the scan's, and a pair already claimed both ways
// is refused outright. Nothing in them says anything about two passes, because
// each of the three is decided from what THIS pass read — and `ghost supersede`
// is a command, not a daemon, so two operators (or an operator and the
// stop-hook's lifecycle phase, with auto_supersede on) can run one each.
//
// The window is real and it is wide. A pass reads the live edges, runs a scan,
// and spends a classify call — seconds to minutes — before its first write. Two
// passes that read the same empty graph and scan the same pair in OPPOSITE
// directions both find nothing to refuse, and both then write: a cycle, whose two
// edges each demote the endpoint the other promotes, so both memories of the pair
// drop out of ranked injection and neither edge withdraws the other. That is the
// shape #778 found in a real store, and the only thing missing was a second
// process to write the other half in the same run.
//
// Opposite orientations are not free, which is what makes this narrow rather
// than constant: they need an endpoint's updated_at to move between the two
// scans, because that is the chronology orient() reads. A tag edit through a
// live `ghost mcp` moves it, and so does anything else that touches a row's
// metadata without touching its text. The fixture below makes that edit between
// the two scans, from the parent, on purpose — the race is the point, so it is
// staged rather than waited for.
//
// The fix is at the store, not in the CLI: the reverse edge is re-checked inside
// the write transaction that creates the edge, which is the only place two
// processes can be serialised against each other without either of them taking a
// lock the other has to know about (see the store method's doc comment).

// TestMain owns the temp dir the child program is built into. The build itself
// is lazy (applyRaceHelper), so a -run filter that selects no multi-process test
// pays nothing for it.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ghost-supersede-race-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "supersede race build dir: %v\n", err)
		os.Exit(1)
	}
	applyRaceBuildDir = dir
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

var (
	applyRaceBuildDir string
	applyRaceOnce     sync.Once
	applyRacePath     string
	applyRaceBuildErr error
)

// applyRaceHelper builds the child program once per test binary and returns its
// path. A build failure is a test failure, never a skip: the alternative is a
// suite that silently stops covering the case on any machine whose toolchain
// cannot build it.
func applyRaceHelper(t *testing.T) string {
	t.Helper()
	applyRaceOnce.Do(func() {
		name := "applyrace"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		applyRacePath = filepath.Join(applyRaceBuildDir, name)
		cmd := exec.Command("go", "build", "-o", applyRacePath, "./testdata/applyrace")
		cmd.Dir = "."
		// The module cache and build cache are the ambient ones, as in
		// internal/memory's multi-process helper: a test may use the toolchain's
		// cache and must not redirect it somewhere it would be wiped between the
		// two invocations.
		if out, err := cmd.CombinedOutput(); err != nil {
			applyRaceBuildErr = fmt.Errorf("go build ./testdata/applyrace: %v\n%s", err, out)
		}
	})
	if applyRaceBuildErr != nil {
		t.Fatalf("building the apply-race helper: %v", applyRaceBuildErr)
	}
	return applyRacePath
}

// applyRaceReport mirrors the report the child program prints. The field names
// must match internal/supersede/testdata/applyrace/main.go; that file documents
// why the two are not one shared type.
type applyRaceReport struct {
	Label     string      `json:"label"`
	Asked     [][2]string `json:"asked"`
	Created   int         `json:"created"`
	Confirmed int         `json:"confirmed"`
	Refused   int         `json:"refused"`
	Opposite  int         `json:"opposite_live"`
	// CausesCreated mirrors the child's `causes_created`: the two relations are
	// written through different writers, so one total cannot say which of them a
	// pass reached for. It counts VERDICTS, and CausesWritten mirrors the child's
	// `causes_written` so a parent can tell the two apart — the race this harness
	// stages is the one state where they differ (#834).
	CausesCreated int    `json:"causes_created"`
	CausesWritten int    `json:"causes_written"`
	Error         string `json:"error"`
}

// applyRaceChild is one spawned pass and everything the parent learns from it.
type applyRaceChild struct {
	label string
	cmd   *exec.Cmd
	out   bytes.Buffer
	errb  bytes.Buffer
	done  chan struct{}
}

// applyRaceBarrierTimeout bounds each of the parent's waits for a child to reach
// a state. It is a deadlock backstop, not a performance budget: the child
// announces itself within milliseconds of starting and the parent releases it
// within milliseconds of that, and a child that is never released is a failed
// run, which the timeout turns into a named failure instead of a hung binary.
const applyRaceBarrierTimeout = 60 * time.Second

// TestTwoApplyPassesCannotWriteACycle is the reproduction and the guard in one:
// two real processes, two real stores on one file, the shipped pass, and a
// classifier that answers SUPERSEDES to whatever direction it is handed — the
// hostile input #778 measured, used here because the CLASSIFIER IS NOT THE
// VARIABLE. Both passes get an answer that says yes; the question is whether the
// graph can end up holding both of them.
func TestTwoApplyPassesCannotWriteACycle(t *testing.T) {
	if testing.Short() {
		// It builds a helper program and starts two processes, which is more
		// than a unit test should cost. It makes no LLM call of any kind, so
		// unlike the harness suites it needs no GHOST_LIVE_TESTS: the expensive
		// part is the process count, and that is what -short opts out of here.
		t.Skip("multi-process test: builds a helper binary and spawns processes; skipped under -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*applyRaceBarrierTimeout)
	defer cancel()

	dir := t.TempDir()
	db := filepath.Join(dir, "ghost.db")
	barrier := filepath.Join(dir, "barrier")
	home := filepath.Join(dir, "home")
	for _, d := range []string{barrier, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}

	// The corpus: a version bump. Two notes, near-identical vectors so the scan
	// proposes the pair, and stamps a month apart so the pair HAS a chronology —
	// the fix is the newer one and the stale claim the older.
	handle, store := seedSharedStore(t, db)
	defer store.Close() //nolint:errcheck
	fix := add(t, store, handle, "the ingest service now runs Redis 7.2", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	stale := add(t, store, handle, "the ingest service runs Redis 6.2", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")

	// Pass one: run it to the point where it has read the graph and spent its
	// classify call, and hold it there. It saw the pair as fix→stale, because
	// that is what the timestamps said when it scanned.
	first := startApplyPass(ctx, t, db, barrier, home, "first")
	if err := waitApplyBarrier(barrier, "first-asked"); err != nil {
		t.Fatalf("%v", err)
	}
	// The edit that flips the pair's chronology, between the two scans. A TAG
	// edit and not a content one, because that is what a live `ghost mcp` does
	// to a note nobody rewrote — and because a content edit would delete the
	// row's embedding, which would remove it from the second pass's scan and
	// quietly turn this into a test of a different thing (a pair neither pass
	// proposes is a test that passes for the wrong reason).
	//
	// Nothing about this write is illegal or unusual. It is a note being
	// retagged while a maintenance pass is mid-flight, which is the ordinary
	// reason a pass's snapshot is stale by the time it writes.
	retag(t, store, stale)

	// Pass two, started after that edit: same graph, same corpus, and now the
	// OTHER orientation. It is held at its own classify call too, so its read of
	// the graph still predades the first pass's write — which is what makes the
	// two writes race rather than queue.
	second := startApplyPass(ctx, t, db, barrier, home, "second")
	if err := waitApplyBarrier(barrier, "second-asked"); err != nil {
		t.Fatalf("%v", err)
	}

	// Release the first pass and wait for it to be gone before releasing the
	// second, so the order the two writes reach the graph in is defined rather
	// than left to the scheduler: the second pass's write is the one that must
	// find the first pass's edge already there.
	releaseApplyPass(barrier, "first")
	firstReport := first.wait(t)
	releaseApplyPass(barrier, "second")
	secondReport := second.wait(t)

	// Both passes really did ask about the same pair in opposite directions.
	// Without this the test could pass because the two passes AGREED with each
	// other and never raced, which is a different (and already-passing) case.
	if len(firstReport.Asked) != 1 || len(secondReport.Asked) != 1 {
		t.Fatalf("the two passes asked about %d and %d pair(s), want 1 each: %v / %v",
			len(firstReport.Asked), len(secondReport.Asked), firstReport.Asked, secondReport.Asked)
	}
	if firstReport.Asked[0] != [2]string{fix, stale} {
		t.Errorf("the first pass asked %v, want [%s %s]: it scanned before the retag", firstReport.Asked[0], fix, stale)
	}
	if secondReport.Asked[0] != [2]string{stale, fix} {
		t.Errorf("the second pass asked %v, want [%s %s]: the retag moved the pair's chronology between the two scans",
			secondReport.Asked[0], stale, fix)
	}

	// The harm, read from the graph rather than from either pass's counters: a
	// pair claimed in both directions demotes both endpoints, and neither edge
	// withdraws the other.
	edges, err := store.SupersedesWithin(ctx, []string{fix, stale})
	if err != nil {
		t.Fatalf("SupersedesWithin: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("live supersedes edges = %v, want exactly 1: two passes that each saw an unclaimed pair wrote both directions of it, and a cycle demotes BOTH endpoints while neither edge withdraws the other",
			edges)
	}
	// The pass that lost the race is told so rather than left to report a link
	// it did not write: one pass wrote the edge, the other had a verdict about a
	// pair that was no longer unclaimed, and a report that said "linked" for both
	// would be claiming a graph change one of them did not make.
	wrote, refused := 0, 0
	for _, rep := range []applyRaceReport{firstReport, secondReport} {
		wrote += rep.Created
		refused += rep.Refused
	}
	if wrote != 1 {
		t.Errorf("the two passes report %d edge(s) written between them, want 1: %+v / %+v", wrote, firstReport, secondReport)
	}
	if refused != 1 {
		t.Errorf("the two passes report %d refused write(s), want 1: the pass that lost the race has to say it wrote nothing, or its report claims a link that is not there (%+v / %+v)",
			refused, firstReport, secondReport)
	}
}

// TestTwoApplyPassesCannotWriteACausesCycle is #823's half of the same race, and
// it is a separate test rather than a table row because the harm is a different
// one: two 'causes' edges demote nothing, so what two concurrent passes produce
// is not a pair that drops out of ranking but a pair whose two notes are recorded
// as having caused each other — a contradiction a reader of the graph cannot
// resolve and no repair can see, because `--reassess` loads live 'supersedes'
// edges only.
//
// Everything else is the shape above, unchanged and for the same reasons: two real
// processes, one file, a barrier held inside the classify call, and a retag
// between the two scans to make them read the pair in opposite directions. The
// direction the retag flips matters for 'causes' as it does for 'supersedes' — the
// pair's chronology is what the scan orients by either way — and the child writes
// its 'causes' edge in the orientation it was asked about, which is the orientation
// that moved.
//
// The refusal is the store's, inside the write transaction, and the loser is told
// so. Both halves matter: a pass that wrote nothing and said nothing would look
// exactly like a pass that found nothing.
func TestTwoApplyPassesCannotWriteACausesCycle(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process test: builds a helper binary and spawns processes; skipped under -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*applyRaceBarrierTimeout)
	defer cancel()

	dir := t.TempDir()
	db := filepath.Join(dir, "ghost.db")
	barrier := filepath.Join(dir, "barrier")
	home := filepath.Join(dir, "home")
	for _, d := range []string{barrier, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}

	handle, store := seedSharedStore(t, db)
	defer store.Close() //nolint:errcheck
	// The two notes a retag is enough to swap: a content edit would drop the
	// embedding and remove the pair from the second pass's scan, which would turn
	// this into a test of a different thing.
	first := add(t, store, handle, "the restore path on one spindle is safe and fast", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	second := add(t, store, handle, "the restore is being rewritten to run on one spindle", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")

	early := startApplyPassAnswering(ctx, t, db, barrier, home, "first", "causes")
	if err := waitApplyBarrier(barrier, "first-asked"); err != nil {
		t.Fatalf("%v", err)
	}
	retag(t, store, second)
	late := startApplyPassAnswering(ctx, t, db, barrier, home, "second", "causes")
	if err := waitApplyBarrier(barrier, "second-asked"); err != nil {
		t.Fatalf("%v", err)
	}

	releaseApplyPass(barrier, "first")
	earlyReport := early.wait(t)
	releaseApplyPass(barrier, "second")
	lateReport := late.wait(t)

	// The two passes really did ask about the same pair in opposite directions,
	// so a "no cycle" result is not two passes that agreed and never raced.
	if len(earlyReport.Asked) != 1 || len(lateReport.Asked) != 1 {
		t.Fatalf("the two passes asked about %d and %d pair(s), want 1 each: %v / %v",
			len(earlyReport.Asked), len(lateReport.Asked), earlyReport.Asked, lateReport.Asked)
	}
	if earlyReport.Asked[0] == lateReport.Asked[0] {
		t.Fatalf("both passes asked about %v: the retag did not move the pair's chronology between the two scans, so this run never staged the race", earlyReport.Asked[0])
	}
	if earlyReport.Error != "" || lateReport.Error != "" {
		t.Fatalf("a child pass failed: %q / %q", earlyReport.Error, lateReport.Error)
	}

	edges := liveCausesEdges(t, store, first, second)
	if len(edges) != 1 {
		t.Fatalf("live 'causes' edges = %v, want exactly 1: two passes that each saw an unclaimed pair wrote both directions of it, and a 'causes' cycle asserts that each of the pair's notes caused the other",
			edges)
	}
	// The edge that stands is the FIRST writer's, in 'causes' convention: the pass
	// asked about (newer, older) and wrote (older → newer), because a cause
	// precedes its effect. A graph holding the other pass's direction would be the
	// same state reached by a different route, and this test is about which write
	// landed, so the direction is checked rather than inferred from the count.
	wantEdge := [2]string{earlyReport.Asked[0][1], earlyReport.Asked[0][0]}
	if edges[0] != wantEdge {
		t.Errorf("live 'causes' edges = %v, want the first writer's direction %v ('causes' is written older → newer, and it asked about %v)",
			edges, wantEdge, earlyReport.Asked[0])
	}

	// Both passes reached a verdict, and exactly one write landed — so this is
	// the one fixture where the two counters disagree, and the disagreement is
	// the whole of #834. CausesCreated counts VERDICTS (2) and CausesWritten
	// counts WRITES (1), so a report can count the writes and the dry-run hint
	// can keep counting the verdicts. Before CausesWritten existed the only way
	// to say "one edge was written" here was to subtract the refusals, which is
	// a report re-deriving a fact the pass already had.
	verdicts, written, refused := 0, 0, 0
	for _, rep := range []applyRaceReport{earlyReport, lateReport} {
		verdicts += rep.CausesCreated
		written += rep.CausesWritten
		refused += rep.Refused
	}
	if verdicts != 2 {
		t.Errorf("the two passes report %d 'causes' verdict(s), want 2: the point of the fixture is that both reach one, in opposite directions (%+v / %+v)",
			verdicts, earlyReport, lateReport)
	}
	if written != 1 {
		t.Errorf("the two passes report %d 'causes' write(s), want 1: two verdicts in opposite directions and one edge in the graph means exactly one of them was written, and a counter that cannot say so is the one the summary line used to print (%+v / %+v)",
			written, earlyReport, lateReport)
	}
	if refused != 1 {
		t.Errorf("the two passes report %d refused write(s), want 1: the pass that lost the race has to say it wrote nothing, or its report claims a link that is not there (%+v / %+v)",
			refused, earlyReport, lateReport)
	}
	// And the two disagree by exactly the refusal, which is the relation #834
	// turned on: CausesCreated is the verdict count the dry-run hint keys off,
	// CausesWritten is what the graph gained, and the difference is the one pair
	// the store refused.
	if verdicts-written != refused {
		t.Errorf("verdicts %d - writes %d = %d, want the %d refused write(s): the write count and the verdict count differ by the refusals and by nothing else (%+v / %+v)",
			verdicts, written, verdicts-written, refused, earlyReport, lateReport)
	}
}

// seedSharedStore opens a FILE-backed store at path, which is the point of the
// fixture: a second process has to be able to open the same database, and an
// in-memory handle cannot be shared with anything.
func seedSharedStore(t *testing.T, path string) (*sql.DB, *memory.Store) {
	t.Helper()
	handle, err := memory.OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	store := memory.NewStore(handle, discardLogger())
	if err := store.EnsureProject(context.Background(), "p", filepath.Join(path, "project"), "p"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return handle, store
}

// retag moves a memory's updated_at without touching its text, which is what an
// ordinary edit through a live `ghost mcp` does to a note nobody rewrote. The
// tag is a real change, so the write is a real one — nothing here pokes the
// column directly, because a fixture that rewrites updated_at behind the store's
// back would also be a fixture that proved nothing about what the store does.
func retag(t *testing.T, store *memory.Store, id string) {
	t.Helper()
	if err := store.UpdateMemory(context.Background(), "p", id, nil, nil, nil, []string{"retagged-mid-pass"}); err != nil {
		t.Fatalf("retag %s: %v", id, err)
	}
}

// startApplyPass spawns one `ghost supersede <project> --apply` pass against the
// shared database. Its HOME family and both XDG roots point into the test's temp
// dir, so a code path inside the child that reaches for the user's real profile —
// a config lookup, a data dir — finds nothing there instead of reading or
// writing the machine's actual Ghost install. The database is passed by path, so
// nothing in this test needs the real one; the guard is what keeps it that way.
func startApplyPass(ctx context.Context, t *testing.T, db, barrier, home, label string) *applyRaceChild {
	return startApplyPassAnswering(ctx, t, db, barrier, home, label, "supersedes")
}

// startApplyPassAnswering is startApplyPass with the relation the child's fake
// classifier answers, which is the only difference between the two races this
// file stages: the interleaving, the barrier and the retag are the same, and only
// the write under test differs.
func startApplyPassAnswering(ctx context.Context, t *testing.T, db, barrier, home, label, verdict string) *applyRaceChild {
	t.Helper()
	c := &applyRaceChild{
		label: label,
		done:  make(chan struct{}),
		cmd: exec.CommandContext(ctx, applyRaceHelper(t),
			"-db", db, "-barrier", barrier, "-label", label, "-project", "p", "-threshold", "0.9",
			"-verdict", verdict),
	}
	c.cmd.Env = append(os.Environ(),
		"HOME="+home,
		"USERPROFILE="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"),
		"XDG_DATA_HOME="+filepath.Join(home, "data"),
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
	)
	c.cmd.Stdout = &c.out
	c.cmd.Stderr = &c.errb
	if err := c.cmd.Start(); err != nil {
		t.Fatalf("starting pass %s: %v", label, err)
	}
	go func() {
		_ = c.cmd.Wait()
		close(c.done)
	}()
	return c
}

// wait collects one child's report. The report is parsed even when the process
// failed: a child that dies still prints what it saw, and that is the only place
// the reason exists — dropping it on a non-zero exit would leave "exit status 1"
// as the whole diagnosis.
func (c *applyRaceChild) wait(t *testing.T) applyRaceReport {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(applyRaceBarrierTimeout):
		t.Fatalf("pass %s never exited; stderr:\n%s", c.label, c.errb.String())
	}
	var rep applyRaceReport
	if err := json.Unmarshal(c.out.Bytes(), &rep); err != nil {
		t.Fatalf("pass %s: decode report: %v\nstdout: %s\nstderr: %s", c.label, err, c.out.String(), c.errb.String())
	}
	if c.errb.Len() > 0 {
		t.Logf("pass %s stderr:\n%s", c.label, c.errb.String())
	}
	return rep
}

// releaseApplyPass lets a held pass proceed to its writes.
func releaseApplyPass(barrier, label string) {
	if err := os.WriteFile(filepath.Join(barrier, label+"-go"), []byte(time.Now().Format(time.RFC3339Nano)), 0o600); err != nil {
		panic(fmt.Sprintf("release pass %s: %v", label, err))
	}
}

// waitApplyBarrier blocks until a pass has announced that it has read the graph
// and reached its classify call, which is the state the run depends on. It names
// the barriers already present when it gives up, because "which of the two passes
// got that far" is the question a failure of this test raises.
func waitApplyBarrier(dir, name string) error {
	deadline := time.Now().Add(applyRaceBarrierTimeout)
	for {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return nil
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

// TestApplyPassWritesAnEdgeWhenNothingOpposesIt is the other half, and it is here
// because a guard that refuses everything would pass the test above. One pass, no
// concurrent writer, one pair: the edge is written, in the direction the
// timestamps gave, and the pass says it linked it.
func TestApplyPassWritesAnEdgeWhenNothingOpposesIt(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-process test: builds a helper binary and spawns processes; skipped under -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*applyRaceBarrierTimeout)
	defer cancel()

	dir := t.TempDir()
	db := filepath.Join(dir, "ghost.db")
	barrier := filepath.Join(dir, "barrier")
	home := filepath.Join(dir, "home")
	for _, d := range []string{barrier, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("MkdirAll %s: %v", d, err)
		}
	}
	handle, store := seedSharedStore(t, db)
	defer store.Close() //nolint:errcheck
	fix := add(t, store, handle, "the ingest service now runs Redis 7.2", []float32{1, 0, 0}, "2026-09-01 00:00:00")
	stale := add(t, store, handle, "the ingest service runs Redis 6.2", []float32{0.98, 0.02, 0}, "2026-01-01 00:00:00")

	c := startApplyPass(ctx, t, db, barrier, home, "only")
	// The child holds itself at its classify call so the parent controls when
	// the write happens; with nothing to race, the only thing left to control is
	// that the pass gets there at all.
	if err := waitApplyBarrier(barrier, "only-asked"); err != nil {
		t.Fatalf("%v", err)
	}
	releaseApplyPass(barrier, "only")
	rep := c.wait(t)
	if rep.Error != "" {
		t.Fatalf("the pass failed: %s", rep.Error)
	}
	if rep.Created != 1 || rep.Refused != 0 {
		t.Errorf("created=%d refused=%d, want 1 and 0: nothing opposed this write, and a guard that refuses an unopposed pair is not a guard", rep.Created, rep.Refused)
	}
	edges, err := store.SupersedesWithin(ctx, []string{fix, stale})
	if err != nil {
		t.Fatalf("SupersedesWithin: %v", err)
	}
	if len(edges) != 1 || edges[0] != [2]string{fix, stale} {
		t.Errorf("live supersedes edges = %v, want exactly [%s %s]", edges, fix, stale)
	}
	// The row the pass wrote is auditable, which is what the guarded write must
	// not cost: a refused write files no history, and a written one files the
	// same `supersede` row it always did.
	entries, err := store.MemoryHistory(ctx, stale, 0)
	if err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Phase == "supersede" {
			found = true
		}
	}
	if !found {
		t.Errorf("no supersede history row for %s: %s", stale, strings.Join(phasesOf(entries), ", "))
	}
}

// phasesOf renders a history listing for a failure message, so the assertion
// above names what IS there rather than nothing.
func phasesOf(entries []memory.HistoryEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Phase)
	}
	return out
}
