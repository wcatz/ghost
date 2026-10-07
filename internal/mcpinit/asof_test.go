package mcpinit

import (
	"context"
	"io"
	"log/slog"
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
	// The project is recorded against the RESOLVED path because the renderers
	// resolve the directory they are given before matching it (Store.
	// ResolveProject compares resolved paths). On Windows t.TempDir() hands back
	// a short-name path that EvalSymlinks expands, so an unresolved fixture path
	// matches nothing there and the block comes back empty — on this path as much
	// as on the historical one, which is what makes it a fixture fault.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

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

// TestRenderSessionContextAtRendersGlobalsOnce: the project half excludes
// _global, exactly as the live loader's `project_id = ?` query does, because the
// Global section renders those rows. A project half that also carried them would
// print every global twice and spend the project's row budget on rows the block
// repeats — and a doubled row is the kind of thing a reader skims past without
// noticing it is wrong.
func TestRenderSessionContextAtRendersGlobalsOnce(t *testing.T) {
	dir, _ := historicalStore(t)
	saveHistoricalGlobal(t, dir, "cross-project rows are listed once")
	at := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	block := RenderSessionContextAt(dir, &at)
	if n := strings.Count(block, "cross-project rows are listed once"); n != 1 {
		t.Errorf("the global row is rendered %d times, want 1:\n%s", n, block)
	}
	if !strings.Contains(block, "Global (applies to all projects)") {
		t.Errorf("the block has no Global section, so the assertion above proved nothing about where the row rendered:\n%s", block)
	}
}

// TestRenderSessionContextAtNamesTheInstantWithNoProjectMatched: the no-project
// branch is reachable from a historical read — a global row recorded at T with an
// unmatched directory — and it used to emit a Global list from an instant under a
// heading that never named it, followed by an instruction aimed at the present.
func TestRenderSessionContextAtNamesTheInstantWithNoProjectMatched(t *testing.T) {
	dir, _ := historicalStore(t)
	saveHistoricalGlobal(t, dir, "a global row at that instant")
	at := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	block := RenderSessionContextAt(filepath.Join(t.TempDir(), "no-project-here"), &at)
	if block == "" {
		t.Fatal("the block is empty, so nothing below can be proved about it")
	}
	if !strings.Contains(block, "no project matched this directory") {
		t.Errorf("the block does not report the unmatched directory:\n%s", block)
	}
	if !strings.Contains(block, "as_of 2035-01-01T00:00:00Z") {
		t.Errorf("the block lists globals from an instant without naming it:\n%s", block)
	}
	if strings.Contains(block, "Save discoveries with ghost_memory_save") {
		t.Errorf("the historical block aims the reader at the present:\n%s", block)
	}
}

// TestRenderSessionContextAtWindowIsTheCurrentOneNotAVerdictAtT fixes the honesty
// property of the historical block's memory rows: memory_history never recorded
// valid_from/valid_until/verified_at, so a row's window comes from the CURRENT
// row. The block may SHOW the window, but it must not draw a verdict from it at T
// — a window opened, moved or closed after T would then read as a past fact it
// never was — and it must say the window is the current one.
func TestRenderSessionContextAtWindowIsTheCurrentOneNotAVerdictAtT(t *testing.T) {
	base := time.Date(2026, 10, 5, 22, 20, 24, 0, time.UTC)
	past := base
	future := base.Add(48 * time.Hour)

	stampLayout := memory.StoredStampLayout

	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghostDir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`,
		"histproj", dir, "histproj"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	// mem1: window closed now, open at T. mem2: window opens after T. mem3:
	// open-ended. The validity columns are read from the current memories table,
	// and the block must render them WITHOUT a verdict at T.
	// Memory valid at T=past but expired since (valid_until = past)
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, valid_from, valid_until, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"mem1", "histproj", "fact", "valid at T, expired since", "manual",
		past.Add(-48*time.Hour).Format(stampLayout), past.Format(stampLayout),
		past.Add(-72*time.Hour).Format(stampLayout), past.Add(-24*time.Hour).Format(stampLayout),
	); err != nil {
		t.Fatalf("insert mem1: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memory_history (memory_id, project_id, category, content, source, recorded_at, phase) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"mem1", "histproj", "fact", "valid at T, expired since", "manual",
		past.Add(-24*time.Hour).Format(stampLayout), "save",
	); err != nil {
		t.Fatalf("insert mem1 history: %v", err)
	}
	// Memory not yet valid at T=past (valid_from = future)
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, valid_from, valid_until, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"mem2", "histproj", "fact", "not yet valid at T", "manual",
		future.Format(stampLayout), future.Add(48*time.Hour).Format(stampLayout),
		past.Add(-72*time.Hour).Format(stampLayout), past.Add(-24*time.Hour).Format(stampLayout),
	); err != nil {
		t.Fatalf("insert mem2: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memory_history (memory_id, project_id, category, content, source, recorded_at, phase) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"mem2", "histproj", "fact", "not yet valid at T", "manual",
		past.Add(-24*time.Hour).Format(stampLayout), "save",
	); err != nil {
		t.Fatalf("insert mem2 history: %v", err)
	}
	// Open-ended memory (no validity window)
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"mem3", "histproj", "fact", "open ended", "manual",
		past.Add(-72*time.Hour).Format(stampLayout), past.Add(-24*time.Hour).Format(stampLayout),
	); err != nil {
		t.Fatalf("insert mem3: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memory_history (memory_id, project_id, category, content, source, recorded_at, phase) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"mem3", "histproj", "fact", "open ended", "manual",
		past.Add(-24*time.Hour).Format(stampLayout), "save",
	); err != nil {
		t.Fatalf("insert mem3 history: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", xdgHome)

	block := RenderSessionContextAt(dir, &past)
	if block == "" {
		t.Fatal("the historical block is empty")
	}

	// Check that the block discloses the instant
	if !strings.Contains(block, "as_of "+past.Format(time.RFC3339)) {
		t.Errorf("the block does not name the instant it read:\n%s", block)
	}

	// No verdict at T on any row, and the block says the window is the current
	// one. The contents deliberately spell "expired" and "not yet valid", so
	// each row's validity SECTION is what gets checked, not the block's text.
	if strings.Contains(block, "Validity judged at") {
		t.Errorf("the block still claims validity was judged at T:\n%s", block)
	}
	if !strings.Contains(block, "CURRENT one") || !strings.Contains(block, past.Format(time.RFC3339)) {
		t.Errorf("the block does not say the validity window is the current one:\n%s", block)
	}

	// mem1: window closed now, open at T -> shown as a window, no verdict.
	if got := sessionValiditySection(block, "valid at T, expired since"); !strings.Contains(got, "until") {
		t.Errorf("mem1's window was dropped rather than shown without a verdict: %q", got)
	} else if strings.Contains(got, "expired") {
		t.Errorf("mem1's window is the current row's, not T's, so it must not read expired: %q", got)
	}

	// mem2: window opens after T -> shown as a window, no verdict at T.
	if got := sessionValiditySection(block, "not yet valid at T"); !strings.Contains(got, "valid from") {
		t.Errorf("mem2's window was dropped rather than shown without a verdict: %q", got)
	} else if strings.Contains(got, "not yet valid") {
		t.Errorf("mem2's window opens after T but the block judged the current window at T: %q", got)
	}

	// mem3: open-ended -> no window and no verdict.
	if got := sessionValiditySection(block, "open ended"); strings.Contains(got, "valid from") || strings.Contains(got, "until") ||
		strings.Contains(got, "expired") || strings.Contains(got, "not yet valid") || strings.Contains(got, "unverified") {
		t.Errorf("mem3 is open-ended but carries a validity claim: %q", got)
	}
}

// sessionValiditySection returns the parenthesized metadata group of the block
// line whose content is content — the group assemble.Item.Line renders the
// importance, scope and validity window in, between the backtick-quoted id and
// the « data delimiter.
func sessionValiditySection(block, content string) string {
	for _, line := range strings.Split(block, "\n") {
		if !strings.Contains(line, content) {
			continue
		}
		if idx := strings.Index(line, "«"); idx >= 0 {
			beforeContent := line[:idx]
			open := strings.Index(beforeContent, "(")
			close := strings.LastIndex(beforeContent, ")")
			if open >= 0 && close > open {
				return strings.TrimSpace(beforeContent[open+1 : close])
			}
		}
		return ""
	}
	return ""
}

// TestRenderSessionContextAtWindowEditedAfterTIsNotJudgedAtT is the case the
// borrow is worst for: a memory that existed at T whose validity window was set
// AFTER T. memory_history records no window, so the block can only show today's —
// and it must not present that as a verdict about T.
func TestRenderSessionContextAtWindowEditedAfterTIsNotJudgedAtT(t *testing.T) {
	past := time.Date(2026, 10, 5, 22, 20, 24, 0, time.UTC)
	afterT := past.Add(24 * time.Hour)
	future := past.Add(72 * time.Hour)
	stampLayout := memory.StoredStampLayout

	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghostDir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`,
		"histproj", dir, "histproj"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	// The version is recorded before T (so the memory existed then), while the
	// row's updated_at is after T and its window opens after T: the window was
	// set after the instant the block reads at.
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, valid_from, valid_until, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"mem1", "histproj", "fact", "a window set after T", "manual",
		future.Format(stampLayout), future.Add(48*time.Hour).Format(stampLayout),
		past.Add(-72*time.Hour).Format(stampLayout), afterT.Format(stampLayout),
	); err != nil {
		t.Fatalf("insert mem1: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO memory_history (memory_id, project_id, category, content, source, recorded_at, phase) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"mem1", "histproj", "fact", "a window set after T", "manual",
		past.Add(-24*time.Hour).Format(stampLayout), "save",
	); err != nil {
		t.Fatalf("insert mem1 history: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", xdgHome)

	block := RenderSessionContextAt(dir, &past)
	if block == "" {
		t.Fatal("the historical block is empty")
	}
	// The row existed at T, so it is shown, with the current window...
	section := sessionValiditySection(block, "a window set after T")
	if !strings.Contains(section, "valid from") {
		t.Errorf("the row's current window is not shown: %q\n%s", section, block)
	}
	// ...but NOT with the verdict the current window would produce at T. The
	// window opens after T, so a T-judgement would say "not yet valid"; the
	// window was set after T and nothing may report it as a fact about T.
	if strings.Contains(section, "not yet valid") {
		t.Errorf("the window was set after T but the block judged it at T: %q", section)
	}
	if !strings.Contains(block, "CURRENT one") {
		t.Errorf("the block does not say the window is the current one:\n%s", block)
	}
}

// TestRenderSessionContextAtKeepsTheClockIndependentUnverifiedMarker pins the
// half of the state a historical read MAY draw: whether the window was ever
// verified is a fact about the current row, not about the instant the block
// names, so the marker survives while the clock-dependent expired /
// not-yet-valid verdicts do not.
func TestRenderSessionContextAtKeepsTheClockIndependentUnverifiedMarker(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	past := now.Add(-48 * time.Hour)
	stampLayout := memory.StoredStampLayout

	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghostDir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`,
		"histproj", dir, "histproj"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	// mem1: a window open now and never verified. mem2: one that has since
	// closed, also never verified.
	for _, m := range []struct{ id, content, from, until string }{
		{"mem1", "open window never verified", now.Add(-24 * time.Hour).Format(stampLayout), ""},
		{"mem2", "closed window never verified", now.Add(-72 * time.Hour).Format(stampLayout), now.Add(-24 * time.Hour).Format(stampLayout)},
	} {
		var until any
		if m.until != "" {
			until = m.until
		}
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, valid_from, valid_until, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.id, "histproj", "fact", m.content, "manual",
			m.from, until, past.Add(-24*time.Hour).Format(stampLayout), past.Add(-12*time.Hour).Format(stampLayout),
		); err != nil {
			t.Fatalf("insert %s: %v", m.id, err)
		}
		if _, err := db.Exec(
			`INSERT INTO memory_history (memory_id, project_id, category, content, source, recorded_at, phase) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			m.id, "histproj", "fact", m.content, "manual", past.Format(stampLayout), "save",
		); err != nil {
			t.Fatalf("insert %s history: %v", m.id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", xdgHome)

	block := RenderSessionContextAt(dir, &past)
	if block == "" {
		t.Fatal("the historical block is empty")
	}
	if got := sessionValiditySection(block, "open window never verified"); !strings.Contains(got, "unverified") {
		t.Errorf("the historical block dropped the clock-independent unverified marker: %q\n%s", got, block)
	}
	closed := sessionValiditySection(block, "closed window never verified")
	if !strings.Contains(closed, "unverified") {
		t.Errorf("the historical block dropped unverified on a never-verified window: %q", closed)
	}
	if strings.Contains(closed, "expired") {
		t.Errorf("the historical block judged the borrowed window expired: %q", closed)
	}
}

// saveHistoricalGlobal writes a _global row through the store — so it has a
// recorded version, and a historical read renders it instead of reporting it as a
// gap — and backdates that version to before the instant the tests read at. The
// append path stamps datetime('now'), which is before the test's instant anyway;
// the backdating exists so the row's recorded past is unambiguous.
func saveHistoricalGlobal(t *testing.T, dir, content string) {
	t.Helper()
	db, err := memory.OpenDB(dbPathOf(t))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	if err := store.EnsureProject(ctx, memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
		t.Fatalf("EnsureProject(_global): %v", err)
	}
	id, _, _, err := store.UpsertWithProvenance(ctx, memory.GlobalProjectID, "preference", content, "manual", 0.7, nil, memory.Provenance{})
	if err != nil {
		t.Fatalf("save the global memory: %v", err)
	}
	if _, err := db.Exec(`UPDATE memory_history SET recorded_at = ? WHERE memory_id = ?`,
		"2030-01-01 00:00:00", id); err != nil {
		t.Fatalf("backdate the global memory's version: %v", err)
	}
}

// dbPathOf re-derives the store path the fixture built, which is
// $XDG_DATA_HOME/ghost/ghost.db — the same layout config.DataDir() returns.
func dbPathOf(t *testing.T) string {
	t.Helper()
	return filepath.Join(os.Getenv("XDG_DATA_HOME"), "ghost", "ghost.db")
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
