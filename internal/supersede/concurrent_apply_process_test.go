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
const applyRaceBarrierTimeout = 60 * time.Second

// TestTwoApplyPassesCannotWriteACycle is the reproduction and the guard in one:
func TestTwoApplyPassesCannotWriteACycle(t *testing.T) {
	if testing.Short() {
		// It builds a helper program and starts two processes, which is more
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
	retag(t, store, stale)

	// Pass two, started after that edit: same graph, same corpus, and now the
	second := startApplyPass(ctx, t, db, barrier, home, "second")
	if err := waitApplyBarrier(barrier, "second-asked"); err != nil {
		t.Fatalf("%v", err)
	}

	// Release the first pass and wait for it to be gone before releasing the
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
	wantEdge := [2]string{earlyReport.Asked[0][1], earlyReport.Asked[0][0]}
	if edges[0] != wantEdge {
		t.Errorf("live 'causes' edges = %v, want the first writer's direction %v ('causes' is written older → newer, and it asked about %v)",
			edges, wantEdge, earlyReport.Asked[0])
	}

	// Both passes reached a verdict, and exactly one write landed — so this is
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
func retag(t *testing.T, store *memory.Store, id string) {
	t.Helper()
	if err := store.UpdateMemory(context.Background(), "p", id, nil, nil, nil, []string{"retagged-mid-pass"}); err != nil {
		t.Fatalf("retag %s: %v", id, err)
	}
}

// startApplyPass spawns one `ghost supersede <project> --apply` pass against the
func startApplyPass(ctx context.Context, t *testing.T, db, barrier, home, label string) *applyRaceChild {
	return startApplyPassAnswering(ctx, t, db, barrier, home, label, "supersedes")
}

// startApplyPassAnswering is startApplyPass with the relation the child's fake
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
