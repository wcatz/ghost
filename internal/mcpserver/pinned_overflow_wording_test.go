package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

func TestPinnedOverflowNoticeAndHealthPinLowestByImportance(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	dbPath := dir + "/ghost.db"
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "proj", dir+"/p", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// 16 project rows with distinct importances, pinned
	for i := 0; i < 16; i++ {
		id := fmt.Sprintf("p%03d", i)
		imp := float32(0.9 - float32(i)*0.01) // p000 highest, p015 lowest
		content := strings.Repeat("x", 20)
		stamp := fmt.Sprintf("2026-01-01 00:00:%02d", i%60)
		if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at) VALUES (?, 'proj', 'fact', ?, 'manual', ?, 1, ?, ?)`, id, content, imp, stamp, stamp); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	// Pin-time notice should use new wording and list lowest-importance pin
	srv := &Server{store: s}
	note := srv.pinnedOverflowNotice(ctx, "proj")
	if note == "" {
		t.Fatalf("expected notice")
	}
	if !strings.Contains(note, "lowest-ranked by importance") || !strings.Contains(note, "most at risk of being cut") {
		t.Fatalf("notice missing required wording: %s", note)
	}
	// Lowest importance is p015
	if !strings.Contains(note, "p015") {
		t.Fatalf("notice should list lowest-importance pin p015: %s", note)
	}

	// Health should also use wording
	rd := &diagnosticRead{store: s}
	overflows, err := srv.pinnedOverflowsForHealth(ctx, rd)
	if err != nil {
		t.Fatalf("pinnedOverflowsForHealth: %v", err)
	}
	if len(overflows) != 1 {
		t.Fatalf("expected 1 overflow, got %d", len(overflows))
	}
	if len(overflows[0].CutIDs) == 0 {
		t.Fatalf("expected CutIDs")
	}
	if overflows[0].CutIDs[0] != "p015" {
		t.Fatalf("health CutIDs[0]=%s want p015", overflows[0].CutIDs[0])
	}
}

func TestPinnedOverflowNoticePinLowestFailsIfReversed(t *testing.T) {
	// This test is designed to show the correct behavior with pinnedLess as-is.
	// If pinnedLess were reversed, the lowest-ranked by importance would change.
	// We assert the id of lowest importance is listed.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	dbPath := dir + "/ghost.db"
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "proj", dir+"/p", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	// Create 16 with p000 highest importance, p015 lowest
	for i := 0; i < 16; i++ {
		id := fmt.Sprintf("p%03d", i)
		imp := float32(0.99 - float32(i)*0.01)
		content := strings.Repeat("y", 15)
		stamp := time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC).Format(time.RFC3339)
		if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at) VALUES (?, 'proj', 'fact', ?, 'manual', ?, 1, ?, ?)`, id, content, imp, stamp, stamp); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	srv := &Server{store: s}
	note := srv.pinnedOverflowNotice(ctx, "proj")
	if note == "" {
		t.Fatalf("expected notice")
	}
	// The lowest importance is p015; with correct ordering it must be listed
	if !strings.Contains(note, "p015") {
		t.Fatalf("FAIL: notice does not list lowest-importance pin p015 when ordering is correct by importance desc? note=%s", note)
	}
}

func TestPinnedOverflowHealthListsLowestByImportance(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	db, err := memory.OpenDB(dir + "/g.db")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "proj", dir+"/p", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for i := 0; i < 16; i++ {
		id := fmt.Sprintf("z%03d", i)
		imp := float32(0.8 - float32(i)*0.02)
		if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, created_at, updated_at) VALUES (?, 'proj', 'fact', 'x', 'manual', ?, 1, '2026-01-01', '2026-01-01')`, id, imp); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	srv := &Server{store: s}
	rd := &diagnosticRead{store: s}
	ovs, err := srv.pinnedOverflowsForHealth(ctx, rd)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(ovs) == 0 || len(ovs[0].CutIDs) == 0 {
		t.Fatalf("bad")
	}
	if ovs[0].CutIDs[0] != "z015" {
		t.Fatalf("want z015, got %s", ovs[0].CutIDs[0])
	}
}
