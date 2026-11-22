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

func TestHandoffReserve(t *testing.T) {
	if got := handoffReserve(""); got != 0 {
		t.Errorf("handoffReserve(\"\") = %d, want 0", got)
	}
	if got := handoffReserve("abc"); got != 4 {
		t.Errorf("handoffReserve(\"abc\") = %d, want 4", got)
	}
}

// reserveBlock renders a session start with no handoff: reserveRows memory rows,
// a project name padded by namePad bytes (printed twice, so 2 bytes each) and a
// learned summary n bytes long (1 byte each, bounded at 1000), and returns the
// block.
const reserveRows = 10

func reserveBlock(t *testing.T, namePad, n int) string {
	t.Helper()
	ghostDir := filepath.Join(t.TempDir(), "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(ghostDir, "ghost.db")
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	projectPath := filepath.Join(t.TempDir(), "reserveproj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	ts := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).Format("2006-01-02 15:04:05")
	for _, q := range []struct {
		q    string
		args []any
	}{
		{`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`, nil},
		{`INSERT INTO projects (id, path, name) VALUES ('preserve', ?, ?)`, []any{projectPath, "reserveproj" + strings.Repeat("p", namePad)}},
		{`INSERT INTO ghost_state (project_id, learned_context) VALUES ('preserve', ?)`, []any{strings.Repeat("l", n)}},
	} {
		if _, err := db.Exec(q.q, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < reserveRows; i++ {
		id := fmt.Sprintf("row%02d", i)
		if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at)
			VALUES (?, 'preserve', 'fact', ?, 'manual', ?, 0, ?, ?)`, id, strings.Repeat("x", 700), 0.9-float64(i)*0.01, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	cfg := mustHookConfig(t)
	now := time.Date(2026, 1, 2, 12, 0, 0, 0, time.UTC)
	projectID, project, memories, globals, learned, tasks, decisions, count, tally :=
		loadSessionContextFrom(dbPath, projectPath, cfg, func() time.Time { return now }, "", nil, "")
	return formatSessionContext(projectID, project, nil, memories, learned, tasks, decisions, count, globals, tally)
}

// TestSessionStartWithoutHandoffUsesTheFullCap pins that a session start with
// no handoff is fitted to exactly sessionStartByteCap: a block that fits in
// 9000 bytes but not 8999 is kept whole.
func TestSessionStartWithoutHandoffUsesTheFullCap(t *testing.T) {
	const probe = 100
	gap := sessionStartByteCap - len(reserveBlock(t, 0, probe))
	namePad := gap/2 - 250
	n := probe + gap - 2*namePad
	if namePad < 0 || n < 1 || n > 1000 {
		t.Fatalf("fixture cannot reach the cap: gap %d, name pad %d, summary %d", gap, namePad, n)
	}
	block := reserveBlock(t, namePad, n)
	if len(block) != sessionStartByteCap {
		t.Fatalf("fixture block is %d bytes, want exactly %d", len(block), sessionStartByteCap)
	}
	if strings.Contains(block, "cut to keep this block under the host's output limit") || strings.Count(block, "- [fact]") != reserveRows {
		t.Errorf("a block of exactly %d bytes lost rows: the cap is short of %d\n%s", sessionStartByteCap, sessionStartByteCap, block)
	}
}
