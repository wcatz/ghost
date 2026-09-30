package memory

// The bounded BEGIN retry (issue #671).
//
// #671 lost a `ghost_memory_save` to `begin upsert tx: database is locked (5)
// (SQLITE_BUSY)` on a loaded runner, and the multi-process test next door
// reproduces it: the write lock is never held long — a save's transaction holds
// it for tens of milliseconds against a five-second budget — and a writer still
// runs out of budget, because SQLite's busy handler re-polls on a schedule that
// grows to 100 ms and keeps no queue, so a waiter asleep at the top of that
// schedule loses every hand-off to a writer that is awake.
//
// A second BEGIN is the fix the measurement supports, and it is a fix rather
// than a bigger budget for one reason: a fresh attempt restarts the busy handler
// at its first, shortest poll. The waiter competes again at 1, 2 and 5 ms instead
// of continuing a schedule that has already grown past 100 ms, which is how it
// lost every one of them.
//
// ONE extra attempt, never a loop. A save that cannot get in after two budgets is
// a machine that is not running this suite, and a retry loop would turn that
// into a tool call that never returns instead of an error the caller can report.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// shortTimeoutStore opens a handle the way OpenDB does — WAL, foreign keys,
// BEGIN IMMEDIATE, one pinned connection — with a shorter busy_timeout, so a
// contention case costs milliseconds rather than the contract's five seconds.
//
// The timeout is the ONLY thing changed, and deliberately so: the property under
// test is that the retry is bounded by one extra ATTEMPT rather than by the
// length of the budget, so a test that waited out the real five seconds twice
// would prove nothing a 150 ms budget does not.
func shortTimeoutStore(t *testing.T, dbPath string, busyMS int) *Store {
	t.Helper()
	dsn := "file:" + dbPath +
		"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(" +
		strconv.Itoa(busyMS) + ")"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open short-timeout handle: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	return NewStore(db, nil)
}

// recordingObserver collects the samples a store reports while a test runs.
type recordingObserver struct {
	mu      sync.Mutex
	samples []WriteLockSample
}

func (r *recordingObserver) install(t *testing.T) {
	t.Helper()
	t.Cleanup(SetWriteLockObserver(r.observe))
}

func (r *recordingObserver) observe(s WriteLockSample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = append(r.samples, s)
}

func (r *recordingObserver) all() []WriteLockSample {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]WriteLockSample(nil), r.samples...)
}

// blocker is a second handle holding SQLite's write lock, which is what a save
// arriving mid-contention actually meets. It is taken BEFORE the write under test
// starts, so the contention is the fixture's and not a race the test lost.
type blocker struct {
	store *Store
	tx    *sql.Tx
}

func newBlocker(t *testing.T, dbPath string) *blocker {
	t.Helper()
	other := upsertLockTestStore(t, dbPath)
	ctx := context.Background()
	tx, err := other.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("blocker begin: %v", err)
	}
	// A real write, so the transaction is not one SQLite may abandon as empty
	// and so the lock is held the way every other writer here holds it.
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET id = id WHERE id = ?`, testProject); err != nil {
		t.Fatalf("blocker write: %v", err)
	}
	// Released when the test ends however it ends. A second rollback on an
	// already-rolled-back transaction returns ErrTxDone, which is ignored, so
	// this is safe to run on the paths that release the lock themselves.
	t.Cleanup(func() { _ = tx.Rollback() })
	return &blocker{store: other, tx: tx}
}

// releaseAfter rolls the blocking transaction back once holdFor has passed. The
// timer is the blocker's own, started before the write under test begins, so the
// two never race each other's scheduling.
func (b *blocker) releaseAfter(holdFor time.Duration) {
	time.AfterFunc(holdFor, func() { _ = b.tx.Rollback() })
}

// contentionHoldFor is how long the blocker holds the write lock in the two
// recovery tests: longer than ONE busy budget, so the first attempt cannot get in
// at all, and shorter than two, so the second can. That window is the whole
// case — a save whose budget ran out and whose next attempt finds the lock going
// free — and a fixture outside it would be testing something else.
func contentionHoldFor(busyMS int) time.Duration {
	return time.Duration(busyMS+90) * time.Millisecond
}

// TestUpsertRetriesBeginOnceWhenTheLockOutlastsTheFirstBudget is the loss #671
// reports, made deterministic: the write lock is held for longer than one
// busy_timeout, which is exactly the state a save that ran out of budget was in.
//
// Without the retry this save fails with SQLITE_BUSY and the memory is lost. The
// content is a fact nothing else in the store states, so there is no fold that
// could produce the row another way — "the save reported success and stored
// nothing" is the defect, and the row is the proof it did not happen.
func TestUpsertRetriesBeginOnceWhenTheLockOutlastsTheFirstBudget(t *testing.T) {
	const busyMS = 150
	dbPath := t.TempDir() + "/contention.sqlite"
	newBlocker(t, dbPath).releaseAfter(contentionHoldFor(busyMS))
	store := shortTimeoutStore(t, dbPath, busyMS)
	obs := &recordingObserver{}
	obs.install(t)

	start := time.Now()
	id, _, _, err := store.Upsert(context.Background(), testProject, "fact",
		"the write lock outlasting one busy budget must not lose a save", "mcp", 0.5, nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Upsert lost to lock contention after %s: %v — one bounded retry should have taken the lock",
			elapsed, err)
	}
	if elapsed < time.Duration(busyMS)*time.Millisecond {
		t.Errorf("the save returned in %s, so it never met the %d ms the blocker holds the write lock: "+
			"the contention under test never happened", elapsed, busyMS)
	}

	// The row is the outcome that matters: a save that reported success and
	// stored nothing is the silent loss the contract forbids.
	var stored int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memories WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("read back the saved row: %v", err)
	}
	if stored != 1 {
		t.Errorf("the save reported id %s but %d rows carry it", id, stored)
	}

	// The retry is asserted, not assumed: a save that simply got lucky on its
	// first attempt would leave the same row, and the difference between the
	// two is the whole fix.
	samples := obs.all()
	if len(samples) != 1 {
		t.Fatalf("the store reported %d committed write transactions, want 1: %+v", len(samples), samples)
	}
	if !samples[0].Retried {
		t.Errorf("the save is reported as a first-attempt success, so it never needed the retry: %+v", samples[0])
	}
	// Wait is the whole time the caller was kept out, both attempts added
	// together. A wait that reported only the successful attempt's would make
	// the fleet's distribution read as if contention were shorter than it is,
	// which is the number this seam exists to report. The floor is the hold plus
	// half a budget: the blocker held for busy+90ms and the first attempt gave
	// up at busy, so an accumulated wait cannot be shorter than that, and one
	// that reported only the retry's would be under 90ms.
	if min := time.Duration(busyMS+busyMS/2) * time.Millisecond; samples[0].Wait < min {
		t.Errorf("the reported wait is %s, under the %s the blocker held the lock for: the wait is not "+
			"accumulated across attempts", samples[0].Wait, min)
	}
}

// TestUpsertGivesUpAfterTheBoundedRetry pins the other half: ONE extra attempt,
// not a loop. A save that keeps asking can never return, and an MCP tool call
// that never returns is worse than one that reports why it could not.
//
// The save runs under a context far longer than two budgets and the assertion is
// that it is back well inside it, so an implementation that looped until the
// context died fails here rather than hanging the suite.
func TestUpsertGivesUpAfterTheBoundedRetry(t *testing.T) {
	const busyMS = 150
	dbPath := t.TempDir() + "/held.sqlite"
	// Held until the test ends: the save has to give up against a lock that is
	// never coming.
	newBlocker(t, dbPath)
	store := shortTimeoutStore(t, dbPath, busyMS)
	obs := &recordingObserver{}
	obs.install(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	_, _, _, err := store.Upsert(ctx, testProject, "fact",
		"a write lock nobody releases must be reported, not waited on", "mcp", 0.5, nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Upsert reported success while another handle held the write lock for the whole run")
	}
	if !isLockContention(err) {
		t.Errorf("Upsert failed with %v, which is not lock contention: the bounded retry must not turn a "+
			"lock refusal into something else", err)
	}
	// Two budgets, with room for a slow machine and none for a loop. A retry
	// that kept going would sit on this call until the 30 s context, which is
	// four times this bound, so the bound is on attempts and not on patience.
	if limit := 4 * time.Duration(busyMS) * time.Millisecond; elapsed > limit {
		t.Errorf("Upsert took %s to report a lock it could not take, over the %s two %d ms budgets allow: "+
			"the retry is not bounded to one attempt", elapsed, limit, busyMS)
	}

	// The lost write is REPORTED, and that is the half of the measurement that
	// has no hold time: a distribution over committed transactions only would
	// describe the contention the fleet survived. This is the sample #671 was
	// missing, and it is the one an operator reads.
	samples := obs.all()
	if len(samples) != 1 {
		t.Fatalf("the store reported %d transactions, want 1 for the one that never took the lock: %+v",
			len(samples), samples)
	}
	got := samples[0]
	if !got.Lost || got.Hold != 0 {
		t.Errorf("the sample is %+v, want one marked lost with no hold time: a BEGIN that never took the "+
			"write lock has none, and reporting it as a hold would understate the contention by everything "+
			"it waited", got)
	}
	if !got.Retried {
		t.Errorf("the lost write is reported as a first-attempt loss, so the retry is not in the sample: %+v", got)
	}
	if min := 2 * time.Duration(busyMS) * time.Millisecond; got.Wait < min {
		t.Errorf("the reported wait is %s, under the %s two budgets it spent: the wait is not accumulated "+
			"across attempts", got.Wait, min)
	}
}

// TestUpdateMemoryRetriesBeginOnceWhenTheLockOutlastsTheFirstBudget covers the
// third knowledge write, and it is here because the measurement named it rather
// than because the issue did: under saturation the fleet lost an EDIT and not a
// save, which is the direction a save-only rule gets wrong. An edit is as much a
// memory as a save is, and `ghost_memory_edit` is a live tool.
func TestUpdateMemoryRetriesBeginOnceWhenTheLockOutlastsTheFirstBudget(t *testing.T) {
	const busyMS = 150
	dbPath := t.TempDir() + "/edit.sqlite"
	// The database and its project are created through OpenDB first, as every
	// real store is, so the handle under test opens an existing schema.
	upsertLockTestStore(t, dbPath)
	saving := shortTimeoutStore(t, dbPath, busyMS)
	// Installed before the seed, so the sample this test reads is the edit's
	// and not a neighbour's: a second sample is not an error here, it is a
	// sample of a different write.
	obs := &recordingObserver{}
	obs.install(t)
	id, _, _, err := saving.Upsert(context.Background(), testProject, "fact",
		"a memory an agent will edit under contention", "mcp", 0.5, nil)
	if err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}
	newBlocker(t, dbPath).releaseAfter(contentionHoldFor(busyMS))

	edited := "a memory an agent edited while another writer held the write lock"
	if err := saving.UpdateMemory(context.Background(), testProject, id, &edited, nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory lost to lock contention: %v — one bounded retry should have taken the lock", err)
	}

	samples := obs.all()
	if len(samples) != 2 {
		t.Fatalf("the store reported %d committed write transactions, want 2 (the seed and the edit): %+v",
			len(samples), samples)
	}
	got := samples[len(samples)-1]
	if got.Op != "update" {
		t.Errorf("the measured transaction is %q, want update", got.Op)
	}
	if !got.Retried {
		t.Errorf("the edit is reported as a first-attempt success, so it never needed the retry: %+v", got)
	}
	var stored string
	if err := saving.db.QueryRowContext(context.Background(),
		`SELECT content FROM memories WHERE id = ?`, id).Scan(&stored); err != nil {
		t.Fatalf("read back the edited row: %v", err)
	}
	if stored != edited {
		t.Errorf("the edit reported success and the row holds %q, want %q", stored, edited)
	}
}

// TestRecordDecisionRetriesBeginOnceWhenTheLockOutlastsTheFirstBudget covers the
// fourth knowledge write, and the review of this PR is what named it: the scope
// sentence claimed the excluded `BeginTx` sites wrote projects, links, history
// and imports, and `RecordDecision` writes none of those — it writes a decision
// row and a COMPANION MEMORY, through the live `ghost_decision_record` tool. A
// lost decision is a lost memory, so it retries; and the test is here because
// nothing else reaches this path under contention, the fleet having no decision
// writer of its own.
func TestRecordDecisionRetriesBeginOnceWhenTheLockOutlastsTheFirstBudget(t *testing.T) {
	const busyMS = 150
	dbPath := t.TempDir() + "/decision.sqlite"
	upsertLockTestStore(t, dbPath)
	store := shortTimeoutStore(t, dbPath, busyMS)
	newBlocker(t, dbPath).releaseAfter(contentionHoldFor(busyMS))
	obs := &recordingObserver{}
	obs.install(t)

	decisionID, memoryID, _, err := store.RecordDecision(context.Background(), testProject,
		"the write lock outlasting one busy budget",
		"a decision record is a memory and retries BEGIN like one",
		"a decision whose companion row was lost is a memory nobody wrote", nil, nil)
	if err != nil {
		t.Fatalf("RecordDecision lost to lock contention: %v — one bounded retry should have taken the lock", err)
	}

	samples := obs.all()
	if len(samples) != 1 {
		t.Fatalf("the store reported %d committed write transactions, want 1: %+v", len(samples), samples)
	}
	got := samples[0]
	if got.Op != "decision-record" {
		t.Errorf("the measured transaction is %q, want decision-record", got.Op)
	}
	if !got.Retried {
		t.Errorf("the decision is reported as a first-attempt success, so it never needed the retry: %+v", got)
	}
	// Both rows, because that is what "one transaction" means here: a decision
	// whose companion memory did not land reports success and is a memory
	// nobody wrote.
	var rows int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT (SELECT count(*) FROM decisions WHERE id = ?) + (SELECT count(*) FROM memories WHERE id = ?)`,
		decisionID, memoryID).Scan(&rows); err != nil {
		t.Fatalf("read back the decision and its companion: %v", err)
	}
	if rows != 2 {
		t.Errorf("the decision reported success and %d of its 2 rows are present (decision %s, memory %s)",
			rows, decisionID, memoryID)
	}
}

// TestApplyReflectionRetriesBeginOnceWhenTheLockOutlastsTheFirstBudget is the
// same property for the other transaction #671's failure queued behind. A
// lifecycle apply that cannot get in is not a retried save: it is a whole
// consolidation refused, and its caller has already spent a harness call to
// produce the batch.
func TestApplyReflectionRetriesBeginOnceWhenTheLockOutlastsTheFirstBudget(t *testing.T) {
	const busyMS = 150
	dbPath := t.TempDir() + "/reflect.sqlite"
	// The database and its project are created through OpenDB first, as every
	// real store is, so the handle under test opens an existing schema.
	upsertLockTestStore(t, dbPath)
	store := shortTimeoutStore(t, dbPath, busyMS)
	// A seed row, so the apply has a corpus to replace the way a real reflect
	// pass arrives with one.
	if _, _, _, err := store.Upsert(context.Background(), testProject, "fact",
		"a note a previous reflection round left behind", "reflection", 0.5, nil); err != nil {
		t.Fatalf("seed Upsert: %v", err)
	}
	newBlocker(t, dbPath).releaseAfter(contentionHoldFor(busyMS))
	obs := &recordingObserver{}
	obs.install(t)

	batch := []Memory{{
		Category:   "fact",
		Content:    "the consolidated wording of the note above",
		Importance: 0.6,
		Tags:       []string{"consolidated"},
		Source:     "reflection",
	}}
	start := time.Now()
	if _, _, _, err := store.ApplyReflection(context.Background(), testProject, batch, nil, "", false); err != nil {
		t.Fatalf("ApplyReflection lost to lock contention after %s: %v — one bounded retry should have taken "+
			"the lock", time.Since(start), err)
	}

	samples := obs.all()
	if len(samples) != 1 {
		t.Fatalf("the store reported %d committed write transactions, want 1: %+v", len(samples), samples)
	}
	got := samples[0]
	if got.Op != "reflect-apply" {
		t.Errorf("the measured transaction is %q, want reflect-apply", got.Op)
	}
	if !got.Retried {
		t.Errorf("the apply is reported as a first-attempt success, so it never needed the retry: %+v", got)
	}
	var consolidated int
	if err := store.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memories WHERE project_id = ? AND content = ?`,
		testProject, batch[0].Content).Scan(&consolidated); err != nil {
		t.Fatalf("read back the consolidated row: %v", err)
	}
	if consolidated != 1 {
		t.Errorf("the apply reported success and %d rows carry the consolidated wording, want 1", consolidated)
	}
}

// writeLockHoldCeiling is the longest a single memory write may hold SQLite's
// write lock. A quarter of the busy timeout the contract gives a writer would be
// 1250 ms, and it is deliberately not that: at 1250 ms the ceiling is 900 times
// the hold this measures, so it would only ever catch a catastrophe and would
// say nothing about a transaction that merely grew. 250 ms is the number that
// does both jobs. It is above the worst hold the multi-process fleet produced
// under deliberate three-way oversubscription on one core (~200 ms), so this
// test cannot fail for a reason the fleet's own measurement says is reachable,
// and it is a twelfth of the quarter-of-budget line at which ONE save starts
// deciding whether four other writers get in at all.
//
// Measured on this laptop: a save's hold p50 0.9 ms, max 1.4 ms over 20
// transactions against a 60-row project — a 180x margin. The test asserts the
// worst save of the CHEAPEST of four rounds rather than the worst save overall,
// because a hold is wall-clock time and includes every millisecond the thread was
// descheduled (#826); the statistic, not the number, is what excludes a runner's
// worst moment.
const writeLockHoldCeiling = 250 * time.Millisecond

// writeLockHoldRounds is how many independent rounds of twenty saves the ceiling
// is asserted over, and the assertion is on the CHEAPEST round's worst save. See
// the test for why one round is not enough.
const writeLockHoldRounds = 4

// writeLockHoldSaves is the saves in one round.
const writeLockHoldSaves = 20

// TestWriteLockHoldLeavesRoomInsideTheBusyTimeout is the ceiling that guards a
// long write transaction, measured where the machine is quiet so the number it
// compares against is stable: the probes, the strengthen, the history append and
// the evidence append of one save, over a project big enough that the dedup probe
// has candidates to score.
//
// It is here and not in the multi-process test, and that placement is the point.
// A hold is wall-clock time for a transaction that does real work, so it scales
// with how loaded the machine is: the same fleet measured a save's median hold at
// 1.3 ms on an idle laptop and 82 ms with three copies of itself on one core, and
// the 60-row batch at 21-600 ms against 0.5-2.0 s. A gate over those numbers would
// be a verdict that depends on the runner — the dependence issue #671 asked to
// remove from that test — so the fleet reports its distributions and this
// asserts.
//
// Twenty saves per round, four rounds, and the assertion is on the CHEAPEST
// round's worst save — not on the worst save of one round (#826).
//
// The reason is what a hold actually is. It is wall-clock time between BEGIN
// IMMEDIATE taking the lock and the commit, so it contains every millisecond the
// thread was descheduled, and a loaded runner hands out those in bursts: PR #818's
// run measured a median of 33 ms and one save of twenty at 264 ms, which is 15 ms
// over this ceiling and 15 ms of nothing a save did. A single round's maximum is
// therefore not a measurement of the save at all — it is a measurement of the
// runner's worst moment in twenty samples — and no ceiling placed above the real
// cost can exclude it.
//
// What separates the two cases is HOW MANY samples are inflated, and the
// statistic does that with the minimum across rounds:
//
//   - a scheduler stall inflates the round it happened in. Three clean rounds out
//     of four keep the assertion, which is what a gate should do with a reading
//     about the machine rather than the code.
//   - a write transaction that grew inflates EVERY round, because the growth is
//     in the work inside the transaction. The cheapest round is then over the
//     ceiling too, and the test fails.
//
// The same asymmetry is why the instrument stays WALL CLOCK rather than CPU time,
// which is what the cost tests in internal/secret switched to for a different
// reason (#815). A hold is precisely what another writer waits through, so a stall
// genuinely does extend it for a real writer; and an injected regression is
// typically a sleep or a slow statement, which CPU time does not see at all. So
// the noise is removed by the statistic and the measurement stays the honest one.
//
// Each round gets its own project, so every round faces the same 60-row corpus
// and the twenty saves cost the same in all four. Rounds sharing a project would
// make each one more expensive than the last, and the cheapest round would always
// be the first, which would quietly bias the assertion towards passing.
func TestWriteLockHoldLeavesRoomInsideTheBusyTimeout(t *testing.T) {
	dbPath := t.TempDir() + "/hold.sqlite"
	upsertLockTestStore(t, dbPath)
	store := shortTimeoutStore(t, dbPath, 5000)
	obs := &recordingObserver{}
	obs.install(t)
	ctx := context.Background()

	roundMax := make([]time.Duration, writeLockHoldRounds)
	var every []time.Duration
	for round := range writeLockHoldRounds {
		project := fmt.Sprintf("hold-ceiling-round-%d", round)
		if err := store.EnsureProject(ctx, project, t.TempDir(), project); err != nil {
			t.Fatalf("EnsureProject %s: %v", project, err)
		}
		// A corpus for the probe to work against. These are near-duplicates of one
		// another by construction, which is the expensive case: the probe scores
		// every candidate the FTS match returns.
		for i := 0; i < 60; i++ {
			if _, _, _, err := store.Upsert(ctx, project, "fact",
				fmt.Sprintf("near-duplicate corpus row %03d about sqlite wal checkpointing and busy timeouts", i),
				"mcp", 0.5, nil); err != nil {
				t.Fatalf("round %d seed Upsert %d: %v", round, i, err)
			}
		}
		before := len(obs.all())

		for i := 0; i < writeLockHoldSaves; i++ {
			if _, _, _, err := store.Upsert(ctx, project, "fact",
				fmt.Sprintf("measured save %03d about sqlite wal checkpointing and busy timeouts", i),
				"mcp", 0.5, nil); err != nil {
				t.Fatalf("round %d measured Upsert %d: %v", round, i, err)
			}
		}

		holds := holdsSince(obs.all(), before)
		if len(holds) != writeLockHoldSaves {
			t.Fatalf("round %d measured %d of the %d saves, so the ceiling would be asserted over the wrong set: %+v",
				round, len(holds), writeLockHoldSaves, obs.all()[before:])
		}
		roundMax[round] = holds[len(holds)-1]
		every = append(every, holds...)
	}

	sorted := append([]time.Duration(nil), every...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	// The count over the ceiling, and NOT a p99: at 80 samples a p99 index lands
	// on the maximum, so the two labels would print the same number and a reader
	// would take the second for evidence of a distribution the sample cannot
	// resolve. What a reader needs when this fails is how MANY saves were over,
	// because that is what separates a runner that was busy for a moment from a
	// transaction that grew.
	over := 0
	for _, hold := range sorted {
		if hold > writeLockHoldCeiling {
			over++
		}
	}
	t.Logf("save write-lock hold over 60 rows, %d rounds of %d saves: p50=%s max=%s, %d of %d over the %s ceiling",
		writeLockHoldRounds, writeLockHoldSaves, sorted[len(sorted)/2], sorted[len(sorted)-1],
		over, len(sorted), writeLockHoldCeiling)
	t.Logf("worst save per round: %s", roundMaxString(roundMax))

	cheapest := 0
	for i, m := range roundMax {
		if m < roundMax[cheapest] {
			cheapest = i
		}
	}
	if roundMax[cheapest] > writeLockHoldCeiling {
		t.Errorf("the cheapest of %d rounds still held the write lock for %s on its worst save, over the %s ceiling: "+
			"a write transaction that long is what turns a queue into a lost memory, and a transaction that grew "+
			"shows up in every round rather than only the one the runner was busy for (worst save per round: %s)",
			writeLockHoldRounds, roundMax[cheapest], writeLockHoldCeiling, roundMaxString(roundMax))
	}
}

// holdsSince returns the hold times of the samples after index before, ascending.
func holdsSince(samples []WriteLockSample, before int) []time.Duration {
	holds := make([]time.Duration, 0, len(samples)-before)
	for _, s := range samples[before:] {
		holds = append(holds, s.Hold)
	}
	sort.Slice(holds, func(i, j int) bool { return holds[i] < holds[j] })
	return holds
}

// roundMaxString renders one duration per round for a failure message.
func roundMaxString(maxes []time.Duration) string {
	parts := make([]string, len(maxes))
	for i, m := range maxes {
		parts[i] = m.String()
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// TestWriteLockContentionIsClassifiedInBothSpellings holds the retry to what it
// may retry. A BEGIN can fail for reasons a second attempt does not fix — a
// closed handle, a cancelled context, a constraint failure — and retrying those
// would turn an immediate error into a slow one, so the classifier has to be
// narrower than "the database said no".
//
// The typed case is not hand-built. A *sqlite.Error cannot be constructed with a
// code outside the driver, so the contended write below produces a real one and
// the test asserts the code it carries is the one the classifier reduces — which
// is the only way to know the driver spells it the way this file assumes.
//
// Named with the subject first so `-run '^Test[^L]'` — the filter that keeps the
// billable live suites off a laptop — does not read "TestL" and skip it.
func TestWriteLockContentionIsClassifiedInBothSpellings(t *testing.T) {
	dbPath := t.TempDir() + "/classify.sqlite"
	// Held until the test ends, so the refusal below is contention and not a
	// write against a database nobody is using.
	newBlocker(t, dbPath)
	// The contender carries its own short timeout so the refusal arrives as an
	// error rather than as a wait.
	contender, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(50)")
	if err != nil {
		t.Fatalf("open contender: %v", err)
	}
	defer contender.Close() //nolint:errcheck
	contender.SetMaxOpenConns(1)
	_, err = contender.ExecContext(context.Background(), `UPDATE projects SET id = id WHERE id = ?`, testProject)
	if err == nil {
		t.Fatal("the contender wrote while another handle held the write lock")
	}
	var typed *sqlite.Error
	if !errors.As(err, &typed) {
		t.Fatalf("the refusal is not a driver error this package can classify: %v", err)
	}
	if code := typed.Code() & 0xff; code != sqlite3.SQLITE_BUSY {
		t.Fatalf("the refusal carries code %d, not SQLITE_BUSY: this file's assumption about how the driver "+
			"reports contention is wrong", code)
	}
	if !isLockContention(err) {
		t.Errorf("isLockContention did not recognise a typed SQLITE_BUSY from the driver: %v", err)
	}
	if !isLockContention(errWrap("begin upsert tx: database is locked (5) (SQLITE_BUSY)")) {
		t.Error("isLockContention did not recognise a lock refusal that reached it as text, which is the shape " +
			"every wrapped store error has")
	}

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"a closed handle", errWrap("sql: database is closed")},
		{"a cancelled context", context.Canceled},
		{"a constraint failure", errWrap("UNIQUE constraint failed: memories.id")},
		{"an unrelated failure", errWrap("disk I/O error")},
	} {
		if isLockContention(tc.err) {
			t.Errorf("isLockContention accepted %s, so a caller would retry something a second attempt cannot fix", tc.name)
		}
	}
}

// TestUpsertReportsAClosedHandleWithoutRetrying is the no-retry-for-other-errors
// half, observed rather than argued: a closed handle comes back as itself, and
// nothing is written. A retry that swallowed the classification would report
// contention here, and the caller would be told to try again for a database that
// is not there.
func TestUpsertReportsAClosedHandleWithoutRetrying(t *testing.T) {
	db, err := OpenDB(t.TempDir() + "/closed.sqlite")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close handle: %v", err)
	}
	obs := &recordingObserver{}
	obs.install(t)
	closed := NewStore(db, nil)

	_, _, _, err = closed.Upsert(context.Background(), testProject, "fact",
		"a save on a handle that is gone", "mcp", 0.5, nil)
	if err == nil {
		t.Fatal("Upsert reported success on a closed handle")
	}
	if isLockContention(err) {
		t.Errorf("Upsert reported %v as lock contention, so a caller would retry it: a closed handle is not "+
			"something a second attempt fixes", err)
	}
	if !strings.Contains(err.Error(), "begin upsert tx") {
		t.Errorf("Upsert failed with %v, which does not name the transaction it could not open", err)
	}
	if samples := obs.all(); len(samples) != 0 {
		t.Errorf("a BEGIN that never opened a transaction reported %d samples, so a failure a second "+
			"attempt cannot fix is being reported as contention: %+v", len(samples), samples)
	}
}

// errWrap is an error with a message and no typed error behind it, standing in
// for the fmt.Errorf wrapping every database error passes through on its way out
// of the store — the case where errors.As cannot see the driver's own code.
type wrapError struct{ msg string }

func (e *wrapError) Error() string { return e.msg }

func errWrap(msg string) error { return &wrapError{msg: msg} }
