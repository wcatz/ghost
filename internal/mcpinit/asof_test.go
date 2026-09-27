package mcpinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// historicalStore builds a store in an isolated data home with one project
// matching dir and one memory in it, and returns the two.
//
// The memory is inserted with SQL rather than through a writer on purpose: the
// history appends live in the Go write paths, not in a trigger, so a row inserted
// this way has no recorded version of itself — which is exactly the state every
// memory written before schema v17 is in, and the one case a historical read has
// to disclose rather than answer.
func historicalStore(t *testing.T) (dir, dbPath string) {
	t.Helper()
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghostDir: %v", err)
	}
	dbPath = filepath.Join(ghostDir, "ghost.db")
	dir = t.TempDir()

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`,
		"histproj", dir, "histproj"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source) VALUES (?, ?, ?, ?, ?)`,
		"histmem1", "histproj", "fact", "the nightly sweep compacts the WAL file", "manual",
	); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", xdgHome)
	return dir, dbPath
}

// TestRenderSessionContextAtDisclosesTheInstantAndTheGap: the CLI half. The block
// has to name the instant it read, and it has to say that a memory with no
// recorded version is missing rather than printing today's text in its place —
// because a reader who does not know the gap will read the shorter block as the
// whole truth.
func TestRenderSessionContextAtDisclosesTheInstantAndTheGap(t *testing.T) {
	dir, _ := historicalStore(t)
	at := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)

	block := RenderSessionContextAt(dir, &at)
	if block == "" {
		t.Fatal("the historical block is empty, so nothing below can be proved about it")
	}
	if !strings.Contains(block, "as_of 2035-01-01T00:00:00Z") {
		t.Errorf("the block does not name the instant it read:\n%s", block)
	}
	if !strings.Contains(block, "unknown before its first recorded version") {
		t.Errorf("the block does not report the memory it cannot place:\n%s", block)
	}
	if strings.Contains(block, "the nightly sweep compacts the WAL file") {
		t.Errorf("the block printed today's text for a memory with no recorded version:\n%s", block)
	}
	if !strings.Contains(block, "are not versioned") {
		t.Errorf("the block does not say the unversioned halves are omitted:\n%s", block)
	}
	// The session instruction aims the reader at the present, which is not what
	// this block is.
	if strings.Contains(block, "Save new discoveries with ghost_memory_save") {
		t.Errorf("the historical block ends with the session instruction:\n%s", block)
	}

	// The current render of the same directory is unchanged, which is what makes
	// the difference above a fact about the instant rather than a broken loader.
	current := RenderSessionContext(dir)
	if !strings.Contains(current, "the nightly sweep compacts the WAL file") {
		t.Errorf("the current block does not show the memory:\n%s", current)
	}
	if strings.Contains(current, "as_of ") {
		t.Errorf("the current block carries a historical note:\n%s", current)
	}
}

// TestRenderSessionContextAtCountsNoSession: a past reading is a diagnostic, and
// the startup side effects the current path runs exist because it backs a session
// start. Counting a replay as a session would move the present's session number
// to answer a question about the past — and the stored count is the only place
// that is observable from outside.
func TestRenderSessionContextAtCountsNoSession(t *testing.T) {
	dir, dbPath := historicalStore(t)
	at := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)

	if block := RenderSessionContextAt(dir, &at); block == "" {
		t.Fatal("the historical block is empty")
	}
	stored, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var count int
	if err := db.QueryRow(`SELECT interaction_count FROM ghost_state WHERE project_id = 'histproj'`).Scan(&count); err != nil {
		// No ghost_state row at all is the stronger form of the same fact: nothing
		// was written by the historical render.
		t.Logf("no ghost_state row for the project after the historical render: %v", err)
		return
	}
	if count != 0 {
		t.Errorf("interaction_count = %d after a historical render, want 0: a past reading is not a session start", count)
	}
	_ = stored
}

// TestRenderSessionContextAtWithNoInstantIsTheCurrentPath keeps the seam honest:
// a nil instant is the current read, with every one of its side effects, so a
// caller cannot get a half-historical block by passing nothing. The two renders
// are not compared byte for byte because the current path counts a session, and
// calling it twice moves the number it prints — the side effect is the thing
// being asserted here.
func TestRenderSessionContextAtWithNoInstantIsTheCurrentPath(t *testing.T) {
	dir, _ := historicalStore(t)
	block := RenderSessionContextAt(dir, nil)
	if !strings.Contains(block, "the nightly sweep compacts the WAL file") {
		t.Errorf("a nil instant did not render the current block:\n%s", block)
	}
	if strings.Contains(block, "as_of ") {
		t.Errorf("a nil instant rendered a historical note:\n%s", block)
	}
	if !strings.Contains(block, "Save new discoveries with ghost_memory_save") {
		t.Errorf("a nil instant dropped the session instruction, so the current path's framing changed:\n%s", block)
	}
	if !strings.Contains(block, "**Session #1**") {
		t.Errorf("a nil instant did not count the session, so the current path lost a side effect:\n%s", block)
	}
}
