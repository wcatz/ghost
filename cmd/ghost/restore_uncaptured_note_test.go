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
		VALUES (?, ?, 'fact', 'a later edit the restore reverts', 'mcp', 0.5, 'project')`, memoryID, project); err != nil {
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

// TestUncapturedCountStaysOnCoveredRowsOnly is the discriminator for the count
// the note prints: a pinned, a persistent and a promoted marker-0 row must not
// be counted, because the restore never touches them — only a marker-0 row the
// restore actually covers does, so a pre-column snapshot of a project that
// fenced off rows through those exempts does not over-report the gap.
func TestUncapturedCountStaysOnCoveredRowsOnly(t *testing.T) {
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
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '/tmp/g', '_global')`); err != nil {
		// _global may already exist on a seeded store; ignore a unique-key clash.
		if !strings.Contains(err.Error(), "UNIQUE") {
			t.Fatalf("plant _global: %v", err)
		}
	}
	const snap = "snap-legged"
	plant := func(id, proj, source string, pinned int, retention string, body string) {
		if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, resolved_at, retention)
			VALUES (?, ?, 'fact', ?, ?, 0.5, ?, NULL, ?)`, id, proj, body, source, pinned, retention); err != nil {
			t.Fatalf("plant memory %s: %v", id, err)
		}
	}
	plantSnap := func(id, source, body string, marker int) {
		if _, err := db.Exec(`INSERT INTO memory_snapshots (snapshot_id, project_id, category, content, importance, source, memory_id, retention, retention_captured)
			VALUES (?, ?, 'fact', ?, 0.5, ?, ?, NULL, ?)`, snap, project, body, source, id, marker); err != nil {
			t.Fatalf("plant snapshot %s: %v", id, err)
		}
	}

	// A covered marker-0 row: pinned=0, retention != 'persistent', live in the
	// project. This is the one row the restore rewrites in place.
	const body = "a coverable row the restore rewrites"
	plant("mcover", project, "mcp", 0, "project", "a later edit the restore reverts")
	plantSnap("mcover", "mcp", body, 0)

	// Pinned: live, but the UPDATE predicate (pinned = 0) excludes it.
	plant("mpinned", project, "mcp", 1, "project", "a pinned row the restore never touches")
	plantSnap("mpinned", "mcp", "a pinned row the restore never touches", 0)

	// Persistent: live, but retentionExemptSQL excludes it.
	plant("mpersist", project, "mcp", 0, "persistent", "a persistent row the restore never touches")
	plantSnap("mpersist", "mcp", "a persistent row the restore never touches", 0)

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
		t.Fatalf("the one coverable row was not restored: stdout=%s stderr=%s", out.String(), errOut.String())
	}
	const note = "Note: the snapshot predates the retention column: 1 rows recorded no tier, so their lifetime was not restored"
	if !strings.Contains(combined, note) {
		t.Fatalf("the note under-counted or over-counted: want exactly %q\nstdout=%s stderr=%s", note, out.String(), errOut.String())
	}
}
