package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestGlob_MatchGoFiles(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), []byte("package main"))
	writeFile(t, filepath.Join(dir, "util.go"), []byte("package main"))
	writeFile(t, filepath.Join(dir, "readme.md"), []byte("# hi"))

	input, _ := json.Marshal(globInput{Pattern: "*.go"})
	r := execGlob(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if !strings.Contains(r.Content, "main.go") || !strings.Contains(r.Content, "util.go") {
		t.Errorf("expected both .go files, got: %s", r.Content)
	}
	if strings.Contains(r.Content, "readme.md") {
		t.Error("should not match .md files")
	}
}

func TestGlob_DoubleStarPattern(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "pkg", "sub")
	mkdirAll(t, sub)
	writeFile(t, filepath.Join(sub, "deep.go"), []byte("package sub"))
	writeFile(t, filepath.Join(dir, "top.go"), []byte("package main"))

	input, _ := json.Marshal(globInput{Pattern: "**/*.go"})
	r := execGlob(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if !strings.Contains(r.Content, "deep.go") {
		t.Errorf("expected deep.go in results: %s", r.Content)
	}
	if !strings.Contains(r.Content, "top.go") {
		t.Errorf("expected top.go in results: %s", r.Content)
	}
}

func TestGlob_SkipsGitDir(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	mkdirAll(t, gitDir)
	writeFile(t, filepath.Join(gitDir, "config"), []byte("gitconfig"))
	writeFile(t, filepath.Join(dir, "main.go"), []byte("package main"))

	input, _ := json.Marshal(globInput{Pattern: "*"})
	r := execGlob(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if strings.Contains(r.Content, "config") {
		t.Error(".git contents should be skipped")
	}
}

func TestGlob_SkipsNodeModules(t *testing.T) {
	dir := t.TempDir()
	nm := filepath.Join(dir, "node_modules", "pkg")
	mkdirAll(t, nm)
	writeFile(t, filepath.Join(nm, "index.js"), []byte("module.exports"))
	writeFile(t, filepath.Join(dir, "app.js"), []byte("const x = 1"))

	input, _ := json.Marshal(globInput{Pattern: "*.js"})
	r := execGlob(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if strings.Contains(r.Content, "index.js") {
		t.Error("node_modules contents should be skipped")
	}
	if !strings.Contains(r.Content, "app.js") {
		t.Error("expected app.js")
	}
}

func TestGlob_NoMatches(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "file.txt"), []byte("hi"))

	input, _ := json.Marshal(globInput{Pattern: "*.xyz"})
	r := execGlob(context.Background(), dir, input)
	if r.IsError {
		t.Fatal("no matches should not be an error")
	}
	if r.Content != "no files found matching pattern" {
		t.Errorf("expected no-match message, got: %q", r.Content)
	}
}

func TestGlob_SortByModTime(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "old.go")
	newer := filepath.Join(dir, "new.go")

	writeFile(t, older, []byte("package old"))
	// Set older file to past time.
	past := time.Now().Add(-1 * time.Hour)
	if err := os.Chtimes(older, past, past); err != nil {
		t.Fatal(err)
	}
	writeFile(t, newer, []byte("package new"))

	input, _ := json.Marshal(globInput{Pattern: "*.go"})
	r := execGlob(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}

	lines := strings.Split(r.Content, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}
	// Newest first.
	if lines[0] != "new.go" {
		t.Errorf("first file should be new.go (newest), got %q", lines[0])
	}
	if lines[1] != "old.go" {
		t.Errorf("second file should be old.go (older), got %q", lines[1])
	}
}

func TestGlob_WithSubPath(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "src")
	mkdirAll(t, sub)
	writeFile(t, filepath.Join(sub, "app.go"), []byte("package src"))
	writeFile(t, filepath.Join(dir, "root.go"), []byte("package main"))

	input, _ := json.Marshal(globInput{Pattern: "*.go", Path: "src"})
	r := execGlob(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if !strings.Contains(r.Content, "app.go") {
		t.Error("expected app.go from src subdir")
	}
	if strings.Contains(r.Content, "root.go") {
		t.Error("root.go should not appear when searching in src/")
	}
}

func TestGlob_PathEscape(t *testing.T) {
	dir := t.TempDir()

	input, _ := json.Marshal(globInput{Pattern: "*", Path: "../../etc"})
	r := execGlob(context.Background(), dir, input)
	if !r.IsError {
		t.Fatal("expected error for path escape")
	}
}

func TestGlob_InvalidJSON(t *testing.T) {
	r := execGlob(context.Background(), "/tmp", json.RawMessage(`{broken`))
	if !r.IsError {
		t.Fatal("expected error for invalid JSON")
	}
}
