package mcpinit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
)

// TestSessionCountsLinePinnedCutWithZeroShown is the regression for #1957's
// contradictory note: when every global row was cut (zero shown) and some of
// those cut rows were pinned, the counts line must not claim "the ones shown
// are the best-ranked of them".
func TestSessionCountsLinePinnedCutWithZeroShown(t *testing.T) {
	tally := assemble.BucketTally{
		Shown:     0,
		RankedOut: 468,
		Withheld:  2,
		ByteCut:   8,
		PinnedCut: 1,
	}
	line := sessionCountsLine(tally, globalsRankPhrase, globalsToolPhrase)
	if line == "" {
		t.Fatal("want a counts line for a bucket with rows not shown")
	}
	if strings.Contains(line, "the ones shown are the best-ranked of them") {
		t.Fatalf("counts line must not claim shown rows are best-ranked when none are shown: %q", line)
	}
	if !strings.Contains(line, "0 shown") {
		t.Fatalf("counts line must still report 0 shown: %q", line)
	}
	if !strings.Contains(line, "1 pinned memories cut") {
		t.Fatalf("counts line must still report pinned cut count: %q", line)
	}
}

// TestSessionStartKeepsGlobalBucketWhenFitCutsRows is the regression for #1957:
// the response-fit pass must not empty the global bucket to zero, including
// pinned rows, when project content consumes the byte budget.
func TestSessionStartKeepsGlobalBucketWhenFitCutsRows(t *testing.T) {
	dbPath, projectPath := seedSessionPressure(t, 2, true)
	cfg := mustHookConfig(t)
	block := SessionBlockAt(dbPath, projectPath, cfg, time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC))
	if !strings.Contains(block, "**Global (applies to all projects):**") {
		t.Fatalf("global section missing entirely:\n%s", block)
	}
	if !strings.Contains(block, "Global row") {
		t.Fatalf("global bucket was emptied to zero rows:\n%s", block)
	}
	if strings.Contains(block, "the ones shown are the best-ranked of them") {
		t.Fatalf("contradictory pinned note present:\n%s", block)
	}
}

func seedSessionPressure(t *testing.T, pinnedGlobals int, heavyFraming bool) (string, string) {
	t.Helper()
	ghostDir := filepath.Join(t.TempDir(), "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`)
	if heavyFraming {
		longName := strings.Repeat("projectname ", 60)
		exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, ?)`, projectPath, longName)
		exec(`INSERT INTO ghost_state (project_id, learned_context) VALUES ('p1', ?)`, strings.Repeat("Learned context sentence with detail for pressure. ", 140))
	} else {
		exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'proj')`, projectPath)
		exec(`INSERT INTO ghost_state (project_id, learned_context) VALUES ('p1', ?)`, strings.Repeat("Learned context sentence with detail for pressure. ", 140))
	}
	for i := 0; i < 15; i++ {
		exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
			VALUES (?, 'p1', 'architecture', ?, 'manual', ?, 1, ?, ?)`,
			fmt.Sprintf("pmem%02d", i), strings.Repeat(fmt.Sprintf("Project row %02d architecture detail for session start pressure and fit behavior. ", i), 50), 0.95-float64(i)*0.01, ts, ts)
	}
	{
		for i := 0; i < 8; i++ {
			exec(`INSERT INTO tasks (id, project_id, title, description, status, priority) VALUES (?, 'p1', ?, ?, 'pending', 1)`,
				fmt.Sprintf("task%02d", i), strings.Repeat("Task title words for session-start pressure ", 16), strings.Repeat("Task description text for session-start framing and budget. ", 24))
		}
		for i := 0; i < 6; i++ {
			exec(`INSERT INTO decisions (id, project_id, title, decision, rationale, status) VALUES (?, 'p1', ?, ?, 'why', 'active')`,
				fmt.Sprintf("dec%02d", i), strings.Repeat("Decision title words for session-start pressure ", 14), strings.Repeat("Decision body text for session-start framing and budget. ", 24))
		}
	}
	for i := 0; i < 8; i++ {
		pinned := 0
		if i < pinnedGlobals {
			pinned = 1
		}
		exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
			VALUES (?, '_global', 'preference', ?, 'manual', ?, ?, ?, ?)`,
			fmt.Sprintf("gmem%02d", i), strings.Repeat(fmt.Sprintf("Global row %02d preference detail. ", i), 25), 0.8-float64(i)*0.01, pinned, ts, ts)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dbPath, projectPath
}
