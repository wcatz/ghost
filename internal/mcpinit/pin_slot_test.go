package mcpinit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

type pinSessionRow struct {
	id, content, category, validUntil string
	importance                        float64
	pinned                            bool
}

// pinSession builds a project holding `high` unpinned rows at importance 0.9
// plus the given rows, and returns its directory. The extra rows are the ones a
// test is about; the unpinned bulk is what would crowd them out of a ranking.
func pinSession(t *testing.T, high int, extra ...pinSessionRow) string {
	t.Helper()
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(ghostDir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global: %v", err)
	}
	projectPath := filepath.Join(t.TempDir(), "pinproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir projectPath: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	insertProject(t, db, "p1", canonical, "pinproj")
	rows := make([]pinSessionRow, 0, high+len(extra))
	for i := 0; i < high; i++ {
		rows = append(rows, pinSessionRow{
			id: fmt.Sprintf("hi-%05d", i), content: fmt.Sprintf("bulk row number %03d", i),
			category: "architecture", importance: 0.9,
		})
	}
	rows = append(rows, extra...)
	for _, r := range rows {
		if _, err := db.Exec(`
			INSERT INTO memories (id, project_id, category, content, source, importance, pinned, valid_until, created_at, updated_at)
			VALUES (?, 'p1', ?, ?, 'manual', ?, ?, ?, '2026-01-02 03:04:05', '2026-01-02 03:04:05')`,
			r.id, r.category, r.content, r.importance, r.pinned, nullIfEmpty(r.validUntil),
		); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	_ = db.Close()
	t.Setenv("XDG_DATA_HOME", xdgHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	return projectPath
}

// TestAPinnedRowIsOnTheSessionStartWhateverItsRank: a pinned row at importance
// 0.1 among 30 and among 60 rows at 0.9. The pinned row is `fact`, which the
// behavioural floor does not name, so the floor is not what shows it. 60 rows is
// more than the project bucket's 45-row window, which is where only the fetch
// order can bring it in.
func TestAPinnedRowIsOnTheSessionStartWhateverItsRank(t *testing.T) {
	for _, bulk := range []int{30, 60} {
		t.Run(fmt.Sprintf("%d bulk rows", bulk), func(t *testing.T) {
			got := renderSessionStart(t, pinSession(t, bulk,
				pinSessionRow{id: "the-pin01", content: "the pinned standing rule", category: "fact", importance: 0.1, pinned: true}))
			if !strings.Contains(got, "the pinned standing rule") {
				t.Errorf("the pinned row is not on the session start. Got:\n%s", got)
			}
		})
	}
}

// TestAnExpiredPinnedRowIsWithheldFromTheSessionStart: the pin guarantees a slot,
// not an exemption from the validity window.
func TestAnExpiredPinnedRowIsWithheldFromTheSessionStart(t *testing.T) {
	got := renderSessionStart(t, pinSession(t, 30,
		pinSessionRow{id: "exp-pin01", content: "an expired pinned rule", category: "fact", importance: 0.1, pinned: true,
			validUntil: "2026-01-15 00:00:00"}))
	if strings.Contains(got, "an expired pinned rule") {
		t.Errorf("an expired pinned row is on the session start. Got:\n%s", got)
	}
}

// TestTheSessionStartSaysHowManyPinnedRowsItCut: 20 pinned rows against the cap
// of 15. The cap is hard, so five are cut, and the header says so in its own
// words instead of leaving them to read as ranked out by a composite score.
func TestTheSessionStartSaysHowManyPinnedRowsItCut(t *testing.T) {
	var pins []pinSessionRow
	for i := 0; i < 20; i++ {
		pins = append(pins, pinSessionRow{
			id: fmt.Sprintf("pin-%05d", i), content: fmt.Sprintf("pinned rule %02d", i),
			category: "fact", importance: 0.2 + float64(i)*0.01, pinned: true,
		})
	}
	got := renderSessionStart(t, pinSession(t, 10, pins...))
	if !strings.Contains(got, "5 pinned memories cut") {
		t.Errorf("the header does not say 5 pinned rows were cut. Got:\n%s", got)
	}
	// The cut rows are the lowest-ranked pinned ones, and no unpinned bulk row
	// took a slot a pinned row was entitled to.
	if strings.Contains(got, "bulk row number") {
		t.Errorf("an unpinned row took a slot while pinned rows were cut. Got:\n%s", got)
	}
	for _, kept := range []string{"pinned rule 19", "pinned rule 05"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q should be among the 15 best pinned rows. Got:\n%s", kept, got)
		}
	}
	if strings.Contains(got, "pinned rule 04") {
		t.Errorf("pinned rule 04 ranks 16th of 20 and should have been cut. Got:\n%s", got)
	}
}

// TestASupersededPinnedRowIsMarkedAndRanksAfterItsReplacementOnTheSessionStart:
// a pin keeps its slot, directly behind the row that replaced it, and the line
// names the replacement.
func TestASupersededPinnedRowIsMarkedAndRanksAfterItsReplacementOnTheSessionStart(t *testing.T) {
	path := pinSession(t, 5,
		pinSessionRow{id: "old-pin01", content: "the old pinned rule", category: "fact", importance: 0.1, pinned: true},
		pinSessionRow{id: "new-row01", content: "the replacement rule", category: "fact", importance: 0.05})
	db, err := memory.OpenDB(filepath.Join(os.Getenv("XDG_DATA_HOME"), "ghost", "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memory_links (source_id, target_id, relation, strength, source) VALUES ('new-row01', 'old-pin01', 'supersedes', 1, 'manual')`); err != nil {
		t.Fatalf("link: %v", err)
	}
	_ = db.Close()
	got := renderSessionStart(t, path)
	newAt, oldAt := strings.Index(got, "the replacement rule"), strings.Index(got, "the old pinned rule")
	if newAt < 0 || oldAt < 0 {
		t.Fatalf("both rows must be on the block. Got:\n%s", got)
	}
	if oldAt < newAt {
		t.Errorf("the superseded pin outranks its replacement. Got:\n%s", got)
	}
	if !strings.Contains(got, "superseded_by=`new-row01`") {
		t.Errorf("the superseded pin is not marked. Got:\n%s", got)
	}
}
