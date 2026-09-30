// Command applyrace is the child-process half of the two-concurrent-passes test
// in internal/supersede (TestTwoApplyPassesCannotWriteACycle).
//
// It exists because that test cannot re-exec its own test binary: under
// `go test` the test binary IS the suite, so a self-spawn re-runs every test in
// the package once per spawn, and a mistake in the barrier logic becomes a fork
// bomb. So the test builds this program once with `go build` into a temp dir and
// starts one process per pass, exactly as internal/memory's multi-process
// contract test does.
//
// Each child is a real, separate `ghost supersede <project> --apply` pass: its
// own *sql.DB against the same file, the shipped supersede.RunWith, and the
// shipped memory.Store underneath. The only thing faked is the classifier, which
// is the pass's most expensive and least deterministic input and is replaced
// there by the documented hostile input — a model that answers SUPERSEDES to
// whatever direction it is handed. Two of those over the same pair is the
// condition #778 measured writing a cycle, and the variable under test is
// whether the two WRITES can both land.
//
// The child stops inside the classify call, which is the only point a pass holds
// between having read the graph and writing to it:
//
//   - it announces `<label>-asked`, having by then read the live edges and run
//     the candidate scan, and it publishes the directions it was asked about;
//   - it waits for `<label>-go`, which is the parent releasing it to write.
//
// That is the whole interleaving the test needs, and it is a barrier rather than
// a sleep so a slow machine makes the test slower instead of wrong.
//
// Every child reports what it measured as one JSON object on stdout. Failures go
// to stderr as text and set a non-zero exit status; the measurements are still
// reported, because a child that dies before printing tells the parent nothing
// about why.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/supersede"
)

// report is the parent-facing result of one child run.
// internal/supersede/concurrent_apply_process_test.go declares the same shape
// for decoding; the two must agree on the JSON field names. This is a test
// protocol, not an API, so the agreement is by review rather than by a shared
// type — the alternative, a self-spawning test binary, is the fork-bomb risk the
// test exists to avoid.
type report struct {
	Label string `json:"label"`
	// Asked is one {newer, older} per pair the pass put to the classifier, in
	// the order it asked. The parent reads it to confirm the two passes really
	// were oriented oppositely, because a test that merely observes "no cycle"
	// without that is also satisfied by two passes that agreed with each other
	// and therefore never raced.
	Asked [][2]string `json:"asked,omitempty"`
	// Created, Confirmed and Refused are the pass's own counters. Refused is the
	// write-time refusal this test is about: the pass had a verdict, wanted to
	// write the edge, and did not because the pair was already claimed the other
	// way round.
	Created   int `json:"created"`
	Confirmed int `json:"confirmed"`
	Refused   int `json:"refused"`
	Opposite  int `json:"opposite_live"`
	// CausesCreated is the 'causes' counterpart of Created, and it exists because
	// the two relations are written through different writers (the second is
	// guarded since #823 and the first always was), so one total cannot answer
	// "did the guarded writer get there" for a run that wrote either.
	//
	// It counts VERDICTS, which is the asymmetry #834 is about: over the race
	// this harness stages both passes reach one and only one write lands, so the
	// two counters here disagree by exactly the refused write. CausesWritten is
	// the write count that pairs with Created.
	CausesCreated int    `json:"causes_created"`
	CausesWritten int    `json:"causes_written"`
	Error         string `json:"error,omitempty"`
}

// barrierWait is how long a child waits for the parent to release it before
// giving up. It is a deadlock backstop, not a performance budget: the parent
// releases a child within milliseconds of seeing its announcement, and a child
// that is never released is a parent that failed, which the timeout turns into a
// named failure instead of a hung test binary.
const barrierWait = 60 * time.Second

func main() {
	var (
		db        = flag.String("db", "", "path to the shared ghost.db")
		project   = flag.String("project", "", "project name or id to pass over")
		barrier   = flag.String("barrier", "", "directory the barriers are files in")
		label     = flag.String("label", "", "this child's name in the barrier directory")
		threshold = flag.Float64("threshold", 0.9, "cosine threshold for a candidate pair")
		// verdict is the relation the fake classifier answers, and it is a flag
		// because the race this harness stages is the same race for both
		// relations — two passes writing the two directions of one pair — and
		// only the WRITE under test differs.
		verdict = flag.String("verdict", "supersedes", "the relation the classifier answers: supersedes or causes")
	)
	flag.Parse()

	rep := report{Label: *label}
	// Printed even on the paths below that fail, because the report is the only
	// record of what this process saw.
	defer func() {
		enc := json.NewEncoder(os.Stdout)
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(os.Stderr, "encode report: %v\n", err)
			os.Exit(1)
		}
	}()

	if *db == "" || *barrier == "" || *label == "" {
		fail(&rep, "missing -db, -barrier or -label")
		return
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	handle, err := memory.OpenDB(*db)
	if err != nil {
		fail(&rep, fmt.Sprintf("open database: %v", err))
		return
	}
	store := memory.NewStore(handle, logger)
	defer store.Close() //nolint:errcheck

	answer, err := relationFor(*verdict)
	if err != nil {
		fail(&rep, err.Error())
		return
	}
	cls := &raceClassifier{barrier: *barrier, label: *label, verdict: answer}
	res, _, err := supersede.RunWith(ctx, store, cls, *project, supersede.Options{
		Threshold: float32(*threshold),
		Apply:     true,
	}, logger)
	rep.Created, rep.Confirmed, rep.Refused, rep.Opposite = res.Created, res.Confirmed, res.ReverseLive, res.OppositeLive
	rep.CausesCreated, rep.CausesWritten = res.CausesCreated, res.CausesWritten
	rep.Asked = cls.asked
	if err != nil {
		rep.Error = err.Error()
		fmt.Fprintf(os.Stderr, "apply pass failed: %v\n", err)
		os.Exit(1)
	}
}

// raceClassifier is the hostile classifier #778 measured, with the two barrier
// stops added: it answers one relation — SUPERSEDES by default, CAUSES on
// -verdict — to every pair, in whichever direction it is handed, after announcing
// that it has been asked and waiting to be released.
//
// The stop is INSIDE the classify call on purpose. It is the only point in a pass
// where the graph has been read and the write has not happened, so it is the
// only place a test can hold a pass between its decision and its effect and let
// another process take the same decision the other way round.
type raceClassifier struct {
	barrier string
	label   string
	verdict supersede.Relation
	asked   [][2]string
}

// relationFor turns the -verdict flag into the relation to answer, and refuses a
// spelling it does not know rather than defaulting to one: a child that answered
// the wrong relation would report a clean run for a race nobody staged.
func relationFor(name string) (supersede.Relation, error) {
	switch supersede.Relation(name) {
	case supersede.RelationSupersedes, supersede.RelationCauses:
		return supersede.Relation(name), nil
	}
	return "", fmt.Errorf("-verdict %q is not a relation this pass writes", name)
}

func (c *raceClassifier) ClassifyBatch(ctx context.Context, pairs []supersede.Candidate) ([]supersede.Relation, error) {
	out := make([]supersede.Relation, len(pairs))
	for i, p := range pairs {
		c.asked = append(c.asked, [2]string{p.NewerID, p.OlderID})
		out[i] = c.verdict
	}
	if err := c.signal("asked"); err != nil {
		return nil, err
	}
	if err := c.wait(ctx, "go"); err != nil {
		return nil, err
	}
	return out, nil
}

// signal announces that this pass has reached the point the run depends on.
func (c *raceClassifier) signal(name string) error {
	path := filepath.Join(c.barrier, c.label+"-"+name)
	if err := os.WriteFile(path, []byte(time.Now().Format(time.RFC3339Nano)), 0o600); err != nil {
		return fmt.Errorf("signal barrier %s: %w", path, err)
	}
	return nil
}

// wait blocks until the parent releases this pass, or the backstop expires.
func (c *raceClassifier) wait(ctx context.Context, name string) error {
	path := filepath.Join(c.barrier, c.label+"-"+name)
	deadline := time.Now().Add(barrierWait)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for barrier %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("barrier %s never appeared in %s", path, c.barrier)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// fail records why the child is giving up and exits non-zero, so the parent
// reads a report that says what went wrong instead of an empty one.
func fail(rep *report, why string) {
	rep.Error = why
	fmt.Fprintf(os.Stderr, "%s\n", why)
	os.Exit(1)
}
