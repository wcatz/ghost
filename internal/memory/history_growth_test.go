package memory

import (
	"context"
	"fmt"
	"math"
	"sort"
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

func memoryRowCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM memories`).Scan(&n); err != nil {
		t.Fatalf("count memories: %v", err)
	}
	return n
}

func warningKinds(warnings []HistoryWarning) []string {
	kinds := make([]string, 0, len(warnings))
	for _, w := range warnings {
		kinds = append(kinds, w.Kind)
	}
	return kinds
}

func wantKinds(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func almostEqual(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

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
//   - quiet: the same damage in miniature, so the busiest memory is not the only
//     memory with restatements and the share is not a property of one memory.
//   - edited: a memory whose save is two days old and whose two edits are not.
//     It is there to separate the two clocks the report runs on — the window
//     excludes its save, and both its edits CHANGE state, so neither is a
//     restatement. Its save is stamped by hand because no call saves a memory in
//     the past.
//
// 12 + 2 + 2 = 16 rows in the window, 12 of them restatements, 17 rows in the
// table, and 12 versions on the busiest memory.
func seedGrowthFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	busy := createCompactMemory(t, s, compactFirstText)
	for range 11 {
		appendVerbatimVersion(t, s, busy)
	}

	quiet := createCompactMemory(t, s, compactSecondText)
	appendVerbatimVersion(t, s, quiet)

	edited := createCompactMemory(t, s, "the staging relay drains to the tablet nightly")
	stampHistoryRow(t, s, edited, phaseSave, stampAgo(2*growthDays))
	if err := s.UpdateMemory(ctx, testProject, edited, strPtr("the staging relay drains hourly"), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
	if err := s.UpdateMemory(ctx, testProject, edited, strPtr("the staging relay drains hourly, alert on failure"), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory: %v", err)
	}
}

// TestHistoryGrowthReportsTheNoOpShareAndTheCapHeadroom is the report itself,
// on a fixture whose every number is arithmetic rather than a recount: 16 version
// rows in the window, 12 of them restating the row before them, one memory
// holding 12 versions, 17 rows in the table.
//
// The four numbers the issue asks for are all here and all distinct on purpose —
// rows in the window, the no-op share, the busiest memory against the per-memory
// cap, and the table against the store cap — because a report that reported one
// number for two of them would pass a fixture where they coincided.
func TestHistoryGrowthReportsTheNoOpShareAndTheCapHeadroom(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	seedGrowthFixture(t, s)

	before := historyRowIDs(t, s)
	memoriesBefore := memoryRowCount(t, s)

	res, err := s.HistoryGrowth(ctx)
	if err != nil {
		t.Fatalf("HistoryGrowth: %v", err)
	}

	if res.RowsInWindow != 16 {
		t.Errorf("RowsInWindow = %d, want 16 (12 busy + 2 quiet + 2 edits; the two-day-old save is outside the window)", res.RowsInWindow)
	}
	if res.NoOpRows != 12 {
		t.Errorf("NoOpRows = %d, want 12 (11 restatements of busy, 1 of quiet; an edit that changes state is not a restatement)", res.NoOpRows)
	}
	// 12/16 is exact in binary, so this asserts the division and not a rounding.
	if !almostEqual(res.NoOpShare, 0.75) {
		t.Errorf("NoOpShare = %v, want 0.75", res.NoOpShare)
	}
	if res.TotalRows != 17 {
		t.Errorf("TotalRows = %d, want 17 — the store total is all-time, so it counts the two-day-old row the window does not", res.TotalRows)
	}
	if res.MaxVersions != 12 {
		t.Errorf("MaxVersions = %d, want 12 (the busiest memory holds 12 versions)", res.MaxVersions)
	}
	if res.BusiestMemoryRows != 12 {
		t.Errorf("BusiestMemoryRows = %d, want 12 (busy wrote 12 rows in the window)", res.BusiestMemoryRows)
	}
	if res.PerMemoryCap != int64(historyVersionsPerMemory) {
		t.Errorf("PerMemoryCap = %d, want the policy's %d", res.PerMemoryCap, historyVersionsPerMemory)
	}
	if res.StoreCap != int64(historyRowsCap) {
		t.Errorf("StoreCap = %d, want the policy's %d", res.StoreCap, historyRowsCap)
	}
	if res.WindowHours != 24 {
		t.Errorf("WindowHours = %d, want 24", res.WindowHours)
	}

	// The projections are per-memory, not global: each memory is measured against
	// ITS OWN count and ITS OWN rate, and the smallest time wins. busy is the
	// answer (50-12 versions left, 12 a day), which is not the memory with the
	// most rows overall nor the one with the highest rate on its own count.
	if !res.Projected {
		t.Error("Projected = false on a store that wrote 16 rows in the window, want true")
	}
	wantPerMemory := float64(historyVersionsPerMemory-12) / 12 // busy: 38 left, 12 a day
	if !almostEqual(res.DaysToPerMemoryCap, wantPerMemory) {
		t.Errorf("DaysToPerMemoryCap = %v, want %v (the busiest memory, not the largest)", res.DaysToPerMemoryCap, wantPerMemory)
	}
	// The store is nowhere near its cap: 19983 rows to go at 16 a day.
	wantStore := float64(historyRowsCap-17) / 16
	if !almostEqual(res.DaysToStoreCap, wantStore) {
		t.Errorf("DaysToStoreCap = %v, want %v", res.DaysToStoreCap, wantStore)
	}

	if got := warningKinds(res.Warnings); !wantKinds(got, HistoryWarnNoOpShare, HistoryWarnPerMemoryCap) {
		t.Errorf("warning kinds = %v, want [%s %s] — a 75%% restatement rate and a memory 3.2 days from its cap",
			got, HistoryWarnNoOpShare, HistoryWarnPerMemoryCap)
	}

	// A status line is read while the store is in use, and it runs on every
	// `ghost mcp status`, so it has to be a read. Both tables, compared by
	// identity rather than by count.
	if after := historyRowIDs(t, s); len(after) != len(before) {
		t.Errorf("HistoryGrowth changed the history table: %d rows before, %d after", len(before), len(after))
	} else {
		for i := range before {
			if before[i] != after[i] {
				t.Fatalf("HistoryGrowth changed history row %d (rowid %d -> %d)", i, before[i], after[i])
			}
		}
	}
	if after := memoryRowCount(t, s); after != memoriesBefore {
		t.Errorf("HistoryGrowth changed the memories table: %d rows before, %d after", memoriesBefore, after)
	}
}

// TestHistoryGrowthWarningsQuoteTheirOwnNumbers: a warning nobody can act on is
// a warning nobody acts on, and two surfaces render these (the status line and
// ghost_health) from ONE Detail string precisely so they cannot say different
// things. This holds each Detail to naming the number that tripped it, the
// threshold it tripped against, and — for the no-op share, the finding a user
// can actually DO something about — the command that repairs it.
func TestHistoryGrowthWarningsQuoteTheirOwnNumbers(t *testing.T) {
	s := testStore(t)
	seedGrowthFixture(t, s)

	res, err := s.HistoryGrowth(context.Background())
	if err != nil {
		t.Fatalf("HistoryGrowth: %v", err)
	}
	byKind := map[string]HistoryWarning{}
	for _, w := range res.Warnings {
		byKind[w.Kind] = w
	}

	noOp, ok := byKind[HistoryWarnNoOpShare]
	if !ok {
		t.Fatalf("no %s warning in %v", HistoryWarnNoOpShare, warningKinds(res.Warnings))
	}
	for _, want := range []string{"75%", "20%", "16", "ghost history compact"} {
		if !strings.Contains(noOp.Detail, want) {
			t.Errorf("%s Detail does not mention %q:\n%s", HistoryWarnNoOpShare, want, noOp.Detail)
		}
	}

	cap, ok := byKind[HistoryWarnPerMemoryCap]
	if !ok {
		t.Fatalf("no %s warning in %v", HistoryWarnPerMemoryCap, warningKinds(res.Warnings))
	}
	for _, want := range []string{"12", "50", "3.2", "14"} {
		if !strings.Contains(cap.Detail, want) {
			t.Errorf("%s Detail does not mention %q:\n%s", HistoryWarnPerMemoryCap, want, cap.Detail)
		}
	}
}

// TestHistoryGrowthWarnsOnlyAboveTheNoOpShareThreshold holds the boundary, and
// it is the boundary that matters: at exactly the threshold the report is silent,
// because a store writing one restatement in five is the ordinary shape of a
// lifecycle that re-emits a memory it decided to keep. One more in twenty and it
// warns.
//
// The cap projections are held out of the way on purpose — sixteen memories of
// one or two rows each sit 24 days from the per-memory cap and about a thousand
// days from the store cap, so the ONLY thing that can move the warning list
// between these two cases is the share. A test where the at-threshold case also
// tripped a cap warning would pass with a broken threshold.
func TestHistoryGrowthWarnsOnlyAboveTheNoOpShareThreshold(t *testing.T) {
	// Four restatements among sixteen saves is exactly the threshold (4 of 20
	// window rows); a fifth is over it (5 of 21). Each restated memory is a save
	// plus one verbatim; the rest are a save alone, which is also what holds the
	// per-memory projection back — a memory that wrote nothing in the window is
	// not one the report should place a countdown on.
	const memories = 16
	for _, tc := range []struct {
		name      string
		restated  int
		wantKinds []string
	}{
		{"at the threshold", 4, nil},
		{"above the threshold", 5, []string{HistoryWarnNoOpShare}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			for i := range memories {
				id := createCompactMemory(t, s, fmt.Sprintf("the relay %d keeps its own address book", i))
				if i < tc.restated {
					appendVerbatimVersion(t, s, id)
				}
			}
			res, err := s.HistoryGrowth(context.Background())
			if err != nil {
				t.Fatalf("HistoryGrowth: %v", err)
			}
			if want := int64(memories + tc.restated); res.RowsInWindow != want {
				t.Fatalf("RowsInWindow = %d, want %d (%d saves plus %d restatements)", res.RowsInWindow, want, memories, tc.restated)
			}
			if res.NoOpRows != int64(tc.restated) {
				t.Fatalf("NoOpRows = %d, want %d", res.NoOpRows, tc.restated)
			}
			want := float64(tc.restated) / float64(memories+tc.restated)
			if !almostEqual(res.NoOpShare, want) {
				t.Errorf("NoOpShare = %v, want %v", res.NoOpShare, want)
			}
			// Held out of the way, and asserted so the case cannot drift into
			// passing for a reason that is not the threshold.
			if res.DaysToPerMemoryCap < HistoryCapHorizonDays {
				t.Errorf("DaysToPerMemoryCap = %v, want at least %d — the fixture is supposed to isolate the share", res.DaysToPerMemoryCap, HistoryCapHorizonDays)
			}
			if res.DaysToStoreCap < HistoryCapHorizonDays {
				t.Errorf("DaysToStoreCap = %v, want at least %d — the fixture is supposed to isolate the share", res.DaysToStoreCap, HistoryCapHorizonDays)
			}
			if got := warningKinds(res.Warnings); !wantKinds(got, tc.wantKinds...) {
				t.Errorf("warning kinds = %v, want %v at a %v share", got, tc.wantKinds, want)
			}
		})
	}
}

// TestHistoryGrowthOnAnEmptyHistoryReportsZeros: a store nobody has written to
// is not a store with a problem, and a report that divides by a zero rows, or
// projects a rate of nothing, or warns about a cap it is 20000 rows from, is a
// report that cries wolf on every fresh install — which is the first thing every
// user sees.
func TestHistoryGrowthOnAnEmptyHistoryReportsZeros(t *testing.T) {
	s := testStore(t)

	res, err := s.HistoryGrowth(context.Background())
	if err != nil {
		t.Fatalf("HistoryGrowth: %v", err)
	}
	if res.RowsInWindow != 0 || res.NoOpRows != 0 || res.TotalRows != 0 {
		t.Errorf("counts on an empty store = %d rows, %d no-ops, %d total; want zero for all three",
			res.RowsInWindow, res.NoOpRows, res.TotalRows)
	}
	if res.NoOpShare != 0 {
		t.Errorf("NoOpShare = %v on an empty store, want 0 (not NaN, not 1)", res.NoOpShare)
	}
	if res.MaxVersions != 0 || res.BusiestMemoryRows != 0 {
		t.Errorf("MaxVersions = %d, BusiestMemoryRows = %d on an empty store, want 0 and 0", res.MaxVersions, res.BusiestMemoryRows)
	}
	if res.Projected {
		t.Error("Projected = true on an empty store, want false — there is no rate to project")
	}
	if res.DaysToPerMemoryCap != 0 || res.DaysToStoreCap != 0 {
		t.Errorf("day projections on an empty store = %v and %v, want 0 and 0", res.DaysToPerMemoryCap, res.DaysToStoreCap)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings on an empty store = %v, want none", warningKinds(res.Warnings))
	}
	// The caps are still reported on an empty store: they are the policy, and a
	// report that could not say what it is measuring against could not explain a
	// store that is close to one.
	if res.PerMemoryCap != int64(historyVersionsPerMemory) || res.StoreCap != int64(historyRowsCap) {
		t.Errorf("caps on an empty store = %d and %d, want %d and %d",
			res.PerMemoryCap, res.StoreCap, historyVersionsPerMemory, historyRowsCap)
	}
}

// TestHistoryGrowthOnAQuietStoreProjectsNothing is the empty-window half of the
// case above, and it is the commoner of the two: a store nobody has written to
// TODAY, which is every store at 9am and every store whose lifecycle has not run.
// Its table is not empty, so the caps still see it — but nothing moved, so there
// is no rate to divide by, and a report that divided anyway would put an infinite
// number of days (or a NaN) into the line a user reads. Silence is the honest
// answer here, and it is a different answer from the empty store's, which is why
// this is a separate test rather than another case of the one above.
func TestHistoryGrowthOnAQuietStoreProjectsNothing(t *testing.T) {
	s := testStore(t)
	id := createCompactMemory(t, s, compactFirstText)
	appendVerbatimVersion(t, s, id)
	// Both rows are two days old, so the window is empty while the table is not.
	for _, phase := range []string{phaseSave, phaseReflect} {
		stampHistoryRow(t, s, id, phase, stampAgo(2*growthDays))
	}

	res, err := s.HistoryGrowth(context.Background())
	if err != nil {
		t.Fatalf("HistoryGrowth: %v", err)
	}
	if res.RowsInWindow != 0 || res.NoOpRows != 0 || res.NoOpShare != 0 {
		t.Errorf("window counts on a quiet store = %d rows, %d no-ops, %v share; want zero for all three",
			res.RowsInWindow, res.NoOpRows, res.NoOpShare)
	}
	if res.Projected {
		t.Error("Projected = true on a store that wrote nothing in the window — there is no rate to project from")
	}
	if res.DaysToPerMemoryCap != 0 || res.DaysToStoreCap != 0 {
		t.Errorf("day projections on a quiet store = %v and %v, want 0 and 0", res.DaysToPerMemoryCap, res.DaysToStoreCap)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings on a quiet store = %v, want none", warningKinds(res.Warnings))
	}
	// The caps are still measured: a memory is holding versions whether or not
	// anyone wrote any today, and that is the number an operator came for.
	if res.TotalRows != 2 || res.MaxVersions != 2 {
		t.Errorf("TotalRows = %d, MaxVersions = %d, want 2 and 2 — the table is not empty, only the window is", res.TotalRows, res.MaxVersions)
	}
	if res.BusiestMemoryRows != 0 {
		t.Errorf("BusiestMemoryRows = %d on a quiet store, want 0", res.BusiestMemoryRows)
	}
}

// TestHistoryGrowthCountsOnlyTheWindow pins the difference between the two
// halves of the report, which run on different clocks and would silently
// converge on one if they were computed from the same rows: the SHARE is a
// statement about the last 24 hours, while the caps are a statement about the
// table as it stands. A row that is a no-op but two days old counts towards the
// cap pressure and must not count towards the rate the user is being warned
// about.
func TestHistoryGrowthCountsOnlyTheWindow(t *testing.T) {
	s := testStore(t)
	id := createCompactMemory(t, s, compactFirstText)
	appendVerbatimVersion(t, s, id) // in the window
	appendVersionRow(t, s, id, phaseReflect, stampAgo(2*growthDays), "", "", nil)
	appendVersionRow(t, s, id, phaseReflect, stampAgo(3*growthDays), "", "", nil)

	res, err := s.HistoryGrowth(context.Background())
	if err != nil {
		t.Fatalf("HistoryGrowth: %v", err)
	}
	if res.RowsInWindow != 2 {
		t.Errorf("RowsInWindow = %d, want 2 — the two rows recorded two and three days ago are outside the window", res.RowsInWindow)
	}
	// All three restatements are no-ops by the compaction's own rule; only one of
	// them is inside the window. A share computed over the whole table would be
	// 3/4 here, and a rate computed over the window must not see the other two.
	if res.NoOpRows != 1 {
		t.Errorf("NoOpRows = %d, want 1 — the window excludes the two older restatements", res.NoOpRows)
	}
	if !almostEqual(res.NoOpShare, 0.5) {
		t.Errorf("NoOpShare = %v, want 0.5 (1 of 2 window rows)", res.NoOpShare)
	}
	if res.TotalRows != 4 || res.MaxVersions != 4 {
		t.Errorf("TotalRows = %d, MaxVersions = %d, want 4 and 4 — the caps are measured over the table, not the window", res.TotalRows, res.MaxVersions)
	}
	// The projection is over what this memory has left (50-4) at what it wrote
	// in the window (2 rows a day), so the older rows affect the numerator and
	// not the denominator. A report that divided by the all-time row count would
	// say 11.5 days here instead of 23.
	want := float64(historyVersionsPerMemory-4) / 2
	if !almostEqual(res.DaysToPerMemoryCap, want) {
		t.Errorf("DaysToPerMemoryCap = %v, want %v", res.DaysToPerMemoryCap, want)
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
		appendVerbatimVersion(t, s, noisy)
	}

	// b: a single restatement, which is entirely the newest version.
	quiet := createCompactMemory(t, s, compactSecondText)
	appendVerbatimVersion(t, s, quiet)

	// c: a resolve, which really changed resolved_at, then a restatement of the
	// resolved state.
	resolved := createCompactMemory(t, s, "the tablet is paired with the kiosk")
	if _, err := s.SetResolved(ctx, []string{resolved}); err != nil {
		t.Fatalf("SetResolved: %v", err)
	}
	appendVerbatimVersion(t, s, resolved)

	// d: a reflect that names another memory — an edge a reader follows from this
	// memory's history into its successor's (#648) — and then a restatement.
	// Both restate the state, and neither is removable: one records a claim
	// rather than a state, the other is the newest version.
	linked := createCompactMemory(t, s, "the kiosk polls the relay every minute")
	appendVersionRow(t, s, linked, phaseReflect, nil, quiet, "", nil)
	appendVerbatimVersion(t, s, linked)

	res, err := s.HistoryGrowth(ctx)
	if err != nil {
		t.Fatalf("HistoryGrowth: %v", err)
	}
	// 5 + 1 + 1 + 2: the resolve is a real change, and so is the edge row's
	// neighbour; everything else restates the row before it.
	if res.NoOpRows != 9 {
		t.Fatalf("NoOpRows = %d, want 9", res.NoOpRows)
	}

	dryRunStart := historyRowIDs(t, s)

	// A dry run first: the count the APPLY would produce, from the same SQL and
	// the same decision, with nothing written — which is also the assertion that
	// this report is a preview of that command rather than a rival to it.
	preview, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{})
	if err != nil {
		t.Fatalf("CompactHistory (dry run): %v", err)
	}
	if preview.Removed != 4 {
		t.Fatalf("compact dry run removed = %d, want 4", preview.Removed)
	}
	if preview.Removed > res.NoOpRows {
		t.Fatalf("compact would remove %d rows from a history holding %d restatements — the repair removes a subset, never more",
			preview.Removed, res.NoOpRows)
	}
	if got, want := res.NoOpRows-preview.Removed, int64(5); got != want {
		t.Errorf("restatements the repair would keep = %d, want %d", got, want)
	}
	if after := historyRowIDs(t, s); len(after) != len(dryRunStart) {
		t.Errorf("the dry run changed the history: %d rows, want the %d it started with", len(after), len(dryRunStart))
	}

	// Now the apply, so the keepers are OBSERVED rather than inferred, and each
	// one is named. Every memory keeps its save; the resolved memory keeps the
	// resolve; the edge row and the four older restatements are gone; and one
	// restatement survives per memory as that memory's newest version.
	applied, err := s.CompactHistory(ctx, testProject, HistoryCompactOptions{Apply: true})
	if err != nil {
		t.Fatalf("CompactHistory (apply): %v", err)
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

// TestHistoryGrowthIsLinearInTableSize is the canary on the plan pin above: a
// plan assertion says the shape is right today, and this says what the shape
// costs, so a rewrite that keeps an index name in the plan while doing the work
// twice still fails.
//
// Sizes are 1,000 and 8,000 rows — 8x, with every row inside the window, which
// is the expensive end: the restatement test then runs its sub-select for every
// row in the table rather than for the handful a quiet store writes in a day.
// Rows are spread over 100 and 800 memories at a constant 10 each, so the growth
// under test is the table, not one memory's history. One memory with 8,000 rows
// would measure a different (and much worse) thing, and it cannot happen: the
// per-memory cap is 50.
//
// Measured on this machine at 20,000 rows: 1.7 ms with a realistic window and
// 183 ms with every row inside it, on a table the retention cap holds at 20,000.
func TestHistoryGrowthIsLinearInTableSize(t *testing.T) {
	const memories, restatements = 100, 9
	small := growthLoadStore(t, memories, restatements)
	big := growthLoadStore(t, 8*memories, restatements)

	const rounds = 5
	var ratios []float64
	for range rounds {
		start := time.Now()
		if _, err := small.HistoryGrowth(context.Background()); err != nil {
			t.Fatalf("HistoryGrowth on the small store: %v", err)
		}
		smallElapsed := time.Since(start)
		start = time.Now()
		if _, err := big.HistoryGrowth(context.Background()); err != nil {
			t.Fatalf("HistoryGrowth on the large store: %v", err)
		}
		bigElapsed := time.Since(start)
		if smallElapsed <= 0 || bigElapsed <= 0 {
			t.Fatal("a measurement was zero, so the ratio is meaningless")
		}
		ratios = append(ratios, float64(bigElapsed)/float64(smallElapsed))
	}
	sort.Float64s(ratios)
	median := ratios[len(ratios)/2]
	t.Logf("per-round ratios %.2f..%.2f, median %.2fx for 8x the rows",
		ratios[0], ratios[len(ratios)-1], median)

	// The geometric midpoint between linear (8x) and quadratic (64x), which is
	// the tightest bar that treats the two failure directions symmetrically. It
	// reads far above the measurement on purpose: this is a canary, and a bar
	// set at the number CI happened to produce is a bar that goes red the first
	// time a runner is busy.
	if median > 24 {
		t.Errorf("8x the rows cost %.2fx the time (median of %d rounds) — worse than linear", median, rounds)
	}

	// And the absolute floor, because a ratio cannot see a cost that is linear
	// but enormous. 20 s on 8,000 rows is two orders of magnitude above the
	// measurement; the regression it exists to catch — the restatement test
	// scanning the table per row — is 64 million row visits.
	start := time.Now()
	if _, err := big.HistoryGrowth(context.Background()); err != nil {
		t.Fatalf("HistoryGrowth on the large store: %v", err)
	}
	elapsed := time.Since(start)
	t.Logf("%d history rows in %v", 8*memories*(restatements+1), elapsed)
	if elapsed > 20*time.Second {
		t.Errorf("HistoryGrowth on %d rows took %v, want well under 20s", 8*memories*(restatements+1), elapsed)
	}
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
