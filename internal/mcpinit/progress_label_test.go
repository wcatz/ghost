package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// hostileName holds a newline, a carriage return and an ESC byte, the way a
// legacy row written by an older build can. It must never reach init output raw.
const hostileName = "bad\nname\r\x1b[31mred"

func TestWriteRedirectsProgressLinePerProject(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	projectPath := filepath.Join(home, "proj")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}

	projects := []projectInfo{
		{ID: "hostile", Path: projectPath, Name: hostileName},
		{ID: "plain", Path: filepath.Join(home, "plainproj"), Name: "plainproject"},
	}
	if err := os.MkdirAll(projects[1].Path, 0o755); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, true)

	got := out.String()
	if n := strings.Count(got, "\n"); n != len(projects) {
		t.Fatalf("expected one progress line per project (%d), got %d newlines:\n%s", len(projects), n, got)
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("raw ESC reached init output:\n%s", got)
	}
	if strings.Contains(got, "\r") {
		t.Errorf("raw CR reached init output:\n%s", got)
	}
	// The hostile name's embedded newline must not have become a line break.
	if strings.Contains(got, "bad\nname") {
		t.Errorf("embedded newline reached init output:\n%s", got)
	}
	if !strings.Contains(got, "plainproject — would create redirect") {
		t.Errorf("plain project name did not print unchanged:\n%s", got)
	}
}

func TestImportMemoriesProgressLinePerProject(t *testing.T) {
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dataDir := filepath.Join(dataHome, "ghost")
	dbPath := storeAt(t, dataDir)

	hostilePath := filepath.Join(root, "hostileproj")
	plainPath := filepath.Join(root, "plainproj")
	mkdirAll(t, hostilePath)
	mkdirAll(t, plainPath)

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, "hostile", hostilePath, hostileName); err != nil {
		t.Fatalf("insert hostile project: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, "plain", plainPath, "plainproject"); err != nil {
		t.Fatalf("insert plain project: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	fakeHome := filepath.Join(root, "home")
	setHome(t, fakeHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	var out bytes.Buffer
	if _, err := importMemories(&out, true); err != nil {
		t.Fatalf("importMemories: %v\n%s", err, out.String())
	}

	got := out.String()
	if n := strings.Count(got, "\n"); n != 2 {
		t.Fatalf("expected one progress line per project (2), got %d newlines:\n%s", n, got)
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("raw ESC reached init output:\n%s", got)
	}
	if strings.Contains(got, "\r") {
		t.Errorf("raw CR reached init output:\n%s", got)
	}
	if strings.Contains(got, "bad\nname") {
		t.Errorf("embedded newline reached init output:\n%s", got)
	}
	if !strings.Contains(got, "plainproject — would scan for importable memories") {
		t.Errorf("plain project name did not print unchanged:\n%s", got)
	}
}
