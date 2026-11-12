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

// budgetRow is one memory the budget fixture holds: the text it stores and the
// reference its rendered line carries beside it.
type budgetRow struct {
	content   string
	sourceRef string
}

// budgetStore builds a store with one project matching dir, the given project
// rows and the given global rows.
//
// Every row is written with its own recorded version, so a historical read can
// place it at an instant — which is what a memory saved through the store
// leaves behind. The version is dated before the instant the tests read at.
//
// The project's NAME is the caller's, because the name is what the block's
// heading and its project_id line render, and a long one is how a framing gets
// over the host's own limit.
func budgetStore(t *testing.T, name string, rows, globals []budgetRow) string {
	t.Helper()
	xdgHome := t.TempDir()
	ghostDir := filepath.Join(xdgHome, "ghost")
	if err := os.MkdirAll(ghostDir, 0o700); err != nil {
		t.Fatalf("mkdir ghostDir: %v", err)
	}
	dir := t.TempDir()
	// The project is recorded against the RESOLVED path, because the renderers
	// resolve the directory they are given before matching it.
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	db, err := memory.OpenDB(filepath.Join(ghostDir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`,
		"budgetproj", dir, name); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`,
		memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
		t.Fatalf("insert _global: %v", err)
	}
	stamp := memory.StoredStampLayout
	created := time.Now().UTC().Add(-2 * time.Hour).Format(stamp)
	recorded := time.Now().UTC().Add(-time.Hour).Format(stamp)
	write := func(project string, rows []budgetRow) {
		for i, r := range rows {
			id := fmt.Sprintf("%s-%02d", project, i)
			var ref any
			if r.sourceRef != "" {
				ref = r.sourceRef
			}
			if _, err := db.Exec(
				`INSERT INTO memories (id, project_id, category, content, source, importance, source_ref, created_at, updated_at)
				 VALUES (?, ?, 'fact', ?, 'manual', 0.7, ?, ?, ?)`,
				id, project, r.content, ref, created, created); err != nil {
				t.Fatalf("insert memory %s: %v", id, err)
			}
			if _, err := db.Exec(
				`INSERT INTO memory_history (memory_id, project_id, category, content, source, importance, recorded_at, phase)
				 VALUES (?, ?, 'fact', ?, 'manual', 0.7, ?, 'save')`,
				id, project, r.content, recorded); err != nil {
				t.Fatalf("insert history %s: %v", id, err)
			}
		}
	}
	write("budgetproj", rows)
	write(memory.GlobalProjectID, globals)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", xdgHome)
	return dir
}

// budgetContent is a memory's stored text near the store's own content cap:
// long enough that a block carrying it whole would blow the byte cap, and
// plain enough that nothing in it is escaped on the way out. Each row's text
// differs in its first line, so no two rows are the same memory.
func budgetContent(i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "row %02d: the nightly sweep compacts the wal file and records the row count it kept. ", i)
	for b.Len() < 7800 {
		b.WriteString("the nightly sweep compacts the wal file and records the row count it kept. ")
	}
	return b.String()[:7800]
}

// TestRenderSessionContextAtFitsTheHistoricalBlockToTheCap: the byte cap is a
// cap and not a hope — a block that would be over it gives up its
// lowest-ranked rows and says how many it gave up. The rows here carry a long
// reference beside their text, so the block is over the cap even with the
// preview applied, and the cut is what brings it under.
func TestRenderSessionContextAtFitsTheHistoricalBlockToTheCap(t *testing.T) {
	ref := strings.Repeat("segment/", 60)
	rows := make([]budgetRow, 0, 20)
	for i := 0; i < 20; i++ {
		rows = append(rows, budgetRow{content: budgetContent(i), sourceRef: ref})
	}
	dir := budgetStore(t, "budgetproj", rows, nil)
	at := time.Now().UTC().Add(time.Hour)

	block := RenderSessionContextAt(dir, &at)
	if block == "" {
		t.Fatal("the historical block is empty")
	}
	if len(block) > sessionStartByteCap {
		t.Errorf("the historical block is %d bytes, over the %d-byte cap", len(block), sessionStartByteCap)
	}
	if !strings.Contains(block, "were cut to keep this block under the host's output limit") {
		t.Error("the block says nothing about the rows the cap cut")
	}
}
