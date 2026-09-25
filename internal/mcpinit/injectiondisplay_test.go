package mcpinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestSessionContextDisplayCapIndependentOfStoreCap proves the session-start
// injection display truncation is a separate, hard-coded per-item byte budget
// (200 bytes for project memories in loadSessionContext) that does not read,
// and is not affected by, the MCP store cap. Raising the store cap to
// memory.MaxContentLen must not bloat injected context: a full-cap memory is
// stored whole yet still injects as a ~200-byte preview ending in "…".
func TestSessionContextDisplayCapIndependentOfStoreCap(t *testing.T) {
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	projectPath := filepath.Join(t.TempDir(), "capproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project path: %v", err)
	}

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	insertProject(t, db, "cap00001", projectPath, "capproj")

	// A record far over the injection display budget, well under the store
	// cap: 5000 chars stored, ~200 bytes injected.
	content := "DELEGHEAD " + strings.Repeat("d", 4990)
	if _, err := db.Exec(
		`INSERT INTO memories (id, project_id, category, content, source, importance) VALUES ('capmem001', 'cap00001', 'gotcha', ?, 'manual', 0.9)`,
		content,
	); err != nil {
		t.Fatalf("insert memory: %v", err)
	}
	_ = db.Close()

	t.Setenv("XDG_DATA_HOME", xdgHome)

	_, _, mems, _, _, _, _, _, _ := loadSessionContext(projectPath)
	if len(mems) != 1 {
		t.Fatalf("expected 1 injected memory, got %d", len(mems))
	}
	injected := mems[0].Content
	if !strings.HasPrefix(injected, "DELEGHEAD ") {
		t.Errorf("injected preview must keep the memory head; got %q", injected[:min(len(injected), 40)])
	}
	if !strings.HasSuffix(injected, "…") {
		t.Errorf("injected preview must end with the display ellipsis; got tail %q", injected[max(0, len(injected)-20):])
	}
	// 200-byte display budget + the appended "…" (3 bytes UTF-8).
	if len(injected) > 203 {
		t.Errorf("injected preview is %d bytes — the session-context display budget (~200 bytes) must not grow with the store cap", len(injected))
	}

	// The stored row must still hold the full content: display truncation
	// only, no store-side loss.
	stored, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer stored.Close() //nolint:errcheck
	var got string
	if err := stored.QueryRow(`SELECT content FROM memories WHERE id = 'capmem001'`).Scan(&got); err != nil {
		t.Fatalf("read stored content: %v", err)
	}
	if got != content {
		t.Errorf("stored content len=%d, want the full %d chars — injection preview must not rewrite the store", len(got), len(content))
	}
}
