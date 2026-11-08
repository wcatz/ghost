package mcpinit

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

func seedConsolidatableRowsFastDB(t *testing.T, db *sql.DB, projectID string, n int) {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(
			`INSERT INTO memories (project_id, category, content, source, importance, created_at, updated_at)
			 VALUES (?, 'fact', ?, 'mcp', 0.6, datetime('now', '-1 hour'), datetime('now', '-1 hour'))`,
			projectID, fmt.Sprintf("relay %05d listens on its own port", i)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("insert row %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestStatusReflectOverBound(t *testing.T) {
	statusEnv(t)
	dataHome := os.Getenv("XDG_DATA_HOME")
	ghostDir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	store := memory.NewStore(db, discardLogger())
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "proj-over", "/tmp/proj-over", "over-project"); err != nil {
		t.Fatalf("EnsureProject over: %v", err)
	}
	if err := store.EnsureProject(ctx, "proj-under", "/tmp/proj-under", "under-project"); err != nil {
		t.Fatalf("EnsureProject under: %v", err)
	}
	// Close store connection in a way that doesn't conflict? Store holds db; just seed directly
	// But easier to seed via db before passing to report functions - store was created from db
	seedConsolidatableRowsFastDB(t, db, "proj-over", reflection.MaxConsolidationInput+1)
	seedConsolidatableRowsFastDB(t, db, "proj-under", 500)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var out bytes.Buffer
	if _, err := Status(&out); err != nil {
		t.Fatalf("Status: %v", err)
	}
	outStr := out.String()

	if !strings.Contains(outStr, "above the") {
		t.Fatalf("expected reflect over-bound section, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, fmt.Sprintf("%d", reflection.MaxConsolidationInput)) {
		t.Fatalf("expected bound mentioned, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "ghost resolve") || !strings.Contains(outStr, "ghost prune") || !strings.Contains(outStr, "ghost reflect") {
		t.Fatalf("expected remediation commands, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, "proj-over") && !strings.Contains(outStr, "over-project") {
		t.Fatalf("expected over-bound project named, got:\n%s", outStr)
	}
	if !strings.Contains(outStr, fmt.Sprintf("%d", reflection.MaxConsolidationInput+1)) {
		t.Fatalf("expected over-bound count 2001, got:\n%s", outStr)
	}
	// Extract the over-bound block: from its header to the next blank line.
	lines := strings.Split(outStr, "\n")
	start := -1
	for i, l := range lines {
		if strings.Contains(l, "above the") && strings.Contains(l, "limit for a single consolidation") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("over-bound header not found in output:\n%s", outStr)
	}
	var block []string
	for i := start; i < len(lines); i++ {
		if i > start && strings.TrimSpace(lines[i]) == "" {
			break
		}
		block = append(block, lines[i])
	}
	blockText := strings.Join(block, "\n")
	if !strings.Contains(blockText, "over-project") && !strings.Contains(blockText, "proj-over") {
		t.Fatalf("over-bound project not named in block:\n%s", blockText)
	}
	if !strings.Contains(blockText, fmt.Sprintf("%d", reflection.MaxConsolidationInput+1)) {
		t.Fatalf("over-bound count not in block:\n%s", blockText)
	}
	if !strings.Contains(blockText, "consolidatable memories") {
		t.Fatalf("per-project bullet missing in block:\n%s", blockText)
	}
	if strings.Contains(blockText, "under-project") || strings.Contains(blockText, "proj-under") {
		t.Fatalf("under-bound project appears in over-bound block:\n%s", blockText)
	}
}
