package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/claudeimport"
	"github.com/wcatz/ghost/internal/memory"
)

// tokenShapedName is a stored project name shaped like a source-host access
// token, the way a row written by an older build can be.
const tokenShapedName = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"

func TestImportMemoriesWithholdsTokenShapedProjectName(t *testing.T) {
	root := t.TempDir()
	dataHome := filepath.Join(root, "data")
	dbPath := storeAt(t, filepath.Join(dataHome, "ghost"))
	projPath := filepath.Join(root, "proj")
	mkdirAll(t, projPath)

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES (?, ?, ?)`, "legacy", projPath, tokenShapedName); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	setHome(t, filepath.Join(root, "home"))
	t.Setenv("XDG_DATA_HOME", dataHome)

	for _, dryRun := range []bool{true, false} {
		var out bytes.Buffer
		if _, err := importMemories(&out, dryRun); err != nil {
			t.Fatalf("importMemories: %v\n%s", err, out.String())
		}
		if strings.Contains(out.String(), tokenShapedName) {
			t.Errorf("dryRun=%v: token-shaped name reached init output:\n%s", dryRun, out.String())
		}
		if !strings.Contains(out.String(), "withheld") {
			t.Errorf("dryRun=%v: output does not say the name was withheld:\n%s", dryRun, out.String())
		}
	}
}

func TestWriteRedirectsWithholdsTokenShapedProjectName(t *testing.T) {
	home := t.TempDir()
	setHome(t, home)
	projPath := filepath.Join(home, "proj")
	mkdirAll(t, projPath)
	projects := []projectInfo{{ID: "legacy", Path: projPath, Name: tokenShapedName}}

	var out bytes.Buffer
	writeRedirects(&out, projects, false)
	if strings.Contains(out.String(), tokenShapedName) {
		t.Errorf("token-shaped name reached init output:\n%s", out.String())
	}
	target := filepath.Join(home, ".claude", "projects", claudeimport.EncodeProjectPath(projPath), "memory", "MEMORY.md")
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read redirect: %v", err)
	}
	if strings.Contains(string(data), tokenShapedName) {
		t.Errorf("token-shaped name reached the redirect file:\n%s", data)
	}

	// A second run recognises the redirect it wrote.
	out.Reset()
	writeRedirects(&out, projects, false)
	if !strings.Contains(out.String(), "redirect exists") {
		t.Errorf("second run did not recognise its own redirect:\n%s", out.String())
	}
}
