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
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
//
// Closed, because that is the state the hook meets: a store left behind by a
// previous session, opened fresh here through config.DataDir. And under a
// sandboxed XDG_DATA_HOME, so the handle the record write opens is opened against
// this file and not against a developer's real store.
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
//
// It fails the test when the fixture did not produce one row per bucket, because
// every assertion below is about a record that MATCHES a rendered block: a test
// that passed on an empty block would prove nothing about either side.
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
//
// A drop is reported rather than skipped: for this block the assertion is that
// the record AGREES with what was rendered, and a silently dropped verdict would
// let "agrees" hold for the wrong reason.
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
//
// The redirect is the package variable rather than a logger injection because the
// handler under test is BUILT from os.Stderr inside the call — sessionStore runs
// per session start — so swapping the variable before the call is what reaches it.
// It is the same mechanism cmd/ghost's captureStdout uses, and the copy runs
// concurrently with the writer so a log larger than the pipe buffer cannot
// deadlock the test.
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

// holdWriteLock takes the write lock on dbPath, holds it for `hold`, and commits.
// It releases the lock before the test returns, so a reader in the test sees a
// committed store.
//
// This is the shape of a busy store in production: a live `ghost mcp` server's own
// write. A WAL reader does not wait for it, which is why the block still renders
// while the record cannot be written.
func holdWriteLock(t *testing.T, dbPath string, hold time.Duration) {
	t.Helper()
	released := make(chan struct{})
	holding := make(chan struct{})
	go func() {
		defer close(released)
		db, err := sql.Open("sqlite", rwDSN(dbPath))
		if err != nil {
			t.Errorf("open the blocking handle: %v", err)
			close(holding)
			return
		}
		defer func() { _ = db.Close() }()
		var one int
		// sql.Open does no I/O, so force a real connection before the transaction.
		if err := db.QueryRow(`SELECT count(*) FROM projects`).Scan(&one); err != nil {
			t.Errorf("blocker query: %v", err)
			close(holding)
			return
		}
		tx, err := db.Begin()
		if err != nil {
			t.Errorf("blocker Begin: %v", err)
			close(holding)
			return
		}
		if _, err := tx.Exec(`UPDATE ghost_state SET updated_at = updated_at WHERE project_id = 'p1'`); err != nil {
			t.Errorf("blocker update: %v", err)
			_ = tx.Rollback()
			close(holding)
			return
		}
		close(holding)
		time.Sleep(hold)
		_ = tx.Commit()
	}()
	<-holding
	t.Cleanup(func() { <-released })
}

// TestSessionStartRecordsTheCallItRendered is the property #850 is about: a
// session start that shows two memories leaves a row saying so.
//
// Every column is asserted rather than the row's existence, because the record's
// value is entirely in its shape. `source` is what puts the row in the
// session_start denominator instead of the search one; an empty `query_hash` is
// the honest value for a call that carried no question — passive retrieval is
// keyed on that absence — where a digest of "" would be the same constant on
// every injection, in a column that reads like a fingerprint; an empty
// `session_id` is the truth over stdio, which reports none. The kept ids are
// compared with the ids the block RENDERED rather than the ids the store holds:
// the block is what the agent was given, and a record naming a row the block
// dropped would be the "used" verdict this table exists to keep honest.
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
		t.Errorf("session_id = %q, want empty: Ghost serves stdio, whose connection reports no session id", rec.sessionID)
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

// TestTheSessionStartRecordCarriesNoMemoryText: the table holds verdicts and ids,
// and never the text those ids name.
//
// A record is a row about a user's own memories, and it outlives the call: a
// `ghost backup` copies the file whole, and a purge deletes the rows NAMING a
// memory rather than any words of it, because this table keeps none. So the
// assertion is over the raw bytes of EVERY column, matched as a substring — a
// struct check would be a claim about this build's type, and equality would miss
// a leak that stored the text inside a longer value (a verdict object's reason,
// say), which is exactly the shape a later writer would produce.
func TestTheSessionStartRecordCarriesNoMemoryText(t *testing.T) {
	projDir, dbPath := sessionRecordFixture(t)
	renderSessionStartBlock(t, projDir)

	db, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("OpenReadDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	columns := []string{"project_id", "session_id", "source", "query_hash", "as_of", "reason", "verdicts", "outcome", "recorded_at"}
	for _, text := range []string{recordProjectSentinel, recordGlobalSentinel} {
		for _, column := range columns {
			// instr rather than LIKE, so no character of the sentinel is a wildcard.
			var leaked int
			if err := db.QueryRow(`SELECT count(*) FROM retrieval_record WHERE instr(`+column+`, ?) > 0`, text).Scan(&leaked); err != nil {
				t.Fatalf("scan retrieval_record.%s for the sentinel: %v", column, err)
			}
			if leaked != 0 {
				t.Errorf("retrieval_record.%s holds a rendered memory's text in %d row(s): the record is verdicts and ids, "+
					"and a memory's content here outlives the call and travels through every backup", column, leaked)
			}
		}
	}
}

// TestASessionStartInAnUnknownDirectoryRecordsNothing: the one session start that
// cannot be recorded, and it is not a bug.
//
// `RecordRetrieval` refuses an empty project id, and an unmatched directory has no
// project to attribute the call to. Both alternatives are worse: a row under
// `_global` would put an injection that was never this project's into that
// project's denominator, and a row under a placeholder id would name a project no
// report can resolve. So the outcome is silence — and it is silent rather than
// noisy, because a store that cannot record must not print a refusal on every
// session a user opens in a directory Ghost has never seen. The block still
// renders, which is the half that must not change.
func TestASessionStartInAnUnknownDirectoryRecordsNothing(t *testing.T) {
	_, dbPath := sessionRecordFixture(t)
	unknown := filepath.Join(t.TempDir(), "nowhere")

	logged := captureStderr(t, func() {
		projectID, _, _, globals, _, _, _, _, _ := loadSessionContext(unknown, config.LoadForHook())
		if projectID != "" {
			t.Fatalf("project = %q, want \"\" — this directory is supposed to match no project", projectID)
		}
		if len(globals) != 1 {
			t.Fatalf("an unmatched directory rendered %d global rows, want 1: the Global section must survive a session "+
				"that matched no project, which is the case worth protecting here", len(globals))
		}
	})

	if calls := recordedCalls(t, dbPath); len(calls) != 0 {
		t.Errorf("a session start with no project recorded %d call(s) (%+v), want none — there is no project to name", len(calls), calls)
	}
	if strings.Contains(logged, "retrieval record not written") {
		t.Errorf("a session start in an unrecognised directory reported a refused record, so every such session would "+
			"print a warning about a store that is fine. stderr held:\n%s", logged)
	}
}

// TestASessionStartWhoseRecordIsRefusedStillRendersTheBlock is the fail-open
// half, and the fixture is a store that REFUSES the write rather than one that is
// slow.
//
// The table is dropped after a first, recorded run, so the one test holds both
// answers: the block the healthy store produced and the block the refusing one
// produces must be the same rows, because a measurement that changed what it
// measured makes every number it produces a description of the measurement rather
// than of the session. The log line is asserted too — fail-open without a word is
// the failure `emit`'s own comment is about, since a record that silently stops
// being written produces a report that is quietly wrong.
func TestASessionStartWhoseRecordIsRefusedStillRendersTheBlock(t *testing.T) {
	projDir, dbPath := sessionRecordFixture(t)

	healthy := renderSessionStartBlock(t, projDir)
	if calls := recordedCalls(t, dbPath); len(calls) != 1 {
		t.Fatalf("the control run recorded %d calls, want 1 — the fixture no longer exercises a recording session start", len(calls))
	}

	// A store that cannot hold the row. Deliberately not the version guard (that is
	// the next test), so the block-unchanged claim does not rest on one refusal.
	drop, err := sql.Open("sqlite", rwDSN(dbPath))
	if err != nil {
		t.Fatalf("open the drop handle: %v", err)
	}
	if _, err := drop.Exec(`DROP TABLE retrieval_record`); err != nil {
		t.Fatalf("drop retrieval_record: %v", err)
	}
	if err := drop.Close(); err != nil {
		t.Fatalf("close the drop handle: %v", err)
	}

	var refused sessionBlock
	logged := captureStderr(t, func() { refused = renderSessionStartBlock(t, projDir) })

	if len(refused.shown) != len(healthy.shown) {
		t.Fatalf("the block rendered %d rows with the record refused, want the healthy store's %d — a failed record "+
			"must cost the session nothing", len(refused.shown), len(healthy.shown))
	}
	for i, id := range healthy.shown {
		if refused.shown[i] != id {
			t.Errorf("row %d is %q with the record refused, want %q: the refused write changed the block", i, refused.shown[i], id)
		}
	}
	if !strings.Contains(logged, "retrieval record not written") {
		t.Errorf("the record was refused and nothing said so; fail-open is correct and silence about it is not. "+
			"stderr held:\n%s", logged)
	}
}

// TestASessionStartIsNotDelayedByARefusedRecordWrite: fail open is half the
// requirement, and the other half is that the refusal is BOUNDED.
//
// The store's own bounds are the assembler's 250ms of context, a TryLock poll
// against the store mutex, and a 150ms scoped busy_timeout on the write — while a
// read handle in WAL never waits for the writer holding the lock. So a session
// start whose record cannot be written returns well inside the second the lock is
// held for, with the block complete. Without the bound the write would ride out
// the lock it is waiting on and the session would hang behind a measurement, which
// is the one direction this must never fail in.
func TestASessionStartIsNotDelayedByARefusedRecordWrite(t *testing.T) {
	projDir, dbPath := sessionRecordFixture(t)

	hold := 2 * time.Second
	holdWriteLock(t, dbPath, hold)

	var block sessionBlock
	var elapsed time.Duration
	logged := captureStderr(t, func() {
		start := time.Now()
		block = renderSessionStartBlock(t, projDir)
		elapsed = time.Since(start)
	})

	if len(block.shown) != 2 {
		t.Fatalf("the block rendered %d rows while the store was locked, want 2 — a read in WAL does not wait for a "+
			"writer, so the block is complete and only the record is lost", len(block.shown))
	}
	// Without this the timing below proves nothing at all: a surface that never
	// recorded would return just as fast. It is the refusal that has the bound.
	if !strings.Contains(logged, "retrieval record not written") {
		t.Errorf("the record was never attempted against the held lock, so the bound below measures a write that did "+
			"not happen:\n%s", logged)
	}
	if elapsed >= hold {
		t.Errorf("the session start took %v with the write lock held for %v, so the record write waited out the lock "+
			"instead of giving up: a session must not be held behind a measurement", elapsed, hold)
	}
	// The loose bound above proves it is not the lock; this one names the budget the
	// write is given (250ms), with room for a loaded machine.
	if elapsed > time.Second {
		t.Errorf("the session start took %v, want well inside the record's own 250ms budget plus the block's reads", elapsed)
	}
}

// TestASessionStartRecordsNothingIntoANewerGhostsStore is the #746 case, and it is
// a different refusal from the one above: the guard that refuses it lives in
// internal/memory, inside the record write's own transaction.
//
// The scenario is the one the guard exists for. The session hook is frequently the
// only Ghost process to touch a store between two MCP sessions, so it runs against
// exactly the databases a stale `ghost mcp` left behind — and a store a newer
// binary migrated underneath it is a store this build's verdict vocabulary cannot
// describe. Refusing is right (a record of what THIS build would have injected,
// written into a store that has since moved, is the unaudited write the refusal
// stops), and refusing must cost the session nothing, because reads against a
// newer store keep working — that is the other half of the same guard.
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

// TestASessionStartDoesNotResolveTheRetrievalKey: a passive call carries no
// question, so it must not touch the key that fingerprints one.
//
// The key is a per-install secret resolved from a file beside the database, and
// resolving it CREATES that file on a first install. A session start asks no
// question, so the honest row carries an empty query_hash (memory.QueryDigest
// returns early for "") — and a hook that resolved the key anyway would write a
// secret to disk for a value nothing will ever read. The assertion is on the
// filesystem rather than on the call, because the file is the consequence an
// operator would find on their disk.
func TestASessionStartDoesNotResolveTheRetrievalKey(t *testing.T) {
	projDir, dbPath := sessionRecordFixture(t)
	renderSessionStartBlock(t, projDir)

	matches, err := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "*key*"))
	if err != nil {
		t.Fatalf("glob the data dir: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("a session start created %v in the data dir; it asked no question, so nothing should have resolved or "+
			"written the per-install retrieval key", matches)
	}
}

// TestTheSessionStoreCarriesTheRecordToo is the structural half, and it is a
// source assertion because the alternative is invisible in a diff: a session-start
// record wired to the read-only store compiles, satisfies `assemble.RecordSink`,
// and fails at run time on every session — with the failure swallowed by the very
// fail-open contract above, so every behavioural test stays green and the audit is
// simply never written.
//
// The properties asserted are the ones that make the second handle safe rather
// than convenient: it is opened on the SAME DSN the hook's one existing write
// uses, so `_txlock=immediate` and busy_timeout are the ones the store's guard
// was reasoned about, and it is opened on the path config.DataDir resolved rather
// than one built here.
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
