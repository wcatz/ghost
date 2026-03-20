package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrep_FindPattern(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc hello() {}\n"), 0o644)

	input, _ := json.Marshal(grepInput{Pattern: "hello"})
	r := execGrep(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if !strings.Contains(r.Content, "hello") {
		t.Errorf("expected match for 'hello', got: %s", r.Content)
	}
}

func TestGrep_NoMatches(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644)

	input, _ := json.Marshal(grepInput{Pattern: "nonexistent_xyz"})
	r := execGrep(context.Background(), dir, input)
	// Exit code 1 = no matches, not an error.
	if r.IsError {
		t.Fatalf("no matches should not be error: %s", r.Content)
	}
	if r.Content != "no matches found" {
		t.Errorf("expected 'no matches found', got: %q", r.Content)
	}
}

func TestGrep_WithGlobFilter(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("func hello() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "data.txt"), []byte("hello world\n"), 0o644)

	input, _ := json.Marshal(grepInput{Pattern: "hello", Glob: "*.go"})
	r := execGrep(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if !strings.Contains(r.Content, "main.go") {
		t.Errorf("expected main.go in results: %s", r.Content)
	}
	// data.txt should be filtered out by glob.
	if strings.Contains(r.Content, "data.txt") {
		t.Errorf("data.txt should be filtered out by *.go glob")
	}
}

func TestGrep_PathEscape(t *testing.T) {
	dir := t.TempDir()

	input, _ := json.Marshal(grepInput{Pattern: "test", Path: "../../etc"})
	r := execGrep(context.Background(), dir, input)
	if !r.IsError {
		t.Fatal("expected error for path escape")
	}
}

func TestGrep_MaxResultsDefault(t *testing.T) {
	dir := t.TempDir()
	// Create file with many matches.
	var content strings.Builder
	for i := 0; i < 100; i++ {
		content.WriteString("match_line\n")
	}
	os.WriteFile(filepath.Join(dir, "data.txt"), []byte(content.String()), 0o644)

	input, _ := json.Marshal(grepInput{Pattern: "match_line"})
	r := execGrep(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	// Default max_results is 50, so we should get at most 50 matches.
	lines := strings.Count(r.Content, "\n") + 1
	if lines > 50*3 { // 50 results * max 3x for context
		t.Errorf("expected at most ~150 lines, got %d", lines)
	}
}

func TestGrep_WithContext(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("line1\nline2\ntarget\nline4\nline5\n"), 0o644)

	input, _ := json.Marshal(grepInput{Pattern: "target", Context: 1})
	r := execGrep(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if !strings.Contains(r.Content, "line2") || !strings.Contains(r.Content, "line4") {
		t.Errorf("expected context lines, got: %s", r.Content)
	}
}

func TestGrep_SpecificFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("needle\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("needle\n"), 0o644)

	input, _ := json.Marshal(grepInput{Pattern: "needle", Path: "a.txt"})
	r := execGrep(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if strings.Contains(r.Content, "b.txt") {
		t.Error("should only search a.txt")
	}
}

func TestGrep_InvalidJSON(t *testing.T) {
	r := execGrep(context.Background(), "/tmp", json.RawMessage(`{bad`))
	if !r.IsError {
		t.Fatal("expected error for invalid JSON")
	}
}
