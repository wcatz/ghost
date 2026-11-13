package mcpinit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	_ "modernc.org/sqlite"
)

// runSessionStartHook feeds a native session-start payload (wrapped with the
// contract-v1 envelope, as every adapter shim does) through RunHostEvent and
// writes the handler's stdout into out.
func runSessionStartHook(t *testing.T, input string, out io.Writer) {
	t.Helper()
	RunHostEvent("session-start", "claude-code",
		strings.NewReader(contractInputFor(t, "SessionStart", "claude-code", "claude-jsonl", input)),
		out, io.Discard)
}

// TestMain isolates every test in this package from the real
// platform Ghost user config path and ~/.local/share/ghost by default. Without
// this, a test that calls runSessionStartHook (which invokes
// ensureObsidianSyncRunning) and forgets to override these itself would load
// the developer's real config; if obsidian.auto_sync is true there, it
// spawns `ghost obsidian sync` — except os.Executable() resolves to this
// test binary under `go test`, so the "ghost" it spawns is actually another
// copy of the test suite, which spawns another, without bound. Individual
// tests may still override via t.Setenv for cases that need specific config
// content.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ghost-mcpinit-test-*")
	if err != nil {
		panic(err)
	}
	kvs := homeVars(dir)
	kvs = append(kvs,
		[2]string{"XDG_CONFIG_HOME", dir},
		[2]string{"XDG_DATA_HOME", dir},
	)
	for _, kv := range kvs {
		if err := os.Setenv(kv[0], kv[1]); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	os.RemoveAll(dir) //nolint:errcheck
	os.Exit(code)
}

// testStore wraps db in a memory.Store with a discard logger, matching the
// package's existing convention (see init.go, status.go).
func testStore(db *sql.DB) *memory.Store {
	return memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// insertProject inserts a project row directly (bypasses EnsureProject uniqueness logic).
func insertProject(t *testing.T, db *sql.DB, id, path, name string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, id, path, name)
	if err != nil {
		t.Fatalf("insertProject: %v", err)
	}
}

// openFileTestDB creates a real on-disk SQLite DB, which is what the passive
// retrieval reads: it takes a Store, and a Store needs a handle the migration
// path has already stamped.
func openFileTestDB(t *testing.T) (db *sql.DB, path string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "ghost.db")
	db, err := memory.OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

// mustHookConfig is config.LoadForHook under a name that says it cannot fail for
// a test: the hook path is handed a value that falls back to the compiled defaults
// rather than returning an error, so every session-start caller already treats it
// as always non-nil. A test that built its own *config.Config instead would be
// testing a policy no user ever gets.
func mustHookConfig(t *testing.T) *config.Config {
	t.Helper()
	return config.LoadForHook()
}

// passiveGlobals runs the session-start passive retrieval against the store at
// dbPath and returns only the `_global` rows, beside the tally the block's
// count lines are built from.
//
// It replaces the direct `loadGlobalMemories` call these tests used to make. The
// global bucket is now ONE slice of one budget, so there is no longer a function
// that returns "the globals" on its own: a test that wants them asks the passive
// retrieval for the project's rows and gets the globals in the other half of the
// same answer, which is also what proves the two buckets cannot drift into
// disagreeing about the same store. The tally is the same trace's count of the
// bucket, so a test that asserts on "the total the header divides by" reads the
// one number the block actually renders.
func passiveGlobals(t *testing.T, db *sql.DB, projectID string) (globals []sessionMemory, tally sessionTally) {
	t.Helper()
	store := memory.NewStoreWithRead(db, db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// nil sink: this helper reads a handle the test owns and asserts on the rows,
	// so the record would be a write into a database the assertions do not expect
	// to change. The recording path is covered by passive_record_test.go.
	_, globals, tally = loadSessionPassive(context.Background(), store, config.LoadForHook(), projectID, time.Now(), nil, "")
	return globals, tally
}

// TestPassiveGlobalsDedupNearDuplicates: two near-duplicate globals
// linked above the globals demotion threshold (0.85) must not both survive,
// even though 0.8857 is below the general DefaultDemotionThreshold (0.90)
// used for project memories.
func TestPassiveGlobalsDedupNearDuplicates(t *testing.T) {
	db, _ := openFileTestDB(t)

	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global project: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES ('gaaa0001', '_global', 'convention', 'ORIGINAL restricted repos are read-only', 'manual', 0.9)`,
	); err != nil {
		t.Fatalf("insert a: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES ('gbbb0001', '_global', 'convention', 'RESTATED restricted repos are read-only too', 'manual', 0.89)`,
	); err != nil {
		t.Fatalf("insert b: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memory_links (source_id, target_id, relation, strength, source) VALUES ('gaaa0001', 'gbbb0001', 'related', 0.8857, 'auto')`,
	); err != nil {
		t.Fatalf("insert link: %v", err)
	}

	globals, tally := passiveGlobals(t, db, "p-nodup")
	// The header's count and the trace agree: the removed loser is the one Deduped
	// row, counted from the assembler's own trace rather than inferred (#894).
	if tally.globals.Deduped != 1 || tally.globals.Total() != 2 {
		t.Errorf("globals tally = %+v, want Deduped 1 of 2 total", tally.globals)
	}
	var sawOriginal, sawRestated bool
	for _, m := range globals {
		if strings.Contains(m.Content, "ORIGINAL") {
			sawOriginal = true
		}
		if strings.Contains(m.Content, "RESTATED") {
			sawRestated = true
		}
	}
	if !sawOriginal {
		t.Errorf("higher-ranked global must survive; got %+v", globals)
	}
	if sawRestated {
		t.Errorf("lower-ranked near-duplicate global must be demoted out; got %+v", globals)
	}
}

// TestSessionCounterIncrements: each hook invocation bumps the project's
// session counter — the one deliberate write the hook makes — and the emitted
// "Session #N" reflects the post-increment count.
func TestSessionCounterIncrements(t *testing.T) {
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")

	projDir := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}
	// EvalSymlinks in the hook canonicalizes cwd; store the canonical path.
	canonical, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'myproj')`, canonical); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	_ = db.Close()

	t.Setenv("XDG_DATA_HOME", xdgHome)

	run := func() string {
		input, _ := json.Marshal(map[string]string{"cwd": projDir})
		var out strings.Builder
		runSessionStartHook(t, string(input), &out)
		return out.String()
	}
	if got := run(); !strings.Contains(got, "Session #1") {
		t.Errorf("first run should show Session #1; got:\n%s", got)
	}
	if got := run(); !strings.Contains(got, "Session #2") {
		t.Errorf("second run should show Session #2; got:\n%s", got)
	}
}

// TestSessionCounterIgnoresResumeClearCompact: resume/clear/compact fire
// SessionStart too, but a user perceives those as continuing the same
// session — only "startup" (or an empty/legacy source) should bump the count.
func TestSessionCounterIgnoresResumeClearCompact(t *testing.T) {
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")

	projDir := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'myproj')`, canonical); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	_ = db.Close()

	t.Setenv("XDG_DATA_HOME", xdgHome)

	run := func(source string) string {
		m := map[string]string{"cwd": projDir}
		if source != "" {
			m["source"] = source
		}
		input, _ := json.Marshal(m)
		var out strings.Builder
		runSessionStartHook(t, string(input), &out)
		return out.String()
	}

	if got := run("startup"); !strings.Contains(got, "Session #1") {
		t.Errorf("startup should show Session #1; got:\n%s", got)
	}
	// resume and compact are now suppressed/shrunk (see
	// TestHandleSessionStartHook_ResumeSuppressed and
	// TestHandleSessionStartHook_CompactShrunk) so they no longer echo the
	// session count at all; clear still gets full injection. What this test
	// cares about is that none of the three bump the underlying counter —
	// proven below by the follow-up startup still landing on Session #2.
	for _, source := range []string{"resume", "clear", "compact"} {
		run(source)
	}
	if got := run("clear"); !strings.Contains(got, "Session #1") {
		t.Errorf("clear should NOT bump the counter, still Session #1; got:\n%s", got)
	}
	if got := run("startup"); !strings.Contains(got, "Session #2") {
		t.Errorf("second startup should show Session #2; got:\n%s", got)
	}
}

// TestBumpSessionCountNoPhantomDB: the counter write path must never create a
// database that isn't there.
func TestBumpSessionCountNoPhantomDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	if n := bumpSessionCount(dbPath, "p1"); n != 0 {
		t.Errorf("bump on missing DB returned %d, want 0", n)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("bumpSessionCount must not create %s", dbPath)
	}
}

// TestDSNBusyTimeoutValues locks in the deliberate #288 asymmetry: the write
// path's busy_timeout now matches Store's (memory.OpenDB, busy_timeout(5000)),
// while the read path stays at the original 1000ms — a reader never blocks on
// a concurrent writer under Store's WAL journal mode, so it was never exposed
// to the write-contention failure #288 reported. A future edit that silently
// drifts either value, or flips mode=ro, should fail this test.
func TestDSNBusyTimeoutValues(t *testing.T) {
	const dbPath = "/unused/ghost.db" // DSN string shape only - never opened

	ro := roDSN(dbPath)
	if !strings.Contains(ro, "busy_timeout(1000)") {
		t.Errorf("roDSN = %q, want it to contain busy_timeout(1000)", ro)
	}
	if !strings.Contains(ro, "mode=ro") {
		t.Errorf("roDSN = %q, want it to contain mode=ro", ro)
	}

	rw := rwDSN(dbPath)
	if !strings.Contains(rw, "busy_timeout(5000)") {
		t.Errorf("rwDSN = %q, want it to contain busy_timeout(5000)", rw)
	}
	if strings.Contains(rw, "mode=ro") {
		t.Errorf("rwDSN = %q, must not be read-only", rw)
	}
}

// TestBumpSessionCountSurvivesWriteContention proves the #288 fix: a
// concurrent writer — standing in for a live MCP server, or the
// linking/embedding background workers — holding SQLite's single write lock
// for longer than the old 1000ms busy_timeout, but less than the new 5000ms,
// must no longer make the bump fail. Uses a real file-backed database and a
// genuine BEGIN IMMEDIATE lock: busy_timeout is a real SQLite locking
// behavior, not something a fake connection could stand in for.
func TestBumpSessionCountSurvivesWriteContention(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	insertProject(t, db, "p1", "/tmp/contend-new", "contend-new")
	db.Close() //nolint:errcheck

	// Longer than the old 1000ms busy_timeout, comfortably short of the new
	// 5000ms one.
	const holdTime = 2 * time.Second

	blocker, err := sql.Open("sqlite", rwDSN(dbPath))
	if err != nil {
		t.Fatalf("open blocker: %v", err)
	}
	defer blocker.Close() //nolint:errcheck

	conn, err := blocker.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	// BEGIN IMMEDIATE (rather than a plain deferred BEGIN) grabs SQLite's
	// write lock right away instead of waiting for the first write statement,
	// so the hold below is deterministic from this point on.
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(holdTime)
		if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
			t.Errorf("release COMMIT: %v", err)
		}
	}()

	start := time.Now()
	n := bumpSessionCount(dbPath, "p1")
	elapsed := time.Since(start)
	<-released

	if n == 0 {
		t.Fatalf("bumpSessionCount failed under %v of write contention; want it to ride out the lock now that it uses busy_timeout(5000)", holdTime)
	}
	if elapsed < holdTime {
		t.Errorf("bumpSessionCount returned after %v, before the %v lock was even released — busy_timeout may not be taking effect", elapsed, holdTime)
	}
}

// TestOldBusyTimeoutFailsUnderSameContention documents the #288 regression
// directly: the exact DSN bumpSessionCount used before this fix
// (busy_timeout(1000), reconstructed verbatim here since rwDSN now reports
// 5000ms) does time out and fail under the same write contention that
// TestBumpSessionCountSurvivesWriteContention proves the new value survives —
// this is the "silently returns 0, showing a stale session count" symptom
// #288 reported.
func TestOldBusyTimeoutFailsUnderSameContention(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "ghost.db")

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	insertProject(t, db, "p1", "/tmp/contend-old", "contend-old")
	db.Close() //nolint:errcheck

	const holdTime = 2 * time.Second // > old 1000ms busy_timeout

	blocker, err := sql.Open("sqlite", rwDSN(dbPath))
	if err != nil {
		t.Fatalf("open blocker: %v", err)
	}
	defer blocker.Close() //nolint:errcheck

	conn, err := blocker.Conn(context.Background())
	if err != nil {
		t.Fatalf("Conn: %v", err)
	}
	defer conn.Close() //nolint:errcheck

	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(holdTime)
		if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
			t.Errorf("release COMMIT: %v", err)
		}
	}()

	oldDSN := url.URL{
		Scheme:   "file",
		Opaque:   (&url.URL{Path: dbPath}).EscapedPath(),
		RawQuery: "_pragma=busy_timeout(1000)",
	}
	oldDB, err := sql.Open("sqlite", oldDSN.String())
	if err != nil {
		t.Fatalf("open with pre-#288 DSN: %v", err)
	}
	defer oldDB.Close() //nolint:errcheck

	start := time.Now()
	var n int
	err = oldDB.QueryRow(`
		INSERT INTO ghost_state (project_id, interaction_count)
		VALUES (?, 1)
		ON CONFLICT(project_id) DO UPDATE SET
			interaction_count = interaction_count + 1,
			updated_at = datetime('now')
		RETURNING interaction_count
	`, "p1").Scan(&n)
	elapsed := time.Since(start)
	<-released

	// The load-bearing assertion is err != nil: that's what proves the old
	// 1000ms value fails under this contention. Elapsed time is logged, not
	// asserted on — SQLite's busy handler retries until ~1000ms of
	// *accumulated* sleep, not a wall-clock guarantee, and a loaded/-race CI
	// runner can push observed elapsed well past that (see the similar
	// caveat in internal/obsidian/sync_test.go). Asserting elapsed < holdTime
	// here would give a test that fails two different ways under slowness.
	t.Logf("pre-#288 DSN write failed after %v (hold time %v)", elapsed, holdTime)
	if err == nil {
		t.Fatalf("expected the pre-#288 busy_timeout(1000) to fail under %v of contention, but it succeeded (n=%d) — the contention setup itself may be wrong", holdTime, n)
	}
}

// TestSessionInjectionRespectsNewCap: the memory cap is 15, not 25 — a
// project with more than 15 active memories must show exactly 15 plus a
// "not shown" count. This test only exercises the cap/not-shown behavior;
// decay-ranking parity with Store.GetTopMemories is covered separately by
// TestSessionInjectionUsesDecayRanking (all rows here are category 'fact',
// which never decays, so this fixture can't distinguish decayed-score
// ordering from raw-importance ordering).
func TestSessionInjectionRespectsNewCap(t *testing.T) {
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")

	projDir := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'myproj')`, canonical); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("capfil%02d", i)
		importance := 0.9 - float64(i)*0.01
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES (?, 'p1', 'fact', ?, ?, ?)`,
			id, fmt.Sprintf("CAPFILLER%02d content", i), "manual", importance,
		); err != nil {
			t.Fatalf("insert cap filler %d: %v", i, err)
		}
	}
	_ = db.Close()

	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	input, _ := json.Marshal(map[string]string{"cwd": projDir})
	var out strings.Builder
	runSessionStartHook(t, string(input), &out)
	result := out.String()

	if !strings.Contains(result, "15 shown of 20 total — 5 not shown") {
		t.Errorf("expected cap of 15 with not-shown count, got:\n%s", result)
	}
	if !strings.Contains(result, "ranked by a composite score of importance, pinned status, and category-aware recency decay") {
		t.Errorf("memories not-shown line must explain the ranking factors; got:\n%s", result)
	}
}

// TestSessionInjectionUsesDecayRanking: ranking must follow the category-aware
// decayed score (parity with Store.GetTopMemories), not raw importance. The
// fixture packs 15 'fact' filler rows (importance 0.2, fresh created_at —
// facts never decay, so score stays 0.2) plus one 'gotcha' row (importance
// 0.9, created_at ~400 days back — decays to the 0.15 floor, score
// 0.9*0.15=0.135). Under raw-importance ordering the gotcha (0.9) would rank
// first; under decayed-score ordering it scores below every fact (0.135 <
// 0.2) and is the LAST row of the 16, which is what this asserts by position.
//
// The share changed which row the 15-cap leaves out, and that is its job. The
// project bucket holds 16 rows under a 15-row cap, so stage 7 divides it: the
// facts fill their half, the gotcha takes the slot reserved for a second
// category, and the remaining facts come back — 14 facts plus the gotcha, with
// one FACT deferred behind the window and cut. A block that is all one thing
// is what the stage exists to prevent, so the marker that used to be absent
// is now the one the block gains; the decay ranking is unchanged, and the
// gotcha still sorts last.
//
// EVERY STAMP COMES FROM ONE FIXED INSTANT rather than from the clock, and that
// is what makes the fixture one answer on every run and every platform. The 15
// fact rows tie on score (0.2), on importance (0.2) and on created_at, and the
// ranking's next key after those is created_at DESC — so rows left to
// datetime('now') are ordered by which side of a wall-clock second each INSERT
// landed on. Fifteen INSERTs take long enough for the loop to straddle a second
// boundary; when the boundary falls between the last two, created_at DESC
// promotes the last fact and the cap then cuts a DIFFERENT fact, so a test
// naming the 14th fact reads as intermittent (#1002 — the bench corpus had the
// same hole before #708, and `TestGoldenFixtureIsStableAcrossRuns` is the
// golden's guard against it). One instant per fixture removes the boundary
// from the picture entirely, and the gotcha stays 400 days behind it whether
// the host clock reads today or a decade from now.
func TestSessionInjectionUsesDecayRanking(t *testing.T) {
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")

	projDir := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatalf("mkdir proj: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projDir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'myproj')`, canonical); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	// The one instant every stamp in this fixture is derived from. Written in
	// memory.StoredStampLayout, which is the shape datetime('now') itself
	// writes, so julianday() parses it exactly as it parses a stored one.
	fixtureNow := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	freshStamp := fixtureNow.Format(memory.StoredStampLayout)
	oldStamp := fixtureNow.AddDate(0, 0, -400).Format(memory.StoredStampLayout)
	// 15 low-importance 'fact' rows — never decay, score stays 0.2 each. All
	// fifteen share freshStamp, so the tie is broken by the ranking's last key
	// (the id) and by nothing about the run.
	for i := 0; i < 15; i++ {
		id := fmt.Sprintf("factfil%02d", i)
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance, created_at)
			 VALUES (?, 'p1', 'fact', ?, ?, ?, ?)`,
			id, fmt.Sprintf("FACTMARKER%02d content", i), "manual", 0.2, freshStamp,
		); err != nil {
			t.Fatalf("insert fact filler %d: %v", i, err)
		}
	}
	// High-importance 'gotcha' with an old created_at — decays to the 0.15
	// floor, decayed score 0.9*0.15=0.135, below every fact's 0.2. The 400-day
	// age is a reading of the fixture's own instant, so the floor is reached by
	// a wide margin rather than by however far the host clock has moved.
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, importance, created_at)
		 VALUES ('gotcha01', 'p1', 'gotcha', 'GOTCHAMARKER old high-importance', 'manual', 0.9, ?)`,
		oldStamp,
	); err != nil {
		t.Fatalf("insert gotcha: %v", err)
	}
	_ = db.Close()

	// Opt out of the category-behavior floor. The fixture's whole point is
	// decay-ranking parity (rank-only selection): an old, high-importance
	// gotcha decays below every fresh fact and must be the row trimmed by the
	// cap. Under the default behavior floor the gotcha would instead be
	// promoted into a behavioral slot — which is TestSessionInjectionBehaviorFloor's
	// job to assert — so set behavior_floor=0 here to pin the legacy rank-only
	// path exactly.
	cfgDir := filepath.Join(t.TempDir(), "ghost")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	cfgPath := filepath.Join(cfgDir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("injection:\n  behavior_floor: 0\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Dir(cfgDir))

	input, _ := json.Marshal(map[string]string{"cwd": projDir})
	var out strings.Builder
	runSessionStartHook(t, string(input), &out)
	result := out.String()

	// The decayed gotcha takes the slot the share reserves for a second category,
	// so the block holds it and one fact is the row left out.
	if !strings.Contains(result, "GOTCHAMARKER") {
		t.Errorf("the project bucket holds 16 rows under a 15-row cap, so the share reserves a slot for a second category and the gotcha takes it; got:\n%s", result)
	}
	if !strings.Contains(result, "FACTMARKER") {
		t.Errorf("expected fact rows (score 0.2) to survive the cap; got:\n%s", result)
	}
	if !strings.Contains(result, "15 shown of 16 total — 1 withheld rather than ranked out") {
		t.Errorf("expected cap of 15 with 1 not-shown, got:\n%s", result)
	}
	// The decay ranking is untouched: the gotcha sorts last in the block, below
	// every fact, which is the assertion the raw-importance ordering fails.
	//
	// Every fact marker rather than one named fact, and that is deliberate.
	// WHICH fact the 15-cap leaves out is the diversity share's business and a
	// legitimate answer to a tie, so a test naming the 14th fact would fail on
	// a selection that is correct and only re-sorted. What must not move is the
	// gotcha's POSITION against all of them.
	gotchaAt := strings.Index(result, "GOTCHAMARKER")
	if gotchaAt < 0 {
		t.Fatalf("the gotcha is absent from the block, so no ordering below it can be asserted:\n%s", result)
	}
	for i := 0; i < 15; i++ {
		factAt := strings.Index(result, fmt.Sprintf("FACTMARKER%02d", i))
		if factAt >= 0 && factAt > gotchaAt {
			t.Errorf("the decayed gotcha must sort last, below every fact: fact%02d at %d, gotcha at %d", i, factAt, gotchaAt)
		}
	}
}
