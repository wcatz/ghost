package mcpinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// outcomeSession builds a store whose project bucket holds nLive live rows and
// nExpired rows whose validity window has closed, points XDG_DATA_HOME at it,
// and returns the project path for the block to read.
//
// The fixture is built the way validitySession's is, for the same reasons: the
// moving window is written relative to now in memory.StoredStampLayout (an RFC
// 3339 value would be UNREADABLE rather than unexpired, and an unreadable stamp
// KEEPS the row), and the expired rows are given the HIGHEST importance and the
// NEWEST created_at on purpose — a row that survives must be the one that earned
// its place, so a dropped row has to be a row the ranking wanted.
func outcomeSession(t *testing.T, nLive, nExpired int) (projectPath string) {
	t.Helper()
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghostDir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(ghostDir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}

	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global: %v", err)
	}
	projectPath = filepath.Join(t.TempDir(), "outcomeproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir projectPath: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	insertProject(t, db, "p1", canonical, "outcomeproj")

	now := time.Now().UTC()
	for i := 0; i < nLive; i++ {
		stamp := time.Date(2026, 1, 2+i, 3, 4, 5, 0, time.UTC).Format("2006-01-02 15:04:05")
		if _, err := db.Exec(`
			INSERT INTO memories (id, project_id, category, content, source, importance, created_at)
			VALUES (?, 'p1', 'fact', ?, 'manual', ?, ?)`,
			"live"+pad2(i), "live row "+pad2(i)+" content for the outcome fixture", 0.9-float64(i)*0.005, stamp,
		); err != nil {
			t.Fatalf("insert live %d: %v", i, err)
		}
	}
	for i := 0; i < nExpired; i++ {
		// The highest importance and the newest created_at, as the validity
		// fixture argues: a row the ranking wanted must be the one withheld, so
		// a header that leaves it out proves the withholding rather than the
		// ranking.
		id := "expired" + pad2(i)
		if _, err := db.Exec(`
			INSERT INTO memories (id, project_id, category, content, source, importance, created_at, valid_until)
			VALUES (?, 'p1', 'fact', ?, 'manual', ?, ?, ?)`,
			id, "expired row "+pad2(i)+" content for the outcome fixture", 0.99+float64(i)*0.001,
			time.Date(2026, 2, 1+i, 3, 4, 5, 0, time.UTC).Format("2006-01-02 15:04:05"),
			now.Add(-24*time.Hour).Format(memory.StoredStampLayout),
		); err != nil {
			t.Fatalf("insert expired %d: %v", i, err)
		}
		// Every stamp above has to be one the store can read, or the row is
		// KEPT as unset and the header below stops testing the withholding.
		if _, ok := memory.ParseStamp(now.Add(-24 * time.Hour).Format(memory.StoredStampLayout)); !ok {
			t.Fatalf("fixture stamp is not one memory.ParseStamp reads")
		}
	}
	_ = db.Close()
	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return projectPath
}

// TestSessionStartSaysRowsWereWithheldWhenEveryRowIsExpired is repro (a) of
// issue #897: a project whose every row's validity window has closed rendered
// no Memories section at all, and no sentence saying why. The rows WERE found
// and then withheld — the block must say that, in the assembler's own words,
// and point at the tool that still shows them with the window they carry.
func TestSessionStartSaysRowsWereWithheldWhenEveryRowIsExpired(t *testing.T) {
	got := renderSessionStart(t, outcomeSession(t, 0, 3))

	if !strings.Contains(got, "**Memories:**\n") {
		t.Errorf("a wholly-withheld project must still render a Memories section with the bare heading; got:\n%s", got)
	}
	if strings.Contains(got, "**Memories (") {
		t.Errorf("a wholly-withheld project must not render a count-line header — there is no shown row to count; got:\n%s", got)
	}
	for _, want := range []string{
		"No sufficiently trustworthy memory found",
		"withheld as out of date",
		"Call ghost_memories_list to see them, still marked with the window they carry",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the block does not say the rows were withheld (%q); got:\n%s", want, got)
		}
	}
	for _, absent := range []string{"expired row 00", "expired row 01", "expired row 02"} {
		if strings.Contains(got, absent) {
			t.Errorf("a withheld row rendered as if current (%q); got:\n%s", absent, got)
		}
	}
}

// TestSessionStartDoesNotCallWithheldRowsRankedOut is repro (b) of issue #897:
// two expired rows beside one live row were counted by the "N not shown, ranked
// by a composite score" line as if the ranking had cut them. Expiry is not the
// ranking's doing, and the header must say the rows were withheld rather than
// ranked out.
func TestSessionStartDoesNotCallWithheldRowsRankedOut(t *testing.T) {
	got := renderSessionStart(t, outcomeSession(t, 1, 2))

	want := "**Memories (1 shown of 3 total — 2 withheld rather than ranked out; " +
		"use ghost_memories_list or ghost_memory_search for the rest):**"
	if !strings.Contains(got, want) {
		t.Errorf("the header does not separate the withheld rows from the ranking:\n want %q\n got:\n%s", want, got)
	}
	for _, absent := range []string{
		"2 not shown, ranked",
		"ranked out by",
	} {
		if strings.Contains(got, absent) {
			t.Errorf("the header attributes the withheld rows to the ranking (%q); got:\n%s", absent, got)
		}
	}
	if strings.Contains(got, "expired row 00") || strings.Contains(got, "expired row 01") {
		t.Errorf("a withheld row rendered as if current; got:\n%s", got)
	}
}

// TestSessionStartMixedHeaderStatesBothFates is the combined line: a block that
// cut rows at the cap AND received rows from a stage that withheld them must
// name the two fates separately, in one header — the ranking's cut and the
// withholding are different authority, and collapsing them would re-introduce
// the repro (b) lie for a block that has both.
func TestSessionStartMixedHeaderStatesBothFates(t *testing.T) {
	got := renderSessionStart(t, outcomeSession(t, 17, 3))

	want := "**Memories (15 shown of 20 total — 5 not shown: 2 ranked out by a composite score of importance, " +
		"pinned status, and category-aware recency decay, 3 withheld rather than ranked out; " +
		"use ghost_memories_list or ghost_memory_search for the rest):**"
	if !strings.Contains(got, want) {
		t.Errorf("the mixed header is not what the issue asks for:\n want %q\n got:\n%s", want, got)
	}
	for _, absent := range []string{"expired row 00", "expired row 01", "expired row 02"} {
		if strings.Contains(got, absent) {
			t.Errorf("a withheld row rendered as if current (%q); got:\n%s", absent, got)
		}
	}
}

// TestSessionStartWithNoRowsKeepsTheAbsentSection: a project the store has
// never seen (or whose rows are all resolved) must keep today's output — no
// Memories section, and no sentence claiming rows were withheld when none were
// found. The withheld sentence is a claim about rows the block was assembled
// from, and a block assembled from nothing cannot say it.
func TestSessionStartWithNoRowsKeepsTheAbsentSection(t *testing.T) {
	got := renderSessionStart(t, outcomeSession(t, 0, 0))

	if !strings.Contains(got, "## Ghost context: outcomeproj") {
		t.Fatalf("the fixture project did not resolve; the block is not exercising the no-rows branch:\n%s", got)
	}
	if strings.Contains(got, "**Memories") {
		t.Errorf("a project with no rows must render no Memories section:\n%s", got)
	}
	if strings.Contains(got, "withheld") {
		t.Errorf("a project with no rows must not claim rows were withheld — nothing was assembled:\n%s", got)
	}
}

// TestSessionStartTotalIsTheStoreNotTheOverFetchWindow: the retrieval reads an
// over-fetched window (three times the cap), so a project holding more rows than
// the window used to read "15 shown of 45 total". The header's total is the
// rows the project holds that the retrieval could draw from, and the rows
// behind the window are ranked out.
func TestSessionStartTotalIsTheStoreNotTheOverFetchWindow(t *testing.T) {
	got := renderSessionStart(t, outcomeSession(t, 60, 0))

	want := "**Memories (15 shown of 60 total — 45 not shown, ranked by a composite score of importance, " +
		"pinned status, and category-aware recency decay; use ghost_memories_list or ghost_memory_search for the rest):**"
	if !strings.Contains(got, want) {
		t.Errorf("the header understates the store:\n want %q\n got:\n%s", want, got)
	}
	if strings.Contains(got, "of 45 total") {
		t.Errorf("the header reports the over-fetch window as the store:\n%s", got)
	}
}

// TestSessionStartTotalCountsWithheldRowsOnceBesideRowsBeyondTheWindow: the
// withheld rows sit inside the window and the count shares the window's
// predicates, so the two never count the same row twice.
func TestSessionStartTotalCountsWithheldRowsOnceBesideRowsBeyondTheWindow(t *testing.T) {
	got := renderSessionStart(t, outcomeSession(t, 60, 3))

	want := "**Memories (15 shown of 63 total — 48 not shown: 45 ranked out by a composite score of importance, " +
		"pinned status, and category-aware recency decay, 3 withheld rather than ranked out; " +
		"use ghost_memories_list or ghost_memory_search for the rest):**"
	if !strings.Contains(got, want) {
		t.Errorf("the mixed header double counts or understates:\n want %q\n got:\n%s", want, got)
	}
}
