package mcpserver

// #850: the project-context surface records the calls it rendered.
//
// This is the second of the two PASSIVE surfaces (the session start is the other),
// and it was the one the audit could name but not count: `retrieval_record` has
// carried a `project_context` source value since #646, and the per-source
// denominators in the report split on it, so the column existed and could only
// ever hold searches.
//
// The two project-context surfaces make DIFFERENT numbers of reads, which is why
// there are two recording tests here rather than one. The tool reads the union
// window once and renders its Global section out of that same window — `limit` caps
// the whole block — so it makes ONE assembler call and owes one row. The resource
// and the prompt cap each SECTION, so the Global section needs a second read at its
// own cap, and those two calls are two retrievals with two budgets and two sets of
// verdicts. Collapsing them into one row for the block would mean a surface
// re-deriving which rows the second read judged, which is the second implementation
// of the assembler's rules that Result.Leaks() and Trace.TrimmedByBudget() exist to
// prevent. So each read gets its own row and the grain stays the call.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// projectContextSentinel is the text a leak would have to carry. It is in a
// memory this fixture saves and the block demonstrably renders, so the scan for it
// cannot pass vacuously.
const projectContextSentinel = "SENTINEL-PROJECT-CONTEXT-TEXT-7b2e"

// projectRecordStore is newValidityStore on a FILE, because one of these tests
// scans the raw columns of a row and an in-memory database has no second handle to
// read them through: OpenDB pins MaxOpenConns(1), so the store under test owns its
// one connection for the whole test.
func projectRecordStore(t *testing.T) (*memory.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB(%s): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	st := memory.NewStore(db, recordTestLogger())
	ctx := context.Background()
	// `_global` is created by seeding rather than by EnsureProject, and a global row
	// carries a foreign key onto it.
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	if err := st.EnsureProject(ctx, "vproj", t.TempDir(), "vproj"); err != nil {
		t.Fatalf("EnsureProject vproj: %v", err)
	}
	return st, dbPath
}

// recordTestLogger is the fixture logger, at Error: a record refusal is a Warn and
// these tests assert on the block rather than on the log, so the level here only has
// to keep a healthy suite quiet.
func recordTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestProjectContextRecordsTheCallItRendered: a project listing that shows a
// memory leaves exactly one row saying so, under its own source.
//
// ONE, and the count is the assertion with teeth: the tool's `## Global` section is
// the `_global` half of its own union window rather than a second read (see the tool
// handler), so a surface that recorded per SECTION would write a row for a read it
// never made, and would name `_global` verdicts inside the requesting project's
// denominator.
//
// The source value is the point of the other assertions, because it is the column
// the audit's denominators are split by — a row recorded as `search` here would be
// counted as a search and would quietly change what every precision figure in the
// report means. An empty query_hash is the honest value for a call that carried no
// question, and an empty session_id is the truth over stdio. The kept ids are
// compared with the memory the tool actually RENDERED rather than with the rows the
// store holds: the block is what the agent was given, and a record naming a row the
// block withheld would be a "used" verdict for a memory nobody saw.
func TestProjectContextRecordsTheCallItRendered(t *testing.T) {
	st, _ := projectRecordStore(t)
	_, session := validityServerFor(t, st)
	id := saveValidityRow(t, session, projectContextSentinel, nil)

	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"}))
	if !strings.Contains(out, projectContextSentinel) {
		t.Fatalf("the block did not render the fixture's memory, so nothing below is about a rendered row:\n%s", out)
	}

	records, err := st.RetrievalRecords(t.Context(), 10)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("one project-context call recorded %d rows, want exactly 1 — the tool reads the union window once and "+
			"renders its Global section out of that same window, so a second row would be a read that never happened. "+
			"Recorded: %+v", len(records), records)
	}

	got := records[0]
	if got.Source != "project_context" {
		t.Errorf("source = %q, want project_context — the audit splits its denominators by this column", got.Source)
	}
	if got.ProjectID != "vproj" {
		t.Errorf("project_id = %q, want vproj", got.ProjectID)
	}
	if got.QueryHash != "" {
		t.Errorf("query_hash = %q, want empty — this call carried no question, and a digest of \"\" would be the same "+
			"constant on every listing", got.QueryHash)
	}
	if got.SessionID != "" {
		t.Errorf("session_id = %q, want empty: Ghost serves stdio, whose connection reports no session id", got.SessionID)
	}
	if got.AsOf != "" {
		t.Errorf("as_of = %q, want empty for a current read", got.AsOf)
	}
	if got.Outcome == "" {
		t.Error("outcome = \"\", want the assembler's own verdict: the audit reads it, and an absent one is a hole rather " +
			"than an answer")
	}
	if len(got.Verdicts) != 1 || got.Verdicts[0].ID != id || !got.Verdicts[0].Kept {
		t.Errorf("verdicts = %+v, want the one rendered row %s kept", got.Verdicts, id)
	}
}

// TestTheProjectContextResourceRecordsBothOfItsReads: the resource and the prompt
// cap each SECTION, so their Global section is a second read — and a second read
// gets its own row.
//
// The second row's attribution is the assertion with teeth. It is attributed to the
// `_global` bucket it READ rather than to the project the call was about, because
// the audit's denominator for a project is a statement about that project's
// retrievals: carrying a window that held none of its rows there would let a
// project's precision be computed over another bucket's windows.
func TestTheProjectContextResourceRecordsBothOfItsReads(t *testing.T) {
	st, _ := projectRecordStore(t)
	srv, session := validityServerFor(t, st)
	id := saveValidityRow(t, session, projectContextSentinel, nil)

	text, err := srv.buildProjectContext(t.Context(), "vproj")
	if err != nil {
		t.Fatalf("buildProjectContext: %v", err)
	}
	if !strings.Contains(text, projectContextSentinel) {
		t.Fatalf("the block did not render the fixture's memory, so nothing below is about a rendered row:\n%s", text)
	}

	records, err := st.RetrievalRecords(t.Context(), 10)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	// Newest first, so the union read (which ran first) is the LAST element.
	if len(records) != 2 {
		t.Fatalf("the resource recorded %d rows, want 2 — the union window plus the Global section's own read at its own "+
			"cap, and each is a retrieval with its own budget and verdicts. Recorded: %+v", len(records), records)
	}

	union := records[len(records)-1]
	if union.Source != "project_context" {
		t.Errorf("the union read's source = %q, want project_context", union.Source)
	}
	if union.ProjectID != "vproj" {
		t.Errorf("the union read's project_id = %q, want vproj", union.ProjectID)
	}
	if len(union.Verdicts) != 1 || union.Verdicts[0].ID != id || !union.Verdicts[0].Kept {
		t.Errorf("the union read's verdicts = %+v, want the one rendered row %s kept", union.Verdicts, id)
	}

	globals := records[0]
	if globals.Source != "project_context" {
		t.Errorf("the Global section's row has source = %q, want project_context", globals.Source)
	}
	if globals.ProjectID != memory.GlobalProjectID {
		t.Errorf("the Global section's row has project_id = %q, want %q — it read the global bucket, and attributing it "+
			"to the requesting project would put a window of other projects' rows in that project's denominator",
			globals.ProjectID, memory.GlobalProjectID)
	}
	if len(globals.Verdicts) != 0 {
		t.Errorf("the Global section's row kept %d row(s) (%+v), want none: this fixture holds no `_global` memory",
			len(globals.Verdicts), globals.Verdicts)
	}
}

// TestTheProjectContextRecordCarriesNoMemoryText: the table holds verdicts and
// ids, and never the text those ids name.
//
// A record outlives the call — `ghost backup` copies the file whole — and a purge
// deletes the rows NAMING a memory rather than any words of it, because this table
// keeps none. The assertion is over the raw bytes of EVERY column as a SUBSTRING: a
// struct check would be a claim about this build's type, and equality would miss a
// leak that stored the text inside a longer value, which is the shape a later writer
// would produce.
func TestTheProjectContextRecordCarriesNoMemoryText(t *testing.T) {
	st, dbPath := projectRecordStore(t)
	_, session := validityServerFor(t, st)
	saveValidityRow(t, session, projectContextSentinel, nil)

	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"}))
	if !strings.Contains(out, projectContextSentinel) {
		t.Fatalf("the block did not render the fixture's memory, so the scan below would be vacuous:\n%s", out)
	}

	// The row count first, because an EMPTY table satisfies every column scan below
	// and the assertion would then be a claim about nothing.
	records, err := st.RetrievalRecords(t.Context(), 10)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("the call recorded no row at all, so the scan below would pass on an empty table")
	}

	db, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	columns := []string{"project_id", "session_id", "source", "query_hash", "as_of", "reason", "verdicts", "outcome", "recorded_at"}
	for _, column := range columns {
		// instr rather than LIKE, so no character of the sentinel is a wildcard.
		var leaked int
		if err := db.QueryRow(`SELECT count(*) FROM retrieval_record WHERE instr(`+column+`, ?) > 0`, projectContextSentinel).Scan(&leaked); err != nil {
			t.Fatalf("scan retrieval_record.%s for the sentinel: %v", column, err)
		}
		if leaked != 0 {
			t.Errorf("retrieval_record.%s holds a rendered memory's text in %d row(s): the record is verdicts and ids, "+
				"and a memory's content here outlives the call and travels through every backup", column, leaked)
		}
	}
}

// TestTheGlobalMemoriesResourceRecordsItsRead: the `ghost://memories/global` resource
// records, because it moved onto the assembler and #850's rule is that every surface
// that assembles a context does.
//
// This is a behaviour CHANGE and not a wiring detail, so it is pinned rather than
// assumed. Before #581 the resource read `_global` through `Store.GetTopMemories`,
// which ranked and trimmed in SQL and wrote nothing at all; reaching the assembler
// through `projectContextGlobals` means it now inherits
// `assembleProjectContext`'s `req.Record`, so an agent reading the cross-project
// memories appends a `retrieval_record` row where it used to append none. The
// decision is to KEEP it — a resource read is a retrieval an agent acted on exactly
// as much as a tool call is, and a listing the audit cannot see is a listing whose
// per-source precision figures mean nothing — but a change nobody wrote down is a
// change the next reader has to rediscover.
//
// ONE row is the count with teeth: this surface reads `_global` once, so a reader
// that recorded per section or per candidate would write more. `_global` as the
// project_id is the other half: it read that bucket, and attributing the row to
// anything else would put a window of other projects' rows in that denominator.
// The empty query_hash is the honest value for a call that carried no question, and
// the kept ids are compared against the rows the RESOURCE RENDERED rather than the
// rows the store holds — a record naming a row the block withheld would be a "used"
// verdict for a memory nobody saw.
func TestTheGlobalMemoriesResourceRecordsItsRead(t *testing.T) {
	st, _ := projectRecordStore(t)
	srv, session := validityServerFor(t, st)
	id := saveGlobalValidityRow(t, session, projectContextSentinel, nil)

	out := renderGlobalMemoriesResource(t, srv)
	if !strings.Contains(out, projectContextSentinel) {
		t.Fatalf("the resource did not render the fixture's memory, so the verdicts below are about a row nobody saw:\n%s", out)
	}

	records, err := st.RetrievalRecords(t.Context(), 10)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("one read of ghost://memories/global recorded %d rows, want exactly 1 — the surface reads the bucket once. "+
			"Recorded: %+v", len(records), records)
	}

	got := records[0]
	if got.Source != "project_context" {
		t.Errorf("source = %q, want project_context — this surface reaches the assembler through "+
			"assembleProjectContext, and the audit splits its denominators by this column", got.Source)
	}
	if got.ProjectID != memory.GlobalProjectID {
		t.Errorf("project_id = %q, want %q — it read the cross-project bucket, so that is what the row is a statement about",
			got.ProjectID, memory.GlobalProjectID)
	}
	if got.QueryHash != "" {
		t.Errorf("query_hash = %q, want empty — a listing carried no question, and a digest of \"\" would be the same "+
			"constant on every read of this resource", got.QueryHash)
	}
	if got.Outcome == "" {
		t.Error("outcome = \"\", want the assembler's own verdict: the audit reads it, and an absent one is a hole rather " +
			"than an answer")
	}
	if len(got.Verdicts) != 1 || got.Verdicts[0].ID != id || !got.Verdicts[0].Kept {
		t.Errorf("verdicts = %+v, want the one rendered row %s kept", got.Verdicts, id)
	}
}

// TestTheGlobalMemoriesResourceRecordCarriesNoQueryOrMemoryText: the row this
// surface now writes holds verdicts and ids, and neither the question it was asked
// nor the text of what it answered.
//
// A resource read carries no question by construction — the handler passes `Query: ""`
// — so a record that named one would be inventing it, and a record that carried the
// content would outlive the call through every `ghost backup` the store is part of.
// The scan is over the raw bytes of EVERY column as a SUBSTRING, for the reason
// TestTheProjectContextRecordCarriesNoMemoryText gives: equality would miss a leak
// stored inside a longer value, which is the shape a later writer would produce.
func TestTheGlobalMemoriesResourceRecordCarriesNoQueryOrMemoryText(t *testing.T) {
	st, dbPath := projectRecordStore(t)
	srv, session := validityServerFor(t, st)
	saveGlobalValidityRow(t, session, projectContextSentinel, nil)

	if out := renderGlobalMemoriesResource(t, srv); !strings.Contains(out, projectContextSentinel) {
		t.Fatalf("the resource did not render the fixture's memory, so the scan below would be vacuous:\n%s", out)
	}

	// The row count first, because an EMPTY table satisfies every column scan below.
	records, err := st.RetrievalRecords(t.Context(), 10)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(records) == 0 {
		t.Fatal("the read recorded no row at all, so the scan below would pass on an empty table")
	}

	db, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, column := range []string{"project_id", "session_id", "source", "query_hash", "as_of", "reason", "verdicts", "outcome", "recorded_at"} {
		var leaked int
		if err := db.QueryRow(`SELECT count(*) FROM retrieval_record WHERE instr(`+column+`, ?) > 0`, projectContextSentinel).Scan(&leaked); err != nil {
			t.Fatalf("scan retrieval_record.%s for the sentinel: %v", column, err)
		}
		if leaked != 0 {
			t.Errorf("retrieval_record.%s holds a rendered global memory's text in %d row(s): the record is verdicts and "+
				"ids, and it now outlives every read of this resource", column, leaked)
		}
	}
}

// refusingRecordStore answers every read and refuses the record write, which is the
// shape a store whose audit path is broken has: the rows are all there and the
// measurement cannot be taken.
type refusingRecordStore struct {
	*memory.Store
	calls atomic.Int32
}

func (s *refusingRecordStore) RecordRetrieval(context.Context, memory.RetrievalRecord) error {
	s.calls.Add(1)
	return errors.New("the audit is on fire")
}

// TestAProjectContextWhoseRecordIsRefusedStillRendersTheSameBlock: fail open, and
// the block is compared against a server whose record SUCCEEDED on the same store.
//
// A retrieval record is a measurement of a call that already happened. If recording
// could change the call, then the act of measuring retrieval would change
// retrieval's availability — and it would do so at exactly the moments the store is
// unhealthy, which is when a report is most wanted. The comparison is against a live
// control rather than a stored golden, so the claim is "the rows this store would
// have rendered", which is the one that can actually fail.
func TestAProjectContextWhoseRecordIsRefusedStillRendersTheSameBlock(t *testing.T) {
	st, _ := projectRecordStore(t)
	_, session := validityServerFor(t, st)
	saveValidityRow(t, session, projectContextSentinel, nil)

	control := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"}))
	if !strings.Contains(control, projectContextSentinel) {
		t.Fatalf("the control block did not render the fixture's memory:\n%s", control)
	}

	refusing := &refusingRecordStore{Store: st}
	refused := resultText(callTool(t, connectedClient(t, New(refusing, recordTestLogger(), "test")),
		"ghost_project_context", map[string]any{"project_id": "vproj"}))

	if refused != control {
		t.Errorf("the block with the record refused differs from the control block, so a failed measurement changed the "+
			"answer. control:\n%s\nrefused:\n%s", control, refused)
	}
	if got := refusing.calls.Load(); got == 0 {
		t.Error("the refusing store was never asked to record, so this test passed without exercising the refusal: a " +
			"surface that recorded nothing at all would render the same block")
	}
	// The control run's row is still the only one in the table, so the refused call
	// really did write nothing rather than something the reader cannot see.
	records, err := st.RetrievalRecords(t.Context(), 10)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("the table holds %d row(s) after a refused record, want the control run's 1 — a refused write must "+
			"leave nothing behind. Recorded: %+v", len(records), records)
	}
}
