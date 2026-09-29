package mcpinit

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// Issue #729, at the command boundary. The arithmetic and the thresholds are
// tested in internal/memory and the sentences come from there too, so what is
// left to test here is the part only this layer owns: that `ghost mcp status`
// prints the report at all, that it prints it as an observation rather than as a
// failed check, and that it says something honest on a store with no history and
// on a store nobody has written to today.

// historyStore is a real store on disk holding `memories` memories with
// `restatements+1` version rows each, every row recorded `ageAgo` in the past
// ("0 seconds" meaning now).
func historyStore(t *testing.T, memories, restatements int, ageAgo string) *memory.Store {
	t.Helper()
	store, _ := historyStoreWithPath(t, memories, restatements, ageAgo)
	return store
}

// historyStoreWithPath is historyStore plus the database's path, for the one
// fixture that has to reach the file from a second handle: memory_history is
// dropped from OUTSIDE the store under test, so nothing about the store's own
// state changes and the only thing the read finds is a missing table.
func historyStoreWithPath(t *testing.T, memories, restatements int, ageAgo string) (*memory.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := memory.NewStore(db, discardLogger())
	if err := store.EnsureProject(context.Background(), "p1", "/tmp/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	seedHistoryVersions(t, db, memories, restatements, ageAgo)
	return store, dbPath
}

// seedHistoryVersions saves `memories` memories and then appends `restatements`
// version rows per memory that restate the live row, which is what every applied
// reflection used to leave behind for every memory it kept.
//
// The restatements are written by raw SQL because no writer produces them any
// more: #727 stopped writing them and #730 exists to remove the ones already
// stored, so there is no call left to make — the same position internal/memory's
// own fixtures are in, and for the same reason. A fixture that drove a current
// writer would be measuring something the report cannot see.
//
// ageAgo moves the save rows too, so a "quiet store" is quiet on BOTH its
// versions rather than holding one in-window save per memory.
func seedHistoryVersions(t *testing.T, db *sql.DB, memories, restatements int, ageAgo string) {
	t.Helper()
	store := memory.NewStore(db, discardLogger())
	ctx := context.Background()
	for range memories {
		if _, err := store.Create(ctx, "p1", memory.Memory{
			Category: "fact", Content: "the relay listens on port 2222 in production",
			Source: "mcp", Importance: 0.5,
		}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	if ageAgo != "0 seconds" {
		if _, err := db.Exec(
			`UPDATE memory_history SET recorded_at = datetime('now', ?) WHERE phase = 'save'`, ageAgo); err != nil {
			t.Fatalf("age the save rows: %v", err)
		}
	}
	for range restatements {
		if _, err := db.Exec(`
			INSERT INTO memory_history
				(memory_id, project_id, phase, recorded_at, content, category, importance, resolved_at, source)
			SELECT m.id, m.project_id, 'reflect', datetime('now', ?), m.content, m.category, m.importance, m.resolved_at, m.source
			FROM memories m`, ageAgo); err != nil {
			t.Fatalf("seed a restatement round: %v", err)
		}
	}
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestReportHistoryGrowthPrintsTheNumbersAndTheFindings: the report is a
// `-` line carrying the four numbers and a `!` line per finding, and the findings
// are the report's OWN sentences rather than anything composed here — so this
// asserts the numbers are the ones the report decided, and that the sentences
// below them are the ones it wrote.
func TestReportHistoryGrowthPrintsTheNumbersAndTheFindings(t *testing.T) {
	// One memory, twenty restatements: 21 rows, 20 of them restatements (95%),
	// and a memory holding 21 of its 50 versions which at that rate gets there in
	// (50-21)/21 = 1.4 days.
	store := historyStore(t, 1, 20, "0 seconds")

	var out bytes.Buffer
	reportHistoryGrowth(&out, store)
	output := out.String()

	if !strings.Contains(output,
		"- history: 21 version rows in 24h, 20 restatements (95%), deepest memory 21/50 versions, store 21/20000 rows") {
		t.Errorf("the history line does not carry the report's numbers:\n%s", output)
	}
	for _, want := range []string{
		"! 95% of the 21 version rows written in the last 24h restate the version before them",
		"warning threshold 20%",
		"ghost history compact",
		"! memory ", // the memory the countdown is about, named
		"is closest to the per-memory cap: it holds 21 of its 50 versions and wrote 21 in the last 24h",
		"the cap is 1.4 days away at that rate",
		"warning threshold 14 days",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("the history findings do not include %q:\n%s", want, output)
		}
	}
	// A finding is a `!` line, never a `✗`: `ghost mcp status` exits non-zero on
	// a failed check and prints "Run `ghost mcp init`", which is the wrong repair
	// for a store whose history needs compacting. The exit code is asserted at
	// the command level below; this is the shape of the line.
	if strings.Contains(output, "✗") {
		t.Errorf("a history finding is printed as a failed check:\n%s", output)
	}
}

// TestReportHistoryGrowthOnAStoreWithNoHistory: a fresh install has no history
// and no growth, and a line of zeroes is a report about a table that does not
// exist. One line saying so is the whole of it.
func TestReportHistoryGrowthOnAStoreWithNoHistory(t *testing.T) {
	store := historyStore(t, 0, 0, "0 seconds")

	var out bytes.Buffer
	reportHistoryGrowth(&out, store)
	output := out.String()

	if !strings.Contains(output, "- history: no version rows recorded yet") {
		t.Errorf("an empty store does not say it has no history yet:\n%s", output)
	}
	if strings.Contains(output, "!") {
		t.Errorf("an empty store produces a finding:\n%s", output)
	}
}

// TestReportHistoryGrowthOnAQuietStoreIsNotAFinding: a store nobody wrote to
// TODAY is every store at 9am, and its table is not empty. The report must say
// the numbers and stop there — a warning here would fire on every quiet morning
// and train the reader to skip the line that matters.
func TestReportHistoryGrowthOnAQuietStoreIsNotAFinding(t *testing.T) {
	store := historyStore(t, 2, 5, "-2 days")

	var out bytes.Buffer
	reportHistoryGrowth(&out, store)
	output := out.String()

	if !strings.Contains(output,
		"- history: 0 version rows in 24h, 0 restatements (0%), deepest memory 6/50 versions, store 12/20000 rows") {
		t.Errorf("a quiet store does not report its numbers:\n%s", output)
	}
	if strings.Contains(output, "!") {
		t.Errorf("a quiet store produces a finding, which is a warning that fires every morning:\n%s", output)
	}
}

// TestReportHistoryGrowthSaysWhenTheReadFails: docs/cli.md states that a check
// which cannot run must not print nothing at all, and this is the branch that
// claim rests on. Returning silently here would make an unreadable history
// indistinguishable from a store with nothing to say — and the read is the most
// expensive statement in the command, so a context deadline reaching it after the
// database check already succeeded is a realistic way to get there.
//
// The failure is induced by dropping memory_history from a SECOND handle, which
// is the only way to make this statement fail on a store that opened cleanly: a
// dropped table is an error the read cannot recover from and cannot mistake for an
// empty report.
//
// The line is a `!` and not a `✗`, for the same reason every other finding is (see
// above): `ghost mcp init` does not repair a history table, so failing the run
// would name the wrong fix for it. ghost_health's counterpart is
// TestHealthSaysWhenTheHistoryReadFails, and the two together are what makes
// "the surfaces cannot disagree about one store" true when the read fails rather
// than only when it succeeds.
func TestReportHistoryGrowthSaysWhenTheReadFails(t *testing.T) {
	store, dbPath := historyStoreWithPath(t, 1, 3, "0 seconds")

	dropHistoryTableFile(t, dbPath)

	var out bytes.Buffer
	reportHistoryGrowth(&out, store)
	output := out.String()

	if !strings.Contains(output, "history growth:") {
		t.Fatalf("a failed history read printed nothing at all:\n%s", output)
	}
	// The error itself, not a summary of it: the operator's next question is
	// which table and why, and "history growth failed" does not answer it.
	if !strings.Contains(output, "memory_history") {
		t.Errorf("the failed read does not name what could not be read:\n%s", output)
	}
	// And it must not have fallen back to reporting numbers it does not have: a
	// store whose table is gone has no share and no cap headroom, and printing
	// zeroes for them would be a claim rather than an absence.
	if strings.Contains(output, "version rows in 24h") || strings.Contains(output, "no version rows recorded yet") {
		t.Errorf("the report published an empty history after a failed read:\n%s", output)
	}
	if strings.Contains(output, "✗") {
		t.Errorf("a failed read is printed as a failed check, naming a repair that does not apply:\n%s", output)
	}
}

// dropHistoryTableFile removes memory_history through its own handle, from outside
// the store under test.
func dropHistoryTableFile(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s to drop memory_history: %v", dbPath, err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(`DROP TABLE memory_history`); err != nil {
		t.Fatalf("drop memory_history: %v", err)
	}
}

// TestStatusOpencode_ReportsHistoryGrowthAndStaysHealthy drives the whole
// command, because the thing that can silently not happen is the call: a report
// function nothing calls compiles, tests green, and prints nothing. It also holds
// the exit code, which is the part a reader of the output would assume: a store
// whose history is filling is a working integration, and `ghost mcp status` must
// still say "All checks passed."
func TestStatusOpencode_ReportsHistoryGrowthAndStaysHealthy(t *testing.T) {
	ollama := ollamaStub("nomic-embed-text:v1.5")
	defer ollama.Close()

	statusEnv(t)
	binDir := writeStubGhost(t)
	t.Setenv("PATH", binDir)
	installOpencodePluginFile(t, stubPath(binDir, "ghost"))
	writeOpencodeMCPConfig(t, "opencode.json", opencodeMCPRegistration(stubPath(binDir, "ghost")))

	ghostDir := filepath.Join(os.Getenv("XDG_DATA_HOME"), "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghost dir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("open fresh db: %v", err)
	}
	store := memory.NewStore(db, discardLogger())
	if err := store.EnsureProject(context.Background(), "p1", "/tmp/p1", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if _, err := store.Create(context.Background(), "p1", memory.Memory{
		Category: "fact", Content: "the relay listens on port 2222 in production",
		Source: "mcp", Importance: 0.5,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Twenty more version rows restating that save, which is what every applied
	// reflection used to leave behind: 21 rows in the window, 20 of them
	// restatements, and a memory 21 versions into its 50.
	for range 20 {
		if _, err := db.Exec(`
			INSERT INTO memory_history
				(memory_id, project_id, phase, recorded_at, content, category, importance, resolved_at, source)
			SELECT m.id, m.project_id, 'reflect', datetime('now'), m.content, m.category, m.importance, m.resolved_at, m.source
			FROM memories m`); err != nil {
			t.Fatalf("seed a restatement: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	var out bytes.Buffer
	healthy, err := StatusOpencode(&out)
	if err != nil {
		t.Fatalf("StatusOpencode: %v", err)
	}
	output := out.String()

	if !strings.Contains(output, "- history: 21 version rows in 24h, 20 restatements (95%)") {
		t.Errorf("`ghost mcp status --client opencode` does not report history growth:\n%s", output)
	}
	if !strings.Contains(output, "restate the version before them") {
		t.Errorf("`ghost mcp status` does not warn about the restatement rate:\n%s", output)
	}
	// The verdict is about wiring, and a filling history is not a wiring fault.
	if !healthy {
		t.Errorf("`ghost mcp status` is unhealthy over a full history table:\n%s", output)
	}
	if !strings.Contains(output, "All checks passed.") {
		t.Errorf("`ghost mcp status` does not report the integration as healthy:\n%s", output)
	}
	if strings.Contains(output, "Run `ghost mcp init") {
		t.Errorf("`ghost mcp status` points at the wrong repair for a history finding:\n%s", output)
	}
}
