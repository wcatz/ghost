package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// pruneFixture writes one row of the given tier and ages it: an expiry and a
// recorded last access in the past, which is the shape prune prefers to see.
//
// lastAccessed may be empty, which leaves last_accessed NULL — a row nothing has
// recorded a read for. The grace then falls back to the row's last WRITE, so a
// fixture that wants such a row to be prunable has to age updated_at too
// (backdateWrite); leaving the write at the save instant is the ordinary case
// and the row is inside the grace.
func pruneFixture(t *testing.T, s *Store, content, tier, expires, lastAccessed string) string {
	t.Helper()
	ctx := context.Background()
	id, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact", content, "mcp", 0.6, nil, UpsertOptions{Retention: tier})
	if err != nil {
		t.Fatalf("UpsertWithOptions(%s): %v", tier, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE memories SET expires_at = ?, last_accessed = ? WHERE id = ?`,
		nullIfEmpty(expires), nullIfEmpty(lastAccessed), id); err != nil {
		t.Fatalf("age %s: %v", id, err)
	}
	return id
}

// backdateWrite ages a row's last write, which is what the grace falls back to
// when no read has ever been recorded for it.
// seedPruneRow writes one prune-shaped row straight into the store, skipping
// the upsert dedup probe. Batch tests seed thousands of rows, and each probe is
// an FTS ranking pass over every same-category row already present, so seeding
// through UpsertWithOptions is quadratic in the backlog — and the race
// detector's per-step instrumentation turns that quadratic into tens of
// minutes, which is how this package misses its own ten-minute test deadline.
// The probe is a dedup concern; the assertions that follow are prune-batching
// concerns, so the direct INSERT keeps the same row shape (the memories→fts
// triggers still index the content) without paying the probe.
func seedPruneRow(t *testing.T, s *Store, content, tier, expires, lastAccessed string) string {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO memories (project_id, category, content, source, importance, tags,
		                      retention, expires_at, last_accessed)
		VALUES (?, 'fact', ?, 'mcp', 0.6, '', ?, ?, ?)
		RETURNING id`,
		testProject, content, tier, nullIfEmpty(expires), nullIfEmpty(lastAccessed)).Scan(&id); err != nil {
		t.Fatalf("seed prune row %q: %v", content, err)
	}
	return id
}

// backdateWrite ages a row's last write, which is what the grace falls back to
// when no read has ever been recorded for it.
func backdateWrite(t *testing.T, s *Store, id, when string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE memories SET updated_at = ?, created_at = ? WHERE id = ?`, when, when, id); err != nil {
		t.Fatalf("backdate the write on %s: %v", id, err)
	}
}

// stamp renders a time the way every timestamp column here stores one.
func stamp(offset time.Duration) string {
	return time.Now().UTC().Add(offset).Format("2006-01-02 15:04:05")
}

func liveCount(t *testing.T, s *Store, projectID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM memories WHERE project_id = ?`, projectID).Scan(&n); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	return n
}

func historyPhases(t *testing.T, s *Store, id string) []string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT phase FROM memory_history WHERE memory_id = ? ORDER BY rowid`, id)
	if err != nil {
		t.Fatalf("read history for %s: %v", id, err)
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var phase string
		if err := rows.Scan(&phase); err != nil {
			t.Fatalf("scan history: %v", err)
		}
		out = append(out, phase)
	}
	return out
}

// TestPruneRemovesOnlyExpiredSessionRowsPastTheGrace: the predicate is the whole
// safety argument, and it has five parts that each have to hold. A durable row is
// never removed however old it is, and neither is a pinned row. A session row is
// not removed before its expiry. A session row whose last activity is inside the
// grace period is not removed even after it expires. And a recorded last access
// is preferred over the row's last write, because a memory somebody is still
// reading is not garbage. TestPruneNeverRemovesAPinnedRow and
// TestPruneRemovesOnlyExpiredSessionRowsPastTheGrace sit side by side because a
// predicate that dangled either clause would look identical to a caller watching
// the other one hold.
func TestPruneRemovesOnlyExpiredSessionRowsPastTheGrace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	longExpired := pruneFixture(t, s, "a session note expired long ago and untouched since", RetentionSession,
		stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	notExpired := pruneFixture(t, s, "a session note that has not reached its expiry", RetentionSession,
		stamp(time.Hour), stamp(-29*24*time.Hour))
	insideGrace := pruneFixture(t, s, "a session note expired but read yesterday", RetentionSession,
		stamp(-30*24*time.Hour), stamp(-24*time.Hour))
	// No recorded access: the grace falls back to the row's last write, so this
	// row has to be old in both columns to be a candidate.
	neverAccessed := pruneFixture(t, s, "a session note expired and never read", RetentionSession,
		stamp(-30*24*time.Hour), "")
	backdateWrite(t, s, neverAccessed, stamp(-30*24*time.Hour))
	// And the row that is expired, never read, and last written a minute ago:
	// inside the grace, so kept. This is the default shape of a real session
	// memory, and it is the case a prune that measured only the expiry would
	// delete.
	freshWrite := pruneFixture(t, s, "a session note expired seconds after it was saved", RetentionSession,
		stamp(-time.Hour), "")
	durable := pruneFixture(t, s, "a durable fact that is just as old", RetentionProject,
		stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	keepForever := pruneFixture(t, s, "a keep-forever fact with a stale expiry", RetentionPersistent,
		stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))

	report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	if report.Removed != 2 {
		t.Errorf("removed %d rows, want 2: the expired, untouched session rows", report.Removed)
	}
	gone := map[string]bool{}
	for _, id := range report.RemovedIDs {
		gone[id] = true
	}
	if !gone[longExpired] {
		t.Error("the expired, untouched session row survived")
	}
	if !gone[neverAccessed] {
		t.Error("the expired session row nobody ever read survived")
	}
	if gone[freshWrite] {
		t.Error("prune removed a row written moments ago: the grace was not measured from anything")
	}
	for _, tc := range []struct{ id, why string }{
		{notExpired, "it has not reached its expiry"},
		{insideGrace, "it was read inside the grace period"},
		{durable, "a project row is not a prune candidate however old it is"},
		{keepForever, "a persistent row is exempt from pruning"},
	} {
		if _, err := s.GetByIDs(ctx, []string{tc.id}); err != nil {
			t.Fatalf("read %s: %v", tc.id, err)
		} else if gone[tc.id] {
			t.Errorf("prune removed %s, but %s", tc.id, tc.why)
		}
	}
}

// TestPrunePinsTheGraceBoundaryFromBothSides: "past the grace period" is a
// threshold, and a threshold nobody can test from both sides is a threshold that
// is either off by a rounding error or off by a factor of grace. The two rows
// here differ by a minute and by nothing else.
func TestPrunePinsTheGraceBoundaryFromBothSides(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const grace = 24 * time.Hour

	// Both expired a month ago, so only the grace decides.
	justOutside := pruneFixture(t, s, "a session note last touched a minute past the grace", RetentionSession,
		stamp(-30*24*time.Hour), stamp(-grace-time.Minute))
	justInside := pruneFixture(t, s, "a session note last touched a minute inside the grace", RetentionSession,
		stamp(-30*24*time.Hour), stamp(-grace+time.Minute))

	report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true, Grace: grace})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	if len(report.RemovedIDs) != 1 || report.RemovedIDs[0] != justOutside {
		t.Fatalf("removed %v, want exactly [%s] (the row just outside the grace)", report.RemovedIDs, justOutside)
	}
	if _, err := s.GetByIDs(ctx, []string{justInside}); err != nil || liveCount(t, s, testProject) != 1 {
		t.Errorf("the row just inside the grace was removed")
	}
}

// TestPrunePinsTheExpiryBoundaryToo: the grace is the outer gate, but the tier's
// own expiry is the inner one, and a prune that only implemented the outer gate
// would delete a session note the user saved a moment ago.
func TestPrunePinsTheExpiryBoundaryToo(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	expired := pruneFixture(t, s, "a session note one minute past its expiry", RetentionSession,
		stamp(-time.Minute), stamp(-30*24*time.Hour))
	live := pruneFixture(t, s, "a session note one minute short of its expiry", RetentionSession,
		stamp(time.Minute), stamp(-30*24*time.Hour))

	report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true, Grace: 0})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	if len(report.RemovedIDs) != 1 || report.RemovedIDs[0] != expired {
		t.Fatalf("removed %v, want exactly [%s]", report.RemovedIDs, expired)
	}
	if liveCount(t, s, testProject) != 1 {
		t.Errorf("%d rows left, want the unexpired session note", liveCount(t, s, testProject))
	}
	_ = live
}

// TestPruneDefaultsToADryRunThatWritesNothing: the default is a report, and the
// reason it has to be a report is that a prune deletes. Nothing may be written on
// the preview path — not the row, and not the history row the apply appends —
// because a preview that left a tombstone behind would make the store's own audit
// claim a removal that did not happen.
func TestPruneDefaultsToADryRunThatWritesNothing(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := pruneFixture(t, s, "a session note an operator is only looking at", RetentionSession,
		stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))

	before := liveCount(t, s, testProject)
	beforeHistory := len(historyPhases(t, s, id))

	report, err := s.PruneSessionMemories(ctx, PruneOptions{})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	if report.Applied {
		t.Error("the report claims it applied the prune")
	}
	if report.Removed != 0 {
		t.Errorf("a dry run reported %d removed, want 0 — it removed nothing", report.Removed)
	}
	if len(report.Candidates) != 1 || report.Candidates[0].ID != id {
		t.Fatalf("a dry run must still NAME what it would remove: %+v", report.Candidates)
	}
	if report.Candidates[0].Content != "a session note an operator is only looking at" {
		t.Errorf("the candidate does not carry the text: %+v", report.Candidates[0])
	}
	if report.Candidates[0].Retention != RetentionSession {
		t.Errorf("candidate retention = %q, want %q", report.Candidates[0].Retention, RetentionSession)
	}
	if got := liveCount(t, s, testProject); got != before {
		t.Errorf("a dry run changed the row count from %d to %d", before, got)
	}
	if got := len(historyPhases(t, s, id)); got != beforeHistory {
		t.Errorf("a dry run appended %d history row(s)", got-beforeHistory)
	}

	// And the same run with Apply does remove it, so the dry run was a preview of
	// this pass rather than of nothing at all.
	applied, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true})
	if err != nil {
		t.Fatalf("PruneSessionMemories(apply): %v", err)
	}
	if applied.Removed != 1 {
		t.Fatalf("the apply removed %d rows, want 1", applied.Removed)
	}
}

// TestPruneLeavesADeleteTombstonePerRemoval: memory_history.memory_id has no
// foreign key precisely so a hard delete leaves a record of the row — and that
// record is the only thing that will still know the text of a memory that a
// prune removed. A prune that deleted without appending would make every
// removal invisible to `ghost history`, which is the one command that can say
// what was lost.
func TestPruneLeavesADeleteTombstonePerRemoval(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first := pruneFixture(t, s, "the first session note a prune will remove", RetentionSession,
		stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	second := pruneFixture(t, s, "the second session note a prune will remove", RetentionSession,
		stamp(-31*24*time.Hour), stamp(-30*24*time.Hour))

	report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	if report.Removed != 2 {
		t.Fatalf("removed %d, want 2", report.Removed)
	}

	for _, id := range []string{first, second} {
		phases := historyPhases(t, s, id)
		if len(phases) == 0 {
			t.Fatalf("%s left no history at all: a prune that erases the record of a removal is undebuggable", id)
		}
		if last := phases[len(phases)-1]; last != phaseDelete {
			t.Errorf("%s last history phase = %q, want %q", id, last, phaseDelete)
		}
		// The tombstone carries the text, which is the point of it surviving the
		// row.
		entries, err := s.MemoryHistory(ctx, id, 10)
		if err != nil {
			t.Fatalf("MemoryHistory(%s): %v", id, err)
		}
		if len(entries) == 0 || entries[len(entries)-1].Content == "" {
			t.Errorf("%s tombstone carries no text: %+v", id, entries)
		}
		if _, err := s.GetByIDs(ctx, []string{id}); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
	}
}

// TestPruneNeverRemovesAPinnedRow is one of the review blockers on #587: prune
// reuses a single predicate for its SELECT and its DELETE, and the predicate
// carried no `pinned = 0`, so a pinned session row whose expiry and activity
// were in the past was removed like any other expired session row. A pin is an
// explicit user override — the same "keep this where it is" the tier design is
// built on — and the session tier is a promise made for rows the user did NOT
// override, so the DELETE must re-check the pin at the same instant it deletes,
// exactly as it re-checks the tier. The unpinned control is in the same test:
// without it, a predicate that spared everything would pass.
func TestPruneNeverRemovesAPinnedRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pinned := pruneFixture(t, s, "a pinned session note that expired long ago", RetentionSession,
		stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	control := pruneFixture(t, s, "an unpinned session note that expired long ago", RetentionSession,
		stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET pinned = 1 WHERE id = ?`, pinned); err != nil {
		t.Fatalf("pin %s: %v", pinned, err)
	}

	report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if report.Removed != 1 {
		t.Errorf("prune removed %d row(s), want exactly the 1 unpinned control", report.Removed)
	}
	if len(report.RemovedIDs) != 1 || report.RemovedIDs[0] != control {
		t.Errorf("removed %v, want only the unpinned control %s", report.RemovedIDs, control)
	}
	if liveCount(t, s, testProject) != 1 {
		t.Errorf("after the prune the project holds %d row(s), want 1 (the pinned one): a pinned session row was removed", liveCount(t, s, testProject))
	}
}

// TestPruneSparesARowThatStoppedMatchingMidRun: Candidates is the PREVIEW's
// selection, and every batch re-derives the predicate under the write lock — so a
// row a write changed between the preview and its own batch stops matching, is
// correctly spared, and is STILL in Candidates while absent from RemovedIDs. That
// asymmetry is the whole reporting contract on the apply path: `ghost prune`
// counted the candidates, so an apply that removed 3049 of 3050 headlined 3050
// removals with the spared row live in the store.
//
// Placing the write is the whole difficulty, and the shape here is the one that
// works. pruneBeforeDelete cannot do it: that seam runs inside the batch
// transaction, which holds the write lock (BEGIN IMMEDIATE) AND has already
// selected the batch's ids, so a second connection's write blocks on the lock and
// an in-transaction write arrives too late to spare anything. So the write is a
// trigger on the tombstone insert — the one write a batch makes before its DELETE.
// It fires inside that transaction and therefore COMMITS with the batch, which is
// exactly a write that landed after the preview and before the next batch
// re-derived the predicate, and it needs no second connection and no timing.
//
// The row it touches has to be the one the first batch cannot reach, so it is
// seeded a day newer and sorts last in the removal order. Two directions, because
// "spared" is not one fact and the rendered line has to hold for both: a pin
// leaves the row in the store, a concurrent delete leaves nothing to spare.
func TestPruneSparesARowThatStoppedMatchingMidRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		// change is the trigger body, and its WHEN clause is what keeps it to one
		// firing: the tombstone statement inserts one history row per batched id,
		// so the trigger runs pruneBatchSize times and only the first may act.
		change string
		live   bool
	}{
		{
			name: "pinned between batches",
			change: `WHEN NEW.phase = 'delete' AND NOT EXISTS (SELECT 1 FROM memories WHERE id = '%[1]s' AND pinned = 1)
				BEGIN
					UPDATE memories SET pinned = 1 WHERE id = '%[1]s';
				END`,
			live: true,
		},
		{
			name: "removed by a write in the same window",
			change: `WHEN NEW.phase = 'delete' AND EXISTS (SELECT 1 FROM memories WHERE id = '%[1]s')
				BEGIN
					DELETE FROM memories WHERE id = '%[1]s';
				END`,
			live: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			const total = pruneBatchSize + 1 // two batches
			for i := 0; i < total-1; i++ {
				seedPruneRow(t, s, fmt.Sprintf("an expired session note %d", i), RetentionSession,
					stamp(-30*24*time.Hour), stamp(-30*24*time.Hour))
			}
			// Newer activity, so it sorts last in the removal order and the first
			// batch cannot reach it.
			spared := seedPruneRow(t, s, "the row a write reaches between batches", RetentionSession,
				stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
			// The id is store-minted hex, so inlining it into the trigger cannot
			// carry a quote out of the literal.
			installPruneSpareTrigger(t, s, fmt.Sprintf(tc.change, spared))

			report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true})
			if err != nil {
				t.Fatalf("prune: %v", err)
			}
			if len(report.Candidates) != total {
				t.Errorf("the report selected %d row(s), want %d: Candidates is the preview's selection whatever the batches took", len(report.Candidates), total)
			}
			if report.Removed != total-1 || len(report.RemovedIDs) != total-1 {
				t.Errorf("the apply removed %d row(s) (%d ids), want %d of each: a row that stopped matching is not a removal", report.Removed, len(report.RemovedIDs), total-1)
			}
			for _, id := range report.RemovedIDs {
				if id == spared {
					t.Errorf("the spared row %s is in RemovedIDs", spared)
				}
			}
			// The pin leaves the row behind and the delete leaves nothing, which is
			// why the rendered line names the change rather than claiming the row is
			// still there. One row survived the first batch in both directions.
			want := 0
			if tc.live {
				want = 1
			}
			if got := liveCount(t, s, testProject); got != want {
				t.Errorf("the project holds %d row(s) after the apply, want %d", got, want)
			}
			if tc.live && liveID(t, s, testProject) != spared {
				t.Errorf("the surviving row is not the spared %s: the run removed a row it never selected", spared)
			}
		})
	}
}

// installPruneSpareTrigger puts a write into the window between the preview and
// the batch that would have taken a row: it fires on the first delete tombstone
// of the run, which is inside the first batch's transaction, and therefore
// commits with it.
func installPruneSpareTrigger(t *testing.T, s *Store, whenAndBody string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `CREATE TEMP TRIGGER prune_spare_mid_run AFTER INSERT ON memory_history `+whenAndBody); err != nil {
		t.Fatalf("install the mid-run write trigger: %v", err)
	}
}

// liveID returns the one live row id in a project, and fails when there is not
// exactly one — the caller here asserts what a single spared row left behind, so
// a second survivor is itself the finding.
func liveID(t *testing.T, s *Store, projectID string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM memories WHERE project_id = ?`, projectID).Scan(&id); err != nil {
		t.Fatalf("read the surviving row id: %v", err)
	}
	return id
}

// TestPruneRollsBackTheBatchThatFailed: a batch that appended its tombstones and
// then failed before the DELETE would leave the store claiming removals that did
// not happen — and the failure that matters is the ordinary one, a busy database
// or a full disk, not a crash between two statements nobody would notice. The
// seam below is the only way to make that happen on purpose, and the single
// candidate here is one batch: the failure must undo the tombstones AND the
// DELETE together, which is the guarantee TestPruneBatchesCommitPerBatch relies
// on for the boundary between two batches.
func TestPruneRollsBackTheBatchThatFailed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := pruneFixture(t, s, "a session note whose removal fails halfway", RetentionSession,
		stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	before := len(historyPhases(t, s, id))

	restore := setPruneBeforeDelete(func() error { return errPruneInterrupted })
	defer restore()

	if _, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true}); err == nil {
		t.Fatal("a prune whose delete failed reported success")
	}
	if liveCount(t, s, testProject) != 1 {
		t.Errorf("the row is gone after a failed prune: the DELETE was not rolled back with the tombstones")
	}
	if got := len(historyPhases(t, s, id)); got != before {
		t.Errorf("a failed prune left %d history row(s) behind; the append was not rolled back", got-before)
	}
}

// TestPruneBatchesCommitPerBatch: the review's bounded-batch ask, proven in the
// two directions it has. A backlog bigger than one batch is fully pruned by a
// clean apply — removed == seeded everywhere. And a failure on the SECOND batch
// leaves the FIRST batch's removals committed (rows gone, tombstones present)
// while everything after the failure stays: each batch is its own transaction,
// so the earlier progress is real, and a rerun continues from where it stopped
// rather than redoing an already-committed window. Without batching (one
// unbounded transaction), the failure would have rolled back the whole backlog
// and the first half would still be in the store.
func TestPruneBatchesCommitPerBatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const total = 3*pruneBatchSize + 50 // four batches
	for i := 0; i < total; i++ {
		seedPruneRow(t, s, fmt.Sprintf("an expired session note %d", i), RetentionSession,
			stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	}
	if liveCount(t, s, testProject) != total {
		t.Fatalf("seeded %d rows, store has %d", total, liveCount(t, s, testProject))
	}

	// Direction 1: a clean apply over a >1-batch backlog removes everything.
	report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true})
	if err != nil {
		t.Fatalf("full prune: %v", err)
	}
	if report.Removed != total || len(report.RemovedIDs) != total {
		t.Errorf("full prune removed %d row(s), %d ids tracked — want %d of each", report.Removed, len(report.RemovedIDs), total)
	}
	if liveCount(t, s, testProject) != 0 {
		t.Errorf("after the full prune %d row(s) remain, want 0", liveCount(t, s, testProject))
	}
	if got := countDeleteTombstones(t, s); got != total {
		t.Errorf("the full prune left %d delete tombstone(s), want %d — one per removal, across all batches", got, total)
	}

	// Direction 2: a failure on the second batch proves each batch commits
	// separately. Re-seed, then interrupt at the batch boundary.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM memory_history`); err != nil {
		t.Fatalf("clear history between directions: %v", err)
	}
	for i := 0; i < total; i++ {
		seedPruneRow(t, s, fmt.Sprintf("an expired session note %d again", i), RetentionSession,
			stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	}

	var seamCalls int
	restore := setPruneBeforeDelete(func() error {
		seamCalls++
		if seamCalls > 1 {
			return errPruneInterrupted
		}
		return nil
	})
	defer restore()

	if _, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true}); err == nil {
		t.Fatal("a prune interrupted on the second batch reported success")
	}
	if got := liveCount(t, s, testProject); got != total-pruneBatchSize {
		t.Errorf("after the interrupted prune %d row(s) remain, want %d: the first batch did not commit", got, total-pruneBatchSize)
	}
	if got := countDeleteTombstones(t, s); got != pruneBatchSize {
		t.Errorf("after the interrupted prune %d delete tombstone(s) remain, want %d: the first batch's tombstones did not commit alongside its DELETE", got, pruneBatchSize)
	}
}

// countDeleteTombstones counts the delete-phase rows in memory_history across the
// whole store — the tombstone is memory_history's own visible world, so a batch
// that committed is a batch whose tombstones a rerun would see too.
func countDeleteTombstones(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_history WHERE phase = ?`, phaseDelete).Scan(&n); err != nil {
		t.Fatalf("count delete tombstones: %v", err)
	}
	return n
}

// TestPruneReadsAnAccessStampInEitherTimestampShape: the grace term coalesces
// three columns and they do not all hold the same shape — Store.Touch writes
// last_accessed as RFC 3339 while datetime('now') writes the other two in
// SQLite's own form. Nothing in production calls Touch today, so a text
// comparison looks correct and quietly becomes wrong the day a surface records a
// read, and a guarantee that holds only while a column has no writer is not a
// guarantee.
//
// The instants are fixed and deliberately all fall on ONE date, because that is
// the only place the two shapes disagree: compared as text, 'T' (0x54) sorts
// above the space (0x20) that separates date from time, so a value stored as
// 11:30 sorts after a cutoff of 11:00 and the row is kept when it should have
// gone. Across dates the two orders agree, which is exactly what makes this a
// silent one-day-late bug rather than an obvious one.
func TestPruneReadsAnAccessStampInEitherTimestampShape(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A fixed clock, so every instant below falls on the same calendar day as the
	// cutoff and the test does not depend on when it runs. The grace window runs
	// BACK from the cutoff, so 10:30 is outside it (prunable) and 11:30 is inside
	// it (kept) — and both are on the cutoff's own day, which is the only place
	// the two shapes disagree.
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	const grace = time.Hour
	cutoff := now.Add(-grace)
	outside := cutoff.Add(-30 * time.Minute)
	inside := cutoff.Add(30 * time.Minute)

	rfcGone := pruneFixture(t, s, "a session note read half an hour before the cutoff, in RFC 3339", RetentionSession,
		"2026-08-01 00:00:00", outside.Format(time.RFC3339))
	sqlGone := pruneFixture(t, s, "a session note read half an hour before the cutoff, in SQLite's shape", RetentionSession,
		"2026-08-01 00:00:00", outside.Format("2006-01-02 15:04:05"))
	rfcKept := pruneFixture(t, s, "a session note read half an hour after the cutoff, in RFC 3339", RetentionSession,
		"2026-08-01 00:00:00", inside.Format(time.RFC3339))
	sqlKept := pruneFixture(t, s, "a session note read half an hour after the cutoff, in SQLite's shape", RetentionSession,
		"2026-08-01 00:00:00", inside.Format("2006-01-02 15:04:05"))

	report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true, Grace: grace, Now: now})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	removed := map[string]bool{}
	for _, id := range report.RemovedIDs {
		removed[id] = true
	}
	// The row that discriminates: read at 10:30 in the shape Store.Touch writes,
	// against a cutoff of 11:00. Compared as text the stored 'T' sorts after the
	// cutoff's space, so the row is KEPT — a prune that runs a day late on every
	// row whose last read fell on the cutoff's own day.
	if !removed[rfcGone] {
		t.Errorf("a row read at %s — before the %s cutoff, in the shape Store.Touch writes — survived: "+
			"the two timestamp shapes are not both read", outside.Format(time.RFC3339), cutoff)
	}
	if !removed[sqlGone] {
		t.Error("the control row, in SQLite's own shape, survived — the fixture is not exercising the predicate")
	}
	for _, tc := range []struct{ id, why string }{
		{rfcKept, "a row read after the cutoff, in the RFC 3339 shape"},
		{sqlKept, "a row read after the cutoff, in SQLite's shape"},
	} {
		if removed[tc.id] {
			t.Errorf("%s was removed: %s", tc.id, tc.why)
		}
	}
}

// TestPruneScopesToOneProjectWhenAsked: `ghost prune --project` is how an
// operator looks at one repository's session notes, and a filter that was ignored
// would report a store-wide number next to a project name.
func TestPruneScopesToOneProjectWhenAsked(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "other-project", "/tmp/other", "other"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	mine := pruneFixture(t, s, "a session note in the project being pruned", RetentionSession,
		stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	theirs := pruneFixture(t, s, "a session note in another project", RetentionSession,
		stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET project_id = 'other-project' WHERE id = ?`, theirs); err != nil {
		t.Fatalf("move the second row: %v", err)
	}

	report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true, ProjectID: testProject})
	if err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	}
	if len(report.RemovedIDs) != 1 || report.RemovedIDs[0] != mine {
		t.Fatalf("removed %v, want only [%s]", report.RemovedIDs, mine)
	}
	if liveCount(t, s, "other-project") != 1 {
		t.Error("a project-scoped prune reached into another project")
	}
}

// TestPruneIgnoresAnUnknownGrace: a caller that passes a negative grace is asking
// for "expired, regardless of when it was last touched", which is not a thing a
// grace period can mean. It is refused rather than clamped, because a clamp to
// zero would silently do exactly that.
func TestPruneIgnoresAnUnknownGrace(t *testing.T) {
	s := testStore(t)
	if _, err := s.PruneSessionMemories(context.Background(), PruneOptions{Grace: -time.Hour}); err == nil {
		t.Error("a negative grace was accepted")
	}
}

// TestABackupTakenAfterAPruneStillVerifies: the prune appends a history row per
// removal, so a store that has been pruned holds rows a backup of the pre-prune
// store did not. The manifest counts the memories (and not the change log), and
// what has to hold is that a backup taken AFTER a prune describes that store
// exactly — the counts move with the data rather than lagging it, which is the
// failure a reader restoring from the manifest would otherwise discover by hand.
func TestABackupTakenAfterAPruneStillVerifies(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	s := NewStore(db, nil)
	if err := s.EnsureProject(ctx, testProject, "/tmp/prune-backup", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for i, tier := range RetentionValues() {
		if _, _, _, err := s.UpsertWithOptions(ctx, testProject, "fact",
			"a fact to survive the backup, number "+string(rune('1'+i)), "mcp", 0.6, nil,
			UpsertOptions{Retention: tier}); err != nil {
			t.Fatalf("UpsertWithOptions: %v", err)
		}
	}
	pruneFixture(t, s, "a session note that the prune removes before the backup", RetentionSession,
		stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))

	before, err := s.Backup(ctx, filepath.Join(t.TempDir(), "before.db"))
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if before.Counts.Memories != 4 {
		t.Fatalf("pre-prune backup holds %d memories, want 4", before.Counts.Memories)
	}
	preReport, err := VerifyBackup(ctx, before.Path)
	if err != nil {
		t.Fatalf("VerifyBackup(pre-prune): %v", err)
	}
	if len(preReport.Problems) != 0 {
		t.Errorf("a fresh backup reported problems: %v", preReport.Problems)
	}

	if report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true}); err != nil {
		t.Fatalf("PruneSessionMemories: %v", err)
	} else if report.Removed != 1 {
		t.Fatalf("prune removed %d, want 1", report.Removed)
	}

	after, err := s.Backup(ctx, filepath.Join(t.TempDir(), "after.db"))
	if err != nil {
		t.Fatalf("Backup after prune: %v", err)
	}
	if after.Counts.Memories != 3 {
		t.Errorf("post-prune backup holds %d memories, want 3: the manifest counted a store the prune had already changed", after.Counts.Memories)
	}
	postReport, err := VerifyBackup(ctx, after.Path)
	if err != nil {
		t.Fatalf("VerifyBackup(post-prune): %v", err)
	}
	if len(postReport.Problems) != 0 {
		t.Errorf("a backup taken after a prune did not verify: %v", postReport.Problems)
	}
	if !postReport.CountsRead || postReport.Counts.Memories != 3 {
		t.Errorf("verify reported %+v, want a read count of 3 memories", postReport.Counts)
	}
	// And the pre-prune copy is still restorable and still describes itself: the
	// prune did not reach back into a file that had already been written.
	preAgain, err := VerifyBackup(ctx, before.Path)
	if err != nil {
		t.Fatalf("VerifyBackup(pre-prune, again): %v", err)
	}
	if len(preAgain.Problems) != 0 {
		t.Errorf("the pre-prune backup stopped verifying after the prune: %v", preAgain.Problems)
	}
}
