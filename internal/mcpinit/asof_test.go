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
	if strings.Contains(block, sessionSaveInstruction) {
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
	if strings.Contains(block, sessionSaveInstruction) {
		t.Errorf("the historical block aims the reader at the present:\n%s", block)
	}
}

// validityRow is one memory a historical-validity fixture writes: its content,
// the three validity columns (empty means NULL) and nothing else, because the
// rest of the row is not what these tests are about.
type validityRow struct {
	id, content, from, until, verified string
}

// validityStore builds a store holding the given rows, each with one recorded
// version before t, and returns the project directory. The validity columns live
// on the memories table only (memory_history never versioned them).
func validityStore(t *testing.T, at time.Time, rows []validityRow) string {
	t.Helper()
	stampLayout := memory.StoredStampLayout
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghostDir: %v", err)
	}
	dir := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	db, err := memory.OpenDB(filepath.Join(ghostDir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, "histproj", dir, "histproj"); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, valid_from, valid_until, verified_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.id, "histproj", "fact", r.content, "manual", nullable(r.from), nullable(r.until), nullable(r.verified),
			at.Add(-72*time.Hour).Format(stampLayout), at.Add(-24*time.Hour).Format(stampLayout),
		); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
		if _, err := db.Exec(
			`INSERT INTO memory_history (memory_id, project_id, category, content, source, recorded_at, phase) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			r.id, "histproj", "fact", r.content, "manual", at.Add(-24*time.Hour).Format(stampLayout), "save",
		); err != nil {
			t.Fatalf("insert %s history: %v", r.id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", xdgHome)
	return dir
}

// TestRenderSessionContextAtJudgesValidityAtTheRequestedInstant fixes the rule
// the block shares with ghost_memory_search and ghost_project_context: validity
// is judged AT the requested instant (#899). A row valid at T and closed since
// is shown as valid at T; a row whose window had closed or not yet opened at T
// is withheld; a bound exactly at T is inside the window; an open-ended or
// unreadable-bound row is kept with no verdict.
func TestRenderSessionContextAtJudgesValidityAtTheRequestedInstant(t *testing.T) {
	at := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	f := memory.StoredStampLayout
	stamp := func(d time.Duration) string { return at.Add(d).Format(f) }
	verified := stamp(-time.Hour)

	dir := validityStore(t, at, []validityRow{
		{id: "m-closed-since", content: "valid at T closed since", from: stamp(-48 * time.Hour), until: stamp(24 * time.Hour), verified: verified},
		{id: "m-future", content: "opens after T", from: stamp(24 * time.Hour), until: stamp(72 * time.Hour), verified: verified},
		{id: "m-expired", content: "closed before T", from: stamp(-72 * time.Hour), until: stamp(-time.Hour), verified: verified},
		{id: "m-open", content: "open ended row"},
		{id: "m-until-at-t", content: "ends exactly at T", from: stamp(-48 * time.Hour), until: stamp(0), verified: verified},
		{id: "m-from-at-t", content: "starts exactly at T", from: stamp(0), until: stamp(48 * time.Hour), verified: verified},
		{id: "m-unreadable", content: "unreadable bound row", until: "not a date"},
		{id: "m-unverified", content: "unverified window row", from: stamp(-time.Hour)},
	})

	block := RenderSessionContextAt(dir, &at)
	if block == "" {
		t.Fatal("the historical block is empty")
	}

	// The block says validity was judged at T.
	if !strings.Contains(block, "Validity judged at "+at.Format(time.RFC3339)) {
		t.Errorf("the block does not say validity was judged at T:\n%s", block)
	}

	// Withheld at T: closed before T, and not yet open at T.
	for _, content := range []string{"closed before T", "opens after T"} {
		if strings.Contains(block, content) {
			t.Errorf("%q is outside its window at T but the block lists it:\n%s", content, block)
		}
	}

	// Shown, and never labelled with the clock's verdict on the current window.
	for _, content := range []string{"valid at T closed since", "ends exactly at T", "starts exactly at T"} {
		got := sessionValiditySection(block, content)
		if got == "" || !strings.Contains(block, content) {
			t.Errorf("%q is inside its window at T but is missing:\n%s", content, block)
			continue
		}
		if strings.Contains(got, "expired") || strings.Contains(got, "not yet valid") || strings.Contains(got, "unverified") {
			t.Errorf("%q is valid at T but is labelled %q", content, got)
		}
	}
	if got := sessionValiditySection(block, "valid at T closed since"); !strings.Contains(got, "until") {
		t.Errorf("the window of a row valid at T is not shown: %q", got)
	}

	// An open-ended row and one with an unreadable bound carry no claim.
	for _, content := range []string{"open ended row", "unreadable bound row"} {
		if !strings.Contains(block, content) {
			t.Errorf("%q states no readable window but is missing:\n%s", content, block)
			continue
		}
		got := sessionValiditySection(block, content)
		for _, bad := range []string{"valid from", "until", "expired", "not yet valid", "unverified"} {
			if strings.Contains(got, bad) {
				t.Errorf("%q carries a validity claim %q: %q", content, bad, got)
			}
		}
	}

	// A readable window nobody verified keeps its unverified marker.
	if got := sessionValiditySection(block, "unverified window row"); !strings.Contains(got, "unverified") {
		t.Errorf("an unverified window lost its marker: %q", got)
	}
}

// TestRenderSessionContextAtCapsAfterWithholdingAtT: a row withheld at T must not
// take a slot from a valid one, or a block would show fewer rows than the store
// holds for that instant.
func TestRenderSessionContextAtCapsAfterWithholdingAtT(t *testing.T) {
	at := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	f := memory.StoredStampLayout
	rows := []validityRow{{id: "m-valid", content: "the only valid row"}}
	for i := 0; i < sessionMemoriesCap+2; i++ {
		rows = append(rows, validityRow{
			id: "m-gone-" + strings.Repeat("x", i+1), content: "withheld at T " + strings.Repeat("y", i+1),
			until: at.Add(-time.Hour).Format(f),
		})
	}
	dir := validityStore(t, at, rows)
	block := RenderSessionContextAt(dir, &at)
	if !strings.Contains(block, "the only valid row") {
		t.Errorf("a withheld row displaced the valid one:\n%s", block)
	}
	if strings.Contains(block, "withheld at T") {
		t.Errorf("a row closed before T is listed:\n%s", block)
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
	if !strings.Contains(block, sessionSaveInstruction) {
		t.Errorf("a nil instant dropped the session instruction, so the current path's framing changed:\n%s", block)
	}
	if !strings.Contains(block, "**Session #1**") {
		t.Errorf("a nil instant did not count the session, so the current path lost a side effect:\n%s", block)
	}
}

// TestRenderSessionContextAtSaysWhenEveryRowWasWithheldAtT: every row out of
// window at T must read as withheld, with a count, rather than as a project that
// held nothing then; a project with no rows at T keeps its output.
func TestRenderSessionContextAtSaysWhenEveryRowWasWithheldAtT(t *testing.T) {
	at := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	f := memory.StoredStampLayout
	dir := validityStore(t, at, []validityRow{
		{id: "m-a", content: "closed one", until: at.Add(-time.Hour).Format(f)},
		{id: "m-b", content: "opens later", from: at.Add(time.Hour).Format(f)},
	})
	block := RenderSessionContextAt(dir, &at)
	if block == "" {
		t.Fatal("an all-withheld block is empty")
	}
	if strings.Contains(block, "closed one") || strings.Contains(block, "opens later") {
		t.Errorf("a row out of window at T is listed:\n%s", block)
	}
	if !strings.Contains(block, "Validity judged at "+at.Format(time.RFC3339)) || !strings.Contains(block, "2 memories were withheld") {
		t.Errorf("an all-withheld block does not say 2 rows were withheld at T:\n%s", block)
	}
	if strings.Count(block, "Validity judged at") != 1 {
		t.Errorf("the validity note is not stated once at block level:\n%s", block)
	}

	empty := validityStore(t, at, nil)
	got := RenderSessionContextAt(empty, &at)
	if strings.Contains(got, "Validity judged at") || strings.Contains(got, "withheld") {
		t.Errorf("a project with no rows at T gained a validity note:\n%s", got)
	}
}
