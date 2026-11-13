package mcpinit

import (
	"fmt"
	"os"
	"path/filepath"
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
