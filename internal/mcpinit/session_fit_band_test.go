package mcpinit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// bandNameBytes is the length of the band fixture's project name. The name is
// the one framing field nothing bounds — it is printed twice, in the heading and
// in the project_id line — so it is what carries a block's framing to within a
// few hundred bytes of the host limit with rows still in it.
const bandNameBytes = 3184

// bandFramingMargin is how close to the host limit the band fixture's framing
// has to sit: the count line a cut adds is a couple of hundred bytes long, so a
// framing this close crosses the limit the moment a single row is cut, which is
// the whole of the band the fit pass has to decide about.
const bandFramingMargin = 250

// seedFitBandStore builds the fixture for the band edge: a store whose
// session-start block has a FRAMING — everything but the memory rows — within a
// few hundred bytes of the host's 10,000-character limit, with rows in it that
// are over the 9,000-byte cap.
//
// The rows are deliberately inside their bucket caps (15 project, 8 global) and
// spread across categories, so no stage withholds or defers one: the block the
// fixture measures is shaped only by the byte cap and the framing, which is the
// pair under test. A stage cut would add clauses of its own to the count line
// and make the framing of the no-row render impossible to state from the test.
func seedFitBandStore(t *testing.T) (dbPath, projectPath string) {
	t.Helper()
	ghostDir := filepath.Join(t.TempDir(), "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath = filepath.Join(ghostDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck

	projectPath = filepath.Join(t.TempDir(), "bandproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project path: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`)
	exec(`INSERT INTO projects (id, path, name) VALUES ('pband', ?, ?)`, canonical, strings.Repeat("n", bandNameBytes))
	// Past its 1,000-byte bound: the read decides how much the block spends.
	exec(`INSERT INTO ghost_state (project_id, learned_context) VALUES ('pband', ?)`,
		strings.Repeat("Learned summary sentence. ", 200))

	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	insert := func(id, project, category string, content string, imp float64) {
		exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'manual', ?, 0, ?, ?)`, id, project, category, content, imp, ts, ts)
	}
	cats := []string{"architecture", "decision", "pattern", "convention", "gotcha", "dependency", "preference", "fact"}
	for i := 0; i < 15; i++ {
		insert(fmt.Sprintf("pmem%02d", i), "pband", cats[i%len(cats)],
			strings.Repeat(fmt.Sprintf("Project row %02d content. ", i), 40), 0.9-float64(i)*0.01)
	}
	for i := 0; i < 8; i++ {
		insert(fmt.Sprintf("gmem%02d", i), "_global", cats[i%len(cats)],
			strings.Repeat(fmt.Sprintf("Global row %02d content. ", i), 40), 0.8-float64(i)*0.01)
	}
	// Titles and descriptions past their own bounds (120 and 200 bytes), so the
	// block spends the bound and not the seed.
	for i := 0; i < 4; i++ {
		exec(`INSERT INTO tasks (id, project_id, title, description, status, priority) VALUES (?, 'pband', ?, ?, 'pending', 1)`,
			fmt.Sprintf("task%02d", i), strings.Repeat("Task title words ", 20), strings.Repeat("Task description. ", 20))
	}
	for i := 0; i < 2; i++ {
		exec(`INSERT INTO decisions (id, project_id, title, decision, rationale, status) VALUES (?, 'pband', ?, ?, 'why', 'active')`,
			fmt.Sprintf("dec%02d", i), strings.Repeat("Decision title words ", 12), strings.Repeat("Decision body. ", 20))
	}
	return dbPath, projectPath
}

// bandBlock renders the fixture the way the hook does, and returns the block
// beside the framing alone — the same render with no memory row in it, which is
// what Budget.Measure reads when the fit pass asks whether the framing is the
// excess.
//
// It is not a second renderer: both calls go through the production
// formatSessionContext with the values loadSessionContextFrom just read, and the
// no-row call is faithful to what Measure sees because the fixture's rows are all
// inside their caps and none is withheld or deferred — so the heading block a
// tally would add for a bucket with withheld rows is absent from both.
func bandBlock(t *testing.T, dbPath, projectPath string) (block string, framing int) {
	t.Helper()
	cfg := mustHookConfig(t)
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	projectID, project, memories, globals, learned, tasks, decisions, interactionCount, tally :=
		loadSessionContextFrom(dbPath, projectPath, cfg, func() time.Time { return now }, "")
	block = formatSessionContext(projectID, project, nil, memories, learned, tasks, decisions, interactionCount, globals, tally)
	framing = len(formatSessionContext(projectID, project, nil, nil, learned, tasks, decisions, interactionCount, nil, sessionTally{}))
	return block, framing
}

// TestSessionStartFitPassDecidesTheCeilingOnce is the band edge of issue #987.
//
// The fit pass asks "can cutting rows help?" by measuring the framing against
// Budget.FramingCeiling, and the framing it measured grew with the cuts: every
// cut adds a clause to the count line, so a block whose framing sat just under
// the limit passed the check on the first iteration, lost rows, crossed the
// limit on the count line it had just earned, and stopped — over the byte cap,
// with rows recorded as cut for nothing and a count line claiming the cut had
// kept the block under a limit it was still over.
//
// The decision is now made ONCE, from a framing that does not depend on the
// cuts: the framing as it stands if every row were cut, which is the longest the
// count line can ever be. So a block in this band either fits under the cap or
// keeps every row, and its count line never claims a cut that did not keep it
// under the limit.
func TestSessionStartFitPassDecidesTheCeilingOnce(t *testing.T) {
	dbPath, projectPath := seedFitBandStore(t)
	block, framing := bandBlock(t, dbPath, projectPath)
	t.Logf("framing %d bytes, block %d bytes", framing, len(block))

	// The fixture, not the behaviour: a framing outside the band tests nothing.
	if framing < sessionHostLimit-bandFramingMargin || framing >= sessionHostLimit {
		t.Fatalf("fixture framing is %d bytes, want within %d of the %d host limit: the band is the case under test",
			framing, bandFramingMargin, sessionHostLimit)
	}
	if len(block) <= sessionStartByteCap {
		t.Fatalf("fixture block is %d bytes, not over the %d cap — the cap has to bind for the band to be reached",
			len(block), sessionStartByteCap)
	}

	// Either the block fits the cap, or nothing was cut for it. A block over the
	// cap whose count line claims a size cut is the bug: the cut was recorded as
	// having kept the block under a limit it is over either way.
	if strings.Contains(block, "cut to keep this block under the host's output limit") {
		t.Errorf("the block is %d bytes with rows cut for size, and the framing alone is %d — cutting cannot "+
			"reach the %d cap, so those rows were cut for nothing:\n%s", len(block), framing, sessionStartByteCap, block)
	}

	// The record half: a cut that did not keep the block under the limit is
	// written to the retrieval record as a row this session was not delivered,
	// which the audit then reads as a memory the agent never saw.
	cuts := recordedResponseFitCuts(t, dbPath)
	if cuts != 0 {
		t.Errorf("retrieval record holds %d response_fit cuts, want 0: the framing (%d) cannot reach the cap, "+
			"so no row was cut for size", cuts, framing)
	}
}

// recordedResponseFitCuts counts the rows the session-start retrieval recorded
// as cut by the response-fit pass. It reads the record the hook wrote into the
// fixture's own store, so it is the audit's own view of the same run.
func recordedResponseFitCuts(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := memory.OpenReadDB(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var verdicts string
	if err := db.QueryRow(`SELECT verdicts FROM retrieval_record WHERE source = 'session_start' ORDER BY rowid DESC LIMIT 1`).Scan(&verdicts); err != nil {
		t.Fatalf("read record: %v", err)
	}
	var rows []struct {
		ID    string `json:"id"`
		Kept  bool   `json:"kept"`
		Stage string `json:"stage"`
	}
	if err := json.Unmarshal([]byte(verdicts), &rows); err != nil {
		t.Fatalf("decode verdicts %q: %v", verdicts, err)
	}
	cuts := 0
	for _, r := range rows {
		if !r.Kept && r.Stage == "response_fit" {
			cuts++
		}
	}
	return cuts
}
