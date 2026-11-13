package mcpserver

// #850: the project-context surface records the calls it rendered.
// carried a `project_context` source value since #646, and the per-source

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

const projectContextSentinel = "SENTINEL-PROJECT-CONTEXT-TEXT-7b2e"

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
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	if err := st.EnsureProject(ctx, "vproj", t.TempDir(), "vproj"); err != nil {
		t.Fatalf("EnsureProject vproj: %v", err)
	}
	return st, dbPath
}

func recordTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestProjectContextRecordsTheCallItRendered(t *testing.T) {
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
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
		t.Errorf("session_id = %q, want empty: this fixture pins CLAUDE_CODE_SESSION_ID empty with t.Setenv, and the "+
			"server reads it once at construction, so the id it records is \"\"; the fixture's transport assigns no id either", got.SessionID)
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

func TestTheProjectContextResourceRecordsBothOfItsReads(t *testing.T) {
	st, _ := projectRecordStore(t)
	srv, session := validityServerFor(t, st)
	id := saveValidityRow(t, session, projectContextSentinel, nil)

	text, err := srv.buildProjectContext(t.Context(), "vproj", "", searchResponseMaxBytes)
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

// records, because it moved onto the assembler and #850's rule is that every surface
// assumed. Before #581 the resource read `_global` through `Store.GetTopMemories`,
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

type refusingRecordStore struct {
	*memory.Store
	calls atomic.Int32
}

func (s *refusingRecordStore) RecordRetrieval(context.Context, memory.RetrievalRecord) error {
	s.calls.Add(1)
	return errors.New("the audit is on fire")
}

// this file — the tool's union read, the resource's two reads, and (since #581) the
func TestTheProjectContextToolRecordsOneRowOnBothOfItsUnmatchedShapes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		projectID string
		why       string
	}{
		{"the bucket itself", memory.GlobalProjectID,
			"IncludeGlobal is unset when the project IS `_global`, so this read is `_global`-only rather than a union " +
				"with an empty half — one read, one row"},
		{"an unresolved name", "nosuchproject",
			"the unresolved branch renders the Global section alone and SKIPS the project-keyed read, so again one " +
				"read and one row; a union here would have matched nothing and still been recorded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, _ := projectRecordStore(t)
			_, session := validityServerFor(t, st)
			id := saveGlobalValidityRow(t, session, projectContextSentinel, nil)

			out := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": tc.projectID}))
			if !strings.Contains(out, projectContextSentinel) {
				t.Fatalf("the block did not render the fixture's global memory, so the row below is about a read "+
					"that returned nothing:\n%s", out)
			}

			records, err := st.RetrievalRecords(t.Context(), 10)
			if err != nil {
				t.Fatalf("RetrievalRecords: %v", err)
			}
			if len(records) != 1 {
				t.Fatalf("recorded %d rows, want exactly 1 — %s. Recorded: %+v", len(records), tc.why, records)
			}
			got := records[0]
			if got.Source != "project_context" {
				t.Errorf("source = %q, want project_context", got.Source)
			}
			if got.ProjectID != memory.GlobalProjectID {
				t.Errorf("project_id = %q, want %q — this read named the `_global` bucket, and attributing it to "+
					"anything else would put another bucket's window in that project's denominator", got.ProjectID, memory.GlobalProjectID)
			}
			if got.QueryHash != "" {
				t.Errorf("query_hash = %q, want empty — a listing carried no question", got.QueryHash)
			}
			if len(got.Verdicts) != 1 || got.Verdicts[0].ID != id || !got.Verdicts[0].Kept {
				t.Errorf("verdicts = %+v, want the one rendered global row %s kept — a verdict naming any other id "+
					"would make the per-call figure in the audit a claim about a row this read never returned",
					got.Verdicts, id)
			}
		})
	}
}

func TestTheProjectContextRecordCarriesNoMemoryText(t *testing.T) {
	st, dbPath := projectRecordStore(t)
	_, session := validityServerFor(t, st)
	saveValidityRow(t, session, projectContextSentinel, nil)

	out := resultText(callTool(t, session, "ghost_project_context", map[string]any{"project_id": "vproj"}))
	if !strings.Contains(out, projectContextSentinel) {
		t.Fatalf("the block did not render the fixture's memory, so the scan below would be vacuous:\n%s", out)
	}

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

func TestTheGlobalMemoriesResourceRecordCarriesNoQueryOrMemoryText(t *testing.T) {
	st, dbPath := projectRecordStore(t)
	srv, session := validityServerFor(t, st)
	saveGlobalValidityRow(t, session, projectContextSentinel, nil)

	if out := renderGlobalMemoriesResource(t, srv); !strings.Contains(out, projectContextSentinel) {
		t.Fatalf("the resource did not render the fixture's memory, so the scan below would be vacuous:\n%s", out)
	}

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
	records, err := st.RetrievalRecords(t.Context(), 10)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(records) != 1 {
		t.Errorf("the table holds %d row(s) after a refused record, want the control run's 1 — a refused write must "+
			"leave nothing behind. Recorded: %+v", len(records), records)
	}
}
