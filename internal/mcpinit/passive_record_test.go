package mcpinit

// #850: the session-start block records the call it rendered.
//
// The retrieval record (#646) exists so an audit can ask "did the agent use what
// Ghost injected", and its denominator is CALLS. For its first two releases only
// ghost_memory_search wrote a row, which made every number in that report a
// statement about searches and nothing at all about the block a session is
// OPENED with — the one call a user believes is the memory. The per-source
// denominators were already there and already split by source, so the audit had a
// column for `session_start` that could only ever be zero.
//
// These tests drive the real hook path (loadSessionContext, over a real on-disk
// store under a sandboxed XDG_DATA_HOME) rather than the seam, because the
// property under test is not "a sink was passed" but "the row landed in the
// database the session was rendered from" — and the store the block is read
// through is READ-ONLY (memory.OpenReadDB), so the record needs a second,
// read-write handle of its own. That is a wiring fact no test of
// loadSessionPassive can see.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// The two sentinels are the text a leak would have to carry: a project memory and
// a `_global` one, because the session block renders both buckets and the record
// is written once for the union.
const (
	recordProjectSentinel = "SENTINEL-PROJECT-TEXT-4c1a"
	recordGlobalSentinel  = "SENTINEL-GLOBAL-TEXT-9f3b"
)

// sessionRecordFixture is a data directory holding a closed, migrated store, one
// project, one memory in it and one in `_global` — plus the directory that
// resolves to that project.
func sessionRecordFixture(t *testing.T) (projDir, dbPath string) {
	t.Helper()
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	dbPath = filepath.Join(ghostDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}

	projDir = filepath.Join(t.TempDir(), "recproj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir project dir: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	// `_global` is created by seeding rather than by EnsureProject, and the global
	// memory below carries a foreign key onto it.
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'recproj')`, canonical); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	for _, row := range []struct{ id, project, category, content string }{
		{"mproj1", "p1", "fact", recordProjectSentinel},
		{"mglob1", memory.GlobalProjectID, "preference", recordGlobalSentinel},
	} {
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance)
			 VALUES (?, ?, ?, ?, 'manual', 0.8)`, row.id, row.project, row.category, row.content,
		); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return projDir, dbPath
}

// sessionBlock is the part of loadSessionContext's answer this suite asserts on:
// the project the session resolved to, and the ids the block rendered in each
// bucket. The remaining returns — learned context, tasks, decisions, the counts —
// are other reads of the same block and are covered by their own tests.
type sessionBlock struct {
	projectID string
	shown     []string
}

// renderSessionStartBlock runs the real session-start read and returns what the block
// rendered.
func renderSessionStartBlock(t *testing.T, projDir string) sessionBlock {
	t.Helper()
	projectID, _, memories, globals, _, _, _, _, _ := loadSessionContext(projDir, config.LoadForHook())
	if projectID != "p1" {
		t.Fatalf("project = %q, want p1 — the fixture's directory did not resolve, so nothing below is exercising a "+
			"session start", projectID)
	}
	if len(memories) != 1 || len(globals) != 1 {
		t.Fatalf("the block rendered %d project and %d global rows, want 1 and 1: the fixture must produce a block "+
			"whose rows a record can be compared against", len(memories), len(globals))
	}
	if !strings.Contains(memories[0].Content, recordProjectSentinel) ||
		!strings.Contains(globals[0].Content, recordGlobalSentinel) {
		t.Fatalf("the block did not carry the fixture's sentinels (%q / %q), so the no-text scan below would be vacuous",
			memories[0].Content, globals[0].Content)
	}
	return sessionBlock{projectID: projectID, shown: []string{memories[0].ID, globals[0].ID}}
}

// recordedCall is one retrieval_record row as this suite reads it: the columns
// that decide WHICH denominator it belongs to, and the verdicts.
type recordedCall struct {
	projectID, sessionID, source, queryHash, asOf, outcome, reason, verdicts string
}

func recordedCalls(t *testing.T, dbPath string) []recordedCall {
	t.Helper()
	db, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT project_id, session_id, source, query_hash, as_of, outcome, reason, verdicts
		FROM retrieval_record ORDER BY rowid`)
	if err != nil {
		t.Fatalf("read retrieval_record: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []recordedCall
	for rows.Next() {
		var c recordedCall
		if err := rows.Scan(&c.projectID, &c.sessionID, &c.source, &c.queryHash,
			&c.asOf, &c.outcome, &c.reason, &c.verdicts); err != nil {
			t.Fatalf("scan retrieval_record: %v", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate retrieval_record: %v", err)
	}
	return out
}

// keptIDs decodes the verdicts column and returns the ids the call KEPT, failing
// on any dropped verdict.
func keptIDs(t *testing.T, verdictsJSON string) []string {
	t.Helper()
	var verdicts []memory.RowVerdict
	if err := json.Unmarshal([]byte(verdictsJSON), &verdicts); err != nil {
		t.Fatalf("decode verdicts %q: %v", verdictsJSON, err)
	}
	var ids []string
	for _, v := range verdicts {
		if !v.Kept {
			t.Errorf("verdict for %s is a drop (%s/%s), but the block rendered that row: the record and the block "+
				"disagree, which is the one thing neither may do", v.ID, v.Stage, v.Reason)
			continue
		}
		ids = append(ids, v.ID)
	}
	return ids
}

// captureStderr runs fn with os.Stderr redirected and returns what it printed.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		_, _ = io.Copy(&sb, r)
		done <- sb.String()
	}()
	fn()
	os.Stderr = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// TestSessionStartRecordsTheCallItRendered is the property #850 is about: a
// session start that shows two memories leaves a row saying so.
func TestSessionStartRecordsTheCallItRendered(t *testing.T) {
	projDir, dbPath := sessionRecordFixture(t)

	block := renderSessionStartBlock(t, projDir)

	calls := recordedCalls(t, dbPath)
	if len(calls) != 1 {
		t.Fatalf("a session start that rendered %d rows recorded %d calls, want exactly 1 — the audit's denominator is "+
			"one row per call, and two rows for one session would double-count every injection", len(block.shown), len(calls))
	}
	rec := calls[0]
	if rec.source != "session_start" {
		t.Errorf("source = %q, want session_start — the audit splits its denominators by this column, so a row under "+
			"another value is counted as a search", rec.source)
	}
	if rec.projectID != block.projectID {
		t.Errorf("project_id = %q, want %q — the row must name the project whose session this was", rec.projectID, block.projectID)
	}
	if rec.sessionID != "" {
		t.Errorf("session_id = %q, want empty: this fixture drives loadSessionContext, the caller with no hook payload, "+
			"so no session id was ever named for the record to carry", rec.sessionID)
	}
	if rec.queryHash != "" {
		t.Errorf("query_hash = %q, want empty — a session start carries no question, and the digest of \"\" would be the "+
			"same constant on every injection, which reads as a fingerprint of something", rec.queryHash)
	}
	if rec.asOf != "" {
		t.Errorf("as_of = %q, want empty for a current read", rec.asOf)
	}
	if rec.outcome == "" {
		t.Error("outcome = \"\", want the assembler's own verdict: the audit reads it, and an absent one is a hole rather than an answer")
	}
	kept := keptIDs(t, rec.verdicts)
	if len(kept) != len(block.shown) {
		t.Fatalf("the record kept %d rows (%v) and the block rendered %d (%v): the record must be the block's own "+
			"verdicts, or the audit's \"used\" ratio is a claim about rows nobody was shown", len(kept), kept, len(block.shown), block.shown)
	}
	for i, id := range block.shown {
		if kept[i] != id {
			t.Errorf("kept[%d] = %q, want %q — the kept ids are the rendered ones, in order", i, kept[i], id)
		}
	}
}

func TestASessionStartRecordsNothingIntoANewerGhostsStore(t *testing.T) {
	projDir, dbPath := sessionRecordFixture(t)
	stampStoreNewer(t, dbPath)

	var block sessionBlock
	logged := captureStderr(t, func() { block = renderSessionStartBlock(t, projDir) })

	if len(block.shown) != 2 {
		t.Errorf("the block rendered %d rows against a store a newer Ghost owns, want 2 — reads must keep working "+
			"against a newer store; only the write is refused", len(block.shown))
	}
	if calls := recordedCalls(t, dbPath); len(calls) != 0 {
		t.Errorf("%d call(s) were recorded into a store a newer Ghost owns (%+v)", len(calls), calls)
	}
	if !strings.Contains(logged, "retrieval record not written") {
		t.Errorf("the newer-store refusal was not reported; stderr held:\n%s", logged)
	}
	// The refusal has to be the store's OWN, not an unrelated failure that happens
	// to be logged: a refusal for any other reason would mean the guard is not what
	// stopped the write and this test would pass for the wrong reason.
	if !strings.Contains(logged, "newer") {
		t.Errorf("the record was refused for a reason other than the store being newer; stderr held:\n%s", logged)
	}
}

func TestTheSessionStoreCarriesTheRecordToo(t *testing.T) {
	src := mcpinitSource(t)
	body := funcBody(t, src["session_passive.go"], "loadSessionPassive")
	if !strings.Contains(body, "Record:") {
		t.Errorf("loadSessionPassive does not put a Record sink on the session-start request, so nothing records the "+
			"injection:\n%s", body)
	}
	if !strings.Contains(body, "Logger:") {
		t.Errorf("loadSessionPassive does not put a Logger on the session-start request: nothing on the hook path calls "+
			"slog.SetDefault, so a refused record would be logged to a handler nobody reads:\n%s", body)
	}

	// The record's own handle, and the guards that keep it from CREATING a store.
	sink := funcBody(t, src["session_passive.go"], "sessionRecordSink")
	if !strings.Contains(sink, "rwDSN(") {
		t.Errorf("the session-start record sink does not open the hook's read-write DSN:\n%s", sink)
	}
	if !strings.Contains(sink, "os.Stat(") {
		t.Errorf("the session-start record sink does not check that the store exists, so a missing database would be "+
			"CREATED by the write meant to describe it:\n%s", sink)
	}
	if !strings.Contains(sink, "TightenPermissions(") {
		t.Errorf("the session-start record sink does not tighten the store's mode, and a record write creates -wal and "+
			"-shm files this connection then leaves behind:\n%s", sink)
	}
}
