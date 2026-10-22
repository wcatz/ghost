package mcpinit

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestSessionStartSameSecondRowsKeepNewestFirst: rows of a burst share a
// created_at second, and the session-start windows (10 open tasks, 5 live
// decisions) must drop the oldest of the burst, not the newest.
func TestSessionStartSameSecondRowsKeepNewestFirst(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	projectPath := filepath.Join(dir, "burst")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	insertProject(t, db, "pburst", canonical, "burst")
	const stamp = "2026-01-02 03:04:05"
	for i := 0; i < 12; i++ {
		id := string(rune('a' + i))
		if _, err := db.Exec(`INSERT INTO tasks (id, project_id, title, created_at) VALUES (?, 'pburst', ?, ?)`, id, "task-"+id, stamp); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO decisions (id, project_id, title, decision, rationale, created_at) VALUES (?, 'pburst', ?, 'd', 'why', ?)`, id, "dec-"+id, stamp); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	_, _, _, _, _, tasks, decisions, _, _ :=
		loadSessionContextFrom(dbPath, canonical, mustHookConfig(t), func() time.Time { return now }, "")
	if len(tasks) != 10 || tasks[0][0] != "l" || tasks[9][0] != "c" {
		t.Errorf("tasks window = %v; want newest l first, oldest kept c last", tasks)
	}
	if len(decisions) != 5 || decisions[0][0] != "l" || decisions[4][0] != "h" {
		t.Errorf("decisions window = %v; want l first, h last", decisions)
	}
}
