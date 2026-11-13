package memory

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func pruneFixtureIn(t *testing.T, s *Store, projectID, content, tier, expires, lastAccessed string) string {
	t.Helper()
	id := pruneRow(t, s, projectID, content, tier)
	agePruneRow(t, s, id, expires, lastAccessed)
	return id
}
func pruneFixture(t *testing.T, s *Store, content, tier, expires, lastAccessed string) string {
	t.Helper()
	id := pruneRow(t, s, testProject, content, tier)
	agePruneRow(t, s, id, expires, lastAccessed)
	return id
}

func pruneRow(t *testing.T, s *Store, projectID, content, tier string) string {
	t.Helper()
	ctx := context.Background()
	if projectID == GlobalProjectID {
		if err := s.EnsureProject(ctx, GlobalProjectID, GlobalProjectID, "global"); err != nil {
			t.Fatalf("EnsureProject(%s): %v", GlobalProjectID, err)
		}
	}
	id, _, _, err := s.UpsertWithOptions(ctx, projectID, "fact", content, "mcp", 0.6, nil, UpsertOptions{Retention: tier})
	if err != nil {
		t.Fatalf("UpsertWithOptions(%s, %s): %v", projectID, tier, err)
	}
	return id
}
func agePruneRow(t *testing.T, s *Store, id, expires, lastAccessed string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE memories SET expires_at = ?, last_accessed = ? WHERE id = ?`,
		nullIfEmpty(expires), nullIfEmpty(lastAccessed), id); err != nil {
		t.Fatalf("age %s: %v", id, err)
	}
}

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

func backdateWrite(t *testing.T, s *Store, id, when string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE memories SET updated_at = ?, created_at = ? WHERE id = ?`, when, when, id); err != nil {
		t.Fatalf("backdate the write on %s: %v", id, err)
	}
}

func stamp(offset time.Duration) string {
	return stampAt(time.Now().UTC(), offset)
}

func stampAt(base time.Time, offset time.Duration) string {
	return base.UTC().Add(offset).Format("2006-01-02 15:04:05")
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

func TestPruneRemovesOnlyExpiredSessionRowsPastTheGrace(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	rows := []struct{ content, tier, expires, lastAccessed string }{
		{content: "a session note expired long ago and untouched since", tier: RetentionSession,
			expires: stampAt(now, -30*24*time.Hour), lastAccessed: stampAt(now, -29*24*time.Hour)},
		{content: "a session note that has not reached its expiry", tier: RetentionSession,
			expires: stampAt(now, time.Hour), lastAccessed: stampAt(now, -29*24*time.Hour)},
		{content: "a session note expired but read yesterday", tier: RetentionSession,
			expires: stampAt(now, -30*24*time.Hour), lastAccessed: stampAt(now, -24*time.Hour)},
		{content: "a session note expired and never read", tier: RetentionSession,
			expires: stampAt(now, -30*24*time.Hour)},
		{content: "a session note expired seconds after it was saved", tier: RetentionSession,
			expires: stampAt(now, -time.Hour)},
		{content: "a durable fact that is just as old", tier: RetentionProject,
			expires: stampAt(now, -30*24*time.Hour), lastAccessed: stampAt(now, -29*24*time.Hour)},
		{content: "a keep-forever fact with a stale expiry", tier: RetentionPersistent,
			expires: stampAt(now, -30*24*time.Hour), lastAccessed: stampAt(now, -29*24*time.Hour)},
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = pruneRow(t, s, testProject, r.content, r.tier)
	}
	for i, r := range rows {
		agePruneRow(t, s, ids[i], r.expires, r.lastAccessed)
	}
	longExpired, notExpired, insideGrace, neverAccessed, freshWrite := ids[0], ids[1], ids[2], ids[3], ids[4]
	durable, keepForever := ids[5], ids[6]
	backdateWrite(t, s, neverAccessed, stampAt(now, -30*24*time.Hour))

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

	applied, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true})
	if err != nil {
		t.Fatalf("PruneSessionMemories(apply): %v", err)
	}
	if applied.Removed != 1 {
		t.Fatalf("the apply removed %d rows, want 1", applied.Removed)
	}
}

func TestPruneLeavesADeleteTombstonePerRemoval(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first := pruneRow(t, s, testProject, "the first session note a prune will remove", RetentionSession)
	second := pruneRow(t, s, testProject, "the second session note a prune will remove", RetentionSession)
	agePruneRow(t, s, first, stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	agePruneRow(t, s, second, stamp(-31*24*time.Hour), stamp(-30*24*time.Hour))

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

func TestPruneNeverRemovesAPinnedRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	pinned := pruneRow(t, s, testProject, "a pinned session note that expired long ago", RetentionSession)
	control := pruneRow(t, s, testProject, "an unpinned session note that expired long ago", RetentionSession)
	now := time.Now().UTC()
	aged, newer := stampAt(now, -30*24*time.Hour), stampAt(now, -29*24*time.Hour)
	agePruneRow(t, s, pinned, aged, newer)
	agePruneRow(t, s, control, aged, newer)
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

func TestAStoreWidePruneReachesGlobalRows(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	globalID := pruneFixtureIn(t, s, GlobalProjectID, "a global note saved as conversation-scoped, long expired", RetentionSession,
		stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))

	report, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if report.Removed != 1 || len(report.RemovedIDs) != 1 || report.RemovedIDs[0] != globalID {
		t.Fatalf("store-wide prune removed %v (%d row(s)), want exactly the global row %s — a store-wide prune must reach _global, or a global session row outlives its own expiry",
			report.RemovedIDs, report.Removed, globalID)
	}
	if liveCount(t, s, GlobalProjectID) != 0 {
		t.Errorf("after the prune _global holds %d row(s), want 0", liveCount(t, s, GlobalProjectID))
	}

	kept := pruneFixtureIn(t, s, GlobalProjectID, "a global note a scoped prune must not touch", RetentionSession,
		stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	scoped, err := s.PruneSessionMemories(ctx, PruneOptions{Apply: true, ProjectID: testProject})
	if err != nil {
		t.Fatalf("scoped prune: %v", err)
	}
	if scoped.Removed != 0 {
		t.Errorf("a --project-scoped prune removed %d row(s), want 0: it must not reach outside its project", scoped.Removed)
	}
	if liveCount(t, s, GlobalProjectID) != 1 {
		t.Errorf("_global holds %d row(s) after the scoped prune, want 1 (%s)", liveCount(t, s, GlobalProjectID), kept)
	}
}

func TestPruneSparesARowThatStoppedMatchingMidRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
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
			now := time.Now().UTC()
			aged, newer := stampAt(now, -30*24*time.Hour), stampAt(now, -29*24*time.Hour)
			for i := 0; i < total-1; i++ {
				seedPruneRow(t, s, fmt.Sprintf("an expired session note %d", i), RetentionSession, aged, aged)
			}
			spared := seedPruneRow(t, s, "the row a write reaches between batches", RetentionSession,
				aged, newer)
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

func installPruneSpareTrigger(t *testing.T, s *Store, whenAndBody string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `CREATE TEMP TRIGGER prune_spare_mid_run AFTER INSERT ON memory_history `+whenAndBody); err != nil {
		t.Fatalf("install the mid-run write trigger: %v", err)
	}
}

func liveID(t *testing.T, s *Store, projectID string) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM memories WHERE project_id = ?`, projectID).Scan(&id); err != nil {
		t.Fatalf("read the surviving row id: %v", err)
	}
	return id
}

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

func TestPruneBatchesCommitPerBatch(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const total = pruneBatchSize + 50 // two batches
	now := time.Now().UTC()
	aged, newer := stampAt(now, -30*24*time.Hour), stampAt(now, -29*24*time.Hour)
	for i := 0; i < total; i++ {
		seedPruneRow(t, s, fmt.Sprintf("an expired session note %d", i), RetentionSession, aged, newer)
	}
	if liveCount(t, s, testProject) != total {
		t.Fatalf("seeded %d rows, store has %d", total, liveCount(t, s, testProject))
	}

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

	if _, err := s.db.ExecContext(ctx, `DELETE FROM memory_history`); err != nil {
		t.Fatalf("clear history between directions: %v", err)
	}
	for i := 0; i < total; i++ {
		seedPruneRow(t, s, fmt.Sprintf("an expired session note %d again", i), RetentionSession, aged, newer)
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

func countDeleteTombstones(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memory_history WHERE phase = ?`, phaseDelete).Scan(&n); err != nil {
		t.Fatalf("count delete tombstones: %v", err)
	}
	return n
}

func TestPruneScopesToOneProjectWhenAsked(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "other-project", "/tmp/other", "other"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	mine := pruneRow(t, s, testProject, "a session note in the project being pruned", RetentionSession)
	theirs := pruneRow(t, s, testProject, "a session note in another project", RetentionSession)
	agePruneRow(t, s, mine, stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
	agePruneRow(t, s, theirs, stamp(-30*24*time.Hour), stamp(-29*24*time.Hour))
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
