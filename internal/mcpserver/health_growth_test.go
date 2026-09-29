package mcpserver

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wcatz/ghost/internal/memory"
)

// Issue #729, at the tool boundary. The arithmetic is tested in internal/memory;
// what only this layer can see is that ghost_health says it AT ALL, that it says
// the same words `ghost mcp status` does, and that it says them additively —
// every field the tool already reported keeps its name and its place.

// ghostHealthText calls ghost_health and returns its text, or fails the test.
func ghostHealthText(t *testing.T, srv *Server) string {
	t.Helper()
	session := connectedClient(t, srv)
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ghost_health"})
	if err != nil {
		t.Fatalf("CallTool ghost_health: %v", err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("ghost_health returned %T, want text", result.Content[0])
	}
	return text.Text
}

// seedRestatements writes `versions` version rows that restate the memory's
// current state, which is the damage #727 stopped new stores taking and #730
// exists to remove. There is no call left that produces them — that is the whole
// point of #727 — so a fixture that wants the report to have something to report
// has to build the table directly, exactly as internal/memory's own fixtures do.
//
// The rows are written to a SEPARATE handle than the one under test, so nothing
// about seeding can leak into the observation: the store the server holds is
// opened before this is called and is not written again.
func seedRestatements(t *testing.T, dbPath, memoryID string, versions int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(dbPath)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open %s read-write to seed history: %v", dbPath, err)
	}
	defer db.Close() //nolint:errcheck
	for i := range versions {
		if _, err := db.Exec(`
			INSERT INTO memory_history
				(memory_id, project_id, phase, recorded_at, content, category, importance, resolved_at, source)
			SELECT id, project_id, 'reflect', datetime('now'), content, category, importance, resolved_at, source
			FROM memories WHERE id = ?`, memoryID); err != nil {
			t.Fatalf("seed restatement %d: %v", i, err)
		}
	}
}

// testStoreWithPath is testStore on a FILE rather than in memory, because
// seeding restatement rows needs a second handle. OpenDB pins MaxOpenConns(1), so
// the store under test owns its one connection for the whole test and a write
// from the test has to come from another one — and an in-memory database has
// exactly one connection's worth of state to share.
func testStoreWithPath(t *testing.T) (*memory.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB(%s): %v", dbPath, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	if err := store.EnsureProject(context.Background(), "abc123", "/tmp/test", "test-project"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return store, dbPath
}

// TestHealthReportsHistoryGrowth: an agent debugging "search seems incomplete"
// is exactly who needs to be told that the history table is filling with
// restatements — because the store cap trims the OLDEST rows, not the noisiest
// ones, so a full table does not just waste space, it starts discarding the record
// of what a memory said first. That has to be visible from the tool, not only
// from a terminal.
func TestHealthReportsHistoryGrowth(t *testing.T) {
	store, dbPath := testStoreWithPath(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	id, err := store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "the relay listens on port 2222 in production",
		Importance: 0.5, Source: "mcp",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// One save, then eleven restatements of it: 92% of the rows in the window say
	// nothing, which is over the 20% threshold by a wide margin.
	seedRestatements(t, dbPath, id, 11)

	text := ghostHealthText(t, srv)

	if !strings.Contains(text, "**History:**") {
		t.Fatalf("ghost_health does not report history growth at all:\n%s", text)
	}
	for _, want := range []string{
		"12 version rows in the last 24h", // the save plus eleven restatements
		"11 restatements (92%)",           // 11 of 12
		"deepest memory holds 12 of its 50 versions",
		"store holds 12 of 20000 rows",
		"restate the version before them",
		"ghost history compact",
		// The cap finding names the memory it is about, by id, with that memory's
		// own two counts — the summary line's "deepest memory" is an aggregate and
		// must not be mistaken for the warning's subject.
		"is closest to the per-memory cap: it holds 12 of its 50 versions and wrote 12 in the last 24h",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("ghost_health does not report %q:\n%s", want, text)
		}
	}
}

// TestHealthHistoryGrowthIsAdditive: ghost_health is a tool other agents parse
// and people read, and its existing fields are the ones a caller keys off. A
// change to this tool that renamed a field, moved a line, or replaced the
// embedding block would break every agent already reading it — so the growth
// block goes at the END, after the fields that were already there, and the
// warning glyphs stay the tool's own.
func TestHealthHistoryGrowthIsAdditive(t *testing.T) {
	store, dbPath := testStoreWithPath(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")
	srv.SetEmbedder(&mockEmbedder{}, make(chan string, 1))
	store.SetEmbeddingIdentity("nomic-embed-text:v1.5:768+prefix")

	ctx := context.Background()
	id, err := store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "the relay listens on port 2222 in production",
		Importance: 0.5, Source: "mcp",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.StoreEmbedding(ctx, id, []float32{1, 0, 0}, "nomic-embed-text:v1.5:768+prefix"); err != nil {
		t.Fatalf("StoreEmbedding: %v", err)
	}
	seedRestatements(t, dbPath, id, 11)

	text := ghostHealthText(t, srv)

	// Everything the tool said before #729, in the order it said it.
	for _, want := range []string{
		"## Ghost Health",
		"**Projects:**",
		"- **test-project** (abc123): 1 memories",
		"**Total memories:** 1",
		"**Embeddings:** enabled — 1/1 memories embedded",
		"**Memory links:**",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("ghost_health lost %q:\n%s", want, text)
		}
	}
	// And the new block is last, so a reader parsing top to bottom sees the same
	// report it saw before with one more section under it.
	links := strings.Index(text, "**Memory links:**")
	history := strings.Index(text, "**History:**")
	if links < 0 || history < 0 || history < links {
		t.Errorf("the history block is not after the existing fields (links at %d, history at %d):\n%s", links, history, text)
	}
	// The findings are warnings, not errors: the tool still succeeded, so a
	// caller reading it is told about the history and not handed a failure.
	if strings.Contains(text, "isError") || strings.Contains(text, "error:") {
		t.Errorf("ghost_health turned a history finding into an error:\n%s", text)
	}
}

// TestHealthOnAStoreWithNoHistorySaysSo: a store nobody has written to is the
// first thing most agents see, and "0 version rows in the last 24h, 0
// restatements (0%), deepest memory holds 0 of its 50 versions" is a report
// about a table that does not exist yet. One line saying so is the honest one.
func TestHealthOnAStoreWithNoHistorySaysSo(t *testing.T) {
	store, _ := testStoreWithPath(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	text := ghostHealthText(t, srv)

	if !strings.Contains(text, "**History:** no version rows recorded yet") {
		t.Errorf("ghost_health does not say the store has no history yet:\n%s", text)
	}
	if strings.Contains(text, "restatements (0%)") {
		t.Errorf("ghost_health reports a share for a table that does not exist:\n%s", text)
	}
	if strings.Contains(text, "⚠") {
		t.Errorf("ghost_health warns about an empty store:\n%s", text)
	}
}

// TestHealthSaysWhenTheHistoryReadFails: the tool used to discard a failed
// HistoryGrowth read and return a report with no **History:** section at all, so
// an agent could not tell "this store has no growth to report" from "the read was
// cut short" — and the read is the most expensive statement in the tool (a pass
// over memory_history plus a correlated sub-select per row), so a request-context
// deadline reaching it after ListProjects already succeeded is a realistic way to
// get there.
//
// The two surfaces are supposed to be unable to disagree about one store, and this
// was a disagreement with a rule attached: `ghost mcp status` prints `! history
// growth: %v` on a failed read, and docs/cli.md states that a check which cannot
// run must not print nothing at all.
//
// memory_history is dropped from a second handle, which is the only way to make
// this statement fail on a store that opened cleanly: a dropped table is an error
// the read cannot recover from and cannot mistake for an empty report.
func TestHealthSaysWhenTheHistoryReadFails(t *testing.T) {
	store, dbPath := testStoreWithPath(t)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := New(store, logger, "test")

	ctx := context.Background()
	if _, err := store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "the relay listens on port 2222 in production",
		Importance: 0.5, Source: "mcp",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	dropHistoryTable(t, dbPath)

	text := ghostHealthText(t, srv)

	if !strings.Contains(text, "**History:**") {
		t.Fatalf("ghost_health dropped the history section after a failed read, so the omission reads as an empty report:\n%s", text)
	}
	if !strings.Contains(text, "could not be read") {
		t.Errorf("ghost_health does not say the read failed:\n%s", text)
	}
	// And it must not have fallen back to reporting numbers it does not have: a
	// store whose table is gone has no share and no cap headroom, and printing
	// zeroes for them would be a claim rather than an absence.
	if strings.Contains(text, "restatements (0%)") || strings.Contains(text, "no version rows recorded yet") {
		t.Errorf("ghost_health reported an empty history after a failed read:\n%s", text)
	}
	// The rest of the report is still there: one unreadable table is not a reason
	// to hand the agent less than it had a moment ago.
	if !strings.Contains(text, "**Total memories:** 1") {
		t.Errorf("ghost_health lost the fields it already reported:\n%s", text)
	}
}

// dropHistoryTable removes memory_history through its own handle, from outside the
// store under test, so nothing about the store's own state changes and the only
// thing the read finds is a missing table.
func dropHistoryTable(t *testing.T, dbPath string) {
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
