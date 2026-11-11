package mcpinit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// TestSessionStartFramingIsLean: open tasks and recent decisions are framing the
// byte cap never cuts, so they are kept small enough to leave room for memory
// rows — five task titles without descriptions, and three decisions.
func TestSessionStartFramingIsLean(t *testing.T) {
	ghostDir := filepath.Join(t.TempDir(), "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(`INSERT INTO projects (id, path, name) VALUES ('p1', ?, 'proj')`, projectPath)
	for i := 0; i < 8; i++ {
		exec(`INSERT INTO tasks (id, project_id, title, description, status, priority) VALUES (?, 'p1', ?, ?, 'pending', 1)`,
			fmt.Sprintf("task%02d", i), fmt.Sprintf("Task title %02d", i), fmt.Sprintf("taskdesc-%02d", i))
	}
	for i := 0; i < 6; i++ {
		exec(`INSERT INTO decisions (id, project_id, title, decision, rationale, status) VALUES (?, 'p1', ?, 'body', 'why', 'active')`,
			fmt.Sprintf("dec%02d", i), fmt.Sprintf("Decision title %02d", i))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	block := SessionBlockAt(dbPath, projectPath, mustHookConfig(t), time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC))
	if got := strings.Count(block, "Task title "); got != 5 {
		t.Errorf("block lists %d open tasks, want 5:\n%s", got, block)
	}
	if strings.Contains(block, "taskdesc-") {
		t.Errorf("block carries task descriptions; it should list titles only:\n%s", block)
	}
	if got := strings.Count(block, "Decision title "); got != 3 {
		t.Errorf("block lists %d decisions, want 3:\n%s", got, block)
	}
}
