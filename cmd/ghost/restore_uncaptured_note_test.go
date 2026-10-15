package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestReflectRestorePrintsTheUncapturedRetentionNote asserts that the note
// naming legacy rows is printed, and with the count of the rows the restore
// actually covered: a pinned, persistent or promoted marker-0 row is not
// counted, because the restore never touched it (#1341).
func TestReflectRestorePrintsTheUncapturedRetentionNote(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)
	dataDir := filepath.Join(dataHome, "ghost")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	db, err := memory.OpenDB(filepath.Join(dataDir, "ghost.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	const project = "myproj"
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, project, "/tmp/x", project); err != nil {
		t.Fatalf("plant project: %v", err)
	}
	const memoryID = "mcov"
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, retention)
		VALUES (?, ?, 'fact', 'a session fact recorded before the tier column', 'mcp', 0.5, 'project')`, memoryID, project); err != nil {
		t.Fatalf("plant memory: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source, memory_id, retention, retention_captured)
		VALUES (?, ?, 'fact', 'a session fact recorded before the tier column', 0.5, 'mcp', ?, NULL, 0)`, project+"-snap", project, memoryID); err != nil {
		t.Fatalf("plant snapshot: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	origArgs := os.Args
	os.Args = []string{origArgs[0], "reflect", project, "--restore"}
	defer func() { os.Args = origArgs }()

	var out, errOut strings.Builder
	captureOutput(t, &out, &errOut, runReflect)
	combined := out.String() + errOut.String()
	if !strings.Contains(combined, "Restored 1 memories from snapshot") {
		t.Fatalf("the restore did not run: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	const note = "Note: the snapshot predates the retention column: 1 rows recorded no tier, so their lifetime was not restored"
	if !strings.Contains(combined, note) {
		t.Fatalf("the note was not printed, or its count was wrong: want %q\nstdout=%q stderr=%q", note, out.String(), errOut.String())
	}
}
