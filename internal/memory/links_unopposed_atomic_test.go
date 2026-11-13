package memory

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// unopposedPark is how long every handle must report its single connection in
// use before the writers count as parked inside BeginTx rather than still on
// their way there. It is a floor, not a deadline: it only has to outlast a
// pre-lock reverse-edge read (one indexed lookup, tens of microseconds).
const unopposedPark = 250 * time.Millisecond

const unopposedParkPoll = 10 * time.Millisecond

// unopposedWait is a deadlock backstop, not a performance budget.
const unopposedWait = 60 * time.Second

// TestCreateLinkUnopposedChecksTheReverseEdgeInsideItsWriteTransaction is #825's
// test: the guard reads the reverse edge INSIDE the BEGIN IMMEDIATE transaction
// that inserts this one, so two writers claiming opposite directions of one pair
// are serialised by SQLite's write lock. Both writers are held behind a third
// handle's write lock and released together; the graph must hold exactly one edge
// (TestCreateLinkUnopposedRefusesTheOppositeDirection covers the sequential case).
func TestCreateLinkUnopposedChecksTheReverseEdgeInsideItsWriteTransaction(t *testing.T) {
	dbPath := t.TempDir() + "/unopposed-atomic.sqlite"
	seed := upsertLockTestStore(t, dbPath)
	newer := makeMemory(t, seed, "the ingest service now runs Redis 7.2")
	stale := makeMemory(t, seed, "bug: the ingest service still runs Redis 6.2")

	// Separate handles, each with its own connection: SQLite serialises writers
	// by connection, not by goroutine.
	first := upsertLockTestStore(t, dbPath)
	second := upsertLockTestStore(t, dbPath)
	ctx := context.Background()
	blocker := newBlocker(t, dbPath)

	results := make(chan unopposedResult, 2)
	go raceOneUnopposed(results, "newer-supersedes-stale", [2]string{newer, stale}, first, ctx)
	go raceOneUnopposed(results, "stale-supersedes-newer", [2]string{stale, newer}, second, ctx)
	requireBothParked(t, first.db, second.db)
	if err := blocker.tx.Rollback(); err != nil {
		t.Fatalf("release the write lock: %v", err)
	}

	got := map[string]unopposedResult{}
	for range 2 {
		select {
		case r := <-results:
			got[r.label] = r
		case <-time.After(unopposedWait):
			t.Fatalf("a writer never returned; the one that did: %v", got)
		}
	}
	for _, r := range got {
		if r.err != nil {
			t.Fatalf("%s: CreateLinkUnopposed: %v", r.label, r.err)
		}
	}
	// The harm, read from the graph: a pair claimed both ways demotes both endpoints.
	edges, err := seed.SupersedesWithin(ctx, []string{newer, stale})
	if err != nil {
		t.Fatalf("SupersedesWithin: %v", err)
	}
	if len(edges) != 1 {
		t.Fatalf("live supersedes edges = %v, want exactly 1: the reverse-edge check is not in the write transaction", edges)
	}
	// Exactly one writer reports a write, and the surviving edge is its direction.
	var writer, refuser unopposedResult
	for _, r := range got {
		if r.wrote {
			if writer.label != "" {
				t.Fatalf("%s and %s both reported a write", writer.label, r.label)
			}
			writer = r
			continue
		}
		refuser = r
	}
	if writer.label == "" || refuser.label == "" {
		t.Fatalf("writers report %v, want exactly one written and one refused", got)
	}
	if edges[0] != writer.claimed {
		t.Errorf("%s reported the write but the graph holds %v", writer.label, edges[0])
	}
}

// unopposedResult is one writer's outcome.
type unopposedResult struct {
	label   string
	claimed [2]string
	wrote   bool
	err     error
}

// raceOneUnopposed claims one direction of the pair on its own handle.
func raceOneUnopposed(out chan<- unopposedResult, label string, claimed [2]string, s *Store, ctx context.Context) {
	wrote, err := s.CreateLinkUnopposed(ctx, claimed[0], claimed[1], "supersedes", 0.95, "llm", "")
	out <- unopposedResult{label: label, claimed: claimed, wrote: wrote, err: err}
}

// requireBothParked waits until every handle reports its single connection in
// use CONTINUOUSLY for unopposedPark: a handle whose writer has not reached
// BeginTx reports InUse 0 and resets that handle's run, so only a writer blocked
// in SQLite's busy handler satisfies the wait.
func requireBothParked(t *testing.T, handles ...*sql.DB) {
	t.Helper()
	deadline := time.Now().Add(unopposedWait)
	need := int(unopposedPark / unopposedParkPoll)
	run := make([]int, len(handles))
	for {
		parked := 0
		for i, h := range handles {
			switch inUse := h.Stats().InUse; inUse {
			case 0:
				run[i] = 0
			case 1:
				run[i]++
				if run[i] >= need {
					parked++
				}
			default:
				t.Fatalf("handle %d reports %d connections in use; OpenDB pins one per handle", i, inUse)
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
			t.Fatalf("not every writer parked inside BeginTx (%s)", strings.Join(states, ", "))
		}
		time.Sleep(unopposedParkPoll)
	}
}
