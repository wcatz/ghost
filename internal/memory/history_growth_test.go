package memory

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Issue #729. `ghost mcp status` and `ghost_health` have to be able to answer two
// questions about memory_history without anybody opening the database: how fast
// is it growing into its retention caps, and how much of that growth is version
// rows that restated the version before them. HistoryGrowth is the one read both
// surfaces share, and these are its tests.
//
// The numbers it reports are answers about a table somebody else wrote, so the
// fixture builds that table directly — the same call the compaction's own tests
// make, and for the same reason: the no-op rows being counted are rows a
// pre-#727 build wrote and no current writer produces, so there is no call left
// to make and a test that drove one would assert the fix rather than the report.

const growthDays = 24 * time.Hour

// stampAgo is a recorded_at the given duration in the past, in the layout the
// store writes. It is compared in SQL as text against datetime('now', ...), so
// it must be UTC exactly as SQLite's own now is, and the offsets here are hours
// wide rather than seconds: a fixture that stamped a row one second either side
// of the window edge would be a test whose answer depends on how fast the
// machine runs the fixture.
func stampAgo(d time.Duration) string {
	return time.Now().UTC().Add(-d).Format(StoredStampLayout)
}

// historyRowIDs is the whole table's identity, in order, for the assertion that
// a read-only report changed nothing. Counting rows would not do: a report that
// rewrote a row in place and left the count alone would pass.
func historyRowIDs(t *testing.T, s *Store) []int64 {
	t.Helper()
	rows, err := s.db.Query(`SELECT rowid FROM memory_history ORDER BY rowid`)
	if err != nil {
		t.Fatalf("read history rowids: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan history rowid: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate history rowids: %v", err)
	}
	return out
}

// seedGrowthFixture builds the 16-row history the report's own numbers are read
// off, shared by the two tests that assert on them so the prose a warning quotes
// is quoted about the same rows the arithmetic is checked on.
//
// The shape, and why each part is there:
//
//   - busy: a save and eleven reflects that restate it. This is the damage #727
//     stopped writing and #730 removes — a store that ran the unattended
//     lifecycle for a while. Twelve rows, eleven of them restatements, and the
//     one that is not is the memory's NEWEST version, which nothing removes.
//   - quiet: the same damage in miniature, so the deepest memory is not the only
//     memory with restatements and the share is not a property of one memory.
//   - edited: a memory whose save is two days old and whose two edits are not.
//     It is there to separate the two clocks the report runs on — the window
//     excludes its save, and both its edits CHANGE state, so neither is a
//     restatement. Its save is stamped by hand because no call saves a memory in
//     the past.
//
// 12 + 2 + 2 = 16 rows in the window, 12 of them restatements, 17 rows in the
// table, and 12 versions on the deepest memory.
//
// appendWindowRestatement appends a version that restates the memory's current
// state, recorded NOW — inside the report's 24-hour window.
//
// It is deliberately NOT appendVerbatimVersion, which #730's own fixtures use and
// which stamps `preFixReflectAt` (2026-08-01). That instant is the right one for
// the repair's tests, which need their rows on the far side of #730's bound, and
// it is 58 days back: every row it writes is outside a 24-hour window as well, so
// a #729 fixture built on it measures an empty day. The report measures a WINDOW,
// so its fixtures have to state one, and this is that statement.
//
// The two populations genuinely differ, and that is the point rather than a
// workaround: what a current build writes today is post-#727 by definition, so it
// is what the report measures and what the repair will not touch. The test that
// reconciles the two pins #730's bound explicitly rather than relying on where the
// wall clock happens to be.
func appendWindowRestatement(t *testing.T, s *Store, memoryID string) {
	t.Helper()
	appendVersionRow(t, s, memoryID, phaseReflect, nil, "", "", nil)
}

func seedGrowthFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	busy := createCompactMemory(t, s, compactFirstText)
	for range 11 {
		appendWindowRestatement(t, s, busy)
	}

	quiet := createCompactMemory(t, s, compactSecondText)
	appendWindowRestatement(t, s, quiet)

	edited := createCompactMemory(t, s, "the staging relay drains to the tablet nightly")
	stampHistoryRow(t, s, edited, phaseSave, stampAgo(2*growthDays))
	if err := s.UpdateMemory(ctx, testProject, edited, strPtr("the staging relay drains hourly"), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	if err := s.UpdateMemory(ctx, testProject, edited, strPtr("the staging relay drains hourly, alert on failure"), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
}

// TestHistoryGrowthNoOpShareAgreesWithWhatCompactWouldRemove is the tie between
// the two commands: the report's numerator and the repair's count are the same
// predicate, so on a history where every row is inside the window the two must
// reconcile exactly, and the difference must be accounted for row by row.
//
// They are not the same NUMBER, and the gap is the whole reason this test
// exists. `ghost history compact` removes a SUBSET of the restatements: a
// memory's newest version is what it says now, and a row naming another memory
// is a thread into a successor's history rather than a restatement. So the
// report's share over-counts the repair on purpose — it is measuring how much
// noise the table is carrying, not how much of it is disposable — and a report
// that instead printed compact's count would be quiet about the rows that will
// never be removed, which are the ones an operator is tempted to blame on the
// tool.
//
// The fixture is built so each surviving restatement has a DIFFERENT named
// reason, so the reconciliation is a list rather than a number that happens to
// come out right.
func TestHistoryGrowthNoOpShareAgreesWithWhatCompactWouldRemove(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// a: five restatations of one save, so the four oldest are removable and the
	// newest is not.
	noisy := createCompactMemory(t, s, compactFirstText)
	for range 5 {
		appendWindowRestatement(t, s, noisy)
	}

	// b: a single restatement, which is entirely the newest version.
	quiet := createCompactMemory(t, s, compactSecondText)
	appendWindowRestatement(t, s, quiet)

	// c: a resolve, which really changed resolved_at, then a restatement of the
	// resolved state.
	resolved := createCompactMemory(t, s, "the tablet is paired with the kiosk")
	if _, err := s.SetResolved(ctx, []string{resolved}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	appendWindowRestatement(t, s, resolved)

	// d: a reflect that names another memory — an edge a reader follows from this
	// memory's history into its successor's (#648) — and then a restatement.
	// Both restate the state, and neither is removable: one records a claim
	// rather than a state, the other is the newest version.
	linked := createCompactMemory(t, s, "the kiosk polls the relay every minute")
	appendVersionRow(t, s, linked, phaseReflect, nil, quiet, "", nil)
	appendWindowRestatement(t, s, linked)

	// e: a memory that took damage and was then RETIRED, which is the fifth guard
	// and the only one #709 turned from rare to routine — retention tiers give
	// `expires_at` a day to arrive, so a store now deletes memories nobody asked
	// it to, and each leaves a history no repair will ever reach.
	//
	// The restatement goes in BEFORE the delete, so it is not the newest version
	// and nothing but the tombstone rule keeps it. That is the case worth having
	// here, because the other four guards are all still true of it: it is before
	// the stated bound, it names no other memory, and its phase is `reflect`. A
	// fixture that only listed this guard would prove nothing about it.
	retired := createCompactMemory(t, s, "the night shift leaves the staging relay warm")
	appendWindowRestatement(t, s, retired)
	if err := s.Delete(ctx, retired); err != nil {
		t.Fatalf("Delete the retired memory: %v", err)
	}
	// The tombstone itself restates its predecessor as faithfully as a no-op
	// reflect does, so the share counts it too. Asserted, because a count of one
	// here would be the fixture disagreeing with what Delete writes rather than
	// with what the repair keeps.
	if n := redundantVersionCount(t, s, retired); n != 2 {
		t.Fatalf("%d of the retired memory's versions restate their predecessor, want 2 (its "+
			"restatement and the tombstone Delete files from the last live state): a count of one "+
			"would mean the fixture is not the case this guard is about", n)
	}
	// The other half of that claim, because a guard that cannot fail is not a guard:
	// its restatement is in the damage shape with NO retention rule at all. Counted
	// through the production predicate rather than restated here, since a
	// hand-written copy of the rules would pass on the day the rules changed.
	//
	// ONE, not two. The tombstone is spared twice over — it is the newest version
	// and its phase is not `reflect` — and a count of two here would mean the
	// fixture staged a row the newest-version guard already spared, and the
	// tombstone rule would be claiming credit for someone else's work.
	reachArgs := []any{retired, testProject}
	for _, p := range compactablePhases() {
		reachArgs = append(reachArgs, p)
	}
	reachArgs = append(reachArgs, "2999-01-01 00:00:00")
	var inReach int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM memory_history h WHERE h.memory_id = ? AND h.project_id = ? AND `+
			historyRemovableLikeSQL("h"), reachArgs...).Scan(&inReach); err != nil {
		t.Fatalf("count the rows the tombstone guard is holding back: %v", err)
	}
	if inReach != 1 {
		t.Fatalf("%d of the retired memory's versions are in the damage shape ignoring every "+
			"retention rule, want 1: the tombstone guard is only load-bearing if that row is "+
			"otherwise in reach", inReach)
	}

	res, err := s.HistoryGrowth(ctx)
	if err != nil {
		t.Fatalf("HistoryGrowth: %v", err)
	}
	// 5 + 1 + 1 + 2 + 2: the resolve is a real change, and so is the edge row's
	// neighbour; everything else restates the row before it, the retired memory's
	// two included.
	if res.NoOpRows != 11 {
		t.Fatalf("NoOpRows = %d, want 11", res.NoOpRows)
	}

	dryRunStart := historyRowIDs(t, s)

	// The bound is STATED rather than inherited, and that is the whole shape of
	// this test. The repair defaults to `reflectNoOpCutoff` — the instant #727
	// reached main — and every row this fixture writes is recorded NOW, which is
	// after that instant on any clock past #727. With the default the run removes
	// NOTHING: the numbers reconcile perfectly, they reconcile about zero, and the
	// test passes while saying nothing about how the two commands relate. An
	// operator with a store whose clock is behind is the same case, and `--before`
	// is how they say so.
	//
	// The year 2999 is not a magic instant. It is "after every row this fixture
	// can write", stated once so the reconciliation below is a statement about the
	// two predicates rather than about today's date.
	opts := HistoryCompactOptions{Before: "2999-01-01"}
	if opts.Before == "" {
		t.Fatal("the stated bound is empty, so the run would use the default")
	}

	// A dry run first: the count the APPLY would produce, from the same SQL and
	// the same decision, with nothing written — which is also the assertion that
	// this report is a preview of that command rather than a rival to it.
	preview, err := s.CompactHistory(ctx, testProject, opts)
	if err != nil {
		t.Fatalf("CompactHistory (dry run): %v", err)
	}
	if preview.Before == "" {
		t.Fatal("the dry run reported no bound, so its counts are counts at no instant")
	}
	if preview.Removed != 4 {
		t.Fatalf("compact dry run removed = %d, want 4", preview.Removed)
	}
	if preview.Removed > res.NoOpRows {
		t.Fatalf("compact would remove %d rows from a history holding %d restatements — the repair removes a subset, never more",
			preview.Removed, res.NoOpRows)
	}
	// Seven kept, and each for its own named reason rather than as a difference
	// that happened to come out right: four are a memory's newest version (noisy,
	// quiet, linked and retired), one is a thread naming another memory, and two
	// are the retired memory's, kept whole because the memory is gone.
	if got, want := res.NoOpRows-preview.Removed, int64(7); got != want {
		t.Errorf("restatements the repair would keep = %d, want %d", got, want)
	}
	if after := historyRowIDs(t, s); len(after) != len(dryRunStart) {
		t.Errorf("the dry run changed the history: %d rows, want the %d it started with", len(after), len(dryRunStart))
	}

	// Now the apply, so the keepers are OBSERVED rather than inferred, and each
	// one is named. Every memory keeps its save; the resolved memory keeps the
	// resolve; the edge row and the four older restatements are gone; and one
	// restatement survives per memory as that memory's newest version.
	//
	// The SAME stated bound, not the default: an apply under the default would
	// remove nothing and agree with a preview that removed nothing, and this test
	// would pass having observed no survivors at all.
	applyOpts := opts
	applyOpts.Apply = true
	applied, err := s.CompactHistory(ctx, testProject, applyOpts)
	if err != nil {
		t.Fatalf("CompactHistory (apply): %v", err)
	}
	if applied.Before != preview.Before {
		t.Errorf("apply ran at bound %q, the dry run at %q — the preview is not a preview",
			applied.Before, preview.Before)
	}
	if applied.Removed != preview.Removed {
		t.Errorf("apply removed %d, dry run said %d — the preview is not a preview", applied.Removed, preview.Removed)
	}
	for _, tc := range []struct {
		memoryID string
		want     []string
	}{
		{noisy, []string{phaseSave, phaseReflect}},
		{quiet, []string{phaseSave, phaseReflect}},
		{resolved, []string{phaseSave, phaseResolve, phaseReflect}},
		// Two reflect rows survive here and they are different rows: the newest
		// version, and the edge that names another memory.
		{linked, []string{phaseSave, phaseReflect, phaseReflect}},
		// All three of the retired memory's versions survive, and the middle one
		// is the case the fifth guard exists for: before the bound, naming no other
		// memory, phase `reflect`, and not the newest.
		{retired, []string{phaseSave, phaseReflect, phaseDelete}},
	} {
		if got := historyPhases(t, s, tc.memoryID); !wantPhases(got, tc.want) {
			t.Errorf("survivors of %s = %v, want %v", tc.memoryID, got, tc.want)
		}
	}
}

// TestHistoryGrowthQueryPlanIsOnePass pins the cost model, because the shape of
// the scan is a property of the statement rather than of this week's data.
//
// The window aggregate cannot use an index for its filter: recorded_at is the
// second column of idx_history_memory, so the only way to ask "in the last 24
// hours" is one pass over the table, and the pass is bounded by the retention
// cap that pruneHistoryTx enforces (20 000 rows). That is the whole reason the
// issue allows pinning the PLAN rather than a row bound, and it is why a
// standalone recorded_at index was not added for this: schema.go already
// records the measured cost of one on the write path, and the ~2 ms it would
// save on a status line is not worth a permanent fifth of the append.
//
// What must not change is that the per-row work stays an index SEEK. The
// restatement test is a correlated sub-select over the row before this one; if
// it ever loses the (memory_id=?) narrowing it becomes a scan per row, and the
// report goes from milliseconds to minutes on exactly the store an operator runs
// it on. The expected plan is therefore pinned verbatim, and re-measured — not
// loosened into a "contains SCAN" — if the driver ever changes its wording.
func TestHistoryGrowthQueryPlanIsOnePass(t *testing.T) {
	s := testStore(t)

	for _, tc := range []struct {
		name  string
		query func() (string, []any)
		want  []string
	}{
		{
			name:  "window aggregate",
			query: historyWindowGrowthStmt,
			want: []string{
				// One pass over the table, because no index leads with
				// recorded_at. Every window row costs a constant amount of work
				// inside that pass.
				"SCAN h",
				// The restatement test: a seek to the row before this one.
				"SEARCH p USING INTEGER PRIMARY KEY (rowid=?)",
				// And a seek to this memory's versions, which is what keeps the
				// test from being a scan of the whole table per row.
				"SEARCH q USING COVERING INDEX idx_history_memory (memory_id=?)",
			},
		},
		{
			name:  "per memory",
			query: historyPerMemoryGrowthStmt,
			// Covering, because idx_history_memory carries memory_id and
			// recorded_at and this query reads both: the per-memory counts come
			// out of the index without touching the table at all. A temp b-tree
			// would mean SQLite could not use the index's order for the GROUP BY,
			// which is the shape that made a previous version of this report
			// spill.
			want: []string{
				"SCAN memory_history USING COVERING INDEX idx_history_memory",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := strings.Join(historyGrowthPlan(t, s, tc.query), " | ")
			for _, want := range tc.want {
				if !strings.Contains(plan, want) {
					t.Errorf("plan does not contain %q:\n%s", want, plan)
				}
			}
			if strings.Contains(plan, "USE TEMP B-TREE") {
				t.Errorf("plan spills to a temp b-tree:\n%s", plan)
			}
		})
	}
}

// historyGrowthPlan is EXPLAIN QUERY PLAN over a statement the production code
// builds, so the pin cannot pass against a spelling the report does not run.
func historyGrowthPlan(t *testing.T, s *Store, build func() (string, []any)) []string {
	t.Helper()
	query, args := build()
	rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan query plan row: %v", err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate query plan: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("query plan is empty, so nothing is pinned")
	}
	return out
}

// growthLoadStore builds a store whose history holds `restatements+1` versions
// for each of `memories` distinct memories, every row inside the window, so the
// read pays for the whole table.
//
// Bulk-loaded rather than built through Create and appendVerbatimVersion, because
// the fixture is 800 memories wide and this runs on every build: the rows are
// appended through the store's own write path in a way that would take tens of
// thousands of single-row statements, and a cost canary that takes 20 seconds to
// set up is a cost canary nobody enables. The shape is what matters here, and the
// shape is checked by TestGrowthLoadStoreHasTheShapeItClaims — because a fixture
// that quietly stopped being this shape turns the ratio into a measurement of
// something else while still looking like a pass.
func growthLoadStore(t *testing.T, memories, restatements int) *Store {
	t.Helper()
	s := testStore(t)
	// memories (project_id, category, content, source, importance, tags, pinned)
	// is the same minimal column list seedMemoryTx uses; every other column has
	// a default. The content is unique per i, which is how the history load below
	// reaches each memory's own row.
	if _, err := s.db.Exec(`
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?)
		INSERT INTO memories (project_id, category, content, source, importance, tags, pinned)
		SELECT ?, 'fact', 'growth fixture ' || i || ' keeps its own address book', 'mcp', 0.5, '[]', 0
		FROM n`, memories, testProject); err != nil {
		t.Fatalf("load %d memories: %v", memories, err)
	}
	// The first version of each memory, copied out of the live row the way a real
	// write records it, so the restatements below have a predecessor to equal.
	if _, err := s.db.Exec(`
		INSERT INTO memory_history
			(memory_id, project_id, phase, recorded_at, content, category, importance, resolved_at, source)
		SELECT m.id, m.project_id, ?, datetime('now'), m.content, m.category, m.importance, m.resolved_at, m.source
		FROM memories m WHERE m.content LIKE 'growth fixture %'`, phaseSave); err != nil {
		t.Fatalf("load the first version of each memory: %v", err)
	}
	for range restatements {
		if _, err := s.db.Exec(`
			INSERT INTO memory_history
				(memory_id, project_id, phase, recorded_at, content, category, importance, resolved_at, source)
			SELECT m.id, m.project_id, ?, datetime('now'), m.content, m.category, m.importance, m.resolved_at, m.source
			FROM memories m WHERE m.content LIKE 'growth fixture %'`, phaseReflect); err != nil {
			t.Fatalf("load a restatement for every memory: %v", err)
		}
	}
	return s
}

// TestGrowthLoadStoreHasTheShapeItClaims: the ratio test below measures a
// fixture, and this is the assertion that the fixture is the one the paragraphs
// there describe — 10 versions per memory, every row inside the window, and
// every memory's versions actually restating each other. Without it, a load
// statement that stopped finding its rows (a LIKE that matched nothing, a
// restatement written with the wrong state) would leave a nearly empty table and
// the ratio would divide a millisecond by a millisecond.
func TestGrowthLoadStoreHasTheShapeItClaims(t *testing.T) {
	const memories, restatements = 25, 9
	s := growthLoadStore(t, memories, restatements)

	res, err := s.HistoryGrowth(context.Background())
	if err != nil {
		t.Fatalf("HistoryGrowth: %v", err)
	}
	if res.TotalRows != int64(memories*(restatements+1)) {
		t.Errorf("TotalRows = %d, want %d (%d memories x %d versions)", res.TotalRows, memories*(restatements+1), memories, restatements+1)
	}
	if res.RowsInWindow != res.TotalRows {
		t.Errorf("RowsInWindow = %d, want all %d rows — the expensive end of the test is every row inside the window", res.RowsInWindow, res.TotalRows)
	}
	if want := int64(memories * restatements); res.NoOpRows != want {
		t.Errorf("NoOpRows = %d, want %d — every version after the first restates its predecessor", res.NoOpRows, want)
	}
	if res.MaxVersions != int64(restatements+1) {
		t.Errorf("MaxVersions = %d, want %d — the rows must be spread over memories, not piled on one", res.MaxVersions, restatements+1)
	}
}

// TestHistoryGrowthIsExcludedByAWriter pins WHERE the read's lock is taken. It
// used to be taken inside the per-memory helper, so the window aggregate had
// already run — unlocked — by the time it was taken, and the comment on it claimed
// "a consistent snapshot of the table across the two statements" while the first
// statement sat outside the critical section. docs/invariants.md made the same
// claim, so the code was contradicting both its own comment and the invariant
// written about it.
//
// The effect on the numbers is small and the effect on the report is not: a
// lifecycle pass or a compaction from this process appending or deleting between
// the two statements leaves TotalRows and RowsInWindow describing different
// moments, and DaysToStoreCap is one of those numbers divided by the other. The
// share is sound either way — its numerator and denominator come out of a single
// statement — so no assertion on the numbers could have caught this.
//
// The seam IS the assertion. A read leaves no trace, so "the first statement has
// not run yet" and "the first statement ran and returned" look identical from
// outside the call; historyGrowthAfterWindow reports the moment between the two
// statements, and while a writer holds the store's write lock that moment must
// never arrive.
func TestHistoryGrowthIsExcludedByAWriter(t *testing.T) {
	s := testStore(t)
	seedGrowthFixture(t, s)

	// A writer holds s.mu for its whole apply path, so this is the state a
	// concurrent save leaves the store in. Unlock is explicit at each exit below
	// rather than deferred, because a deferred unlock after an explicit one is a
	// fatal "unlock of unlocked RWMutex" that would take the whole package's test
	// binary down and hide the assertion it was supposed to follow.
	s.mu.Lock()

	firstStatementDone := make(chan struct{})
	historyGrowthAfterWindow = func() { close(firstStatementDone) }
	// Cleanup only clears the seam: it is a plain assignment, safe to run on every
	// exit, and leaving it set would make an unrelated failure in a later test
	// report itself as this one's.
	t.Cleanup(func() { historyGrowthAfterWindow = nil })

	errc := make(chan error, 1)
	go func() {
		_, err := s.HistoryGrowth(context.Background())
		errc <- err
	}()

	select {
	case <-firstStatementDone:
		s.mu.Unlock()
		t.Fatal("HistoryGrowth read memory_history while a writer held the store's write lock — the lock is taken inside the per-memory helper, so the window aggregate runs outside it")
	case err := <-errc:
		s.mu.Unlock()
		t.Fatalf("HistoryGrowth returned while a writer held the store's write lock (err %v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	s.mu.Unlock()

	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("HistoryGrowth after the writer finished: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("HistoryGrowth did not return after the writer released the store's write lock")
	}
	// And once the writer is done the read really did reach the second statement:
	// a report that took the lock and then returned early would satisfy the wait
	// above without having read the per-memory counts at all.
	select {
	case <-firstStatementDone:
	default:
		t.Error("HistoryGrowth returned without reaching the statement after the window aggregate")
	}
}

// TestHistoryGrowthSaysTheCapHasBeenReached covers the state both cap warnings
// are FOR, which is also the state they used to render as broken English: a memory
// sitting at exactly 50 versions that wrote anything today has zero days left, and
// historyDaysText turned that into the phrase "already there" — which was then
// interpolated into "it reaches the per-memory cap in %s at that rate" and "the
// store cap is %s away at that rate". So the line read "it reaches the per-memory
// cap in already there at that rate", and "the store cap is already there away at
// that rate".
//
// This is not an edge case to be handled for completeness. pruneHistoryTx holds a
// memory at exactly the cap while it is still being written, and a full table
// settles at exactly the store cap, so days == 0 is the ORDINARY steady state of
// the memory the countdown names in an active store — the most common moment for
// the feature to fire was the moment it printed nonsense.
//
// Both arms are read through the real report rather than handed to the sentence
// builder, because the reachable-at-zero-days claim is a property of the READ
// (the projection of a capped memory) and not only of the wording. The store arm
// needs a table at the cap, so it is the load fixture at 2000 memories x 10
// versions: one bulk INSERT, and TotalRows lands on historyRowsCap exactly.
func TestHistoryGrowthSaysTheCapHasBeenReached(t *testing.T) {
	t.Run("per memory", func(t *testing.T) {
		s := testStore(t)
		capped := createCompactMemory(t, s, "the relay publishes its own address book")
		// 50 versions in total: the save plus 49 restatements, all written today.
		for range historyVersionsPerMemory - 1 {
			appendWindowRestatement(t, s, capped)
		}

		// A second memory, DEEPER than the capped one and entirely outside the
		// window. It is here so the count in the sentence is checkable at all:
		// with one memory in the table the capped memory is simultaneously the
		// subject of the countdown and the store's MaxVersions, so a sentence
		// that spliced the store's widest history onto the countdown's own
		// memory would print the same number either way and this subtest would
		// pass on it.
		//
		// Inserted directly rather than through Create, because Create records
		// the save as its first version AT THE CURRENT INSTANT: that one row
		// would put this memory inside the window, and a memory holding 60
		// versions with 1 in the window has negative headroom, so it would win
		// the countdown and this would stop being the at-cap case at all. As
		// written here it has recent == 0, which is what leaves it out of the
		// countdown and is the ordinary shape of a real store — one memory
		// sitting at the cap, and an older one holding more of them.
		if _, err := s.db.Exec(`
			INSERT INTO memories (project_id, category, content, source, importance, tags, pinned)
			VALUES (?, 'architecture', 'an older memory holds more of them', 'mcp', 0.4, '[]', 0)`, testProject); err != nil {
			t.Fatalf("insert the deeper memory: %v", err)
		}
		var deeper string
		if err := s.db.QueryRow(`SELECT id FROM memories WHERE content = 'an older memory holds more of them'`).Scan(&deeper); err != nil {
			t.Fatalf("read back the deeper memory's id: %v", err)
		}
		if _, err := s.db.Exec(`
			WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i < ?)
			INSERT INTO memory_history
				(memory_id, project_id, phase, recorded_at, content, category, importance, resolved_at, source)
			SELECT m.id, m.project_id, ?, datetime('now', '-72 hours'),
			       m.content, m.category, m.importance, m.resolved_at, m.source
			FROM n JOIN memories m ON m.id = ?`,
			historyVersionsPerMemory+10, phaseReflect, deeper); err != nil {
			t.Fatalf("load the deeper out-of-window memory: %v", err)
		}

		res, err := s.HistoryGrowth(context.Background())
		if err != nil {
			t.Fatalf("HistoryGrowth: %v", err)
		}
		if res.DaysToPerMemoryCap != 0 {
			t.Fatalf("DaysToPerMemoryCap = %v, want 0 — a memory at its cap has no headroom left", res.DaysToPerMemoryCap)
		}
		// The premises of the "of its 50 versions" assertion below, stated as
		// failures rather than assumed: the memory the countdown is about, and the
		// store's widest history, have to be DIFFERENT memories here or that
		// assertion cannot tell a spliced sentence from a correct one.
		if res.CapMemoryID != capped {
			t.Fatalf("the countdown is about %s, want the capped memory %s", res.CapMemoryID, capped)
		}
		if res.MaxVersions <= res.CapMemoryVersions {
			t.Fatalf("MaxVersions = %d and the capped memory holds %d — the fixture cannot tell a spliced sentence from a correct one",
				res.MaxVersions, res.CapMemoryVersions)
		}
		detail := warningDetail(t, res, HistoryWarnPerMemoryCap)
		if strings.Contains(detail, "already there") {
			t.Errorf("the per-memory warning splices a state into a duration frame:\n%s", detail)
		}
		for _, want := range []string{
			"at the per-memory cap",
			"50 of its 50 versions",
			"wrote 50 in the last 24h",
			"oldest versions",
		} {
			if !strings.Contains(detail, want) {
				t.Errorf("the per-memory warning does not mention %q:\n%s", want, detail)
			}
		}
	})

	t.Run("store", func(t *testing.T) {
		// 2000 memories x 10 versions = 20000 rows, which is historyRowsCap.
		const restatements = 9
		s := growthLoadStore(t, int(historyRowsCap/(restatements+1)), restatements)

		res, err := s.HistoryGrowth(context.Background())
		if err != nil {
			t.Fatalf("HistoryGrowth: %v", err)
		}
		if res.TotalRows != int64(historyRowsCap) {
			t.Fatalf("TotalRows = %d, want %d — the fixture has to be AT the cap, not near it", res.TotalRows, historyRowsCap)
		}
		if res.DaysToStoreCap != 0 {
			t.Fatalf("DaysToStoreCap = %v, want 0 — a full table has no headroom left", res.DaysToStoreCap)
		}
		detail := warningDetail(t, res, HistoryWarnStoreCap)
		if strings.Contains(detail, "already there") {
			t.Errorf("the store warning splices a state into a duration frame:\n%s", detail)
		}
		for _, want := range []string{
			"at the store cap",
			"20000 of its 20000 history rows",
			"oldest rows",
		} {
			if !strings.Contains(detail, want) {
				t.Errorf("the store warning does not mention %q:\n%s", want, detail)
			}
		}
	})
}

// warningDetail is the one warning of a kind, or a failure: a test that reads a
// sentence out of the result is testing a sentence, and finding it by kind is what
// keeps it from passing on whichever finding happened to come first.
func warningDetail(t *testing.T, res HistoryGrowthResult, kind string) string {
	t.Helper()
	for _, w := range res.Warnings {
		if w.Kind == kind {
			return w.Detail
		}
	}
	t.Fatalf("no %s warning in %v", kind, warningKinds(res.Warnings))
	return ""
}

func warningKinds(warnings []HistoryWarning) []string {
	kinds := make([]string, 0, len(warnings))
	for _, w := range warnings {
		kinds = append(kinds, w.Kind)
	}
	return kinds
}
