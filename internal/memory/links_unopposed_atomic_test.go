package memory

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// This file is #825: the ATOMICITY of the guard in CreateLinkUnopposed.
//
// The property is not that the guard refuses a pair somebody else has already
// claimed — TestCreateLinkUnopposedRefusesTheOppositeDirection proves that, and
// needs no concurrency to do it. The property is that the reverse edge is read
// INSIDE the same BEGIN IMMEDIATE transaction that inserts this one, so two
// writers claiming one pair in opposite directions are serialised by SQLite's
// write lock rather than by anything either of them agreed to.
//
// Nothing proved that. The two-process proof in internal/supersede
// (TestTwoApplyPassesCannotWriteACycle) releases its writers one at a time and
// waits for the first to EXIT before releasing the second, so the second always
// reads an already-committed opposing edge — which is the outcome the guard
// produces whether or not the check shares a transaction with the write. Moving
// the check into its own committed transaction ahead of the write transaction
// passes that test unchanged.
//
// So this is a store-level test on two handles over one file, which is the
// concurrency the rest of this package reasons about the write lock with
// (TestConcurrentProcessesMixedReadWrite: OpenDB pins one connection per handle,
// so N handles is the real concurrency N). No child process is spawned and
// nothing re-executes the test binary; the barrier is a third handle holding
// SQLite's write lock, the same fixture the #671 contention tests use.

// unopposedPark is how long every handle must report its single connection in
// use before the test believes the writers are parked inside BeginTx rather than
// still on their way there.
//
// This is the one timing number in the file and it is a FLOOR, not a deadline.
// Parking is what makes the release below a barrier rather than a delay, and it
// discriminates between the two implementations because of where the read sits
// relative to the lock:
//
//   - correct: the writer is blocked in BEGIN IMMEDIATE, so its reverse-edge
//     read has NOT happened yet. Releasing the lock lets exactly one of them
//     read, insert and commit; the second reads afterwards and sees the edge.
//   - split: the writer did its read in its own committed transaction BEFORE
//     calling beginWrite, so both readers saw an empty pair and both go on to
//     insert. Parking is what has already let them both do that.
//
// The floor only has to outlast a pre-lock read, which is one indexed lookup —
// tens of microseconds. 250 ms is four orders of magnitude above that, and the
// only cost of overshooting is that the release happens later.
const unopposedPark = 250 * time.Millisecond

// unopposedParkPoll is how often the handles are sampled while waiting.
const unopposedParkPoll = 10 * time.Millisecond

// unopposedWait bounds how long the test waits for anything at all. It is a
// deadlock backstop, not a performance budget: the writers park within
// milliseconds of starting and the release follows a fixed window later.
const unopposedWait = 60 * time.Second

// TestCreateLinkUnopposedChecksTheReverseEdgeInsideItsWriteTransaction is #825's
// test. Both writers want opposite directions of one pair, neither edge exists,
// both are held at the barrier and released together, and the graph must end up
// holding exactly one of them.
func TestCreateLinkUnopposedChecksTheReverseEdgeInsideItsWriteTransaction(t *testing.T) {
	dbPath := t.TempDir() + "/unopposed-atomic.sqlite"
	// The file and its project exist before the writer handles are opened, so
	// every handle under test opens an existing schema and none of them is
	// racing a migration.
	seed := upsertLockTestStore(t, dbPath)
	newer := makeMemory(t, seed, "the ingest service now runs Redis 7.2")
	stale := makeMemory(t, seed, "bug: the ingest service still runs Redis 6.2")

	// Two independent handles, each with its own single connection. Separate
	// connections are the concurrency: SQLite serialises writers by connection,
	// not by goroutine, so two Stores over one *sql.DB would serialise on that
	// pool and prove nothing about the lock.
	first := upsertLockTestStore(t, dbPath)
	second := upsertLockTestStore(t, dbPath)

	ctx := context.Background()
	// The blocker takes SQLite's write lock BEFORE either writer starts, so
	// neither can get past BeginTx until the test releases it, and the
	// interleaving is the fixture's rather than a race the test lost.
	blocker := newBlocker(t, dbPath)

	results := make(chan unopposedResult, 2)
	go raceOneUnopposed(results, "newer-supersedes-stale", [2]string{newer, stale}, first, ctx)
	go raceOneUnopposed(results, "stale-supersedes-newer", [2]string{stale, newer}, second, ctx)

	requireBothParked(t, first.db, second.db)

	// Released here rather than by a timer, so the two writes leave the barrier
	// together and neither writer has an ordering advantage. newBlocker's cleanup
	// rolls back again afterwards, which returns ErrTxDone and is ignored.
	if err := blocker.tx.Rollback(); err != nil {
		t.Fatalf("release the write lock: %v", err)
	}

	got := map[string]unopposedResult{}
	for range 2 {
		select {
		case r := <-results:
			if _, dup := got[r.label]; dup {
				t.Fatalf("two results reported for %s", r.label)
			}
			got[r.label] = r
		case <-time.After(unopposedWait):
			t.Fatalf("a writer never returned; the one that did: %v", got)
		}
	}
	for _, r := range got {
		if r.err != nil {
			t.Fatalf("%s: CreateLinkUnopposed: %v — one bounded retry should have taken the lock",
				r.label, r.err)
		}
	}

	// The harm, read from the graph rather than from either return value: a pair
	// claimed in both directions demotes both endpoints and neither edge withdraws
	// the other, so the row count is the whole of what went wrong. The IDs are in
	// the message because the direction is the diagnosis — two edges here means
	// each writer saw an empty pair, which is what a check outside the write
	// transaction produces.
	edges, err := seed.SupersedesWithin(ctx, []string{newer, stale})
	if err != nil {
		t.Fatalf("SupersedesWithin: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("live supersedes edges = %v, want exactly 1: both writers read the pair as unopposed and both wrote it, "+
			"which is what happens when the reverse-edge check is not in the transaction that writes the edge (%s and %s)",
			edges, newer, stale)
	}

	// The refusal is REPORTED, and the edge that survived is the direction of the
	// writer that reported writing it. Both are separate assertions from the row
	// count: a guard that wrote one edge and told both callers it wrote one would
	// pass the count, and a guard that refused both would fail it while leaving a
	// pair nothing can ever claim again.
	var writer, refuser unopposedResult
	for _, r := range got {
		if r.wrote {
			if writer.label != "" {
				t.Fatalf("%s and %s both reported a write: the guard refused neither", writer.label, r.label)
			}
			writer = r
			continue
		}
		refuser = r
	}
	if writer.label == "" || refuser.label == "" {
		t.Fatalf("the two writers report %v, want exactly one written and one refused: the writer that lost the race has to "+
			"say it wrote nothing, or its report claims a link that is not there", got)
	}
	if edges[0] != writer.claimed {
		t.Errorf("%s reported writing the edge and the graph holds %v, which is the direction %s claimed: the two "+
			"reports and the graph do not agree", writer.label, edges[0], refuser.label)
	}
}

// unopposedResult is one writer's outcome: the label it was started under, the
// direction it asked for, and what CreateLinkUnopposed said.
type unopposedResult struct {
	label   string
	claimed [2]string
	wrote   bool
	err     error
}

// raceOneUnopposed claims one direction of the pair on its own handle. The
// judged stamp is empty, so the write takes the write clock like any other
// caller's: nothing here is about freshness.
func raceOneUnopposed(out chan<- unopposedResult, label string, claimed [2]string, s *Store, ctx context.Context) {
	wrote, err := s.CreateLinkUnopposed(ctx, claimed[0], claimed[1], "supersedes", 0.95, "llm", "")
	out <- unopposedResult{label: label, claimed: claimed, wrote: wrote, err: err}
}

// requireBothParked waits until every handle given reports its single connection
// in use, CONTINUOUSLY, for unopposedPark.
//
// "Continuously" is load-bearing: a handle whose writer has not reached BeginTx
// reports InUse 0 and resets that handle's run, so a writer that is merely slow
// to be scheduled cannot satisfy the wait. What is left is a writer blocked in
// SQLite's busy handler with the connection checked out, which is the only state
// from which the release is meaningful.
func requireBothParked(t *testing.T, handles ...*sql.DB) {
	t.Helper()
	deadline := time.Now().Add(unopposedWait)
	need := int(unopposedPark / unopposedParkPoll)
	// Consecutive parked samples per handle, so one handle parking early cannot
	// stand in for the other.
	run := make([]int, len(handles))
	for {
		parked := 0
		for i, h := range handles {
			inUse := h.Stats().InUse
			switch inUse {
			case 0:
				run[i] = 0
			case 1:
				run[i]++
				if run[i] >= need {
					parked++
				}
			default:
				t.Fatalf("handle %d reports %d connections in use; OpenDB pins one per handle, so this is not the "+
					"fixture the barrier was written against", i, inUse)
			}
		}
		if parked == len(handles) {
			return
		}
		if time.Now().After(deadline) {
			var states []string
			for i, h := range handles {
				states = append(states, "handle "+itoa(i)+": inUse="+itoa(h.Stats().InUse))
			}
			t.Fatalf("not every writer parked inside BeginTx (%s): a writer that never reaches the write lock means the "+
				"barrier is not holding, and the release below would prove nothing", strings.Join(states, ", "))
		}
		time.Sleep(unopposedParkPoll)
	}
}
