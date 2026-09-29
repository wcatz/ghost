package memory

// #646 part 1: the retrieval record — what a formatted retrieval call RETRIEVED
// and what it KEPT, persisted so the next part can compare it against the
// transcript.
//
// The properties here are the ones the issue's own constraint states: verdicts
// only, ids only, no text. Everything else about the record is decided by what a
// later reader has to be able to tell apart, which is why the tests below pin
// the SURFACE (search vs session_start) and the OUTCOME as first-class columns
// rather than leaving them to be inferred from an empty field.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleRetrievalRecord() RetrievalRecord {
	return RetrievalRecord{
		ProjectID: testProject,
		Source:    "search",
		QueryHash: digest(1),
		Outcome:   "answerable",
		Reason:    "floor_met",
		Verdicts: []RowVerdict{
			{ID: "MEM-1", Kept: true, Stage: "validity", Reason: "valid"},
			{ID: "MEM-2", Kept: false, Stage: "validity", Reason: "expired"},
		},
	}
}

// digest is a distinct 64-character hex hash per call number — the shape the
// column's CHECK admits, and the shape assemble produces. Written as a number
// rather than a letter so a fixture cannot drift out of the vocabulary the
// constraint states: the constraint is the point (a column that cannot hold text
// cannot hold text), so a test that needed a non-hex hash to be distinguishable
// would be asking the constraint to be dropped.
func digest(n int) string { return fmt.Sprintf("%064x", n) }

// TestRecordRetrievalStoresOneRowPerCall: the record's grain. One formatted
// call is one row, so a reader can count calls.
//
// The tempting alternative — a row per (call, memory) pair — is what makes the
// "did the agent use what Ghost injected" question unanswerable, because the
// denominator of that ratio is CALLS, and a table that only holds the calls that
// happened to admit something has already dropped the ones worth auditing.
func TestRecordRetrievalStoresOneRowPerCall(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for _, rec := range []RetrievalRecord{
		// An answerable call.
		sampleRetrievalRecord(),
		// An empty call, which is the one a report most needs and the one a
		// "only record what we returned" design would drop.
		{ProjectID: testProject, Source: "search", QueryHash: digest(2),
			Outcome: "empty", Reason: "no_candidates"},
		// A session-start injection, which carries no query at all.
		{ProjectID: testProject, Source: "session_start", QueryHash: "",
			Outcome: "answerable", Reason: "floor_met",
			Verdicts: []RowVerdict{{ID: "MEM-3", Kept: true, Stage: "validity", Reason: "valid"}}},
	} {
		if err := s.RecordRetrieval(ctx, rec); err != nil {
			t.Fatalf("RecordRetrieval(%s/%s): %v", rec.Source, rec.Outcome, err)
		}
	}

	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil {
		t.Fatalf("count retrieval rows: %v", err)
	}
	if n != 3 {
		t.Errorf("retrieval_record holds %d rows after three calls, want 3 — the grain is not one row per call", n)
	}
}

// TestRecordRetrievalStoresVerdictsAndNotText: the record's whole content
// discipline, asserted against the stored bytes rather than against the Go
// struct.
//
// A struct assertion ("RetrievalRecord has no Content field") is a claim about
// THIS build's type; this one is a claim about what is on the disk, which is
// what a reader of the store — or a purge, or an export — will actually meet. The
// text is searched for across every column of the row, not only the ones the
// insert names, so a column added later without thought is caught here rather
// than in production.
func TestRecordRetrievalStoresVerdictsAndNotText(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const query = "how does the k3s ingress controller rotate its certificate"
	const content = "the ingress controller reloads its certificate on SIGHUP only"

	rec := sampleRetrievalRecord()
	if err := s.RecordRetrieval(ctx, rec); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}

	leaked, err := countOccurrences(t, s.db, query)
	if err != nil {
		t.Fatalf("scan for the query text: %v", err)
	}
	if leaked != 0 {
		t.Errorf("the query text survives in %d row(s) of this database", leaked)
	}
	leaked, err = countOccurrences(t, s.db, content)
	if err != nil {
		t.Fatalf("scan for the content text: %v", err)
	}
	if leaked != 0 {
		t.Errorf("a memory's content survives in %d row(s) of this database", leaked)
	}
}

// TestRecordRetrievalRoundTripsEveryVerdict: the verdicts have to come back
// EXACTLY, because the audit compares them against the transcript — a kept row
// read back as dropped, or a reason lost, turns a real report into a false
// contradiction.
func TestRecordRetrievalRoundTripsEveryVerdict(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	want := sampleRetrievalRecord()
	want.SessionID = "sess-abc"
	want.AsOf = "2026-09-01T00:00:00Z"
	if err := s.RecordRetrieval(ctx, want); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}

	got, err := s.RetrievalRecords(ctx, 10)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read back %d records, want 1", len(got))
	}
	r := got[0]
	if r.ProjectID != want.ProjectID || r.Source != want.Source || r.QueryHash != want.QueryHash {
		t.Errorf("header = (%q, %q, %q), want (%q, %q, %q)",
			r.ProjectID, r.Source, r.QueryHash, want.ProjectID, want.Source, want.QueryHash)
	}
	if r.SessionID != want.SessionID {
		t.Errorf("session_id = %q, want %q", r.SessionID, want.SessionID)
	}
	if r.AsOf != want.AsOf {
		t.Errorf("as_of = %q, want %q", r.AsOf, want.AsOf)
	}
	if r.Outcome != want.Outcome || r.Reason != want.Reason {
		t.Errorf("verdict = (%q, %q), want (%q, %q)", r.Outcome, r.Reason, want.Outcome, want.Reason)
	}
	if len(r.Verdicts) != len(want.Verdicts) {
		t.Fatalf("read %d verdicts, want %d: %+v", len(r.Verdicts), len(want.Verdicts), r.Verdicts)
	}
	for i := range want.Verdicts {
		if r.Verdicts[i] != want.Verdicts[i] {
			t.Errorf("verdict %d = %+v, want %+v", i, r.Verdicts[i], want.Verdicts[i])
		}
	}
	if r.RecordedAt == "" {
		t.Error("recorded_at is empty — a record with no instant cannot be placed against a transcript")
	}
}

// TestRetrievalRecordsAreNewestFirst: the reader's order, which the cap depends
// on. recorded_at is second-precision, so two calls inside one second have no
// defined order by it; rowid is the insertion order and is never reused.
func TestRetrievalRecordsAreNewestFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i := range 3 {
		rec := sampleRetrievalRecord()
		rec.QueryHash = digest(i)
		if err := s.RecordRetrieval(ctx, rec); err != nil {
			t.Fatalf("RecordRetrieval %d: %v", i, err)
		}
	}
	got, err := s.RetrievalRecords(ctx, 2)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("a limit of 2 returned %d records", len(got))
	}
	if got[0].QueryHash != digest(2) || got[1].QueryHash != digest(1) {
		t.Errorf("records came back newest-last: the reader returned call %d then call %d, want 2 then 1",
			digestOf(got[0].QueryHash), digestOf(got[1].QueryHash))
	}
}

// digestOf reads a fixture digest back as the number it was built from, so a
// failure names the call rather than 64 hex characters.
func digestOf(hash string) int {
	var n int
	if _, err := fmt.Sscanf(hash, "%x", &n); err != nil {
		return -1
	}
	return n
}

// TestRecordRetrievalCapsTheTableOldestFirst: the growth policy, and it is
// inside the appending transaction so a table cannot exceed the cap even
// transiently — the same reason memory_history's prune rides along with the
// append.
func TestRecordRetrievalCapsTheTableOldestFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	restore := retrievalRecordRowsCap
	retrievalRecordRowsCap = 10
	t.Cleanup(func() { retrievalRecordRowsCap = restore })

	for i := range 25 {
		rec := sampleRetrievalRecord()
		rec.QueryHash = digest(i)
		if err := s.RecordRetrieval(ctx, rec); err != nil {
			t.Fatalf("RecordRetrieval %d: %v", i, err)
		}
	}

	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil {
		t.Fatalf("count retrieval rows: %v", err)
	}
	if n > retrievalRecordRowsCap {
		t.Errorf("retrieval_record holds %d rows, want at most the cap of %d", n, retrievalRecordRowsCap)
	}
	got, err := s.RetrievalRecords(ctx, retrievalRecordRowsCap)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	// Newest kept, oldest gone: of 25 calls at a cap of 10, the window is calls
	// 15..24 — the last one written and the sixteenth.
	if n := digestOf(got[0].QueryHash); n != 24 {
		t.Errorf("newest record is call %d, want the last one written (24) — the cap kept the oldest rows", n)
	}
	if n := digestOf(got[len(got)-1].QueryHash); n != 15 {
		t.Errorf("oldest surviving record is call %d, want the sixteenth written (15) — the cap kept a different window", n)
	}
}

// TestRecordRetrievalIsRefusedOnAStoreANewerGhostOwns: the #746 write refusal
// reaches the new table.
//
// A retrieval record is the audit trail of what an agent was told, and a stale
// server's records are exactly the ones that lie — the store it wrote them into
// may since have gained a stage, a validity rule or a retention tier the
// verdict vocabulary of that day cannot name. Recording into it under a newer
// schema is the same unaudited write #746 exists to stop, so the record is
// refused the same way, and the refusal is the one error a caller can match.
func TestRecordRetrievalIsRefusedOnAStoreANewerGhostOwns(t *testing.T) {
	s, path := fileBackedStore(t)
	ctx := context.Background()

	if err := s.RecordRetrieval(ctx, sampleRetrievalRecord()); err != nil {
		t.Fatalf("RecordRetrieval before the bump: %v", err)
	}

	stampFromAnotherHandle(t, path, schemaVersion+1)

	err := s.RecordRetrieval(ctx, sampleRetrievalRecord())
	if !errors.Is(err, ErrStoreNewer) {
		t.Fatalf("RecordRetrieval on a newer store: err = %v, want a refusal matching ErrStoreNewer", err)
	}

	// The refusal rolled back rather than half-writing: the row count is still
	// what the pre-bump call left.
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil {
		t.Fatalf("count retrieval rows: %v", err)
	}
	if n != 1 {
		t.Errorf("retrieval_record holds %d rows after a refused write, want the 1 written before the bump", n)
	}
}

// TestPurgeRemovesTheRetrievalRecordsNamingTheMemory: a purge has to reach this
// table, and the reason is the one memory_provenance already has.
//
// A retrieval record carries no text of the memory — but it does NAME it, and a
// name is exactly what a redaction is asked to remove when the id itself is
// sensitive (a memory named for a customer, a credential's identifier, an
// incident ticket). Leaving the row would report success on a redaction that the
// record still holds: Store.RetrievalRecords reads the id back out of it today,
// and `ghost context --audit` — the report this issue's next part adds — will
// read it out of a store the operator believed they had redacted.
func TestPurgeRemovesTheRetrievalRecordsNamingTheMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	rec := sampleRetrievalRecord()
	rec.Verdicts = []RowVerdict{
		{ID: "SECRET-MEMORY-ID", Kept: true, Stage: "validity", Reason: "valid"},
		{ID: "OTHER-MEMORY-ID", Kept: true, Stage: "validity", Reason: "valid"},
	}
	if err := s.RecordRetrieval(ctx, rec); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	// A second call that never touched the memory, so the purge's reach is
	// measured rather than assumed.
	if err := s.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: testProject, Source: "search", QueryHash: digest(3),
		Outcome: "answerable", Reason: "floor_met",
		Verdicts: []RowVerdict{{ID: "UNRELATED-MEMORY-ID", Kept: true, Stage: "validity", Reason: "valid"}},
	}); err != nil {
		t.Fatalf("RecordRetrieval (unrelated): %v", err)
	}

	if _, err := s.PurgeMemoryHistory(ctx, "SECRET-MEMORY-ID"); err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}

	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM retrieval_record WHERE EXISTS (
			SELECT 1 FROM json_each(retrieval_record.verdicts) WHERE value->>'id' = ?
		)`, "SECRET-MEMORY-ID").Scan(&n); err != nil {
		t.Fatalf("count records naming the purged id: %v", err)
	}
	if n != 0 {
		t.Errorf("%d retrieval record(s) still name the purged memory", n)
	}
	// And the unrelated call survived, so the purge is a targeted reach rather
	// than a table wipe.
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil {
		t.Fatalf("count retrieval rows: %v", err)
	}
	if n != 1 {
		t.Errorf("retrieval_record holds %d rows after the purge, want 1 (the unrelated call)", n)
	}
}

// TestPurgeToleratesARowWhoseVerdictsAreNotJSON: the purge predicate reads the
// verdicts column with json_each, and json_each RAISES on a document it cannot
// read rather than skipping it.
//
// The second fixture is the one that matters, and it is NOT a malformed
// document: `{"id":"X"}` is perfectly valid JSON and json_each WALKS it without
// complaint. What refuses it is the ->> path accessor, applied to the string
// json_each yields from it (measured on the SQLite 3.53.4 this build links: the
// two raise in different places — json_each on `not json at all`, ->> on
// `{"id":"X"}`). So a guard written as `json_valid(verdicts) AND ...` would pass
// this test's first fixture and still raise on its second, and the only shape
// that tolerates both substitutes a readable array before either function sees
// the column. See readableVerdicts for the full table.
//
// Every row Ghost writes is well-formed, so this is a hand-edited store, a
// restored snapshot of an older build, or a future writer that changed the
// shape. A purge that REFUSES on one of those is worse than the leak it was
// asked to close: the operator's remedy is to stop purging, not to fix the row.
func TestPurgeToleratesARowWhoseVerdictsAreNotJSON(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A row whose verdicts column is not a JSON document at all, and one that is
	// JSON but not the array shape.
	for _, bad := range []string{`not json at all`, `{"id":"X"}`} {
		if _, err := s.db.Exec(
			`INSERT INTO retrieval_record (project_id, source, query_hash, outcome, reason, verdicts)
			 VALUES (?, 'search', ?, 'answerable', 'floor_met', ?)`,
			testProject, digest(4), bad,
		); err != nil {
			t.Fatalf("insert a malformed row (%q): %v", bad, err)
		}
	}
	// A well-formed row naming the memory, so the purge has real work to do
	// alongside the malformed ones.
	rec := sampleRetrievalRecord()
	rec.Verdicts = []RowVerdict{{ID: "SECRET-MEMORY-ID", Kept: true, Stage: "validity", Reason: "valid"}}
	if err := s.RecordRetrieval(ctx, rec); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}

	if _, err := s.PurgeMemoryHistory(ctx, "SECRET-MEMORY-ID"); err != nil {
		t.Fatalf("PurgeMemoryHistory on a store with malformed verdicts: %v", err)
	}

	// Matched as TEXT, not through json_each: the two malformed rows are still
	// in the table, and json_each raises on a document it cannot read — which is
	// the very failure the purge has to tolerate, so an assertion that raised on
	// it would fail the test for the wrong reason and prove nothing. A substring
	// match is total, and it is the stronger claim anyway: the purged id appears
	// nowhere in the column, not merely nowhere in a parsed id field.
	var n int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM retrieval_record WHERE verdicts LIKE '%SECRET-MEMORY-ID%'`,
	).Scan(&n); err != nil {
		t.Fatalf("count records naming the purged id: %v", err)
	}
	if n != 0 {
		t.Errorf("%d retrieval record(s) still name the purged memory", n)
	}
	// The malformed rows are not the purge's business — it removes rows that
	// NAME the memory, and neither of these does — but they must still be there,
	// because a purge that silently deleted unparseable rows would be discarding
	// the audit trail of every other call.
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil {
		t.Fatalf("count retrieval rows: %v", err)
	}
	if n != 2 {
		t.Errorf("retrieval_record holds %d rows after the purge, want 2 (the two malformed ones)", n)
	}
}

// TestPurgeReachesARowWhoseVerdictsNameTheMemoryButCannotBeRead: the gap a
// parsed-only predicate leaves, and it is the one that matters here.
//
// readableVerdicts exists so json_each cannot RAISE on a column it cannot read,
// and that is a real fix — but it has a cost the reader's tolerance does not: a
// substituted '[]' is an empty array, so a row whose verdicts column is malformed
// matches nothing. If such a row happens to name the memory, a parsed-only
// purge reports success and leaves the name in the store. A redaction that says
// it erased an id and did not is worse than one that deleted an extra audit row,
// so the predicate also matches the id textually — and the test pins that
// direction by requiring the UNREADABLE row to go.
func TestPurgeReachesARowWhoseVerdictsNameTheMemoryButCannotBeRead(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A row whose verdicts column is not a document at all, and which still names
	// the memory inside it. Nothing Ghost writes produces this; a hand-edited
	// store, a restored snapshot, or a future writer that changed the shape does.
	if _, err := s.db.Exec(
		`INSERT INTO retrieval_record (project_id, source, query_hash, outcome, reason, verdicts)
		 VALUES (?, 'search', ?, 'answerable', 'floor_met', ?)`,
		testProject, digest(9), `[{"id":"SECRET-MEMORY-ID","kept":true} truncated`,
	); err != nil {
		t.Fatalf("insert an unreadable row naming the memory: %v", err)
	}
	// And one that names nothing, so the reach is measured rather than assumed.
	if _, err := s.db.Exec(
		`INSERT INTO retrieval_record (project_id, source, query_hash, outcome, reason, verdicts)
		 VALUES (?, 'search', ?, 'answerable', 'floor_met', 'also not json')`,
		testProject, digest(10),
	); err != nil {
		t.Fatalf("insert an unreadable row naming nothing: %v", err)
	}

	if _, err := s.PurgeMemoryHistory(ctx, "SECRET-MEMORY-ID"); err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}

	var left int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM retrieval_record WHERE verdicts LIKE '%SECRET-MEMORY-ID%'`,
	).Scan(&left); err != nil {
		t.Fatalf("count rows naming the purged id: %v", err)
	}
	if left != 0 {
		t.Errorf("%d unreadable record(s) still name the purged memory — the purge reported success over a name it left behind", left)
	}
	// The row that names nothing survives, so the textual arm is a targeted reach
	// rather than a wipe of every unreadable row.
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&left); err != nil {
		t.Fatalf("count retrieval rows: %v", err)
	}
	if left != 1 {
		t.Errorf("retrieval_record holds %d rows after the purge, want 1 (the unreadable row naming nothing)", left)
	}
}

// TestPurgeDoesNotTreatAnIdAsAPattern: the textual arm must be a LITERAL
// substring search, and the difference is not a detail.
//
// `ghost import` writes an artifact's ids verbatim, so an id in a real store can
// be anything the artifact carried — including `%` and `_`. A LIKE-based arm
// reads both as wildcards, so purging the id `A_1` would also match the
// unrelated row whose verdicts name `Ax1`, and delete another call's audit
// record for a memory nobody asked to erase. Measured on this build's SQLite:
// instr finds `A_1` in `[{"id":"A_1"}]` and does not find it in `[{"id":"Ax1"}]`,
// while LIKE matches both. The row that must SURVIVE is the assertion; a purge
// that is too eager here is the same failure as one that reaches too little, and
// the test is written to fail if either arm widens.
func TestPurgeDoesNotTreatAnIdAsAPattern(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// The memory being purged, with an id carrying BOTH LIKE metacharacters.
	const id = "A_1%done"
	if err := s.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: testProject, Source: "search", QueryHash: digest(11),
		Outcome: "answerable", Reason: "floor_met",
		Verdicts: []RowVerdict{{ID: id, Kept: true, Stage: "validity", Reason: "valid"}},
	}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	// Two rows a wildcard pattern would sweep up: one where `_` stands in for
	// any single character, one where `%` stands for any run.
	for _, other := range []string{"Ax1%done", "A_1anything"} {
		if err := s.RecordRetrieval(ctx, RetrievalRecord{
			ProjectID: testProject, Source: "search", QueryHash: digest(12),
			Outcome: "answerable", Reason: "floor_met",
			Verdicts: []RowVerdict{{ID: other, Kept: true, Stage: "validity", Reason: "valid"}},
		}); err != nil {
			t.Fatalf("RecordRetrieval(%q): %v", other, err)
		}
	}

	if _, err := s.PurgeMemoryHistory(ctx, id); err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}

	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil {
		t.Fatalf("count retrieval rows: %v", err)
	}
	if n != 2 {
		t.Errorf("retrieval_record holds %d rows after purging %q, want 2 — the id was matched as a pattern, not a literal",
			n, id)
	}
	var left int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM retrieval_record WHERE instr(verdicts, ?) > 0`, id).Scan(&left); err != nil {
		t.Fatalf("count rows naming the purged id: %v", err)
	}
	if left != 0 {
		t.Errorf("%d row(s) still name the purged id %q", left, id)
	}
}

// TestPurgeRefusesAnEmptyMemoryID: the substring search makes an empty id the
// most destructive input the store accepts, and it is refused rather than
// handled.
//
// SQLite's instr returns 1 for an empty needle (measured), so the textual arm of
// the retrieval-record delete would match EVERY row while the equality-keyed
// deletes around it matched nothing — the purge would report success having
// removed the entire audit trail, which is the opposite of what "erase this
// memory" means and not something an operator could detect from the count.
//
// No shipped caller passes an empty id today; this is a refusal on a reachable
// exported method, in the same spirit as RecordRetrieval refusing an empty
// project. The test asserts BOTH halves, because a refusal that still deleted
// something would be worse than no refusal.
func TestPurgeRefusesAnEmptyMemoryID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i := range 3 {
		if err := s.RecordRetrieval(ctx, RetrievalRecord{
			ProjectID: testProject, Source: "search", QueryHash: digest(i + 20),
			Outcome: "answerable", Reason: "floor_met",
			Verdicts: []RowVerdict{{ID: "M" + digest(i), Kept: true}},
		}); err != nil {
			t.Fatalf("RecordRetrieval %d: %v", i, err)
		}
	}

	if _, err := s.PurgeMemoryHistory(ctx, ""); err == nil {
		t.Fatal("PurgeMemoryHistory with no memory id returned no error, want a refusal")
	}

	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil {
		t.Fatalf("count retrieval rows: %v", err)
	}
	if n != 3 {
		t.Errorf("%d of 3 records survived a refused purge — the empty id matched rows it should never have been compared against", n)
	}
}

// TestMigrateV20RefusesATableThatIsNotGhosts: the collision a copied DDL cannot
// see.
//
// `CREATE TABLE IF NOT EXISTS` is a SILENT no-op against a table that already
// exists under the name, so migrateV20's create against a database holding
// someone else's `retrieval_record` does nothing at all — and the step would
// still stamp v20 over it. The store would then open, and every search that
// tried to record would fail with "no such column: verdicts": a store that will
// not work, from an error naming neither the table nor the way out. This is
// migrateV18's refuseForeignProvenanceTable case exactly, and a copy of that
// step's DDL does not bring its guard with it.
//
// The remedy has to be in the ERROR rather than the log, because the operator who
// hits this cannot act on a warning they did not see and every later write fails
// with the same unnamed error.
func TestMigrateV20RefusesATableThatIsNotGhosts(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "ghost.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("initSQL: %v", err)
	}
	// A same-named table of a different shape, as a development build or another
	// tool would leave behind.
	if _, err := db.Exec(`DROP TABLE retrieval_record`); err != nil {
		t.Fatalf("drop the real table: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE retrieval_record (
		id TEXT PRIMARY KEY, note TEXT
	)`); err != nil {
		t.Fatalf("create the foreign table: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	err = migrateV20(tx)
	_ = tx.Rollback() //nolint:errcheck

	if err == nil {
		t.Fatal("migrateV20 accepted a retrieval_record table that is not Ghost's — IF NOT EXISTS made the create a no-op and the step would have stamped v20 over it")
	}
	for _, want := range []string{"retrieval_record", "DROP TABLE retrieval_record"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %q, so the operator has no remedy: %v", want, err)
		}
	}
}

// TestOpenDBRefusesAForeignRetrievalRecordBeforeItBacksUp: the second call site,
// and the ordering is the claim.
//
// refuseForeignRetrievalRecordTable is also called from OpenDB, in the
// version < schemaVersion branch, BEFORE backupBeforeMigrate. migrateV20's own
// check catches the step; this one catches the cost. The condition is permanent
// (nothing converts a foreign table, the operator drops it) and the step rolls
// back on refusal, so user_version stays put and every later open re-enters the
// branch — which is why refusing after the backup writes a full VACUUM INTO of a
// store that can never open, once per open, forever, and why two opens in the
// same wall-clock second collide on the copy's name so the retry the operator is
// certain to make reports "refusing to overwrite an existing file" and names
// neither the table nor the remedy.
//
// So the assertion is not only the error: it is that no .pre-migrate- copy was
// written. An error with a backup beside it has already spent the copy.
func TestOpenDBRefusesAForeignRetrievalRecordBeforeItBacksUp(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("initSQL: %v", err)
	}
	for _, stmt := range []string{
		`DROP TABLE retrieval_record`,
		`CREATE TABLE retrieval_record (id TEXT PRIMARY KEY, note TEXT)`,
		`DROP INDEX IF EXISTS idx_retrieval_record_project`,
		fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion-1),
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	opened, err := OpenDB(dbPath)
	if err == nil {
		_ = opened.Close()
		t.Fatal("OpenDB accepted a database holding a retrieval_record that is not Ghost's")
	}
	if !strings.Contains(err.Error(), "retrieval_record") {
		t.Errorf("the error does not name the table, so the operator has no remedy: %v", err)
	}
	backups, err := filepath.Glob(dbPath + ".pre-migrate-*")
	if err != nil {
		t.Fatalf("look for backups: %v", err)
	}
	if len(backups) != 0 {
		t.Errorf("a refused open wrote %d pre-migrate backup(s) (%v) — the refusal must arrive before backupBeforeMigrate, or every open of this store costs a copy forever",
			len(backups), backups)
	}
}

// TestDeleteProjectTakesTheRecordsWithIt: a project delete must not orphan the
// rows that describe it.
//
// project_id is deliberately NOT a foreign key, because the memory purge has to
// reach these rows with a plain predicate — which is exactly what makes this
// delete explicit: nothing else will. A row left behind outlives the project it
// describes, so a report over the store would carry calls for a corpus an
// operator asked to remove, and the row's own project would name something the
// store no longer has.
func TestDeleteProjectTakesTheRecordsWithIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.RecordRetrieval(ctx, sampleRetrievalRecord()); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	// A second project's record, so the delete's reach is measured rather than
	// assumed: a table wipe would pass a one-project test.
	if err := s.EnsureProject(ctx, "other-project", "/tmp/test-other", "other"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := s.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: "other-project", Source: "search", QueryHash: digest(30),
		Outcome: "answerable", Reason: "floor_met",
		Verdicts: []RowVerdict{{ID: "OTHER", Kept: true}},
	}); err != nil {
		t.Fatalf("RecordRetrieval (other project): %v", err)
	}

	if _, err := s.DeleteProject(ctx, testProject, true); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	var left int
	if err := s.db.QueryRow(
		`SELECT count(*) FROM retrieval_record WHERE project_id = ?`, testProject,
	).Scan(&left); err != nil {
		t.Fatalf("count the deleted project's records: %v", err)
	}
	if left != 0 {
		t.Errorf("%d retrieval record(s) outlive the project they describe", left)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&left); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if left != 1 {
		t.Errorf("retrieval_record holds %d rows after the delete, want 1 (the other project's)", left)
	}
}

// TestAForeignRetrievalRecordCheckIgnoresAColumnThisBuildDoesNotKnow: the guard
// must recognise Ghost's table by IDENTITY, not by matching a column list.
//
// The remedy it prints when it refuses is `DROP TABLE retrieval_record`, so a
// guard that compared EVERY column would issue a data-loss instruction against a
// store whose table is one version AHEAD of this build — and this guard runs from
// OpenDB on every open, not only mid-migration, so it would fire on a store that
// is perfectly healthy. evidenceTableIdentity states this rule for the evidence
// table and the retrieval guard has to follow it: a store that gained a column
// must open, and must open quietly.
//
// The column added here is one a future version would plausibly add, so the shape
// is the real one rather than a contrived extra.
func TestAForeignRetrievalRecordCheckIgnoresAColumnThisBuildDoesNotKnow(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(initSQL); err != nil {
		t.Fatalf("initSQL: %v", err)
	}
	// A later build's addition to the real table. The guard must not notice.
	if _, err := db.Exec(`ALTER TABLE retrieval_record ADD COLUMN scope TEXT`); err != nil {
		t.Fatalf("add a column this build does not know: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	opened, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB refused Ghost's own table carrying a column from a later build: %v", err)
	}
	defer opened.Close() //nolint:errcheck
	// And it is still the table Ghost writes, not a recreated one.
	s := NewStore(opened, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.RecordRetrieval(context.Background(), sampleRetrievalRecord()); err != nil {
		t.Fatalf("RecordRetrieval into the table with the extra column: %v", err)
	}
}

// TestMergeProjectCarriesTheRecordsWithIt: a merge must reassign project_id, for
// the same reason the delete must remove it.
//
// project_id is deliberately not a foreign key, which is what makes both of these
// explicit. A record left naming the merged-away project outlives it: the report
// attributes calls about memories that now live elsewhere to a project that no
// longer exists, and a later DeleteProject of that stale id could not reach it —
// so the row would be unreachable and unremovable at once.
func TestMergeProjectCarriesTheRecordsWithIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Three projects, so the reassignment is scoped: a table-wide UPDATE would
	// pass a two-project test, and the third is what catches it.
	for _, p := range []struct{ id, path, name string }{
		{"keep-project", "/tmp/test-keep", "keep"},
		{"other-project", "/tmp/test-other", "other"},
	} {
		if err := s.EnsureProject(ctx, p.id, p.path, p.name); err != nil {
			t.Fatalf("EnsureProject(%s): %v", p.id, err)
		}
	}
	// One record for the surviving project, one for the project being merged away,
	// one for a project that has nothing to do with the merge.
	for i, pid := range []string{testProject, "keep-project", "other-project"} {
		rec := sampleRetrievalRecord()
		rec.ProjectID = pid
		rec.QueryHash = digest(40 + i)
		if err := s.RecordRetrieval(ctx, rec); err != nil {
			t.Fatalf("RecordRetrieval(%s): %v", pid, err)
		}
	}

	if err := s.MergeProject(ctx, "keep-project", testProject); err != nil {
		t.Fatalf("MergeProject: %v", err)
	}

	for _, c := range []struct {
		project string
		want    int
		why     string
	}{
		{"keep-project", 0, "the merged-away project no longer exists, so nothing may still name it"},
		{testProject, 2, "its own record plus the one the merge carried"},
		{"other-project", 1, "an unrelated project's record must not be touched"},
	} {
		var n int
		if err := s.db.QueryRow(
			`SELECT count(*) FROM retrieval_record WHERE project_id = ?`, c.project,
		).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", c.project, err)
		}
		if n != c.want {
			t.Errorf("%s holds %d record(s), want %d — %s", c.project, n, c.want, c.why)
		}
	}
}

// TestDeleteProjectSummaryCountsTheRecords: the summary is documented as covering
// every table that references the project, and DeleteProject now removes these
// rows — so a count that omitted them would be a summary under-reporting its own
// command's work, in the dry-run a user reads BEFORE deciding to delete.
func TestDeleteProjectSummaryCountsTheRecords(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i := range 2 {
		rec := sampleRetrievalRecord()
		rec.QueryHash = digest(i + 50)
		if err := s.RecordRetrieval(ctx, rec); err != nil {
			t.Fatalf("RecordRetrieval %d: %v", i, err)
		}
	}

	summary, err := s.DeleteProject(ctx, testProject, false)
	if err != nil {
		t.Fatalf("DeleteProject (dry run): %v", err)
	}
	if summary.RetrievalRecords != 2 {
		t.Errorf("the dry run reports %d retrieval records, want 2 — a user decides whether to delete from this line",
			summary.RetrievalRecords)
	}
	// And the apply path removes exactly what it counted.
	applied, err := s.DeleteProject(ctx, testProject, true)
	if err != nil {
		t.Fatalf("DeleteProject (apply): %v", err)
	}
	if applied.RetrievalRecords != summary.RetrievalRecords {
		t.Errorf("the apply path removed/counted %d records, the dry run counted %d",
			applied.RetrievalRecords, summary.RetrievalRecords)
	}
	var left int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&left); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if left != 0 {
		t.Errorf("%d record(s) survived the project delete", left)
	}
}

// TestBothMergeStatementListsReassignTheRecords: the merge reassignment exists
// in TWO copies, and projectMergeStatements' own comment says they are kept in
// step deliberately rather than deduplicated — which is a promise a comment
// cannot keep.
//
// projectMergeStatements serves the merge/migration paths; the list inside
// (*Store).mergeProjectTx serves the bind-recovery paths (line 900, 1310). A
// table added to one and not the other is invisible to any test that only calls
// MergeProject, because that path never reads the second list — which is exactly
// how retrieval_record would have been reassigned on the happy path and orphaned
// on the recovery one.
//
// The two lists are the same statements, so the comparison is exact and a
// divergence in either direction fails here.
func TestBothMergeStatementListsReassignTheRecords(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatalf("read store.go: %v", err)
	}
	const stmt = "UPDATE retrieval_record SET project_id = ? WHERE project_id = ?"
	if n := strings.Count(string(src), stmt); n != 2 {
		t.Errorf("the merge reassignment appears %d time(s) in store.go, want 2 — the two lists are documented as "+
			"deliberately duplicated, and a table in one and not the other is reassigned on the merge path and "+
			"orphaned on the bind-recovery path", n)
	}
}

// TestEveryRendererOfTheDeleteSummaryNamesTheRecords: the summary has THREE
// renderers, and adding a field to one while forgetting another is exactly what
// happened twice on this table.
//
// cmd/ghost's printDeleteSummary, the MCP tool's inline rendering of the same
// struct, and the durable slog line all report what a project delete removes. A
// field present in one and absent in another is not a cosmetic difference: the
// dry run is what a reader decides from, and the log line is the durable record
// of what was actually removed. So this test asserts the FIELD is named in each
// source, which is what keeps the three in step — a test asserting the rendered
// text of one surface cannot see the other two drift.
func TestEveryRendererOfTheDeleteSummaryNamesTheRecords(t *testing.T) {
	// The count is populated by the delete itself; what needs pinning is that
	// every renderer mentions it.
	for _, c := range []struct{ path, needle, why string }{
		{"../../cmd/ghost/project.go", "RetrievalRecords", "the CLI's delete report"},
		{"../mcpserver/mcpserver.go", "RetrievalRecords", "the MCP tool's identical report"},
		{"store.go", "retrieval_records", "the durable delete log line"},
	} {
		src, err := os.ReadFile(c.path)
		if err != nil {
			t.Fatalf("read %s: %v", c.path, err)
		}
		if !strings.Contains(string(src), c.needle) {
			t.Errorf("%s never reads DeleteProjectSummary.%s — a count the %s does not show is a count the reader cannot see",
				c.path, c.needle, c.why)
		}
	}
}

// TestRecordRetrievalRejectsAnEmptyProject: the record's project column is
// NOT NULL, and a caller that has not resolved a project is a bug rather than
// a row to file. Failing here is better than a row no audit can attribute.
func TestRecordRetrievalRejectsAnEmptyProject(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	err := s.RecordRetrieval(ctx, RetrievalRecord{
		Source: "search", QueryHash: digest(1), Outcome: "answerable",
	})
	if err == nil {
		t.Fatal("RecordRetrieval with no project: no error, want a refusal")
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM retrieval_record`).Scan(&n); err != nil {
		t.Fatalf("count retrieval rows: %v", err)
	}
	if n != 0 {
		t.Errorf("a refused record left %d row(s) behind", n)
	}
}

// TestRetrievalRecordsSurvivesAMalformedVerdictColumn: the READER is the other
// half of the same tolerance, and a reader that errors on one bad row cannot
// report on a store at all — which is the state an operator is in precisely when
// they need the report.
func TestRetrievalRecordsSurvivesAMalformedVerdictColumn(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.RecordRetrieval(ctx, sampleRetrievalRecord()); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO retrieval_record (project_id, source, query_hash, outcome, reason, verdicts)
		 VALUES (?, 'search', ?, 'answerable', 'floor_met', 'not json')`,
		testProject, digest(5),
	); err != nil {
		t.Fatalf("insert a malformed row: %v", err)
	}

	got, err := s.RetrievalRecords(ctx, 10)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d records, want 2 — one malformed row took the reader with it", len(got))
	}
	// Newest first, so the hand-inserted malformed row is first and the record
	// Ghost wrote is second.
	if len(got[0].Verdicts) != 0 {
		t.Errorf("the malformed record reports %d verdicts, want none — an unreadable column is not a guess",
			len(got[0].Verdicts))
	}
	if len(got[1].Verdicts) != 2 {
		t.Errorf("the well-formed record came back with %d verdicts, want 2", len(got[1].Verdicts))
	}
}

// TestThePurgeDoesNotFuzzyMatchAWellFormedRecord: the textual arm's SCOPE, and
// the reason it is gated on unreadable rows rather than OR'd in freely.
//
// For a well-formed record the parsed arm knows the exact ids the stages
// recorded. An unscoped textual arm would then OVERWRITE that knowledge with a
// substring search: purging the id `A_1` would delete a record whose verdict names
// `A_1%done` — a memory that was never purged, and whose audit trail goes with
// it. Having the exact answer and then searching for it approximately is strictly
// worse than not having the answer, so the arm only runs on rows nothing else can
// read.
func TestThePurgeDoesNotFuzzyMatchAWellFormedRecord(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A well-formed record naming an id that CONTAINS the one we will purge.
	const purgeID = "A_1"
	const neighbour = "A_1%done"
	if err := s.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: testProject, Source: "search", QueryHash: digest(70),
		Outcome: "answerable", Reason: "floor_met",
		Verdicts: []RowVerdict{{ID: neighbour, Kept: true, Stage: "validity", Reason: "valid"}},
	}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	// And the record that really does name the purged memory, so the purge has
	// real work alongside the one it must not do.
	if err := s.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: testProject, Source: "search", QueryHash: digest(71),
		Outcome: "answerable", Reason: "floor_met",
		Verdicts: []RowVerdict{{ID: purgeID, Kept: true, Stage: "validity", Reason: "valid"}},
	}); err != nil {
		t.Fatalf("RecordRetrieval (target): %v", err)
	}

	if _, err := s.PurgeMemoryHistory(ctx, purgeID); err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}

	// Read the rows back through the reader rather than matching the column, and
	// assert on the DECODED ids: a LIKE built from an id containing `%` is a
	// pattern, so a LIKE-based check here would fail on the very neighbour it is
	// meant to prove survived.
	recs, err := s.RetrievalRecords(ctx, 10)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("retrieval_record holds %d rows, want 1 (the neighbour %q) — the textual arm ran against a "+
			"well-formed row the parsed arm had already decided about", len(recs), neighbour)
	}
	if len(recs[0].Verdicts) != 1 || recs[0].Verdicts[0].ID != neighbour {
		t.Errorf("the surviving record is %+v, want the one naming %q", recs[0].Verdicts, neighbour)
	}
}

// TestTheLeakScanCatchesTextHiddenInsideALongerValue: the scan's sensitivity is
// substring, and this is what proves it.
//
// countOccurrences is this tree's "this text is nowhere in the store" check, and
// for the retrieval_record columns it uses instr rather than `=`. With equality
// the whole list would be decorative: a record that stored the question inside a
// longer string, or a memory's content inside a verdict object, would never equal
// the text searched for and the scan would report a clean store. This writes
// exactly those two shapes and asserts the scan finds them.
func TestTheLeakScanCatchesTextHiddenInsideALongerValue(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const query = "how does the k3s ingress controller rotate its certificate"
	const content = "the ingress controller reloads its certificate on SIGHUP only"

	// The query buried in a longer value, and the content buried inside a verdict
	// object — neither equals the text a caller would search for.
	if err := s.RecordRetrieval(ctx, RetrievalRecord{
		ProjectID: testProject, Source: "search", QueryHash: digest(60),
		Outcome: "answerable", Reason: "the caller asked: " + query,
		Verdicts: []RowVerdict{
			{ID: "MEM-1", Kept: true, Stage: "validity", Reason: "valid"},
			{ID: "MEM-2", Kept: false, Stage: "budget", Reason: "dropped because: " + content},
		},
	}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}

	for _, text := range []string{query, content} {
		n, err := countOccurrences(t, s.db, text)
		if err != nil {
			t.Fatalf("scan for %q: %v", text[:20], err)
		}
		if n == 0 {
			t.Errorf("text hidden inside a longer stored value was not found by the scan — the privacy " +
				"assertion is equality-based and would report a clean store")
		}
	}
}

// TestRowVerdictJSONShape pins the STORED shape of a verdict, because the next
// part's reader and any future SQL that reaches into this column are both
// written against it, and a renamed field is a silent data loss rather than a
// compile error.
func TestRowVerdictJSONShape(t *testing.T) {
	b, err := json.Marshal(RowVerdict{ID: "M", Kept: true, Stage: "validity", Reason: "valid"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got, want := string(b), `{"id":"M","kept":true,"stage":"validity","reason":"valid"}`; got != want {
		t.Errorf("verdict JSON = %s, want %s", got, want)
	}
}
