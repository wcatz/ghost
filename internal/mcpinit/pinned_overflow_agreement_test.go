package mcpinit

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// insertPinned inserts a pinned row with given id, importance, created_at and content.
func insertPinned(t *testing.T, db *sql.DB, projectID, id string, importance float32, content string, createdAt string) {
	t.Helper()
	if createdAt == "" {
		createdAt = "2026-01-01 00:00:00"
	}
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at) VALUES (?, ?, 'fact', ?, 'manual', ?, 1, ?, ?)`, id, projectID, content, importance, createdAt, createdAt); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

func TestPinnedOverflowByteCaseAgreesWithSessionBlock(t *testing.T) {
	// Pinned rows under count cap but with very long content; with per-row preview
	// truncation and KeepPinned in the fit pass, neither PinnedOverflow nor the
	// session-start block should cut pinned rows for bytes. If bytes were the
	// trigger, the agreement would change; with count-only and last-resort byte
	// behavior documented, this asserts the block still shows them and overflow
	// is nil.
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		projectID string
		cap       int
	}{
		{
			name:      "project",
			projectID: "proj",
			cap:       memory.SessionProjectCap,
		},
		{
			name:      "global",
			projectID: memory.GlobalProjectID,
			cap:       memory.SessionGlobalCap,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "ghost.db")
			db, err := memory.OpenDB(dbPath)
			if err != nil {
				t.Fatalf("OpenDB: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			projectPath := filepath.Join(dir, "projdir")
			if err := os.MkdirAll(projectPath, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			canon, err := filepath.EvalSymlinks(projectPath)
			if err != nil {
				t.Fatalf("EvalSymlinks: %v", err)
			}
			if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('proj', ?, 'proj'), ('_global', '_global', 'global')`, canon); err != nil {
				t.Fatalf("insert projects: %v", err)
			}
			store := memory.NewStore(db, nil)
			ctx := context.Background()
			n := tc.cap - 1 // under cap
			// Each row well over display bytes
			longContent := strings.Repeat("x", 20000)
			var ids []string
			for i := 0; i < n; i++ {
				id := fmt.Sprintf("b%03d", i)
				ids = append(ids, id)
				insertPinned(t, db, tc.projectID, id, 0.8-float32(i)*0.01, longContent, fmt.Sprintf("2026-01-01 00:00:%02d", i))
			}
			ov, err := store.PinnedOverflow(ctx, tc.projectID, now, nil)
			if err != nil {
				t.Fatalf("PinnedOverflow: %v", err)
			}
			if ov != nil {
				t.Fatalf("expected no overflow for %d long pinned under cap, got %+v", n, ov)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			cfg := mustHookConfig(t)
			block := SessionBlockAt(dbPath, projectPath, cfg, now)
			for _, id := range ids {
				if !strings.Contains(block, "`"+id+"`") {
					t.Errorf("id %s should appear (under cap, long but preview-truncated), missing from block:\n%s", id, block)
				}
			}
		})
	}
}
